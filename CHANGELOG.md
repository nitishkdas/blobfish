# Changelog

Each release's section here becomes its GitHub release notes. "Data format"
says whether an upgrade changes the data dir (see docs/deploy.md, section 7,
for what that means for rolling back).

## [Unreleased]

Data format: **6** (was 5; adds user emails). The first start upgrades the
data dir; after that, an older binary refuses to open it, so take a backup
before upgrading if you might roll back.

### Upgrade notes

- **The default `listen` is now `127.0.0.1:8080`** (was `:8080`), so a bare
  start is no longer reachable from other machines. In containers, or if
  something on another machine connects directly, set `listen` /
  `BLOBFISH_LISTEN` to `:8080`. The official image already does.
- API error `message`s are now full sentences ("Bucket not found: docs.")
  and JSON errors name the field instead of Go types. Codes are unchanged;
  programs should match on `code`, never on `message`.
- `PUT /u/{token}/{name}` (upload links) now answers `{"size": n}` only. It
  no longer returns the stored name, which told uploaders whether someone
  else's file of that name existed.
- Share links and upload links now act for their creator: they stop working
  (404 `NoSuchLink`) while the key or user that made them is deleted or no
  longer has access, and work again if access comes back. Link listings
  show this as `"active": false`.

### Added

- User accounts: people sign in with a name and password; apps keep API
  keys. Admins manage users in the UI or at `/v1/users`.
- Optional email per user: they can sign in with it instead of their user
  name. It can be changed (the user name can't), is unique across users and
  is never logged. No mail is sent yet. When adding a user in the UI, the
  user name is filled in from the email.
- Redesigned web UI with light and dark themes, an API keys page, and
  bucket quotas entered in MB (10 MB minimum).
- `operator_name` and `privacy_url` settings, shown on upload-link pages.
- Release builds: Linux amd64/arm64 archives with checksums, a multi-arch
  image on ghcr.io, and `install.sh` (optionally sets up systemd).
- `docs/api.md` (full API reference), `docs/privacy.md` (what BlobFish
  stores and logs), `SECURITY.md`, `CONTRIBUTING.md`, `NOTICE` and
  `THIRD_PARTY_NOTICES.md`.

### Security

- Sign-in limits can no longer be bypassed by sending many guesses at
  once: each guess is counted before its password is checked.
- Flooding the sign-in limiter from many addresses can no longer erase a
  user's failure count, and an attacker can no longer lock a user out of
  networks they signed in from during the last 7 days.
- A sign-in that was checking a password while an admin reset it can no
  longer create a session with the old password.
- Two simultaneous deletes can no longer remove the last two admin keys.
- Access logs no longer contain share-link or upload-link tokens, webhook
  URL paths, or names typed into the sign-in form that match no user.
- Responses served over HTTPS (through a trusted proxy) send HSTS.
- Webhooks made by bucket admins refuse every special-purpose address
  range, including NAT64, 6to4, Teredo and benchmarking ranges.
- `/healthz` runs its disk check at most every 5 seconds.
- A warning at startup when BlobFish listens on a public address over plain
  HTTP with no trusted proxy configured.

### Fixed

- Browser access (CORS) dialog failed to open for a bucket that already had
  an allowed origin.
- Clearer, consistent wording across the web UI and upload page (see
  `docs/ui-writing.md`); errors show a written message and the request ID
  instead of raw codes.

- An upload whose metadata commit failed with an I/O error is now reported
  as `500 CommitUncertain` (it may have been stored), and an upload link
  keeps counting it toward `maxFiles`.
- Session write failures (for example on a full disk) are now logged.
- Web UI accessibility: text contrast meets WCAG AA in both themes; pages
  have a main landmark, a skip link and their own titles, and keyboard
  focus moves to the page after navigation and stays put when lists
  refresh; dialogs are named; upload progress no longer floods screen
  readers; errors stay until dismissed.
- The upload-link page has cancel and retry per file and warns before the
  tab is closed mid-upload.
- The web UI's files are served with ETags and gzip, without directory
  listings, and marked `noindex`.
