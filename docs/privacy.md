# Privacy notes for operators

BlobFish is software you run yourself. Whoever runs an instance decides
what is stored in it and why, so under laws like the GDPR or India's DPDP
Act **you, the operator, are the controller** (data fiduciary) for its
data. The BlobFish project receives nothing from your server. This page
lists what BlobFish stores and logs, how long, and how to delete it, so you
can describe it in your own privacy notice. It is not legal advice.

## Nothing leaves your server unless you set it up

- No telemetry, update checks, crash reports or analytics.
- The web UI loads nothing from other sites (no CDNs, fonts or scripts);
  its Content-Security-Policy only allows this server.
- No cookies. The UI keeps its sign-in token in the browser's
  `sessionStorage`, or `localStorage` when the user ticks "remember", plus
  the theme and list preferences.
- The only outgoing requests are **webhooks** that a bucket admin adds.
  They carry the bucket, object key, size, checksum and content type, never
  file contents, who made the change or IP addresses.

## What is stored

In `<data_dir>` (the files and `meta.db`):

| Data | Kept until |
| --- | --- |
| Uploaded files, their names (keys), content type, size, checksum, time and `X-Meta-*` metadata | Deleted, then `trash_days` (default 7) in the bucket's trash, unless emptied sooner |
| Users: name, email (optional; BlobFish sends nothing to it), scopes, PBKDF2 password hash, created / password-changed / last-sign-in times | The user is deleted, or the email is removed |
| Sessions: a hash of the token, user, start, last use (updated at most every 5 minutes) and expiry. No IP address or browser details | 12 hours unused or 7 days, then purged within an hour; at once on sign-out, password change or user deletion |
| API keys: name, scopes, hash of the token | Revoked |
| Share and upload links: token, file or folder, expiry, label, and the creator's key ID and name | Revoked, or the file or bucket is deleted. A link whose creator is deleted stops working but its record stays until revoked |
| Webhooks: address, signing secret, delivery health, queued events | The webhook or bucket is deleted; undelivered events are dropped after about a day |

The first admin key is also written to `<data_dir>/initial-admin-key` until
you delete that file.

In memory only (lost on restart): failed sign-in counts per username and
network (IPv4 address or IPv6 /64) for 15 minutes, and the networks each
user signed in from during the last 7 days, so an attacker elsewhere can't
lock them out.

## What is logged

BlobFish writes one line per request to standard error, which systemd
sends to the journal (or Docker to its log driver). A line holds the time,
method, path, status, size, duration, the client's IP address, and who made
the request: an API key's name and ID, `user:<name>`, `link:<id>`,
`uplink:<id>`, `presigned`, `public`, or `login:<name>` for a sign-in
attempt (only when that user exists; otherwise `login:(unknown name)`, so
a password typed into the name field is never logged). Email addresses are
never logged: a sign-in by email is logged under the user's name.

Paths contain bucket names and object keys, which may be personal (for
example `invoices/jane-doe.pdf`). Link tokens are replaced by `<token>`,
query strings (presigned signatures) are not logged, and webhook addresses
are logged as scheme and host only. Events such as "user signed in" or
"file uploaded through upload link" are logged with the user, bucket and
key.

**BlobFish never deletes logs: set a retention period.** For the journal,
`MaxRetentionSec=30day` in `/etc/systemd/journald.conf` (or `journalctl
--vacuum-time=30d`); for Docker, the log driver's `max-size`/`max-file`;
for Caddy, its own `log` settings.

## Backups

A backup (`/v1/backup` or `blobfish backup`) holds everything in
`<data_dir>`, including files still in trash, key and password hashes, link
tokens and the presign secret. Logs are not included. Copies live as long
as you keep them, so deleting someone's data also means waiting for (or
rotating out) the backups that contain it. Say so in your notice and keep a
fixed number of backups (see [deploy.md](deploy.md#6-back-up)).

## Handling requests from people

- **Delete a person's account**: Users → Delete (or
  `DELETE /v1/users/{name}`). Their sessions end at once and links they made
  stop working; revoke those links too if their name must go
  (`GET /v1/links?bucket=…` shows `createdByName`).
- **Delete a file for good**: delete it, then delete it from the bucket's
  trash (or empty the trash). This removes the file and its share links
  immediately. Copies remain in backups and the access log until they age
  out.
- **Give someone their files**: download them in the UI or API, or share a
  link. There is no search by person: object keys are free text, so name
  files in a way that lets you find a person's data.
- **Correct data**: upload the file again under the same key.

## Upload links collect data from outsiders

An upload link lets people outside your organisation send you files, so
you owe them a notice when they send. Set `operator_name` and `privacy_url`
in the config: the upload page then says who receives the files and links
to your privacy notice. The page already tells uploaders that they can't
see, change or delete what they sent, and that the BlobFish project does
not receive their files.
