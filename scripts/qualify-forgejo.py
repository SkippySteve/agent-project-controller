#!/usr/bin/env python3
"""Run real Forgejo qualification against a disposable, loopback-only instance.

Set FORGEJO_BINARY to a verified Forgejo binary. No production credentials or
repositories are used. Run from the repository root with go on PATH.
"""
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import tempfile
import time
import urllib.request

binary = Path(os.environ['FORGEJO_BINARY']).resolve()
with tempfile.TemporaryDirectory(prefix='apc-forgejo-') as directory:
    root = Path(directory)
    root.chmod(0o700)
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        port = listener.getsockname()[1]
    config = root / 'app.ini'
    config.write_text(f'''APP_NAME = APC qualification
RUN_USER = {os.environ.get('USER', 'steve')}
RUN_MODE = prod
[server]
HTTP_ADDR = 127.0.0.1
HTTP_PORT = {port}
ROOT_URL = http://127.0.0.1:{port}/
DISABLE_SSH = true
APP_DATA_PATH = {root}/data
[database]
DB_TYPE = sqlite3
PATH = {root}/forgejo.db
[repository]
ROOT = {root}/repositories
[security]
INSTALL_LOCK = true
SECRET_KEY = {secrets.token_hex(32)}
INTERNAL_TOKEN = {secrets.token_hex(32)}
[service]
DISABLE_REGISTRATION = true
REQUIRE_SIGNIN_VIEW = true
[log]
MODE = file
ROOT_PATH = {root}/logs
LEVEL = Error
''')
    config.chmod(0o600)
    command = [str(binary), '-w', str(root), '-c', str(config)]
    def cli(*args):
        result = subprocess.run(command + list(args), capture_output=True, text=True)
        if result.returncode:
            raise RuntimeError('Fixture Forgejo command failed')
        return result.stdout
    cli('migrate')
    cli('admin', 'user', 'create', '--username', 'operator', '--password', secrets.token_urlsafe(32),
        '--email', 'operator@example.invalid', '--admin', '--must-change-password=false')
    token = cli('admin', 'user', 'generate-access-token', '--username', 'operator', '--token-name', 'qualification', '--scopes', 'all', '--raw').strip().splitlines()[-1]
    tokenfile = root / 'token'
    tokenfile.write_text(token)
    tokenfile.chmod(0o600)
    log = (root / 'server.log').open('w')
    process = subprocess.Popen(command + ['web'], stdout=log, stderr=log)
    origin = f'http://127.0.0.1:{port}'
    try:
        for _ in range(100):
            try:
                request = urllib.request.Request(origin + '/api/v1/version', headers={'Authorization': 'token ' + token})
                urllib.request.urlopen(request, timeout=1)
                break
            except Exception:
                if process.poll() is not None:
                    raise RuntimeError('Fixture Forgejo exited')
                time.sleep(.1)
        else:
            raise RuntimeError('Fixture Forgejo did not become ready: ' + (root / 'server.log').read_text()[-1500:].replace(token, '[redacted]'))
        env = dict(os.environ, APC_TEST_FORGEJO_ORIGIN=origin, APC_TEST_FORGEJO_TOKEN_FILE=str(tokenfile))
        subprocess.run(['go', 'test', '-count=1', '-v', './internal/forgejo', '-run', 'TestRealForgejo'], env=env, check=True)
    finally:
        process.terminate()
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()
        log.close()
