# DebridUp on Unraid

DebridUp is published as a public multi-architecture image, so it runs on both x86-64 and ARM64 Unraid servers.

## Quick install

1. In the Unraid terminal, create separate persistent data and secret-only directories. The default template assumes the appdata share uses a pool named `cache`. If Unraid uses another pool, replace `/mnt/cache` with `/mnt/<pool-name>` in these commands and in both template host paths.

   ```sh
   install -d -m 0700 -o 99 -g 100 /mnt/cache/appdata/debridup
   install -d -m 0700 -o root -g root /mnt/cache/appdata/debridup-secrets
   umask 077
   openssl rand -base64 32 > /mnt/cache/appdata/debridup-secrets/encryption_key
   chown root:root /mnt/cache/appdata/debridup-secrets/encryption_key
   chmod 0400 /mnt/cache/appdata/debridup-secrets/encryption_key
   ```

2. Import the template from the Unraid terminal:

   ```sh
   wget -O /boot/config/plugins/dockerMan/templates-user/debridup.xml https://raw.githubusercontent.com/thetoadsage/debridup/main/unraid/debridup.xml
   ```

3. Open **Docker → Add Container**, choose **debridup** from the template list, set a strong admin password, and click **Apply**.

4. Open `http://YOUR-UNRAID-IP:8080` and sign in.

The template runs DebridUp as `PUID=99` and `PGID=100`—Unraid's usual `nobody:users` ownership—uses a direct pool path for SQLite, mounts the root-owned encryption key read-only from a separate directory, keeps the container root filesystem read-only, and provides a temporary `/tmp` directory for report generation. It drops every Linux capability before adding only `SETUID` and `SETGID` for the entrypoint's one-way user/group transition; the non-root application process retains no effective capabilities.

## Updates

Use Unraid's **Check for Updates** / update action from the Docker tab. The template tracks `ghcr.io/thetoadsage/debridup:latest`.

### Upgrade an existing key location

Older templates may keep the encryption key inside the writable Appdata directory. Preserve that exact key when moving to the current secret-only path:

1. Stop the DebridUp container and back up the Appdata directory and existing encryption key together.
2. Create the separate secret directory with the first `install -d` command from Quick install. Do not generate a new key.
3. Copy the existing key into the new directory as `encryption_key`, then set it to `root:root` mode `0400` with the `chown` and `chmod` commands from Quick install.
4. Update the template's **Encryption key file** host path to the new file. Keep its container target `/run/secrets/encryption_key` and access mode read-only.
5. Apply the template, confirm `/readyz` is healthy, sign in, and run an authenticated provider test.
6. Remove the old key copy only after the updated container is verified. Keep the protected backup.

The current template keeps writable database data and the root-owned key in sibling directories, mounts only the key file read-only, uses a read-only container root filesystem and temporary `/tmp`, enables `no-new-privileges`, and drops all capabilities except `SETUID` and `SETGID` for the entrypoint's one-way transition to `PUID`/`PGID`. The application process retains no effective capabilities.

## Backups

Back up the appdata directory and the separate encryption-key file together using a process authorized to read both locations. Restoring the SQLite database without its matching key makes the configured provider credentials and notification URL unreadable.

Database migrations run automatically and transactionally at startup. Raw check history defaults to 90 days; incidents remain available after their raw samples expire. Before an image update, keep a consistent SQLite backup and its matching key so both can be restored together.
