import importlib.util
import json
import tempfile
import os
import signal
import sys
from unittest import mock
from pathlib import Path
import time
import unittest

spec=importlib.util.spec_from_file_location('bridge',Path(__file__).with_name('bridge.py'))
b=importlib.util.module_from_spec(spec);spec.loader.exec_module(b)

class BridgePolicy(unittest.TestCase):
    def test_account_mutation_and_reserved_requests_denied(self):
        for method in ('account/login/start','account/logout','account/chatgptAuthTokens/refresh'):
            with self.assertRaises(ValueError):b.translate({'id':1,'method':method})
        with self.assertRaises(ValueError):b.translate({'id':'apc-internal-1','method':'model/list'})
        self.assertFalse(b.translate({'method':'account/read','params':{'refreshToken':True}})['params']['refreshToken'])

    def test_workspace_paths_and_symlink_escape_denied(self):
        with tempfile.TemporaryDirectory() as d:
            root=Path(d)/'repo';root.mkdir();outside=Path(d)/'other';outside.mkdir();(root/'escape').symlink_to(outside)
            for cwd in (outside,root/'escape',Path(d)/'repo-sibling'):
                with self.assertRaises((ValueError,OSError)):
                    b.translate({'method':'thread/start','params':{'cwd':str(cwd)}},root)
            value=b.translate({'method':'thread/start','params':{'cwd':str(root),'sandbox':'danger-full-access','config':{'bad':True},'permissions':'unrestricted','runtimeWorkspaceRoots':[str(outside)]}},root)
            self.assertEqual(value['params']['runtimeWorkspaceRoots'],[str(root)])
            self.assertEqual(value['params']['sandbox'],'read-only')
            self.assertNotIn('permissions',value['params'])
            self.assertNotIn('bad',value['params']['config'])

    def test_outer_sandbox_policy_cannot_be_overridden(self):
        value=b.translate({'method':'turn/start','params':{'sandboxPolicy':{'type':'dangerFullAccess'},'permissions':'unrestricted'}})
        self.assertEqual(value['params']['sandboxPolicy'],{'type':'externalSandbox','networkAccess':'enabled'})
        self.assertNotIn('permissions',value['params'])

    def test_grant_never_accepts_refresh_or_expired_record(self):
        with tempfile.TemporaryDirectory() as d:
            root=Path(d)
            for k,v in {'access-token':'synthetic','chatgpt-account-id':'account','expires-at':str(time.time()+600)}.items(): (root/k).write_text(v)
            self.assertEqual(b.grant(root)['accessToken'],'synthetic')
            (root/'refresh-token').write_text('forbidden')
            with self.assertRaises(ValueError):b.grant(root)
            (root/'refresh-token').unlink()
            for value in ('0','NaN','Infinity'):
                (root/'expires-at').write_text(value)
                with self.assertRaises(ValueError):b.grant(root)

class InterruptedRPC(unittest.TestCase):
    def test_interruption_terminates_child_process_group(self):
        with tempfile.TemporaryDirectory() as d:
            executable=Path(d)/'codex'
            executable.write_text('#!'+sys.executable+'\nimport subprocess,json,time\np=subprocess.Popen(["sleep","60"],stdout=subprocess.DEVNULL)\nprint(json.dumps({"pid":p.pid}),flush=True)\ntime.sleep(60)\n')
            executable.chmod(0o700)
            rpc=b.RPC(dict(os.environ,PATH=d+':'+os.environ['PATH']))
            try:
                child=rpc.receive(time.monotonic()+5)['pid']
            finally:
                rpc.close()
                rpc.process.stdin.close()
                rpc.process.stdout.close()
            deadline=time.monotonic()+5
            while time.monotonic()<deadline:
                status=Path('/proc')/str(child)/'stat'
                if not status.exists() or status.read_text().split()[2]=='Z': break
                time.sleep(.05)
            else: self.fail('app-server descendant survived interruption')

    def test_already_exited_child_cleanup_is_idempotent(self):
        rpc=b.RPC.__new__(b.RPC)
        rpc.process=mock.Mock(pid=4321)
        with mock.patch.object(b.os, 'killpg', side_effect=ProcessLookupError):
            rpc.close()
        rpc.process.wait.assert_called_once_with(timeout=10)

if __name__=='__main__':unittest.main()
