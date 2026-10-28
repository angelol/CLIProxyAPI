# Passwordless local management

This fork adds an opt-in management API and dashboard mode for a proxy running
on the same computer as its user:

```yaml
host: "127.0.0.1"
port: 8317
remote-management:
  allow-remote: false
  allow-local-without-key: true
  secret-key: ""
  disable-control-panel: false
  disable-auto-update-panel: true
usage-statistics-enabled: true
redis-usage-queue-retention-seconds: 3600
```

Open `http://127.0.0.1:8317/management.html`. The dashboard connects automatically.
The API is available under `/v0/management/`, without an Authorization header.
Inference endpoints continue to require their existing API keys.

The mode requires a literal loopback address at server startup and in the current
configuration. A config reload cannot unlock an existing wildcard/LAN listener.
Enabling `allow-remote` disables passwordless access. The actual socket peer must
be loopback; forwarded-client headers are rejected. Requests must use a loopback
Host with the listener's port, and browser Origin, Referer, and Fetch Metadata
must identify this server. The panel cannot be framed and its bootstrap is not
cached. This mode deliberately trusts local processes; do not put it behind a
reverse proxy. The default management-key behavior is unchanged when it is off.

The existing panel expects a nonempty key in its browser connection state. In
local mode, the server injects a bootstrap using a public placeholder, not a
secret. That placeholder grants no access outside the guarded local mode.
Turning local mode off may require signing back into the panel with a normal key.

This v7-based fork was browser-tested with official management panel **v1.24.2**.
The current v1.26.7 panel targets `/v8/management` and is incompatible. Install
`management.html` from the v1.24.2 GitHub release in the config directory's
`static/` folder, and keep automatic panel updates disabled until a compatible
upgrade is verified. A configured `MANAGEMENT_STATIC_PATH` overrides that folder.

With management enabled, usage statistics feed the in-memory usage queue. The
configured retention is capped at one hour, and the UI consumes that queue to
build its views. Records are not retroactive or durable across server restarts.
The routing logs remain separate, persisted files.

Validation covers local/remote peers, forwarded IP spoofing, DNS rebinding Host
headers, opaque and foreign origins, same-site requests from different ports,
duplicate origins, real HTTP/TLS state, config hot toggles, usage queue activation,
panel bootstrap, and the unchanged non-HTTP key check. A fresh Chromium browser
was also used to verify automatic dashboard connection and cross-origin denial.
