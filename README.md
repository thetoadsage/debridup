# DebridUp

Private, self-hosted uptime monitoring for debrid providers. DebridUp monitors TorBox, Premiumize, AllDebrid, Real-Debrid, Torrin, PikPak, Offcloud, Debrid-Link, EasyDebrid, Debrider, and Deepbrid with encrypted credentials, SQLite history, incidents, a dashboard, and ntfy notifications.

All authenticated checks are read-only account or history requests. Most providers accept their normal API key or token. PikPak currently requires an access token accepted by its user API; because that token can expire, replace it from the provider settings when authentication begins failing.

## Run with Docker

1. Copy `.env.example` to `.env` and set a unique admin password.
2. Create a 32-byte encryption key:

   ```sh
   install -d -m 0700 secrets
   openssl rand -base64 32 | sudo install -m 0400 -o root -g root /dev/stdin secrets/encryption_key
   ```

   The `secrets` directory stays owned by the invoking user so Git and Docker can traverse the tracked placeholder, while only `encryption_key` becomes root-owned and read-only. Compose bind-mounts the key without changing its host ownership or mode, which lets the capability-restricted startup process open it before dropping to the application user. Do not make the key group- or world-readable. If a previous setup made the directory root-owned, restore directory ownership with `sudo chown "$(id -u):$(id -g)" secrets` before running these commands. If the key already exists, use `sudo chown root:root secrets/encryption_key && sudo chmod 0400 secrets/encryption_key` instead of generating a replacement.

3. Start it:

   ```sh
   docker compose pull
   docker compose up -d
   ```

Open `http://localhost:8080`. Use a TLS reverse proxy before exposing the dashboard beyond a trusted private network.

The published image is `ghcr.io/thetoadsage/debridup:latest` for both AMD64 and ARM64 servers. Set `DEBRIDUP_IMAGE_TAG` to a published version tag when you want to pin a deployment. To update an existing installation, run `docker compose pull && docker compose up -d`.

If an older installation keeps its encryption key inside writable application data, stop DebridUp and back up the database and key together before upgrading. Copy the existing key without changing its contents to `secrets/encryption_key`, make the file `root:root` mode `0400` as shown above, and then start the updated Compose service. Confirm that login and an authenticated provider check succeed before removing the old key copy. Never generate a replacement key for an existing database: the stored provider credentials and notification URL require the original key.

To build directly from source instead, run `docker build -t debridup:local .`.

Compose keeps `/data` writable while mounting only the encryption-key file read-only at `/run/secrets/encryption_key`. The container root filesystem is read-only, `/tmp` is a temporary filesystem, all capabilities are dropped except the two needed for the entrypoint's one-way user/group transition, and `no-new-privileges` is enabled. The entrypoint opens the root-readable key on inherited file descriptor 3, removes the key path and direct-key variables from the application environment, drops privileges, and then starts DebridUp.

## Dashboard

The dashboard starts at 24 hours and supports coordinated `24h`, `7d`, and `30d` ranges. Each range is returned by one authenticated `/api/dashboard` request and updates the summary, provider pulse, provider table, latency comparison, and incidents together. Server-side buckets keep each provider series bounded:

| Range | Bucket width | Maximum points per provider |
| --- | ---: | ---: |
| 24 hours | 15 minutes | 96 |
| 7 days | 2 hours | 84 |
| 30 days | 8 hours | 90 |

Availability is the percentage of completed authenticated checks that succeeded in the selected range. p50 is the nearest-rank median latency and p95 is the nearest-rank 95th-percentile latency across those completed samples. In the provider pulse, healthy means every completed check in the bucket succeeded, degraded means the bucket contains both successes and failures, outage means every completed check failed, and unknown means no completed check exists; unknown buckets are not counted as downtime.

The dashboard refreshes every 30 seconds while its tab is visible, pauses while hidden, and refreshes immediately when the tab becomes visible again. A response more than 90 seconds old is labeled stale. If a later refresh fails, the last successful data remains visible with its age and a retry action; an initial failure shows an explicit unavailable state.

