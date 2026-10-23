#!/usr/bin/env python3
"""Push allowed-extensions.conf to all SFTPGo users via REST API."""
import base64
import json
import os
import sys
import urllib.error
import urllib.request
from datetime import datetime, timezone


def log(msg):
    ts = datetime.now(timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
    print(f"{ts} {msg}", flush=True)


def load_credentials(path):
    creds = {}
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith('#'):
                continue
            if '=' in line:
                k, v = line.split('=', 1)
                creds[k.strip()] = v.strip()
    return creds


def load_patterns(config_path):
    patterns = []
    with open(config_path) as f:
        for line in f:
            line = line.split('#')[0].strip()
            if line:
                patterns.append(f"*.{line}")
    return patterns


def api(url, method='GET', data=None, token=None, auth=None):
    req = urllib.request.Request(url, method=method)
    if token:
        req.add_header('Authorization', f'Bearer {token}')
    elif auth:
        encoded = base64.b64encode(f"{auth[0]}:{auth[1]}".encode()).decode()
        req.add_header('Authorization', f'Basic {encoded}')
    if data is not None:
        req.add_header('Content-Type', 'application/json')
        req.data = json.dumps(data).encode()
    try:
        with urllib.request.urlopen(req) as resp:
            return json.load(resp)
    except urllib.error.HTTPError as e:
        body = e.read().decode()
        raise RuntimeError(f"HTTP {e.code} {method} {url}: {body}") from e


def main():
    creds_path = os.path.expanduser('~/.config/sftpgo-admin/credentials')
    creds = load_credentials(creds_path)

    base_url = creds['SFTPGO_URL'].rstrip('/')

    default_config = os.path.join(
        os.path.dirname(os.path.realpath(__file__)),
        '../allowed-extensions.conf',
    )
    config_path = os.path.realpath(sys.argv[1] if len(sys.argv) > 1 else default_config)

    patterns = load_patterns(config_path)
    log(f"loaded {len(patterns)} allowed patterns from {config_path}")

    # Authenticate: API key takes priority over user/password
    if 'SFTPGO_API_KEY' in creds:
        token = creds['SFTPGO_API_KEY']
        log("using API key authentication")
    else:
        resp = api(f"{base_url}/api/v2/token",
                   auth=(creds['SFTPGO_ADMIN'], creds['SFTPGO_PASSWORD']))
        token = resp['access_token']
        log("authenticated with admin credentials")

    # Fetch all users (paginated)
    users = []
    limit = 100
    while True:
        page = api(f"{base_url}/api/v2/users?limit={limit}&offset={len(users)}", token=token)
        users.extend(page)
        if len(page) < limit:
            break
    log(f"found {len(users)} user(s)")

    file_patterns = [{
        "path": "/",
        "allowed_patterns": patterns,
        "denied_patterns": [],
        "deny_policy": 0,
    }]

    for user in users:
        username = user['username']
        user.setdefault('filters', {})['file_patterns'] = file_patterns
        api(f"{base_url}/api/v2/users/{username}", method='PUT', data=user, token=token)
        log(f"{username}: applied {len(patterns)} allowed patterns")

    log("done")


if __name__ == '__main__':
    main()
