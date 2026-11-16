"""A failed rerun must invalidate a previous passing primary summary."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


HERE = Path(__file__).resolve().parent


class VerificationEvidenceLifecycle(unittest.TestCase):
    def test_invalid_database_cannot_leave_previous_success_authoritative(self):
        for script, relative in (
            ("native-postgres.sh", "native-summary.json"),
            ("binary-smoke.py", "binary-smoke/binary-summary.json"),
            ("restore-smoke.py", "restore/restore-summary.json"),
        ):
            with self.subTest(script=script), tempfile.TemporaryDirectory() as name:
                directory = Path(name)
                summary = directory / relative
                summary.parent.mkdir(parents=True, exist_ok=True)
                summary.write_text(json.dumps({"passed": True, "prior_run": True}))
                environment = dict(os.environ, TEST_DATABASE_URL="", VERIFY_OUTPUT_DIR=name)
                command = [str(HERE / script)]
                if script.endswith(".py"):
                    command.insert(0, sys.executable)
                result = subprocess.run(command, env=environment, capture_output=True, text=True, timeout=10)
                self.assertNotEqual(result.returncode, 0)
                current = json.loads(summary.read_text())
                self.assertFalse(current["passed"], current)
                self.assertNotIn("prior_run", current)

    def test_invalid_binary_arguments_invalidate_prior_summary(self):
        with tempfile.TemporaryDirectory() as name:
            summary = Path(name) / "binary-smoke/binary-summary.json"
            summary.parent.mkdir()
            summary.write_text(json.dumps({"passed": True}))
            result = subprocess.run([sys.executable, str(HERE / "binary-smoke.py"), "--invalid-fixture-flag"],
                                    env=dict(os.environ, VERIFY_OUTPUT_DIR=name), capture_output=True, text=True, timeout=10)
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(json.loads(summary.read_text())["passed"])


if __name__ == "__main__":
    unittest.main()
