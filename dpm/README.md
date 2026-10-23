# dpm/ - deploying SFTPGo - DPM Edition

Everything needed to build and run this fork on a Docker host. Host-specific values and secrets
live only in `dpm/.env`, which is ignored by git; `dpm/.env.example` lists every setting.

## Files

| File | Purpose |
| :--- | :--- |
| `docker-compose.yml` | Builds the image from this repository (`sftpgo-dpm:latest`) and runs the `sftpgo` container. Compose project name `dpm`. |
| `.env.example` | Template for `dpm/.env`: ACME domain and email, the VPN address for the admin UI, host paths, run-as user, backup folder. |
| `deploy.sh` | Build, test and deploy with a database backup and a rollback image (see below). |
| `scripts/renew-cert-fallback.sh` | Daily fallback certificate renewal over TLS-ALPN-01, for when the built-in HTTP-01 renewal cannot reach port 80. |
| `scripts/apply-extensions.py` | Pushes `allowed-extensions.conf` to every user through the REST API. Reads admin credentials from `~/.config/sftpgo-admin/credentials` (outside the repository). |
| `allowed-extensions.conf` | The extension allow-list used by the script above. |

## Setting up a host

1. Install Docker with the compose plugin, Python 3 and curl.
2. Clone the repository and create the settings file:

   ```
   git clone git@github.com:mmittek/sftpgo.git && cd sftpgo
   cp dpm/.env.example dpm/.env    # then edit dpm/.env
   ```

3. Create `DATA_PATH` owned by `SFTPGO_UID:SFTPGO_GID`. On first start SFTPGo creates the
   database, host keys and certificates there.
4. Start it: `cd dpm && docker compose up -d --build`, then create the first admin at
   `http://<ADMIN_BIND_IP>:8081/web/admin`.
5. Schedule the certificate fallback in the deploy user's crontab:

   ```
   17 4 * * * <repo>/dpm/scripts/renew-cert-fallback.sh 2>&1 | logger -t sftpgo-cert
   ```

6. Optional: link the extension script, `ln -s <repo>/dpm/scripts/apply-extensions.py ~/bin/sftpgo-apply-extensions`.

The previews need ffmpeg and exiftool, which the Dockerfile installs, and `SHARE_PREVIEWS=true`,
which the compose file sets.

## Deploying a new version

```
git pull            # or check out the commit to deploy
dpm/deploy.sh
```

`deploy.sh` stops at the first problem and changes nothing it cannot undo:

1. Backs up the database with the sqlite online backup (safe while SFTPGo runs), checks its
   integrity, and saves `dpm/.env` and the current container settings next to it in
   `BACKUP_PATH/<timestamp>/`.
2. Tags the running image as `sftpgo-dpm:rollback-<timestamp>`.
3. Builds the new image.
4. Runs the new build's `sftpgo initprovider` against a copy of the backup. If the new version
   cannot open or migrate the database, the deploy stops here and the running service is untouched.
5. Warns when transfers were logged in the last 10 minutes, since the restart interrupts them.
6. Restarts the container and checks it: running, version, web client on 443, admin UI on the
   VPN address, ffmpeg, ffprobe and exiftool present, no schema errors in the log.

After a good deploy it prunes what deploys leave behind: rollback images beyond the newest 3,
database backups beyond the newest 20 and the Docker build cache above 20 GB (`KEEP_ROLLBACKS`,
`KEEP_BACKUPS`, `BUILD_CACHE_KEEP` in `dpm/.env`).

## Logs

Everything logs to one place, the host journal, which rotates and caps its own files (no
external log rotation, nothing to stall):

```
journalctl -t sftpgo -t sftpgo-deploy -t sftpgo-cert
```

`sftpgo` is the server (Docker journald driver in non-blocking mode: if the journal stalls, log
lines are dropped instead of freezing the server), `sftpgo-deploy` is `deploy.sh`,
`sftpgo-cert` the certificate fallback. Nothing is written inside the container.

`--yes` skips the questions. `--status` shows the running version, the schema version and the
rollback images.

## Rolling back

```
dpm/deploy.sh --rollback rollback-<timestamp>
```

If the failed version migrated the database schema (`deploy.sh` says so at the end of the
deploy), the older version cannot open the migrated database, so restore the backup made by
that deploy as well:

```
dpm/deploy.sh --rollback rollback-<timestamp> --restore-db <BACKUP_PATH>/<timestamp>/sftpgo.db
```

Changes made after that deploy (new shares, users) are lost by a restore; the current database
is kept as `sftpgo.db.before-restore-<timestamp>`.

## Updating from upstream

See "Keeping up with upstream" in the main README: rebase only the fork commits onto upstream
`main`, then deploy with `dpm/deploy.sh`, which tests the schema migration before restarting.
