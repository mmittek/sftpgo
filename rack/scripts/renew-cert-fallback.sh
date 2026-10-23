#!/usr/bin/env bash
# Fallback cert renewal for files.drpolishmatt.com (added 2026-09-04).
#
# SFTPGo's built-in ACME renewal uses HTTP-01, which needs WAN port 80
# reaching this host. If that path works, the cert is renewed ~30 days
# before expiry and this script never acts. If the cert gets within
# RENEW_DAYS of expiry anyway (built-in path broken, e.g. no port-80
# forward), this script renews via TLS-ALPN-01 on port 443, which only
# needs the existing 443 forward. Costs ~60s of sftpgo downtime.
#
# Run daily from cron. Exit 0 = nothing to do or renewed OK.
set -u

COMPOSE_DIR=/home/dpm/sftpgo/rack
RENEW_DAYS=21

cd "$COMPOSE_DIR" || exit 1
# ACME_DOMAIN, ACME_EMAIL, DATA_PATH
set -a; . ./.env; set +a

CERT="$DATA_PATH/certs/$ACME_DOMAIN.crt"
LOCK="$DATA_PATH/certs/lock"
ts() { date '+%F %T'; }

if [ ! -f "$CERT" ]; then
    echo "$(ts) ERROR: cert file $CERT not found" >&2
    exit 1
fi

# Heal a zero-byte lock file (disk-full corruption wedges all renewals,
# seen 2026-09-01: sftpgo aborts on unparseable lock).
if [ -f "$LOCK" ] && [ ! -s "$LOCK" ]; then
    echo "$(ts) WARNING: empty ACME lock file, rewriting"
    printf '0' > "$LOCK"
fi

# Cert still good for more than RENEW_DAYS? Then the built-in renewal
# still has time to do its job - stay out of the way.
if openssl x509 -checkend $((RENEW_DAYS * 86400)) -noout -in "$CERT" >/dev/null; then
    exit 0
fi

echo "$(ts) cert $ACME_DOMAIN expires within $RENEW_DAYS days; built-in HTTP-01 renewal has not succeeded - running TLS-ALPN-01 fallback"

docker compose stop sftpgo || { echo "$(ts) ERROR: failed to stop sftpgo" >&2; exit 1; }

docker run --rm --user 1000:1000 -p 443:443 \
    -v "$DATA_PATH":/var/lib/sftpgo \
    -e SFTPGO_ACME__DOMAINS="$ACME_DOMAIN" \
    -e SFTPGO_ACME__EMAIL="$ACME_EMAIL" \
    -e SFTPGO_ACME__HTTP01_CHALLENGE__PORT=0 \
    -e SFTPGO_ACME__TLS_ALPN01_CHALLENGE__PORT=443 \
    rack-sftpgo sftpgo acme run
rc=$?

# Always restart the server, even if renewal failed.
docker compose start sftpgo || { echo "$(ts) ERROR: failed to restart sftpgo" >&2; exit 1; }

if [ "$rc" -ne 0 ]; then
    echo "$(ts) ERROR: acme run failed (rc=$rc); cert NOT renewed" >&2
    exit "$rc"
fi

echo "$(ts) renewed OK:"
openssl x509 -noout -dates -in "$CERT"
