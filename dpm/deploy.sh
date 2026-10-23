#!/usr/bin/env bash
# Build and deploy SFTPGo - DPM Edition on rack, with a database backup, a migration test on a
# copy of the database, a rollback image and health checks.
#
#   dpm/deploy.sh                 build the checked-out commit and deploy it
#   dpm/deploy.sh --yes           same, without asking (also when transfers look active)
#   dpm/deploy.sh --status        show what is running and the rollback images
#   dpm/deploy.sh --rollback TAG [--restore-db BACKUP_DB]
#                                 go back to sftpgo-dpm:TAG, optionally restoring a database
#                                 backup (needed when the failed version migrated the schema)
#
# Host settings come from dpm/.env (see dpm/.env.example). Backups go to BACKUP_PATH,
# default: the parent of DATA_PATH + /backups.
set -euo pipefail

DPM_DIR=$(cd "$(dirname "$(readlink -f "$0")")" && pwd)
REPO_DIR=$(dirname "$DPM_DIR")
IMAGE=sftpgo-dpm
CONTAINER=sftpgo
cd "$DPM_DIR"

ts() { date '+%F %T'; }
log() { echo "$(ts) $*"; }
die() { echo "$(ts) ERROR: $*" >&2; exit 1; }

[ -f .env ] || die "dpm/.env is missing, copy dpm/.env.example and fill it in"
set -a; . ./.env; set +a
: "${DATA_PATH:?DATA_PATH must be set in dpm/.env}"
: "${ACME_DOMAIN:?ACME_DOMAIN must be set in dpm/.env}"
: "${ADMIN_BIND_IP:?ADMIN_BIND_IP must be set in dpm/.env}"
: "${SFTPGO_UID:?SFTPGO_UID must be set in dpm/.env}"
: "${SFTPGO_GID:?SFTPGO_GID must be set in dpm/.env}"
BACKUP_PATH=${BACKUP_PATH:-$(dirname "$DATA_PATH")/backups}
DB="$DATA_PATH/sftpgo.db"

ASSUME_YES=0
MODE=deploy
ROLLBACK_TAG=""
RESTORE_DB=""
while [ $# -gt 0 ]; do
    case "$1" in
        --yes) ASSUME_YES=1 ;;
        --status) MODE=status ;;
        --rollback) MODE=rollback; ROLLBACK_TAG=${2:?--rollback needs an image tag}; shift ;;
        --restore-db) RESTORE_DB=${2:?--restore-db needs a backup file}; shift ;;
        *) die "unknown option $1" ;;
    esac
    shift
done

confirm() {
    [ "$ASSUME_YES" = 1 ] && return 0
    read -r -p "$1 [y/N] " answer
    [ "$answer" = y ] || [ "$answer" = Y ]
}

# sqlite online backup: consistent while SFTPGo keeps running
backup_db() {
    python3 - "$1" "$2" <<'EOF'
import sqlite3, sys
src = sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True)
dst = sqlite3.connect(sys.argv[2])
src.backup(dst)
check = dst.execute("pragma integrity_check").fetchone()[0]
version = dst.execute("select version from schema_version").fetchone()[0]
if check != "ok":
    sys.exit(f"integrity check failed: {check}")
print(version)
EOF
}

schema_version() {
    python3 -c 'import sqlite3,sys; print(sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True).execute("select version from schema_version").fetchone()[0])' "$1"
}

health_check() {
    local ok=1 i
    for i in $(seq 1 30); do
        [ "$(docker inspect -f '{{.State.Running}}' "$CONTAINER" 2>/dev/null)" = true ] && break
        sleep 1
    done
    sleep 5
    if [ "$(docker inspect -f '{{.State.Running}}' "$CONTAINER")" != true ]; then
        log "FAIL: container is not running"
        docker logs --tail 30 "$CONTAINER" || true
        return 1
    fi
    log "running: $(docker exec "$CONTAINER" sftpgo -v | head -1)"
    local web admin
    web=$(curl -sk -o /dev/null -w '%{http_code}' -H "Host: $ACME_DOMAIN" https://localhost/web/client/login || true)
    admin=$(curl -s -o /dev/null -w '%{http_code}' "http://$ADMIN_BIND_IP:8081/web/admin/login" || true)
    log "web client (443): $web, admin ($ADMIN_BIND_IP:8081): $admin"
    [ "$web" = 200 ] || ok=0
    [ "$admin" = 200 ] || ok=0
    local tool
    for tool in ffmpeg ffprobe exiftool; do
        docker exec "$CONTAINER" sh -c "command -v $tool >/dev/null" || { log "FAIL: $tool missing in the image"; ok=0; }
    done
    local errors
    errors=$(journalctl -t sftpgo --since "-1 min" --no-pager -o cat 2>/dev/null \
        | grep '"level":"error"' | grep -v '"sender":"connection_failed"' || true)
    if [ -n "$errors" ]; then
        log "errors logged since the start:"
        echo "$errors" | cut -c1-300 | head -10
        echo "$errors" | grep -q 'schema version' && ok=0
    fi
    [ "$ok" = 1 ]
}

# compose names the stack "dpm"; a container left by an older stack name must go first
remove_foreign_container() {
    local project
    project=$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' "$CONTAINER" 2>/dev/null || true)
    if [ -n "$project" ] && [ "$project" != dpm ]; then
        log "removing container $CONTAINER of the old compose project \"$project\""
        docker rm -f "$CONTAINER" >/dev/null
    fi
}

case "$MODE" in
status)
    docker ps -a --filter "name=^${CONTAINER}$" --format 'container: {{.Names}} {{.Status}} ({{.Image}})'
    docker exec "$CONTAINER" sftpgo -v 2>/dev/null | head -1 || true
    echo "schema version: $(schema_version "$DB")"
    echo "rollback images:"
    docker images "$IMAGE" --format '  {{.Repository}}:{{.Tag}}  {{.CreatedSince}}' | sort -r
    exit 0
    ;;
