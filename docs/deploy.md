# Running BlobFish in production

BlobFish speaks plain HTTP. Put [Caddy](https://caddyserver.com) in front of
it: Caddy gets and renews HTTPS certificates automatically, and BlobFish
listens only on localhost.

```
internet ──https──► Caddy :443 ──http──► BlobFish 127.0.0.1:8080 ──► /var/lib/blobfish
```

## 1. Install the binary and a user

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
  "log_format": "json"
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

## 5. Monitor

- `GET /healthz` (no key) answers 503 when the metadata store or the data
  disk fails (it writes and fsyncs a small probe file each time), and
  `"status":"degraded"` when free space is down to `min_free_space`. Point
  your uptime monitor at `https://files.example.com/healthz`.
- `GET /v1/status` (admin key) shows version, uptime, free disk space and
  request/byte counters since start.
- Each access log line names who made the request (`key=app1/3f9c…`,
  `presigned`, `link:<id>` or `public`) and the real client address.

## 6. Back up

Backups are your only second copy on a single server. For example, nightly
with a key kept in `/etc/blobfish/backup-key` (mode 0600, a `*:admin` key):

```sh
curl -fsS -H "Authorization: Bearer $(cat /etc/blobfish/backup-key)" \
  https://files.example.com/v1/backup -o /backups/blobfish-$(date +%F).tar
```

Check a backup now and then by restoring it somewhere else:
`blobfish restore -from FILE -data-dir /tmp/check` reads every file back and
verifies its checksum. Backups contain key hashes and share-link tokens:
store them as carefully as the data.

## 7. Upgrade

Stop the service, replace `/usr/local/bin/blobfish`, start it again. The data
dir records its format version: a newer binary upgrades it in place (all or
nothing), and an older binary refuses to open data from a newer one rather
than misreading it. Take a backup before upgrading.
