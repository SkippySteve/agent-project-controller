#!/usr/bin/env python3
"""Disposable recovery checks for one qualification workspace.

Refuses a workspace or repository whose name does not start with
qualification-. Secret material is encrypted with AES-GCM in a mode 0600 file and is never
printed. The encryption key stays on the operator machine. The script does not change CoreDNS, the broker, or any other
workspace. Forgejo itself is not stopped; revocation while Forgejo is
unavailable is covered by the controller unit tests.
"""

import argparse
import json
import os
import stat
import subprocess
import sys
import tempfile
import time
from pathlib import Path

PREFIX = "qualification-"
OWNER_LABEL = "projects.agentwork.dev/workspace-uid"


def valid_name(value, what):
    if not value.startswith(PREFIX) or len(value) > 40:
        raise SystemExit(f"{what} must start with {PREFIX}")
    rest = value[len(PREFIX):]
    if rest == "" or any(c not in "abcdefghijklmnopqrstuvwxyz0123456789-" for c in rest):
        raise SystemExit(f"{what} must be a lowercase qualification name")


def run(args, input_bytes=None, check=True):
    result = subprocess.run(["kubectl", *args], input=input_bytes, capture_output=True)
    if check and result.returncode != 0:
        # kubectl errors on these objects do not include Secret data.
        sys.stderr.write(result.stderr.decode())
        raise SystemExit(f"kubectl {' '.join(args[:4])} failed")
    return result


def workspace(namespace, name):
    valid_name(name, "workspace")
    raw = run(["-n", namespace, "get", "projectworkspace", name, "-o", "json"]).stdout
    obj = json.loads(raw)
    repo = obj.get("spec", {}).get("repository", "")
    owner, _, repo_name = repo.partition("/")
    if owner == "" or not repo_name.startswith(PREFIX):
        raise SystemExit("repository must be <owner>/qualification-...")
    others = json.loads(run(["-n", namespace, "get", "projectworkspaces", "-o", "json"]).stdout)
    for item in others.get("items", []):
        if item["metadata"]["name"] == name:
            continue
        phase = item.get("status", {}).get("phase", "")
        if phase not in ("", "Removed"):
            raise SystemExit(f"refusing while {item['metadata']['name']} is {phase or 'active'}")
    return obj


def identity(obj):
    return "apc-" + obj["metadata"]["uid"].replace("-", "")


def project_namespace(obj):
    recorded = obj.get("status", {}).get("namespace", "")
    expected = identity(obj)
    if recorded != expected:
        raise SystemExit("workspace status namespace does not match its ledger name")
    return recorded


def secret_document(namespace, secret_name):
    raw = run(["-n", namespace, "get", "secret", secret_name, "-o", "json"]).stdout
    obj = json.loads(raw)
    labels = obj.get("metadata", {}).get("labels", {})
    if OWNER_LABEL not in labels:
        raise SystemExit("ledger is missing its workspace label")
    data = obj.get("data") or {}
    for key in ("username", "password", "ca.crt", "ca.key"):
        if not data.get(key):
            raise SystemExit("ledger is missing required fields")
    return {
        "apiVersion": "v1",
        "kind": "Secret",
        "type": obj.get("type", "Opaque"),
        "metadata": {"name": secret_name, "namespace": namespace, "labels": labels},
        "data": data,
    }


BACKUP_HEADER = b'APC-LEDGER-AESGCM-1\n'


