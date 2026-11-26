# Running BlobFish in production

BlobFish speaks plain HTTP. Put [Caddy](https://caddyserver.com) in front of
it: Caddy gets and renews HTTPS certificates automatically, and BlobFish
listens only on localhost.

```
internet ──https──► Caddy :443 ──http──► BlobFish 127.0.0.1:8080 ──► /var/lib/blobfish
```

## 1. Install the binary and a user

The quick way does steps 1–3 for you (it checks the download's SHA-256 and
never overwrites an existing config or data):

```sh
curl -fsSL https://raw.githubusercontent.com/nitishkdas/blobfish/main/install.sh | sh -s -- --systemd
```

By hand, with a binary from the releases page (or built from source):

```sh
sudo install -m 0755 blobfish /usr/local/bin/blobfish
sudo useradd --system --home /var/lib/blobfish --shell /usr/sbin/nologin blobfish
sudo install -d -m 0750 -o blobfish -g blobfish /var/lib/blobfish /etc/blobfish
```

## 2. Configure BlobFish

`/etc/blobfish/config.json`:

```json
{
  "listen": "127.0.0.1:8080",
  "data_dir": "/var/lib/blobfish",
  "public_url": "https://files.example.com",
  "trusted_proxies": ["127.0.0.1", "::1"],
  "max_object_size": 5368709120,
  "min_free_space": 1073741824,
  "transfer_idle_timeout": "60s",
  "max_connections": 1000,
  "log_format": "json",
  "operator_name": "Example Ltd",
  "privacy_url": "https://example.com/privacy"
}
```

- `listen` on `127.0.0.1` keeps BlobFish unreachable except through Caddy,
  so API keys never cross the network unencrypted.
- `public_url` makes presigned and share links use your HTTPS address.
- `trusted_proxies` lets BlobFish log the real client address that Caddy
  passes in `X-Forwarded-For`. Only list proxies you run: anyone listed can
  claim to be any client.
- `min_free_space` (here 1 GiB) is never used by uploads, so deletes and
  metadata updates always have room.
- `operator_name` and `privacy_url` are shown on upload-link pages, so
  people sending you files know who gets them (see
  [privacy.md](privacy.md)). Leave them out if you don't use upload links.
- BlobFish warns at startup if it listens on a public address over plain
  HTTP with no `trusted_proxies`: that setup sends passwords unencrypted.

## 3. Run it with systemd

`/etc/systemd/system/blobfish.service`:

```ini
[Unit]
Description=BlobFish object store
After=network.target

[Service]
User=blobfish
Group=blobfish
ExecStart=/usr/local/bin/blobfish -config /etc/blobfish/config.json
Restart=on-failure
# Graceful shutdown waits up to 15 s for in-flight requests.
TimeoutStopSec=30
LimitNOFILE=65536

# Hardening: BlobFish only needs its data dir.
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ReadWritePaths=/var/lib/blobfish
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged @resources
CapabilityBoundingSet=
AmbientCapabilities=
RestrictSUIDSGID=yes
RestrictRealtime=yes
ProtectClock=yes
ProtectHostname=yes
ProtectKernelLogs=yes
ProtectProc=invisible
ProcSubset=pid
PrivateUsers=yes
RemoveIPC=yes
UMask=0077

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now blobfish
sudo cat /var/lib/blobfish/initial-admin-key   # first start only: copy it somewhere safe
sudo rm /var/lib/blobfish/initial-admin-key
journalctl -u blobfish -f                      # logs (never contain the key)
```

Browser uploads straight from your app's web pages (presigned URLs) are
allowed per bucket: in the web UI open the bucket's **Settings** tab and use
**Browser access**.

## 4. Put Caddy in front

`/etc/caddy/Caddyfile`:

```caddy
files.example.com {
	reverse_proxy 127.0.0.1:8080 {
		# Stream downloads straight through instead of buffering them.
		flush_interval -1
	}
}
```

Point the DNS name at the server, open ports 80 and 443, and reload Caddy
(`sudo systemctl reload caddy`). Caddy fetches the certificate on its own.
Caddy does not limit request body size by default, so large uploads pass
through; BlobFish enforces `max_object_size` itself.

BlobFish adds `Strict-Transport-Security` (one year) to every response that
reached it over HTTPS through a trusted proxy, so browsers never fall back
to plain HTTP for this host. That is why `trusted_proxies` must list Caddy.
It compresses the web UI's files itself; don't add Caddy's `encode` for
object downloads (they are mostly compressed already, and compressing
breaks `Range` requests).

### With Docker

The image (`ghcr.io/nitishkdas/blobfish`) runs as a non-root user, listens on
`:8080` inside the container and keeps data in the `/data` volume. Publish
the port only to your proxy, never to the internet:

```yaml
# compose.yml
services:
  blobfish:
    image: ghcr.io/nitishkdas/blobfish:v1.0.0   # pin a version; upgrade on purpose
    restart: unless-stopped
    environment:
      BLOBFISH_PUBLIC_URL: https://files.example.com
      BLOBFISH_TRUSTED_PROXIES: 172.16.0.0/12      # the Docker network Caddy is on
      BLOBFISH_LOG_FORMAT: json
    volumes: [blobfish:/data]
    networks: [web]                                # shared with Caddy; no ports:
    mem_limit: 512m
volumes: { blobfish: {} }
networks: { web: { external: true } }
```

Caddy then proxies to `blobfish:8080`. The first admin key is in the
volume: `docker cp blobfish:/data/initial-admin-key -` (the image has no
shell; delete the file the same way you'd edit any volume, e.g. with a
throwaway `docker run --rm -v blobfish:/data busybox rm /data/initial-admin-key`).

## 5. Monitor

- `GET /healthz` (no key) answers 503 when the metadata store or the data
  disk fails (it writes and fsyncs a small probe file, at most once every
  5 seconds however often it is called), and `"status":"degraded"` when
  free space is down to `min_free_space`. Point your uptime monitor at
  `https://files.example.com/healthz`.
- `GET /v1/status` (admin key) shows version, uptime, free disk space and
  request/byte counters since start.
- Each access log line names who made the request (`key=app1/3f9c…`,
  `presigned`, `link:<id>`, `uplink:<id>`, `login:<user>` or `public`) and
  the real client address. Link tokens are replaced by `<token>` in logged
  paths, and webhook addresses are logged as scheme and host only, so the
  log holds no credentials. It does hold object names and IP addresses:
  set a retention (`journalctl --vacuum-time=30d`, or `MaxRetentionSec=` in
  `journald.conf`).

## 6. Back up

Backups are your only second copy on a single server, so they must leave
the machine: a backup on the same disk dies with it, and a nightly full copy
that is never deleted fills the disk (uploads then fail with 507).

`/usr/local/bin/blobfish-backup` (mode 0700, owned by root):

```sh
#!/bin/sh
# Nightly BlobFish backup: stream a snapshot to another machine, keep 14.
set -eu
key=$(cat /etc/blobfish/backup-key)          # a *:admin key, mode 0600
name="blobfish-$(date +%F).tar"
curl -fsS -H "Authorization: Bearer $key" https://files.example.com/v1/backup |
  ssh -i /root/.ssh/backup backup@backup-host "cat > backups/$name.partial && mv backups/$name.partial backups/$name"
ssh -i /root/.ssh/backup backup@backup-host \
  'ls -1t backups/blobfish-*.tar | tail -n +15 | xargs -r rm --'
```

Run it from a systemd timer (or cron at 03:00) and make the job alert you
when it fails (for example `OnFailure=` in its unit, or a dead-man's-switch
monitor pinged after the last line). Streaming over `ssh` needs no space on
the server. Any off-machine target works (`rclone rcat`, `restic backup
--stdin`, an S3 bucket); the point is a second machine and a fixed number of
copies. If you must keep copies locally, put them on a different disk and
delete old ones the same way.

Check a backup now and then by restoring it somewhere else:
`blobfish restore -from FILE -data-dir /tmp/check` reads every file back and
verifies its checksum. Backups contain key hashes, share-link tokens and the
presign secret: store them as carefully as the data, encrypted if the
target isn't yours.

Check the live data the same way every month or so. `blobfish verify` reads
every file from disk against its SHA-256 and changes nothing; the server must
be stopped while it runs (about as long as reading the whole disk once):

```sh
sudo systemctl stop blobfish
sudo -u blobfish /usr/local/bin/blobfish verify -config /etc/blobfish/config.json
sudo systemctl start blobfish
```

Exit 0 means every file matched. On exit 1 it lists each damaged file;
restore those from a backup. If any line says `unindexed`, save those files
before starting the server again, since it would delete them as leftovers.

## 7. Upgrade and roll back

1. Read the release notes (`CHANGELOG.md`): they say when a release changes
   the data format.
2. Take a backup (section 6) and keep it until the new version has run
   for a while.
3. Keep the old binary: `sudo cp /usr/local/bin/blobfish /usr/local/bin/blobfish.prev`.
4. Install the new one (re-run `install.sh`, or replace the file), then
   `sudo systemctl restart blobfish` and check `journalctl -u blobfish`
   and `/healthz`.

The data dir records its format version. A newer binary upgrades it in
place on start, all or nothing. An older binary refuses to open data in a
newer format ("data dir was written by a newer BlobFish") rather
than misreading it. So:

- **No format change** in the notes: roll back by putting `blobfish.prev`
  back and restarting.
- **Format changed**: the old binary can't read the upgraded data. Stop the
  service, move the data dir aside, restore the pre-upgrade backup into an
  empty dir with the old binary (`blobfish.prev restore -from FILE -data-dir
  /var/lib/blobfish`, then `chown -R blobfish:blobfish`), put the old binary
  back and start it. Changes made since the upgrade are lost, which is why
  step 2 matters.

With Docker, pin image versions (`:v1.0.0`, not `:latest`) so an upgrade
only happens when you change the tag; rolling back is changing it back,
with the same rule about format changes.
