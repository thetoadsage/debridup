# DebridUp on Unraid

DebridUp is published as a public multi-architecture image, so it runs on both x86-64 and ARM64 Unraid servers.

## Quick install

1. In the Unraid terminal, create a persistent appdata directory and encryption key. The default template assumes your pool is called `cache`; replace `/mnt/cache` if yours has a different name.

   ```sh
   mkdir -p /mnt/cache/appdata/debridup
   openssl rand -base64 32 > /mnt/cache/appdata/debridup/encryption_key
   chown -R 99:100 /mnt/cache/appdata/debridup
   chmod 600 /mnt/cache/appdata/debridup/encryption_key
   ```

2. Import the template from the Unraid terminal:

   ```sh
   wget -O /boot/config/plugins/dockerMan/templates-user/debridup.xml https://raw.githubusercontent.com/thetoadsage/debridup/main/unraid/debridup.xml
   ```

3. Open **Docker → Add Container**, choose **debridup** from the template list, set a strong admin password, and click **Apply**.

4. Open `http://YOUR-UNRAID-IP:8080` and sign in.

The template runs DebridUp as `PUID=99` and `PGID=100`—Unraid's usual `nobody:users` ownership—uses `/mnt/cache/appdata/debridup` for SQLite, mounts the encryption key read-only, and keeps the container root filesystem read-only.

## Updates

Use Unraid's **Check for Updates** / update action from the Docker tab. The template tracks `ghcr.io/thetoadsage/debridup:latest`.

## Backups

Back up the appdata directory and encryption-key file together. Restoring the SQLite database without its matching key makes the configured provider credentials and notification URL unreadable.
