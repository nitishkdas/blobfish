# BlobFish HTTP API (v1)

Everything is under `/v1`, plus the key-less pages `/s/…` (share links),
`/u/…` (upload links) and `/healthz`. Request and response bodies are JSON,
except object bodies, which are raw bytes. Times are RFC 3339 in UTC.

## Authentication

Send `Authorization: Bearer <token>`. A token is either

- an **API key** (`bf_…`), made with `POST /v1/keys`, for apps; or
- a **session** (`bfs_…`), from `POST /v1/login`, for people.

There are no cookies, so there is no CSRF, and any web page may call the
API with a token it holds. Object requests may instead carry a
**presigned** signature (`?exp=…&sig=…`, from `POST /v1/presign`), and
objects in a **public** bucket can be read with no token at all.

**Scopes** are `<bucket>:<perm>`. `<bucket>` is a bucket name or `*` (every
bucket); `<perm>` is `r` (read and list), `w` (write and delete), `rw`, or
`admin` (rw plus the bucket's settings, webhooks and quota-free changes).
Server-wide actions (create buckets, keys, users, backups, status) need
`*:admin`.

A request without permission gets **403 `AccessDenied`** before anything
is looked up, so a 403 never tells whether a bucket or object exists.

## Errors

Every error has the same shape and a stable `code`:

```json
{ "error": { "code": "NoSuchKey", "message": "object not found", "requestId": "01J…" } }
```

`requestId` is also in the `X-Request-Id` header and in the server log.

| Status | Code | Meaning |
| --- | --- | --- |
| 400 | `InvalidRequest` | Malformed JSON, unknown field, bad parameter |
| 400 | `InvalidBucketName` | 3–63 chars of `a-z 0-9 - .`, starting and ending alphanumeric |
| 400 | `InvalidKey` | Empty, over 1024 bytes, not UTF-8, or contains NUL |
| 400 | `InvalidCursor` | A `cursor` not returned by the previous page |
| 400 | `MetadataTooLarge` | `X-Meta-*` values over 2 KB in total |
| 400 | `InvalidScope` | Not `bucket:r|w|rw|admin` |
| 400 | `InvalidUserName` / `InvalidPassword` / `InvalidEmail` | User name, password or email rules |
| 400 | `InvalidOrigin` | A `corsOrigins` entry isn't `scheme://host[:port]` |
| 400 | `IncompleteBody` | The client hung up mid-upload; nothing was stored |
| 400 | `NotSignedInAsUser` | Needs a user session, not an API key |
| 401 | `Unauthorized` | No token, or an unknown, revoked or expired one |
| 401 | `InvalidCredentials` | Wrong user name, email or password (never says which) |
| 403 | `AccessDenied` | The token lacks the scope |
| 403 | `SignatureInvalid` / `SignatureExpired` | Presigned URL tampered with or past `exp` |
| 403 | `CORSNotAllowed` | Browser preflight from an origin the bucket doesn't allow |
| 403 | `UploadLimitReached` | The upload link has taken `maxFiles` files |
| 404 | `NoSuchBucket` / `NoSuchKey` / `NoSuchAPIKey` / `NoSuchUser` / `NoSuchWebhook` / `NoSuchTrashItem` | Not found |
| 404 | `NoSuchLink` | Link unknown, revoked, its file deleted, or its creator removed or without access |
| 404 | `NotFound` | No such route |
| 405 | `MethodNotAllowed` | |
| 408 | `RequestTimeout` | The upload made no progress for `transfer_idle_timeout` |
| 409 | `BucketAlreadyExists` / `UserAlreadyExists` | Name taken |
| 409 | `EmailTaken` | Another user has this email |
| 409 | `BucketNotEmpty` | Delete objects and empty the trash first |
| 409 | `KeyExists` | Restore target is taken by another object |
| 409 | `LastAdminKey` | Would delete the last `*:admin` API key |
| 409 | `TooManyWebhooks` | 10 per bucket |
| 410 | `LinkExpired` | Share or upload link past its expiry |
| 412 | `PreconditionFailed` | `If-None-Match: *` and the object exists |
| 413 | `EntityTooLarge` | Over `max_object_size` (or the link's `maxFileSize`) |
| 416 | `InvalidRange` | Range outside the object |
| 429 | `TooManyAttempts` | Sign-in throttled; see `Retry-After` |
| 500 | `InternalError` | Details are in the server log under the request ID |
| 500 | `DataCorrupt` | The stored bytes failed their checksum; restore from backup |
| 500 | `CommitUncertain` | Storage reported an I/O error after writing: the object may or may not be stored; check with HEAD |
| 507 | `InsufficientStorage` | The upload would cut into `min_free_space` |
| 507 | `QuotaExceeded` | The upload would take the bucket over its quota |

## Objects

Keys may contain any UTF-8 except NUL, including `/`, spaces and `..`;
percent-encode them in the path. Folders are just key prefixes.

### `PUT /v1/b/{bucket}/{key}` (w)

Body: the bytes, streamed. Headers: `Content-Type` (stored and served
back), `X-Meta-<name>: <value>` (user metadata, names lowercased, 2 KB
total), `If-None-Match: *` (create only: 412 if the key exists). Overwrites
replace the object atomically; the old version is not kept.

```json
200 {"bucket":"docs","key":"a/b.pdf","size":48213,"etag":"9f2c…","contentType":"application/pdf","modified":"…","meta":{"customer":"acme"}}
```

`etag` is the SHA-256 of the content (also in the `ETag` header). A failed
upload stores nothing and closes the connection.

### `GET` / `HEAD /v1/b/{bucket}/{key}` (r, presigned, or public bucket)

Returns the bytes with `Content-Type`, `Content-Length`, `ETag`,
`Last-Modified`, `Accept-Ranges: bytes` and `X-Meta-*`.

- `Range: bytes=0-99` (one range; suffix and open-ended forms work) → 206.
- `If-None-Match: "<etag>"` → 304.
- `?download` or `?download=<file name>` → `Content-Disposition: attachment`.

Responses carry `Cache-Control: private, no-cache` (`public, no-cache` for
key-less reads of a public bucket): caches revalidate each time, so
revoking access takes effect at once. HTML, SVG and XML files get
`Content-Security-Policy: sandbox`, so they can't run script on this
origin. If the stored bytes fail their checksum mid-download the
connection is cut rather than finishing with bad data.

### `DELETE /v1/b/{bucket}/{key}` (w)

204, also when the key doesn't exist. With `trash_days` > 0 the object goes
to the bucket's trash.

### `GET /v1/b/{bucket}` (r): list

Query: `prefix`, `delimiter` (usually `/`), `limit` (1–1000, default 1000),
`cursor` (from the previous page).

```json
200 {"objects":[{…object…}],"prefixes":["2026/01/"],"nextCursor":"…"}
```

`nextCursor` is absent on the last page. Objects and prefixes come in key
order; `limit` counts both.

## Buckets

| Request | Scope | Body / result |
| --- | --- | --- |
| `GET /v1/buckets` | any token | `{"buckets":[{"name","created","public","corsOrigins","quotaBytes","usedBytes","objects"}]}`: the buckets the token can read |
| `PUT /v1/buckets/{bucket}` | `*:admin` | 201 |
| `DELETE /v1/buckets/{bucket}` | `*:admin` | 204; 409 `BucketNotEmpty` while objects or trash remain. Also deletes its links, upload links and webhooks |
| `PATCH /v1/buckets/{bucket}` | bucket admin | Any of `{"public":bool}`, `{"corsOrigins":["https://app.example.com"]}` (max 20, `"*"` allowed), and `{"quotaBytes":n}` (server admin only; 0 = none, else ≥ `minQuotaBytes`). Returns the bucket |

`usedBytes`/`objects` count live objects; trash doesn't count toward the
quota. CORS settings apply to object routes and share links: allowed
origins get preflight 204 and exposed `ETag`, `Content-Range`,
`X-Request-Id`.

## Presigned URLs

`POST /v1/presign` (the caller must itself hold the permission):

```json
{"bucket":"docs","key":"a/b.pdf","method":"GET","expiresIn":3600}
200 {"url":"https://files.example.com/v1/b/docs/a/b.pdf?exp=…&sig=…","method":"GET","expires":"…"}
```

`method` is `GET` (also allows HEAD) or `PUT`. `expiresIn` is 1 s to 7 days
(default 1 hour). The signature covers method, bucket, key and expiry.

## Share links

Key-less, revocable links to one object, for people.

| Request | Scope | |
| --- | --- | --- |
| `POST /v1/links` | read on the object | `{"bucket","key","expiresIn","filename"}`: `expiresIn` in seconds, 0 = never (max 10 years); `filename` = the name it is shown and saved under |
| `GET /v1/links?bucket=&key=` | read on the bucket | `{"links":[…]}`; without `bucket`, all links (`*:admin`) |
| `DELETE /v1/links/{id}` | its creator, or bucket write | 204 |
| `GET /s/{token}[/{name}]` | none | The file. `?download` forces a save |

A link: `{"id","url","bucket","key","created","expires","createdBy","createdByName","filename","active"}`.
A link acts for its creator: `active` is false, and `/s/…` answers 404
`NoSuchLink`, while the key or user that made it is deleted or can no longer
read the file. Deleting the object (or purging it from trash) deletes its
links; while it is in trash they answer 404 and only restoring that same
trash item brings them back. Overwriting keeps them.

## Upload links

Let people without a key add files to one folder, through a page at
`/u/{token}`. They can't list, read, replace or delete anything.

| Request | Scope | |
| --- | --- | --- |
| `POST /v1/uploadlinks` | bucket write | `{"bucket","folder","label","expiresIn","maxFiles","maxFileSize"}` (0 = never / no limit / server limit) |
| `GET /v1/uploadlinks?bucket=` | bucket write | `{"uploadLinks":[…]}` |
| `DELETE /v1/uploadlinks/{id}` | its creator, or bucket write | 204 |
| `GET /u/{token}` | none | The upload page |
| `GET /u/{token}/info` | none | `{"label","maxFileSize","filesLeft","expires","operator":{"name","privacyUrl"}}` (`operator` only when configured). Never reveals the bucket, folder or files |
| `PUT /u/{token}/{name}` | none | Raw body; 201 `{"size":n}` |

A taken name is stored as `name (2).ext` (or with a random suffix after 100
tries); the uploader isn't told which. Like share links, upload links stop
working (404) while their creator is deleted or can't write the bucket.

## Trash

Needs bucket write. Items: `{"id","object":{…},"deleted","purgeAt"}`.

| Request | |
| --- | --- |
| `GET /v1/trash/{bucket}?cursor=&limit=` | `{"items":[…],"nextCursor":"…"}`, newest first |
| `POST /v1/trash/{bucket}/{id}/restore` | 200 object; 409 `KeyExists` if the key is taken |
| `DELETE /v1/trash/{bucket}/{id}` | Delete forever, 204 |
| `DELETE /v1/trash/{bucket}` | Empty the trash, 204 |

## Webhooks

Bucket admin. `POST /v1/webhooks` `{"bucket","url","events"}` (events from
`object.created`, `object.deleted`, `object.restored`; default all) returns
the webhook with its `secret`, shown only this once. `GET
/v1/webhooks?bucket=` lists them with delivery health (`pending`,
`dropped`, `gaveUp`, `lastSuccess`, `lastFailure`, `lastError`,
`retryAfter`). `DELETE /v1/webhooks/{id}` removes one and its queued
events; `POST /v1/webhooks/{id}/test` queues a `ping`. Payloads, signature
and retries are described in the README. Webhooks made by non-server-admins
may only call public internet addresses.

## Users and sessions

| Request | Scope | |
| --- | --- | --- |
| `POST /v1/login` | none | `{"username","password"}`, where `username` may also be the user's email → `{"token","expires","idleTimeoutSeconds","user"}`. 401 `InvalidCredentials`; 429 `TooManyAttempts` with `Retry-After` |
| `POST /v1/logout` | session | Ends this session, 204 |
| `POST /v1/password` | session | `{"currentPassword","newPassword"}`; ends the user's other sessions |
| `GET /v1/users` | `*:admin` | `{"users":[{"name","email","scopes","created","passwordChanged","lastLogin"}]}` (`email` only when set) |
| `POST /v1/users` | `*:admin` | `{"name","email","password","scopes"}` (`email` optional) → 201 user |
| `PATCH /v1/users/{name}` | `*:admin` | Any of `{"password"}` (ends their sessions), `{"scopes"}` (applies at once), `{"email"}` (`""` removes it). All are checked first: a refused request changes nothing |
| `DELETE /v1/users/{name}` | `*:admin` | Deletes the user and their sessions; their links stop working |
| `DELETE /v1/users/{name}/sessions` | `*:admin` | Signs them out everywhere |

Names: 2–64 of `a-z 0-9 . _ -`, starting alphanumeric (input is
lowercased); a name is the user's fixed identity and never changes. Emails
are optional, one plain address of at most 254 characters with a dotted
domain, stored lowercased and unique across users; they can be changed.
Nothing is ever sent to them (no verification yet). Signing in by email or
by name counts against the same limits. Passwords: 10–256 characters, not
the name or the email. Sessions end after
12 hours unused or 7 days at most. Wrong passwords are limited per 15
minutes: 10 per name and network, 100 per name, 30 per network (IPv6 per
/64); networks the user signed in from in the last 7 days are exempt from
the per-name total.

## API keys

`*:admin`. `POST /v1/keys` `{"name","scopes"}` → 201 `{"id","name","scopes","created","key"}`
(the token, shown only once). `GET /v1/keys` lists them without tokens.
`DELETE /v1/keys/{id}` revokes one at once (409 `LastAdminKey` for the last
`*:admin` key); links it made stop working.

## Server

| Request | Scope | |
| --- | --- | --- |
| `GET /healthz` | none | `{"status":"ok|degraded|unavailable","checks":{"metadata","disk","space"}}`; 503 when metadata or disk fails. Checked at most every 5 s |
| `GET /v1/whoami` | any token | `{"keyId","name","scopes","maxObjectSize","trashDays","minQuotaBytes","user"}` |
| `GET /v1/status` | `*:admin` | Version, data format, uptime, disk free and reserve, request and byte counters |
| `GET /v1/backup` | `*:admin` | A consistent `.tar` snapshot, streamed while the server runs. Restore with `blobfish restore` |
