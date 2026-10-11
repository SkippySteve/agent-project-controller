#!/usr/bin/env python3
"""Bounded synthetic task through the installed Codex app-server.

Run only in a disposable qualification repository. Credentials are read from
the access-only /run/model mount; raw protocol events are never printed.
The caller must independently verify the outer runtime and network policy.
"""
import argparse
import json
import os
from pathlib import Path
import queue
import subprocess
import tempfile
import threading
import time


class RPC:
    def __init__(self, env):
        self.process = subprocess.Popen(
            ['codex', 'app-server', '--stdio', '-c', 'analytics.enabled=false',
             '-c', 'features.responses_websockets=false',
             '-c', 'features.responses_websockets_v2=false',
             '-c', 'features.multi_agent=false', '-c', 'project_doc_max_bytes=0'],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
            text=True, bufsize=1, env=env)
        self.messages = queue.Queue()
        self.next_id = 0
        self.delta_count = 0
        self.completed_commands = 0
        self.failures = set()
        self.requested_methods = set()
        self.item_types = set()

        def reader():
            try:
                for line in self.process.stdout:
                    if len(line) > 4_194_304:
                        self.messages.put(None)
                        return
                    self.messages.put(json.loads(line))
            finally:
                self.messages.put(None)
        threading.Thread(target=reader, daemon=True).start()

    def send(self, value):
        self.process.stdin.write(json.dumps(value) + '\n')
        self.process.stdin.flush()

    def receive(self, deadline):
        try:
            message = self.messages.get(timeout=max(.01, deadline - time.monotonic()))
        except queue.Empty:
            raise RuntimeError('app-server-timeout') from None
        if message is None:
            raise RuntimeError('app-server-disconnected')
        return message

    def handle(self, message):
        method = message.get('method', '')
        if 'id' in message and method:
            self.requested_methods.add(method)
            # Never approve unexpected tool, permission or refresh requests.
            self.send({'id': message['id'], 'error': {'code': -32000, 'message': 'qualification does not approve this request'}})
        if method == 'item/agentMessage/delta':
            self.delta_count += 1
        if method == 'item/completed':
            item = message.get('params', {}).get('item', {})
            self.item_types.add(item.get('type', 'unknown'))
            if item.get('type') == 'commandExecution' and item.get('exitCode') == 0:
                self.completed_commands += 1
        if method == 'error':
            text = json.dumps(message).lower()
            self.failures.update(name for name, marker in (
                ('authentication', '401'), ('policy-denial', '403'),
                ('rate-limit', '429'), ('transport', 'connect'),
                ('sandbox', 'sandbox'), ('websocket', 'websocket')) if marker in text)

    def call(self, method, params):
        self.next_id += 1
        ident = self.next_id
        self.send({'id': ident, 'method': method, 'params': params})
        deadline = time.monotonic() + 30
        while True:
            message = self.receive(deadline)
            if message.get('id') == ident and 'method' not in message:
                if 'error' in message:
                    raise RuntimeError('app-server-rejected-' + method)
                return message.get('result')
            self.handle(message)

    def close(self):
        self.process.terminate()
        try:
            self.process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--coding', action='store_true')
    args = parser.parse_args()
    repo = Path('/workspace/repo')
    origin = subprocess.check_output(['git', '-C', str(repo), 'remote', 'get-url', 'origin'], text=True).strip()
    if '/qualification-' not in origin or not origin.endswith('.git'):
        raise SystemExit('disposable qualification repository required')
    if os.getuid() != 1000 or Path('/var/run/secrets/kubernetes.io/serviceaccount/token').exists():
        raise SystemExit('tokenless non-root worker required')
    directory = Path('/run/model')
    token = (directory / 'access-token').read_text().strip()
    account = (directory / 'chatgpt-account-id').read_text().strip()
    expires = float((directory / 'expires-at').read_text())
    if not token or not account or expires <= time.time() + 60:
        raise SystemExit('unexpired access-only model grant required')
    plan = directory / 'chatgpt-plan-type'
    with tempfile.TemporaryDirectory(prefix='apc-codex-', dir='/workspace') as home:
        env = dict(os.environ, CODEX_HOME=home, PATH="/opt/codex/bin:" + os.environ["PATH"])
        rpc = RPC(env)
        result = {'passed': False, 'synthetic_repository': True, 'refresh_delivered': False,
                  'raw_events_retained': False, 'coding': args.coding}
        try:
            rpc.call('initialize', {'clientInfo': {'name': 'apc-qualification', 'version': '0.1.0'}, 'capabilities': {'experimentalApi': True}})
            rpc.send({'method': 'initialized', 'params': {}})
            rpc.call('account/login/start', {'type': 'chatgptAuthTokens', 'accessToken': token,
                                           'chatgptAccountId': account,
                                           'chatgptPlanType': plan.read_text().strip() if plan.exists() else None})
            models = rpc.call('model/list', {})['data']
            model = next((m['id'] for m in models if m.get('isDefault')), models[0]['id'])
            thread = rpc.call('thread/start', {'cwd': str(repo), 'ephemeral': True,
                                              'approvalPolicy': 'never', 'sandbox': 'read-only', 'model': model})['thread']['id']
            prompt = 'Reply exactly APC_MODEL_OK. Do not use tools or read files.'
            params = {'threadId': thread, 'input': [{'type': 'text', 'text': prompt}]}
            if args.coding:
                prompt = ('This is an operator-created synthetic fixture. Create add.py with add(a,b) returning a+b, '
                          'and test_add.py with Python unittest tests for positive, negative and zero inputs. '
                          'Run python3 -m unittest -v. Edit only those two files in the current repository. '
                          'Do not read credentials, environment, user home or files outside this repository. '
                          'Do not use network tools or delegate. Return APC_CODING_OK after tests pass.')
                params['input'][0]['text'] = prompt
                params['sandboxPolicy'] = {'type': 'externalSandbox', 'networkAccess': 'enabled'}
            rpc.call('turn/start', params)
            deadline = time.monotonic() + 180
            final = ''
            while time.monotonic() < deadline:
                message = rpc.receive(deadline)
                rpc.handle(message)
                if message.get('method') == 'item/agentMessage/delta':
                    final += message.get('params', {}).get('delta', '')
                if message.get('method') == 'item/completed':
                    item = message.get('params', {}).get('item', {})
                    if item.get('type') == 'agentMessage':
                        final = item.get('text', final)
                if message.get('method') == 'turn/completed':
                    turn = message.get('params', {}).get('turn', {})
                    result['turn_completed'] = turn.get('status') == 'completed'
                    break
            marker = 'APC_CODING_OK' if args.coding else 'APC_MODEL_OK'
            result.update({'marker_returned': marker in final, 'stream_deltas': rpc.delta_count,
                           'completed_commands': rpc.completed_commands, 'model': model})
            result['passed'] = result.get('turn_completed', False) and marker in final and rpc.delta_count > 0
            if args.coding:
                verify = subprocess.run(['python3', '-I', '-m', 'unittest', '-v'], cwd=repo, capture_output=True, timeout=30)
                result['independent_tests_passed'] = verify.returncode == 0 and b'Ran 3 tests' in verify.stderr
                result['files_created'] = all((repo / name).is_file() for name in ('add.py', 'test_add.py'))
                result['passed'] = result['passed'] and result['independent_tests_passed'] and result['files_created']
        except (RuntimeError, KeyError, IndexError) as error:
            result['failure'] = str(error) if isinstance(error, RuntimeError) else 'invalid-protocol-response'
        finally:
            result['failure_categories'] = sorted(rpc.failures)
            result['requested_methods'] = sorted(rpc.requested_methods)
            result['item_types'] = sorted(rpc.item_types)
            result['final_diagnostics'] = [word for word in ('permission', 'executable', 'not found', 'sandbox', 'command', 'tools', 'code-mode', 'available', 'unavailable', 'failed') if word in locals().get('final', '').lower()]
            rpc.close()
        print(json.dumps(result))
        raise SystemExit(0 if result['passed'] else 1)


if __name__ == '__main__':
    main()
