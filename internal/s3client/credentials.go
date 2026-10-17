// SPDX-License-Identifier: Apache-2.0

package s3client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/smithy-go"
)

// CredentialKind says whether a mount's credentials will survive the mount.
//
// lith does not build credentials itself: it takes the AWS SDK's default chain, and the SDK's
// credentials cache re-retrieves only credentials that REPORT an expiry. That covers every source
// that knows how to renew itself -- an instance role, IRSA / Pod Identity, an SSO or assume-role
// profile, a credential_process that returns an Expiration -- and it covers nothing else. So the
// question that matters for a mount that runs for days is not "which source" but which of these.
type CredentialKind string

const (
	// CredAnonymous is --no-sign-request: there is nothing to expire.
	CredAnonymous CredentialKind = "anonymous"
	// CredRefreshing reports an expiry, so the SDK renews it before it lapses.
	CredRefreshing CredentialKind = "refreshing"
	// CredStaticPermanent is a long-term access key with no session token. It does not expire.
	CredStaticPermanent CredentialKind = "static-permanent"
	// CredStaticTemporary is the gap: a SESSION TOKEN handed in statically -- exported STS
	// credentials, or a credentials file written by a SAML/SSO helper. Measured against the SDK
	// this project pins: the SDK reports CanExpire=false for both, so it never re-reads them, and
	// the mount starts failing every uncached read with EIO the moment the token lapses. STS
	// sessions run from 15 minutes to 12 hours; an HPC mount runs for days.
	CredStaticTemporary CredentialKind = "static-temporary"
)

// ClassifyCredentials is the decision, separated from retrieval so it is testable without AWS.
func ClassifyCredentials(c aws.Credentials, anonymous bool) CredentialKind {
	switch {
	case anonymous:
		return CredAnonymous
	case c.CanExpire:
		return CredRefreshing
	case c.SessionToken != "":
		return CredStaticTemporary
	default:
		return CredStaticPermanent
	}
}

// fromSharedFile reports whether static credentials came from the shared credentials/config
// file, as opposed to the environment. It matters because a FILE can be rewritten underneath a
// running process -- which is exactly what saml2aws, gimme-aws-creds and similar helpers do --
// whereas a process's environment cannot be changed from outside it.
func fromSharedFile(c aws.Credentials) bool {
	return strings.HasPrefix(c.Source, "SharedConfigCredentials")
}

// credentialRecheck is how often lith re-reads the shared credentials file when the credentials
// in it are temporary but static.
//
// DERIVED, NOT TUNED: one third of the shortest session STS issues (15 minutes, the minimum for
// both AssumeRole and GetSessionToken). Rechecking at a third of the minimum lifetime means a
// rotated file is picked up with at least two-thirds of even the shortest new session left. The
// cost of a recheck is a local file read, so erring short is cheap.
const credentialRecheck = 5 * time.Minute

// recheckEvery is credentialRecheck, as a variable only so a test can drive the refresh path
// without waiting five minutes.
var recheckEvery = credentialRecheck

// reloadingProvider re-resolves credentials from the shared config on every Retrieve, and marks
// what it returns as expiring after credentialRecheck so the SDK's cache asks again.
//
// It is the refresh path for CredStaticTemporary credentials that came from the file: the SDK
// itself reads such a file once and keeps the result forever, so a helper that rotates the file
// was never seen by a running mount. This makes the file the source of truth for the life of the
// mount instead of for its first second.
type reloadingProvider struct {
	load func(ctx context.Context) (aws.Credentials, error)
	now  func() time.Time
}

func (p *reloadingProvider) Retrieve(ctx context.Context) (aws.Credentials, error) {
	c, err := p.load(ctx)
	if err != nil {
		return aws.Credentials{}, err
	}
	// Report an expiry so the cache re-asks. The credentials themselves may last much longer;
	// this is only when lith next LOOKS, which is what a static source never got.
	c.CanExpire = true
	c.Expires = p.now().Add(recheckEvery)
	return c, nil
}

