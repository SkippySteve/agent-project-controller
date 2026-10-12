import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec=importlib.util.spec_from_file_location('recovery',Path(__file__).with_name('qualify-recovery.py'))
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)

class RecoveryCrypto(unittest.TestCase):
    def test_authenticated_roundtrip_and_tamper_denial(self):
        with tempfile.TemporaryDirectory() as root:
            key=Path(root)/'key';key.write_bytes(os.urandom(32));key.chmod(0o600)
            record={'data':{'password':'synthetic-only'}}
            path=Path(m.write_private(record,key))
            try:
                encrypted=path.read_bytes()
                self.assertNotIn(b'synthetic-only',encrypted)
                self.assertEqual(path.stat().st_mode & 0o777,0o600)
                self.assertEqual(json.loads(m.decrypt_backup(path,key)),record)
                path.write_bytes(encrypted[:-1]+bytes([encrypted[-1]^1]))
                with patch.object(m,'run') as run:
                    with self.assertRaises(SystemExit):m.restore_ledger(path,key)
                    run.assert_not_called()
                path.write_bytes(encrypted)
                key.write_bytes(os.urandom(32))
                with self.assertRaises(Exception):m.decrypt_backup(path,key)
            finally:path.unlink()

    def test_key_permissions_and_symlink_denied(self):
        with tempfile.TemporaryDirectory() as root:
            key=Path(root)/'key';key.write_bytes(os.urandom(32));key.chmod(0o644)
            with self.assertRaises(SystemExit):m.private_key(key)
            key.chmod(0o600);link=Path(root)/'link';link.symlink_to(key)
            with self.assertRaises(OSError):m.private_key(link)

if __name__=='__main__':unittest.main()