rollback)
    docker image inspect "$IMAGE:$ROLLBACK_TAG" >/dev/null || die "no image $IMAGE:$ROLLBACK_TAG"
    confirm "Roll back to $IMAGE:$ROLLBACK_TAG${RESTORE_DB:+ and restore $RESTORE_DB}?" || die "aborted"
    docker tag "$IMAGE:$ROLLBACK_TAG" "$IMAGE:latest"
    remove_foreign_container
    if [ -n "$RESTORE_DB" ]; then
        [ -f "$RESTORE_DB" ] || die "no backup file $RESTORE_DB"
        docker compose stop "$CONTAINER" || true
        cp -a "$DB" "$DB.before-restore-$(date +%Y%m%d-%H%M%S)"
        rm -f "$DB-wal" "$DB-shm"
        cp "$RESTORE_DB" "$DB"
        log "database restored from $RESTORE_DB (schema $(schema_version "$DB"))"
    fi
    docker compose up -d --no-build
    health_check && log "rollback OK" || die "rollback is not healthy, see above"
    exit 0
    ;;
esac

# --- deploy ---
STAMP=$(date +%Y%m%d-%H%M%S)
log "deploying $(git -C "$REPO_DIR" log -1 --format='%h %s')"
if [ -n "$(git -C "$REPO_DIR" status --porcelain --untracked-files=no)" ]; then
    git -C "$REPO_DIR" status --short --untracked-files=no
    confirm "The working tree has uncommitted changes, deploy anyway?" || die "aborted"
fi

BACKUP_DIR="$BACKUP_PATH/$STAMP"
mkdir -p "$BACKUP_DIR"
OLD_SCHEMA=$(backup_db "$DB" "$BACKUP_DIR/sftpgo.db")
cp .env "$BACKUP_DIR/env"
docker inspect "$CONTAINER" > "$BACKUP_DIR/container-inspect.json" 2>/dev/null || true
log "database backed up to $BACKUP_DIR (schema $OLD_SCHEMA, integrity ok)"

CURRENT_IMAGE=$(docker inspect -f '{{.Config.Image}}' "$CONTAINER" 2>/dev/null || true)
if [ -n "$CURRENT_IMAGE" ] && docker image inspect "$CURRENT_IMAGE" >/dev/null 2>&1; then
    docker tag "$CURRENT_IMAGE" "$IMAGE:rollback-$STAMP"
    log "running image kept as $IMAGE:rollback-$STAMP"
else
    log "WARNING: the running image ($CURRENT_IMAGE) has no tag to keep, no rollback image"
fi

log "building"
docker compose build

log "testing the database migration on a copy"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
cp "$BACKUP_DIR/sftpgo.db" "$WORK/sftpgo.db"
docker run --rm --user "$SFTPGO_UID:$SFTPGO_GID" -v "$WORK:/t" \
    -e SFTPGO_DATA_PROVIDER__DRIVER=sqlite -e SFTPGO_DATA_PROVIDER__NAME=/t/sftpgo.db \
    --entrypoint sftpgo "$IMAGE:latest" initprovider --config-dir /t \
    || die "the new build cannot open a copy of the database, nothing was deployed"
NEW_SCHEMA=$(schema_version "$WORK/sftpgo.db")
log "migration test OK: schema $OLD_SCHEMA -> $NEW_SCHEMA"

RECENT=$(journalctl -t sftpgo --since "-10 min" --no-pager -o cat 2>/dev/null \
    | grep -cE '"sender":"(Upload|Download)"' || true)
if [ "${RECENT:-0}" -gt 0 ]; then
    log "$RECENT uploads or downloads logged in the last 10 minutes, a restart interrupts running transfers"
    confirm "Restart anyway?" || die "aborted before the restart, the new image is built"
fi

log "restarting"
remove_foreign_container
docker compose up -d --no-build
if health_check; then
    log "deploy OK"
    [ "$OLD_SCHEMA" != "$NEW_SCHEMA" ] && log "the schema moved $OLD_SCHEMA -> $NEW_SCHEMA: a rollback also needs --restore-db $BACKUP_DIR/sftpgo.db"
    exit 0
fi
ROLLBACK="dpm/deploy.sh --rollback rollback-$STAMP"
[ "$OLD_SCHEMA" != "$NEW_SCHEMA" ] && ROLLBACK="$ROLLBACK --restore-db $BACKUP_DIR/sftpgo.db"
log "the new version is not healthy. To go back: $ROLLBACK"
exit 1
