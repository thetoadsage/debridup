# DebridUp on Unraid

DebridUp is published as a public multi-architecture image, so it runs on both x86-64 and ARM64 Unraid servers.

## Quick install

1. In the Unraid terminal, create separate persistent data and secret-only directories. The default template assumes your pool is called `cache`; replace `/mnt/cache` if yours has a different name.

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

The template runs DebridUp as `PUID=99` and `PGID=100`—Unraid's usual `nobody:users` ownership—uses `/mnt/cache/appdata/debridup` for SQLite, mounts the root-owned encryption key read-only from the separate `/mnt/cache/appdata/debridup-secrets` directory, keeps the container root filesystem read-only, and enables `no-new-privileges`. It drops every Linux capability before adding only `SETUID` and `SETGID` for the entrypoint's one-way user/group transition; the non-root application process retains no effective capabilities.

## Updates

Use Unraid's **Check for Updates** / update action from the Docker tab. The template tracks `ghcr.io/thetoadsage/debridup:latest`.

## Backups

Back up the appdata directory and the separate encryption-key file together using a process authorized to read both locations. Restoring the SQLite database without its matching key makes the configured provider credentials and notification URL unreadable.
