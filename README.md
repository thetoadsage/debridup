# DebridUp

Private, self-hosted uptime monitoring for debrid providers. DebridUp monitors TorBox, Premiumize, AllDebrid, Real-Debrid, Torrin, PikPak, Offcloud, Debrid-Link, EasyDebrid, Debrider, and Deepbrid with encrypted credentials, SQLite history, incidents, a dashboard, and ntfy notifications.

All authenticated checks are read-only account or history requests. Most providers accept their normal API key or token. PikPak currently requires an access token accepted by its user API; because that token can expire, replace it from the provider settings when authentication begins failing.

## Run with Docker

1. Copy `.env.example` to `.env` and set a unique admin password.
2. Create a 32-byte encryption key:

   ```sh
   mkdir -p secrets
   openssl rand -base64 32 > secrets/encryption_key
   chmod 600 secrets/encryption_key
   ```

3. Start it:

   ```sh
   docker compose pull
   docker compose up -d
   ```

Open `http://localhost:8080`. Use a TLS reverse proxy before exposing the dashboard beyond a trusted private network.

The published image is `ghcr.io/thetoadsage/debridup:latest` for both AMD64 and ARM64 servers. Set `DEBRIDUP_IMAGE_TAG` to a published version tag when you want to pin a deployment. To update an existing installation, run `docker compose pull && docker compose up -d`.

To build directly from source instead, run `docker build -t debridup:local .`.

## Unraid

An importable Unraid Docker template is included at [`unraid/debridup.xml`](unraid/debridup.xml). It defaults to the public GHCR image, stores SQLite data in appdata, runs as Unraid's `nobody:users` (`PUID=99`, `PGID=100`), and uses a read-only root filesystem.

Use the [Unraid guide](unraid/README.md) for the short key-generation and template-import steps. Keep a separate protected backup of the encryption key: restoring a database requires its matching key.

## Security model

- API keys and notification endpoints are encrypted with XChaCha20-Poly1305 before reaching SQLite.
- The encryption key is loaded from a file/secret and is never persisted in the database.
- The container entrypoint reads the root-mounted Docker secret, then drops privileges before starting the application.
- Secrets are write-only in the API and are never sent to the browser.
- The app is single-admin; its initial password is supplied through `DEBRIDUP_ADMIN_PASSWORD` and stored as an Argon2id hash.
- SQLite runs in WAL mode. Back up using SQLite's online backup mechanism, not by copying only the database file while the app is live.

## Environment

| Variable | Purpose |
| --- | --- |
| `DEBRIDUP_ADMIN_PASSWORD` | Required on first startup; initializes the single admin account. |
| `DEBRIDUP_ENCRYPTION_KEY_FILE` | Path to a base64-encoded 32-byte master key. |
| `DEBRIDUP_ENCRYPTION_KEY` | Alternative master-key source for non-Docker development. |
| `DEBRIDUP_DATA_DIR` | Defaults to `./data`. |
| `DEBRIDUP_ADDR` | Defaults to `:8080`. |
| `DEBRIDUP_HISTORY_RETENTION` | Raw `check_results` retention as a Go duration; defaults to `2160h` (90 days), with a minimum of `24h`. Expired raw checks are pruned at startup and after each UTC date change; incidents and incident events are retained. |
| `PUID` / `PGID` | Optional runtime user and group IDs for bind-mounted data; Unraid template defaults to `99` / `100`. |