// assessCredentials retrieves the resolved credentials once, classifies them, installs the
// file-reloading refresh path where it applies, and logs what the mount can expect. It returns
// the kind and the source label for later error messages.
//
// Retrieval failure is NOT an error here: lith has always surfaced a missing credential at the
// first request, with the SDK's own message, and doctor reports it explicitly. This only adds
// information when there is something to classify.
func assessCredentials(ctx context.Context, awsCfg *aws.Config, region string, log *slog.Logger) (CredentialKind, string) {
	if awsCfg.Credentials == nil {
		return "", ""
	}
	c, err := awsCfg.Credentials.Retrieve(ctx)
	if err != nil {
		return "", ""
	}
	kind := ClassifyCredentials(c, false)
	switch {
	case kind == CredStaticTemporary && fromSharedFile(c):
		awsCfg.Credentials = aws.NewCredentialsCache(&reloadingProvider{
			now: time.Now,
			load: func(ctx context.Context) (aws.Credentials, error) {
				fresh, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
				if err != nil {
					return aws.Credentials{}, err
				}
				return fresh.Credentials.Retrieve(ctx)
			},
		})
		log.Info("credentials are temporary (session token) and come from the shared "+
			"credentials file; lith re-reads that file every "+credentialRecheck.String()+
			", so a helper that rotates it is picked up without remounting",
			"source", c.Source)
	case kind == CredStaticTemporary:
		log.Warn("credentials are TEMPORARY but STATIC: a session token from the environment, "+
			"which a running process cannot be handed a replacement for. When the token "+
			"expires this mount will fail every uncached read with EIO until it is remounted "+
			"with fresh credentials. For a long-running mount use a source that renews itself: "+
			"an instance role, IRSA / Pod Identity, an SSO or assume-role profile "+
			"(AWS_PROFILE), or a credential_process that returns an Expiration",
			"source", c.Source)
	case kind == CredRefreshing && !c.Expires.IsZero():
		log.Debug("credentials renew themselves", "source", c.Source, "expires", c.Expires)
	}
	return kind, c.Source
}

// expiredCredentialCodes are the S3/STS error codes meaning the request was signed with
// credentials that have lapsed. RequestExpired is deliberately NOT here: it means the request's
// own timestamp is stale, which is clock skew, and telling an operator their credentials expired
// when their clock is wrong would send them to fix the wrong thing.
var expiredCredentialCodes = map[string]bool{
	"ExpiredToken":          true,
	"ExpiredTokenException": true,
	"TokenRefreshRequired":  true,
}

// IsExpiredCredentials reports whether err is S3 rejecting the request's credentials as expired.
func IsExpiredCredentials(err error) bool {
	var ae smithy.APIError
	return errors.As(err, &ae) && expiredCredentialCodes[ae.ErrorCode()]
}

// credentialAlarm logs, at most once per credentialAlarmEvery, that requests are failing on
// expired credentials -- with the remedy that fits the mount's credential kind.
//
// WHY THIS EXISTS: without it an expired token is indistinguishable, from the reader's side, from
// any other S3 failure -- every uncached read returns EIO and the log says nothing specific. That
// is the outcome a long-running HPC mount on exported STS credentials hits on its first night.
type credentialAlarm struct {
	kind   CredentialKind
	source string
	log    func() *slog.Logger
	last   atomic.Int64 // unix nanos of the last report
}

const credentialAlarmEvery = time.Minute

func (a *credentialAlarm) note(err error) {
	if a == nil || !IsExpiredCredentials(err) {
		return
	}
	now := time.Now().UnixNano()
	last := a.last.Load()
	if last != 0 && time.Duration(now-last) < credentialAlarmEvery {
		return
	}
	if !a.last.CompareAndSwap(last, now) {
		return
	}
	remedy := "remount with fresh credentials, or use a source that renews itself"
	switch a.kind {
	case CredStaticTemporary:
		remedy = "these were temporary credentials handed in statically, which lith cannot " +
			"renew; " + remedy
	case CredRefreshing:
		remedy = "the credential source should have renewed these and did not -- check the " +
			"instance role / IRSA token / SSO session behind it"
	}
	a.log().Error(fmt.Sprintf("S3 rejected lith's credentials as EXPIRED; reads that are not "+
		"already cached will fail with EIO until this is fixed: %s", remedy),
		"source", a.source, "error", err)
}
