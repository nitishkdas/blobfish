# Security policy

## Reporting a vulnerability

Please report security problems privately, not in a public issue: use
**[Report a vulnerability](https://github.com/nitishkdas/blobfish/security/advisories/new)**
on GitHub (Security → Advisories). Include what you found, how to reproduce
it, and the version (`blobfish -version`).

You'll get an answer within 7 days. Once a fix is released, the advisory is
published with credit to you, unless you'd rather stay anonymous. Please
give us 90 days, or until a fix is out if that is sooner, before telling
others.

## Supported versions

Until 1.0, only the latest release gets security fixes. From 1.0 on, the
latest minor release of the current major version does.

## Scope

In scope: the `blobfish` binary, its HTTP API, the web UI, the upload-link
page, `install.sh` and the release images. The project's own deployment
advice (docs/deploy.md) counts too: if following it leaves a server open,
that is a bug.

Out of scope: problems that need an admin key or shell access on the
server, missing hardening on a deployment that ignores docs/deploy.md
(for example, BlobFish listening on a public address without TLS), and
denial of service by sheer traffic volume.

## Design notes for reviewers

How BlobFish defends itself (token storage, scopes, presigned URL
signatures, CSP for served files, webhook SSRF rules, sign-in throttling)
is described in CLAUDE.md and docs/api.md.