def private_key(path):
    with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW), 'rb') as handle:
        info = os.fstat(handle.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o600:
            raise SystemExit('backup key must be an owned mode 0600 regular file')
        key = handle.read(33)
    if len(key) != 32:
        raise SystemExit('backup key must contain 32 random bytes')
    return key


def write_private(document, key_path):
    from cryptography.hazmat.primitives.ciphers.aead import AESGCM
    nonce = os.urandom(12)
    encrypted = BACKUP_HEADER + nonce + AESGCM(private_key(key_path)).encrypt(
        nonce, json.dumps(document).encode(), BACKUP_HEADER)
    fd, path = tempfile.mkstemp(prefix='apc-ledger-', suffix='.encrypted')
    os.fchmod(fd, 0o600)
    with os.fdopen(fd, 'wb') as handle:
        handle.write(encrypted)
    return path


def decrypt_backup(path, key_path):
    from cryptography.hazmat.primitives.ciphers.aead import AESGCM
    raw = Path(path).read_bytes()
    if len(raw) > 1048576 or not raw.startswith(BACKUP_HEADER):
        raise ValueError('invalid encrypted backup')
    body = raw[len(BACKUP_HEADER):]
    return AESGCM(private_key(key_path)).decrypt(body[:12], body[12:], BACKUP_HEADER)


def restore_ledger(path, key_path):
    try:
        document = decrypt_backup(path, key_path)
    except Exception:
        raise SystemExit('encrypted ledger authentication failed; nothing restored') from None
    # Plaintext stays in memory and stdin. Server errors may repeat input.
    result = run(['create', '-f', '-'], input_bytes=document, check=False)
    if result.returncode:
        raise SystemExit('ledger restoration failed; credentials withheld')


def wait_until(namespace, name, predicate, timeout):
    deadline = time.time() + timeout
    last = ""
    while time.time() < deadline:
        raw = run(["-n", namespace, "get", "projectworkspace", name, "-o", "json"], check=False)
        if raw.returncode != 0:
            last = "unavailable"
        else:
            obj = json.loads(raw.stdout)
            last = obj.get("status", {}).get("phase", "")
            message = ""
            for condition in obj.get("status", {}).get("conditions", []):
                if condition.get("type") == "Ready":
                    message = condition.get("message", "")
            if predicate(obj, last, message):
                return obj
        time.sleep(3)
    raise SystemExit(f"timed out waiting for {name}; last phase {last}")


def command_status(args):
    obj = workspace(args.namespace, args.workspace)
    phase = obj.get("status", {}).get("phase", "")
    print(f"{args.workspace}\t{phase}\t{project_namespace(obj)}")


def command_controller_restart(args):
    obj = workspace(args.namespace, args.workspace)
    secret_name = identity(obj)
    ns = project_namespace(obj)
    before = run(["-n", args.namespace, "get", "secret", secret_name, "-o", "jsonpath={.metadata.uid}"]).stdout
    pvc = run(["-n", ns, "get", "pvc", "workspace", "-o", "jsonpath={.metadata.uid}"]).stdout
    if not before or not pvc:
        raise SystemExit("ledger or volume is missing")
    run(["-n", args.namespace, "rollout", "restart", "deploy/controller"])
    run(["-n", args.namespace, "rollout", "status", "deploy/controller", "--timeout=180s"])
    after = run(["-n", args.namespace, "get", "secret", secret_name, "-o", "jsonpath={.metadata.uid}"]).stdout
    pvc_after = run(["-n", ns, "get", "pvc", "workspace", "-o", "jsonpath={.metadata.uid}"]).stdout
    if before != after or pvc != pvc_after:
        raise SystemExit("controller restart replaced the ledger or the volume")
    print("controller-restart\tok")


def worker_pods(namespace, uid):
    raw = run(["-n", namespace, "get", "pods", "-l", f"{OWNER_LABEL}={uid},projects.agentwork.dev/role=worker", "-o", "json"]).stdout
    return json.loads(raw).get("items", [])


def ready_worker(items):
    ready = []
    for item in items:
        if item["metadata"].get("deletionTimestamp"):
            continue
        statuses = item.get("status", {}).get("containerStatuses", [])
        if any(c.get("name") == "workspace" and c.get("ready") for c in statuses):
            ready.append(item)
    return ready


def command_volume_marker(args):
    obj = workspace(args.namespace, args.workspace)
    if obj.get("status", {}).get("phase") != "Ready":
        raise SystemExit("volume marker requires a Ready workspace")
    ns = project_namespace(obj)
    uid = obj["metadata"]["uid"]
    current = ready_worker(worker_pods(ns, uid))
    if len(current) != 1:
        raise SystemExit("workspace does not have exactly one ready worker pod")
    pod = current[0]["metadata"]["name"]
    pod_uid = current[0]["metadata"]["uid"]
    marker = "qualification-marker"
    write = "umask 077; printf '%s\\n' " + marker + " > /workspace/" + marker
    run(["-n", ns, "exec", pod, "-c", "workspace", "--", "/bin/sh", "-c", write])
    run(["-n", ns, "delete", "pod", pod, "--wait=true"])
    deadline = time.time() + 180
    replacement = ""
    while time.time() < deadline:
        raw = run(["-n", ns, "get", "pods", "-l", f"{OWNER_LABEL}={uid},projects.agentwork.dev/role=worker", "-o", "json"], check=False)
        if raw.returncode == 0:
            running = [item for item in ready_worker(json.loads(raw.stdout).get("items", [])) if item["metadata"]["uid"] != pod_uid]
            if len(running) == 1:
                replacement = running[0]["metadata"]["name"]
                break
        time.sleep(3)
    if replacement == "":
        raise SystemExit("replacement worker did not become ready")
    shown = run(["-n", ns, "exec", replacement, "-c", "workspace", "--", "cat", f"/workspace/{marker}"]).stdout.decode().strip()
    if shown != marker:
        raise SystemExit("volume marker did not survive the worker restart")
    pvc = run(["-n", ns, "get", "pvc", "workspace", "-o", "name"]).stdout.decode().strip()
    git_id = run(["-n", ns, "get", "secret", "git-identity", "-o", "name"]).stdout.decode().strip()
    if pvc != "persistentvolumeclaim/workspace" or git_id != "secret/git-identity":
        raise SystemExit("volume or git identity disappeared")
    print("volume-marker\tok")


def command_ledger_roundtrip(args):
    obj = workspace(args.namespace, args.workspace)
    secret_name = identity(obj)
    ns = project_namespace(obj)
    path = write_private(secret_document(args.namespace, secret_name), args.backup_key)
    restored = False
    scaled_down = False
    try:
        # Stop reconciliation so a Running workspace cannot mint a replacement ledger
        # between the delete and the removal request.
        run(["-n", args.namespace, "scale", "deploy/controller", "--replicas=0"])
        scaled_down = True
        run(["-n", args.namespace, "wait", "--for=delete", "pod", "-l", "app.kubernetes.io/name=agent-project-controller", "--timeout=120s"])
        run(["-n", args.namespace, "delete", "secret", secret_name, "--wait=true"])
        run(["-n", args.namespace, "patch", "projectworkspace", args.workspace, "--type=merge", "-p", '{"spec":{"desiredState":"Removed"}}'])
        run(["-n", args.namespace, "scale", "deploy/controller", "--replicas=1"])
        scaled_down = False
        run(["-n", args.namespace, "rollout", "status", "deploy/controller", "--timeout=180s"])

        def blocked(obj, phase, message):
            return phase == "Removing" and "credential ledger missing" in message

        wait_until(args.namespace, args.workspace, blocked, 90)
        if run(["get", "namespace", ns], check=False).returncode != 0:
            raise SystemExit("namespace was deleted without the ledger")
        if run(["-n", ns, "get", "pvc", "workspace"], check=False).returncode != 0:
            raise SystemExit("volume was deleted without the ledger")
        restore_ledger(path, args.backup_key)
        restored = True

        def removed(obj, phase, message):
            return phase == "Removed"

        wait_until(args.namespace, args.workspace, removed, 240)
        deadline = time.time() + 60
        while time.time() < deadline and run(["get", "namespace", ns], check=False).returncode == 0:
            time.sleep(3)
        if run(["get", "namespace", ns], check=False).returncode == 0:
            raise SystemExit("removed workspace kept its namespace")
        recovery_complete = True
    except BaseException:
        if scaled_down:
            run(["-n", args.namespace, "scale", "deploy/controller", "--replicas=1"], check=False)
        if not restored and os.path.exists(path):
            restore_ledger(path, args.backup_key)
            sys.stderr.write(f"restored ledger for {args.workspace} from private backup\n")
        raise
    finally:
        # Retain the encrypted recovery record after any interrupted failure.
        if restored and locals().get("recovery_complete", False):
            os.unlink(path)
        elif os.path.exists(path):
            sys.stderr.write(f"encrypted recovery record retained at {path}\n")
    print("ledger-roundtrip\tok")


def self_check():
    try:
        valid_name("production-repo", "workspace")
    except SystemExit as exc:
        if PREFIX not in str(exc):
            raise
    else:
        raise SystemExit("self-check accepted a real workspace name")
    valid_name("qualification-recovery", "workspace")
    fd, path = tempfile.mkstemp(prefix="apc-ledger-")
    os.fchmod(fd, 0o600)
    os.close(fd)
    mode = stat.S_IMODE(os.stat(path).st_mode)
    os.unlink(path)
    if mode != 0o600:
        raise SystemExit("self-check could not keep a backup private")
    print("self-check\tok")


def main():
    parser = argparse.ArgumentParser(description="Disposable project-controller recovery checks")
    parser.add_argument("--namespace", default=os.environ.get("CONTROL_NAMESPACE", "agent-project-controller"))
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("self-check")
    for name in ("status", "controller-restart", "volume-marker", "ledger-roundtrip"):
        cmd = sub.add_parser(name)
        cmd.add_argument("--workspace", required=True)
        if name == "ledger-roundtrip":
            cmd.add_argument("--backup-key", type=Path, required=True)
    args = parser.parse_args()
    if args.command == "self-check":
        self_check()
        return
    {"status": command_status, "controller-restart": command_controller_restart, "volume-marker": command_volume_marker, "ledger-roundtrip": command_ledger_roundtrip}[args.command](args)


if __name__ == "__main__":
    main()
