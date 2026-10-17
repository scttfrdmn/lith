// SPDX-License-Identifier: Apache-2.0

package s3client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"
)

func TestClassifyCredentials(t *testing.T) {
	cases := []struct {
		name string
		c    aws.Credentials
		anon bool
		want CredentialKind
	}{
		{"no-sign-request", aws.Credentials{}, true, CredAnonymous},
		{"instance role / IRSA / SSO: reports an expiry", aws.Credentials{AccessKeyID: "ASIA", SessionToken: "t", CanExpire: true}, false, CredRefreshing},
		{"long-term key, no session token", aws.Credentials{AccessKeyID: "AKIA"}, false, CredStaticPermanent},
		{"exported STS creds: a session token the SDK will never renew", aws.Credentials{AccessKeyID: "ASIA", SessionToken: "t"}, false, CredStaticTemporary},
	}
	for _, c := range cases {
		if got := ClassifyCredentials(c.c, c.anon); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestIsExpiredCredentials(t *testing.T) {
	for _, code := range []string{"ExpiredToken", "ExpiredTokenException", "TokenRefreshRequired"} {
		err := &smithy.GenericAPIError{Code: code}
		if !IsExpiredCredentials(err) {
			t.Errorf("%s not recognised as expired credentials", code)
		}
		if !IsExpiredCredentials(fmt.Errorf("get range: %w", err)) {
			t.Errorf("%s not recognised through a wrapped error", code)
		}
	}
	// RequestExpired is CLOCK SKEW, not credentials. Misreporting it would send an operator to
	// rotate credentials when their clock is wrong.
	if IsExpiredCredentials(&smithy.GenericAPIError{Code: "RequestExpired"}) {
		t.Error("RequestExpired (clock skew) was classified as expired credentials")
	}
	if IsExpiredCredentials(&smithy.GenericAPIError{Code: "AccessDenied"}) {
		t.Error("AccessDenied was classified as expired credentials")
	}
	if IsExpiredCredentials(errors.New("connection reset")) {
		t.Error("a transport error was classified as expired credentials")
	}
}

// isolatedAWSEnv points the SDK at test-only config files and clears every credential variable,
// so a developer's own AWS_PROFILE or exported keys cannot leak into the result -- which they did
// in the probe this test grew out of.
func isolatedAWSEnv(t *testing.T, credFile string) {
	t.Helper()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "absent-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credFile)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true") // never fall through to IMDS on a dev box
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE"} {
		if v, ok := os.LookupEnv(k); ok {
			if err := os.Unsetenv(k); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Setenv(k, v) })
		}
	}
}