Select a provider name or pulse bucket to open its detail drawer. The drawer includes state duration, availability, p50 and p95 latency, last and slowest checks, the latest event, and recent incidents. It traps keyboard focus while open, closes with Escape or its close control, and restores focus to the invoking control. Statuses use text, symbols, and patterns in addition to color; latency charts include a text table; motion is disabled when reduced motion is requested; and the layout adapts to tablet and mobile widths.

## Runtime data and checks

Database migrations are versioned and applied transactionally at startup. Raw authenticated and public check rows are retained for 90 days by default; pruning runs at startup and after each UTC date change. Incidents and incident events remain after their raw check rows expire. Back up the SQLite database and its matching encryption key together before upgrades.

Only one check can run for a provider monitor at a time. A scheduled check that overlaps its previous run is skipped until the next interval, and a global worker limit bounds checks across all monitors. `DEBRIDUP_MAX_CONCURRENT_CHECKS` defaults to `4` and accepts values from `1` through `32`.

`GET /healthz` is a liveness check: it reports whether the HTTP process is running. `GET /readyz` also performs a bounded database query and reports whether the application is ready to serve database-backed requests. Container health checks use `/readyz`.

## Unraid

An importable Unraid Docker template is included at [`unraid/debridup.xml`](unraid/debridup.xml). It defaults to the public GHCR image, stores SQLite data in appdata, runs as Unraid's `nobody:users` (`PUID=99`, `PGID=100`), and uses a read-only root filesystem.

Use the [Unraid guide](unraid/README.md) for the short key-generation and template-import steps. Keep a separate protected backup of the encryption key: restoring a database requires its matching key.

## Security model

- API keys and notification endpoints are encrypted with XChaCha20-Poly1305 before reaching SQLite.
- The encryption key is loaded from a file/secret and is never persisted in the database.
- The container entrypoint opens the root-owned, read-only Docker secret, then drops privileges before starting the application.
- Secrets are write-only in the API and are never sent to the browser.
- The app is single-admin; its initial password is supplied through `DEBRIDUP_ADMIN_PASSWORD` and stored as an Argon2id hash.
- SQLite runs in WAL mode. Back up using SQLite's online backup mechanism, not by copying only the database file while the app is live.

## Verification and release safety

Pull requests and pushes to `main` run formatting, Go tests, race tests, static analysis, the browser-module suite, vulnerability analysis, a container build, and release-safety scanning. Container publication depends on that reusable verification workflow.

The module keeps Go 1.24 language compatibility, while the security gate uses exact Go 1.25.14 because the final Go 1.24 release cannot satisfy the unsuppressed standard-library vulnerability gate. The vulnerability scanner is pinned to `govulncheck` v1.7.0. Release-safety scanning covers tracked and staged content, filenames and binary/image metadata, commit messages and identities, and proposed change text supplied through `CHANGE_TEXT`. Optional private patterns belong in the workflow secret named `PRIVATE_PATTERNS`, one pattern per line; do not commit them.

## Environment

| Variable | Purpose |
| --- | --- |
| `DEBRIDUP_ADMIN_PASSWORD` | Required on first startup; initializes the single admin account. |
| `DEBRIDUP_ENCRYPTION_KEY_FILE` | Path to a base64-encoded 32-byte master key. |
| `DEBRIDUP_ENCRYPTION_KEY_FD` | Inherited descriptor containing the key. Container startup sets this internally; operators should normally use the read-only key-file mount. |
| `DEBRIDUP_ENCRYPTION_KEY` | Alternative master-key source for non-Docker development. |
| `DEBRIDUP_DATA_DIR` | Defaults to `./data`. |
| `DEBRIDUP_ADDR` | Defaults to `:8080`. |
| `DEBRIDUP_HISTORY_RETENTION` | Raw `check_results` retention as a Go duration; defaults to `2160h` (90 days), with a minimum of `24h`. Expired raw checks are pruned at startup and after each UTC date change; incidents and incident events are retained. |
| `DEBRIDUP_MAX_CONCURRENT_CHECKS` | Maximum checks running across all monitors; defaults to `4` and accepts `1` through `32`. |
| `PUID` / `PGID` | Optional runtime user and group IDs for bind-mounted data; Unraid template defaults to `99` / `100`. |
