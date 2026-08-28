# DebridUp

DebridUp is a private, self-hosted status dashboard for debrid services. It checks your configured providers, tracks availability and response times, records incidents, and can notify you through ntfy.

Supported providers: TorBox, Premiumize, AllDebrid, Real-Debrid, Torrin, PikPak, Offcloud, Debrid-Link, EasyDebrid, Debrider, and Deepbrid.

## What it does

- Shows whether each provider is up, degraded, slow, or unavailable.
- Tracks incidents, recoveries, availability, and p50/p95 response times.
- Displays a graph-based service history for 24 hours, 7 days, or 30 days.
- Exports readable HTML reports for 1, 7, 30, or 90 days, or all retained history.
- Sends incident and recovery notifications through ntfy.
- Encrypts provider credentials and notification URLs before storing them in SQLite.
- Runs on AMD64 and ARM64 with Docker Compose or the included Unraid template.

Authenticated checks use read-only account or history endpoints. PikPak requires a compatible access token, which may need to be replaced when it expires.

## Docker quick start

1. Copy the example environment file and set an admin password of at least 12 characters:

   ```sh
   cp .env.example .env
   ```

2. Create the encryption key:

   ```sh
   install -d -m 0700 secrets
   openssl rand -base64 32 | sudo install -m 0400 -o root -g root /dev/stdin secrets/encryption_key
   ```

3. Start DebridUp:

   ```sh
   docker compose pull
   docker compose up -d
   ```

Open `http://localhost:8080`, sign in, and add providers under **Settings → Provider settings**.

Use a TLS reverse proxy before making DebridUp available outside a trusted private network. The published image is `ghcr.io/thetoadsage/debridup:latest`.

> [!IMPORTANT]
> Back up `/data` and `secrets/encryption_key` together. Never replace the encryption key for an existing database—stored provider credentials and notification settings cannot be recovered without the matching key.

## Using DebridUp

- **Dashboard** gives an at-a-glance health summary.
- **Incidents** lists current and recovered incidents.
- **Providers** shows the latest status and response time for every configured service.
- **Service history** shows availability, status changes, and latency trends.
- **Settings** manages providers, ntfy notifications, display time zone, and theme.
- **Report** downloads a self-contained HTML report for the selected period.

The dashboard refreshes every 30 seconds while visible. Raw checks are retained for 90 days by default; incidents remain after their raw samples expire.

## Unraid

An importable template is included at [`unraid/debridup.xml`](unraid/debridup.xml).

Follow the [Unraid installation guide](unraid/README.md) to create the encryption key and import the template. The default paths use `/mnt/cache/appdata`; if your appdata share uses another pool, change both template host paths to `/mnt/<pool-name>/appdata/...` before applying it.

The template uses the public multi-architecture image, stores the database in appdata, runs the application as Unraid's usual `nobody:users` account, and keeps the encryption key in a separate read-only mount.

## Updating and backups

Update Docker Compose installations with:

```sh
docker compose pull
docker compose up -d
```

On Unraid, use **Docker → Check for Updates**. Before updating, keep a consistent backup of the SQLite data and its matching encryption key.

For a live database, use SQLite's online backup mechanism instead of copying only `debridup.db`; SQLite runs in WAL mode.

## Configuration

| Variable | Purpose | Default |
| --- | --- | --- |
| `DEBRIDUP_ADMIN_PASSWORD` | Initializes the single administrator account on first startup. | Required |
| `DEBRIDUP_ENCRYPTION_KEY_FILE` | Path to a base64-encoded 32-byte encryption key. | Required in Docker |
| `DEBRIDUP_DATA_DIR` | SQLite database and application-data directory. | `./data` (`/data` in Docker) |
| `DEBRIDUP_ADDR` | HTTP listen address. | `:8080` |
| `DEBRIDUP_HISTORY_RETENTION` | Raw response-history retention as a Go duration. | `2160h` (90 days) |
| `DEBRIDUP_MAX_CONCURRENT_CHECKS` | Maximum provider checks running at once, from 1 to 32. | `4` |
| `PUID` / `PGID` | Runtime user and group IDs for bind-mounted data. | Image user; Unraid uses `99` / `100` |

`GET /healthz` checks process health. `GET /readyz` also confirms that the database is ready.

## Security notes

- Provider credentials and ntfy URLs are encrypted with XChaCha20-Poly1305.
- Secrets are write-only and are never returned to the browser.
- Sessions are stored server-side and expire after 24 hours.
- The supplied container runs with a read-only root filesystem, a writable temporary directory, reduced Linux capabilities, and `no-new-privileges`.

To build locally instead of using the published image:

```sh
docker build -t debridup:local .
```
