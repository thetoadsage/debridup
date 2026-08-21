# DebridUp

Private, self-hosted uptime monitoring for debrid providers. The first release monitors TorBox and Premiumize with encrypted API keys, SQLite history, incidents, a dashboard, and ntfy notifications.

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
   docker compose up -d --build
   ```

Open `http://localhost:8080`. Use a TLS reverse proxy before exposing the dashboard beyond a trusted private network.

## Security model

- API keys and notification endpoints are encrypted with XChaCha20-Poly1305 before reaching SQLite.
- The encryption key is loaded from a file/secret and is never persisted in the database.
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
