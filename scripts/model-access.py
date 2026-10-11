#!/usr/bin/env python3
"""Operator-managed access-only model grants. No refresh or coding task launch.

Publish from a private JSON file or stdin containing an enabled/access record.
Pause and verify worker termination before changing a grant, then restore the
previous lifecycle state. Failed operations leave the workspace paused.
"""
import argparse
import base64
import copy
import json
import math
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import time

OWNER = 'projects.agentwork.dev/workspace-uid'


def access_fields(record, now):
    if not isinstance(record, dict) or set(record) != {'enabled', 'access'} or record['enabled'] is not True:
        raise ValueError('enabled access-only record required')
    value = record['access']
    allowed = {'accessToken', 'chatgptAccountId', 'chatgptPlanType', 'expiresAt'}
    if not isinstance(value, dict) or set(value) - allowed:
        raise ValueError('unreviewed credential fields')
    token, account, expires = (value.get(k) for k in ('accessToken', 'chatgptAccountId', 'expiresAt'))
    if not isinstance(token, str) or not 0 < len(token) <= 32768 or not re.fullmatch(r'[A-Za-z0-9._~+/=-]+', token):
        raise ValueError('invalid access token')
    if not isinstance(account, str) or not 0 < len(account) <= 256 or any(c in account for c in '\r\n\t {}[]"'):
        raise ValueError('invalid account')
    if isinstance(expires, bool) or not isinstance(expires, (int, float)) or not math.isfinite(expires) or expires <= now + 60:
        raise ValueError('expired access record')
    fields = {'access-token': token, 'chatgpt-account-id': account, 'expires-at': str(expires)}
    plan = value.get('chatgptPlanType')
    if plan is not None:
        if not isinstance(plan, str) or len(plan) > 256 or any(c in plan for c in '\r\n\x00'):
            raise ValueError('invalid plan')
        fields['chatgpt-plan-type'] = plan
    return fields


class Operator:
    def __init__(self, args):
        self.k = ['kubectl'] + (['--context', args.context] if args.context else [])
        self.control = args.control_namespace
        self.name = args.workspace

    def run(self, args, document=None, absent=False):
        result = subprocess.run(self.k + args, input=json.dumps(document).encode() if document is not None else None,
                                capture_output=True, timeout=30)
        if result.returncode:
            if absent and b'(NotFound)' in result.stderr:
                return None
            # Server errors can include submitted objects. Never print them.
            raise RuntimeError('Kubernetes grant operation failed')
        return result.stdout

    def get(self, kind, name, namespace, absent=False):
        raw = self.run(['-n', namespace, 'get', kind, name, '-o', 'json'], absent=absent)
        return json.loads(raw) if raw is not None else None

    def workspace(self):
        w = self.get('projectworkspace', self.name, self.control)
        if w['spec']['desiredState'] == 'Removed':
            raise RuntimeError('removed workspace cannot receive a grant')
        expected = 'apc-' + w['metadata']['uid'].replace('-', '')
        if w.get('status', {}).get('namespace') != expected:
            raise RuntimeError('workspace namespace not verified')
        return w

    def state(self, desired, uid):
        w = self.workspace()
        if w["metadata"]["uid"] != uid:
            raise RuntimeError("workspace replaced during grant operation")
        patch = [{'op': 'test', 'path': '/metadata/uid', 'value': w['metadata']['uid']},
                 {'op': 'test', 'path': '/metadata/resourceVersion', 'value': w['metadata']['resourceVersion']},
                 {'op': 'replace', 'path': '/spec/desiredState', 'value': desired}]
        self.run(['-n', self.control, 'patch', 'projectworkspace', self.name, '--type=json', '-p', json.dumps(patch)])

    def paused(self, uid):
        for _ in range(40):
            w = self.workspace()
            if w['metadata']['uid'] != uid:
                raise RuntimeError('workspace replaced during pause')
            namespace = w['status']['namespace']
            pods = json.loads(self.run(['-n', namespace, 'get', 'pods', '-l', OWNER + '=' + w['metadata']['uid'], '-o', 'json']))
            workers = [p for p in pods['items'] if p['metadata']['labels'].get('projects.agentwork.dev/role') == 'worker']
            if w['spec']['desiredState'] == 'Paused' and w['status'].get('phase') == 'Paused' and not workers:
                return w
            time.sleep(1)
        raise RuntimeError('worker termination not verified; grant unchanged')

    def change(self, fields, revoke=False):
        w = self.workspace()
        previous = w['spec']['desiredState']
        namespace = w['status']['namespace']
        current = self.get('secret', 'model-access', namespace, absent=True)
        if current and current['metadata'].get('labels', {}).get(OWNER) != w['metadata']['uid']:
            raise RuntimeError('grant ownership mismatch')
        if current and fields:
            prior = base64.b64decode(current['data'].get('chatgpt-account-id', '')).decode()
            if prior and prior != fields['chatgpt-account-id']:
                raise RuntimeError('renewal cannot change the model account')
        self.state('Paused', w['metadata']['uid'])
        self.paused(w['metadata']['uid'])
        if revoke:
            if current:
                self.run(['-n', namespace, 'delete', 'secret', 'model-access'])
            return {'workspace': self.name, 'revoked': True, 'worker_terminated': True, 'phase': 'Paused'}
        if float(fields['expires-at']) <= time.time() + 60:
            raise RuntimeError('grant expired while pausing; not resumed')
        data = {key: base64.b64encode(value.encode()).decode() for key, value in fields.items()}
        if current:
            # Use stdin so credentials never appear in process arguments.
            secret = copy.deepcopy(current)
            secret.pop('stringData', None)
            secret['data'] = data
            self.run(['replace', '-f', '-'], secret)
        else:
            self.run(['create', '-f', '-'], {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'model-access',
                      'namespace': namespace, 'labels': {OWNER: w['metadata']['uid']}}, 'data': data})
        # Recheck lifecycle and ownership after the Secret write, before resume.
        after = self.workspace()
        if after['metadata']['uid'] != w['metadata']['uid'] or after['spec']['desiredState'] != 'Paused':
            raise RuntimeError('workspace changed during grant publication; not resumed')
        if previous == 'Running':
            self.state('Running', w['metadata']['uid'])
        return {'workspace': self.name, 'published': True, 'renewed': current is not None,
                'worker_terminated_before_update': True, 'refresh_delivered': False,
                'desired_state': previous, 'secret_revision': self.get('secret', 'model-access', namespace)['metadata']['resourceVersion']}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--context')
    parser.add_argument('--control-namespace', default='agent-project-controller')
    parser.add_argument('--workspace', required=True)
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument('--record-file', type=Path)
    source.add_argument('--record-stdin', action='store_true')
    source.add_argument('--revoke', action='store_true')
    args = parser.parse_args()
    fields = None
    try:
        if not args.revoke:
            if args.record_file:
                with os.fdopen(os.open(args.record_file, os.O_RDONLY | os.O_NOFOLLOW), 'rb') as handle:
                    info = os.fstat(handle.fileno())
                    if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_uid != os.getuid():
                        raise ValueError('record file must be a private owned regular file')
                    raw = handle.read(131073)
            else:
                raw = sys.stdin.buffer.read(131073)
            if len(raw) > 131072:
                raise ValueError('record exceeds limit')
            fields = access_fields(json.loads(raw), time.time())
        print(json.dumps(Operator(args).change(fields, args.revoke)))
    except (ValueError, RuntimeError, OSError, subprocess.TimeoutExpired):
        raise SystemExit('model grant operation failed; workspace may remain paused; credentials withheld') from None


if __name__ == '__main__':
    main()
