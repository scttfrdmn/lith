## Summary

<!-- What does this change and why. -->

## Related issue

<!-- e.g. Closes #12 / Refs #12 -->

## Checklist

- [ ] Commits follow Conventional Commits
- [ ] **CHANGELOG updated** under `[Unreleased]`
- [ ] `make lint` is clean (`go vet` + `golangci-lint`)
- [ ] `make test` passes (race-enabled) and new code has tests
- [ ] No network access in unit tests
- [ ] New `.go` files carry the `// SPDX-License-Identifier: Apache-2.0` header