func writeCreds(t *testing.T, path, keyID string) {
	t.Helper()
	body := fmt.Sprintf("[default]\naws_access_key_id=%s\naws_secret_access_key=secret\naws_session_token=tok-%s\n", keyID, keyID)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// ★ THE REFRESH PATH, END TO END THROUGH New.
//
// Temporary credentials in the shared credentials file are what a SAML/SSO helper writes, and it
// rewrites them before they lapse. The SDK reads that file ONCE and keeps the result forever
// (it reports CanExpire=false for it, verified against the pinned SDK), so before this a running
// mount never saw the rotation and failed when the first set expired. This drives the real
// constructor and reads the credentials back out of the S3 client it built -- the provider the
// requests will actually be signed with -- so a refresh path installed anywhere else, or not at
// all, fails here.
func TestRotatedCredentialsFileIsPickedUpWithoutRemounting(t *testing.T) {
	credFile := filepath.Join(t.TempDir(), "credentials")
	isolatedAWSEnv(t, credFile)
	writeCreds(t, credFile, "ASIAFIRST")
	logs := captureLog(t)

	old := recheckEvery
	recheckEvery = time.Millisecond
	t.Cleanup(func() { recheckEvery = old })

	// Region given, so New does no network I/O.
	c, err := New(context.Background(), Config{Bucket: "b", Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	signer := c.s3.Options().Credentials

	got, err := signer.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessKeyID != "ASIAFIRST" {
		t.Fatalf("first retrieval gave %q, want ASIAFIRST", got.AccessKeyID)
	}

	writeCreds(t, credFile, "ASIASECOND") // the helper rotates the file
	time.Sleep(5 * time.Millisecond)

	got, err = signer.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessKeyID != "ASIASECOND" {
		t.Errorf("after the credentials file was rotated the client still signs with %q. The "+
			"SDK reads a static credentials file once, so without a reloading provider a running "+
			"mount never sees the rotation and fails when the first set expires", got.AccessKeyID)
	}
	if !strings.Contains(logs.String(), "re-reads that file") {
		t.Errorf("the mount did not say it will re-read the credentials file; log was:\n%s", logs)
	}
}

// The case lith CANNOT fix, and so must say so loudly: a session token in the environment. No
// running process can be handed a new environment, so the only honest behaviour is a startup
// warning that names the remedy.
func TestEnvironmentSessionTokenWarnsAtStartup(t *testing.T) {
	isolatedAWSEnv(t, filepath.Join(t.TempDir(), "absent-credentials"))
	t.Setenv("AWS_ACCESS_KEY_ID", "ASIAENV")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_SESSION_TOKEN", "tok")
	logs := captureLog(t)

	c, err := New(context.Background(), Config{Bucket: "b", Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "TEMPORARY but STATIC") {
		t.Errorf("a session token from the environment produced no startup warning; log was:\n%s", out)
	}
	if c.alarm == nil || c.alarm.kind != CredStaticTemporary {
		t.Errorf("the client's alarm does not know its credentials are static-temporary (%+v), so "+
			"an expiry later would be reported without the right remedy", c.alarm)
	}
}

// A long-term key does not expire, and must not be warned about -- a warning that fires on a fine
// configuration teaches operators to ignore the one that matters.
func TestLongTermKeyDoesNotWarn(t *testing.T) {
	isolatedAWSEnv(t, filepath.Join(t.TempDir(), "absent-credentials"))
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIALONGTERM")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	logs := captureLog(t)
	if _, err := New(context.Background(), Config{Bucket: "b", Region: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("a long-term key produced a warning:\n%s", logs)
	}
}

// An expired-credentials failure is reported, once per interval, with the remedy that fits.
func TestCredentialAlarmIsSpecificAndRateLimited(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, nil))
	a := &credentialAlarm{kind: CredStaticTemporary, source: "EnvConfigCredentials", log: func() *slog.Logger { return lg }}

	expired := &smithy.GenericAPIError{Code: "ExpiredToken"}
	a.note(errors.New("connection reset")) // not credentials: silent
	a.note(expired)
	a.note(expired) // inside the interval: suppressed
	a.note(expired)

	out := buf.String()
	if n := strings.Count(out, "EXPIRED"); n != 1 {
		t.Errorf("logged %d expired-credential reports for three failures inside one interval, want "+
			"exactly 1 -- an alarm per failed read would flood the log of a mount doing thousands", n)
	}
	if !strings.Contains(out, "cannot renew") {
		t.Errorf("the report for static-temporary credentials does not say lith cannot renew them:\n%s", out)
	}
	var nilAlarm *credentialAlarm
	nilAlarm.note(expired) // anonymous clients have no alarm; must not panic
}

// ★ EVERY S3 CALL MUST REPORT TO THE ALARM, and only a structural check can see one forgotten.
// A behavioural test would need a TLS S3 endpoint returning ExpiredToken; the property is simply
// that no method of Client calls the SDK without passing its error through note().
func TestEveryS3CallReportsExpiredCredentials(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "aws.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil {
			continue
		}
		var callsS3, notes bool
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if inner, ok := sel.X.(*ast.SelectorExpr); ok {
				if x, ok := inner.X.(*ast.Ident); ok && x.Name == "c" {
					if inner.Sel.Name == "s3" {
						callsS3 = true
					}
					if inner.Sel.Name == "alarm" && sel.Sel.Name == "note" {
						notes = true
					}
				}
			}
			return true
		})
		if !callsS3 {
			continue
		}
		checked++
		if !notes {
			t.Errorf("%s calls S3 but never passes its error to c.alarm.note: an expired token on "+
				"that path would surface as a bare EIO", fn.Name.Name)
		}
	}
	if checked < 4 {
		t.Fatalf("found %d Client methods calling S3; this check is broken, not the code", checked)
	}
}
