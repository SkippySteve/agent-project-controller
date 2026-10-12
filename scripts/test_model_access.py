import argparse
import base64
import importlib.util
import time
import unittest
from unittest.mock import patch
from pathlib import Path

spec = importlib.util.spec_from_file_location('model_access', Path(__file__).with_name('model-access.py'))
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)

class Grants(unittest.TestCase):
    def record(self):
        return {'enabled': True, 'access': {'accessToken': 'test.token', 'chatgptAccountId': 'account', 'expiresAt': time.time()+600}}

    def test_reject_nested_refresh_and_invalid_expiry(self):
        for key in ('refreshToken', 'auth.json', 'tokens'):
            record = self.record()
            record['access'][key] = {'refresh_token': 'forbidden'}
            with self.assertRaises(ValueError): m.access_fields(record, time.time())
        for expiry in (0, float('nan'), float('inf'), True):
            record = self.record(); record['access']['expiresAt'] = expiry
            with self.assertRaises(ValueError): m.access_fields(record, time.time())

    def operator(self, current=None, fail=False):
        op = m.Operator(argparse.Namespace(context=None, control_namespace='control', workspace='fixture'))
        w = {'metadata': {'uid': 'uid', 'resourceVersion': '1'}, 'spec': {'desiredState': 'Running'}, 'status': {'namespace':'apc-uid'}}
        states = []; writes = []
        op.workspace = lambda: w
        def state(desired, uid):
            self.assertEqual(uid, 'uid'); states.append(desired); w['spec']['desiredState'] = desired
        op.state = state
        op.paused = lambda uid: w
        op.get = lambda *a, **kw: current
        def run(args, document=None, **kw):
            if fail: raise RuntimeError('simulated API outage')
            writes.append(document)
            return b''
        op.run = run
        return op, states, writes

    def test_interrupted_publication_stays_paused(self):
        op, states, writes = self.operator(fail=True)
        with self.assertRaises(RuntimeError): op.change(m.access_fields(self.record(), time.time()))
        self.assertEqual(states, ['Paused'])
        self.assertFalse(writes)

    def test_renewal_preserves_metadata_and_resumes(self):
        current = {'apiVersion':'v1','kind':'Secret','metadata': {'name':'model-access','resourceVersion':'7','labels':{m.OWNER:'uid','custom':'kept'},'annotations':{'purpose':'kept'}}, 'data':{'chatgpt-account-id':base64.b64encode(b'account').decode()}}
        op, states, writes = self.operator(current)
        result = op.change(m.access_fields(self.record(), time.time()))
        self.assertTrue(result['renewed']); self.assertEqual(states, ['Paused', 'Running'])
        self.assertEqual(writes[0]['metadata'],current['metadata'])
        self.assertNotIn('refresh-token', writes[0]['data'])

    def test_foreign_grant_and_account_change_are_denied_before_pause(self):
        for owner, account in [('other','account'),('uid','other')]:
            current = {'metadata':{'labels':{m.OWNER:owner}},'data':{'chatgpt-account-id':base64.b64encode(account.encode()).decode()}}
            op, states, writes = self.operator(current)
            with self.assertRaises(RuntimeError):op.change(m.access_fields(self.record(),time.time()))
            self.assertEqual(states,[]);self.assertEqual(writes,[])

    def test_revoke_leaves_paused(self):
        op, states, writes = self.operator()
        result = op.change(None, revoke=True)
        self.assertTrue(result['revoked']);self.assertEqual(states,['Paused'])

    def test_recreated_workspace_cannot_resume(self):
        op = m.Operator(argparse.Namespace(context=None, control_namespace='control',workspace='fixture'))
        op.workspace = lambda: {'metadata':{'uid':'new'}}
        with self.assertRaises(RuntimeError):op.state('Running','old')

if __name__ == '__main__':unittest.main()
