<!--
glue is built one issue at a time. See CONTRIBUTING.md for the full
contract; this template is the short version.
-->

## Summary

<!-- One or two sentences on what this PR ships. -->

Closes #<!-- issue number; required. PRs without a linked issue should be filed against a new issue first. -->

## Verification

<!--
Paste the literal output (or a faithful summary) of the verification
commands listed in the linked issue. At minimum:

  go build ./...
  go vet ./...
  go test ./...           # or the touched packages, if you ran narrower

Live provider tests are gated behind their API keys and skipped in CI
unless you ran them locally on purpose — call that out if so.
-->

```
go build ./...        # ok
go vet ./...          # ok
go test ./...         # ok
```

## Notes for the reviewer

<!--
Optional: trade-offs, follow-ups you filed, anything non-obvious. If
this is a breaking change, lead with "**Breaking:**" and a migration
note — per ADR-0013 we only break API on minor bumps and only with a
CHANGELOG entry.
-->

<!--
Co-Authored-By trailer (drop if not applicable). The repo convention is
to credit AI pair-programmers like:

  Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
-->
