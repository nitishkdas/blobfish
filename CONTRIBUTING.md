# Contributing to BlobFish

Thanks for helping. BlobFish stays small on purpose, so please open an
issue to talk about a feature before writing it. Bug fixes, tests and docs
are always welcome.

## Ground rules

- Read CLAUDE.md: it explains the architecture and lists decisions that are
  settled (single node, own API with S3 only as a later optional module, no
  clustering, no virus scanning in core, Go standard library first).
- Data safety comes first. Any change to the write path needs a test that
  simulates a crash mid-write (see `internal/engine/crash_test.go`).
- Stream object bodies; never read a whole body into memory.
- Security problems: see [SECURITY.md](SECURITY.md), not public issues.

## Before sending a pull request

```sh
gofmt -l .                     # must print nothing
go vet ./...
go test -race -count=1 ./...   # full suite; -short skips the slow tests
for f in internal/webui/static/js/*.js; do node --check "$f"; done
scripts/third-party-notices.sh # if you changed dependencies; commit the result
```

Write commit messages that say why, in plain words. By sending a pull
request you agree that your contribution is licensed under the Apache
License 2.0, like the rest of the project (no CLA).

Adding a dependency needs a clear reason; it must have a permissive licence
(MIT, BSD, Apache-2.0, ISC), and its licence text goes into
THIRD_PARTY_NOTICES.md via the script above.
