"""Exercise the standalone restore gate without claiming database evidence.

The synthetic Go executable checks command/environment and process-result
handling. Real PostgreSQL, backup and keyring acceptance belongs to the native
restore smoke and the deployment's separately recorded restore drill.
"""

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().with_name("verify-restore.sh")
PACKAGE = "socialpublisher/internal/ops"
TEST = "TestRestoredDatabaseSafety"


def passed_events():
    return [
        {"Action": "start", "Package": PACKAGE},
        {"Action": "run", "Package": PACKAGE, "Test": TEST},
        {"Action": "pass", "Package": PACKAGE, "Test": TEST},
        {"Action": "pass", "Package": PACKAGE},
    ]


class StandaloneRestoreGate(unittest.TestCase):
    def run_gate(self, events=None, *, environment=None, test_exit=0, native=None):
        with tempfile.TemporaryDirectory(prefix="restore-gate-unit-") as directory:
            directory = Path(directory)
            tool = directory / "go"
            tool.write_text("""#!/usr/bin/env python3
import json, os, pathlib, sys
record = {'args': sys.argv[1:], 'env': {key: os.environ.get(key) for key in
          ('GOFLAGS','GOWORK','GOENV','TEST_DATABASE_URL','RUN_RESTORE_VERIFY','PGOPTIONS')}}
with open(os.environ['FAKE_CALLS'], 'a') as output:
    output.write(json.dumps(record) + '\\n')
if sys.argv[1] == 'run':
    print(os.environ['FAKE_NATIVE'])
    sys.exit(0)
print(os.environ['FAKE_EVENTS'])
sys.exit(int(os.environ['FAKE_TEST_EXIT']))
""")
            tool.chmod(0o700)
            calls = directory / "calls.jsonl"
            env = dict(os.environ)
            env.pop("TEST_PGLITE", None)
            env.update(
                GO=str(tool),
                RESTORE_DATABASE_URL="postgres://fixture@127.0.0.1:1/isolated?sslmode=disable",
                RESTORE_EXPECTED_POSTS="2", RESTORE_EXPECTED_TARGETS="2",
                RESTORE_EXPECTED_ENCRYPTED_ACCOUNTS="1",
                RESTORE_EXPECTED_EVENTS="4", RESTORE_MAX_EVENT_ID="4",
                TOKEN_ENCRYPTION_KEYS="synthetic-keyring", ACTIVE_TOKEN_KEY_ID="fixture",
                RESTORE_ISOLATED="true", RESTORE_MAINTENANCE_CONFIRMED="true",
                RESTORE_ALLOW_LOCAL_PLAINTEXT="true",
                FAKE_CALLS=str(calls), FAKE_TEST_EXIT=str(test_exit),
                FAKE_EVENTS="\n".join(json.dumps(event) for event in (passed_events() if events is None else events)),
                FAKE_NATIVE=json.dumps(native if native is not None else {
                    "kind": "native_postgres_preflight", "passed": True,
                    "independent_sessions": True, "advisory_lock_exclusion": True,
                    "backend_pids": [123, 456], "server_version": "PostgreSQL 18.4", "server_version_num": 180004,
                }),
            )
            env.update(environment or {})
            result = subprocess.run([str(SCRIPT)], env=env, capture_output=True, text=True, timeout=15)
            recorded = [json.loads(line) for line in calls.read_text().splitlines()] if calls.exists() else []
            return result, recorded

    def test_zero_tests_cannot_pass_even_when_go_exits_zero(self):
        result, _ = self.run_gate([
            {"Action": "start", "Package": PACKAGE},
            {"Action": "pass", "Package": PACKAGE},
        ])
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_named_test_and_independent_session_proof_pass(self):
        result, calls = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        summary = json.loads(result.stdout)
        self.assertEqual(summary["kind"], "native_postgres_restore_acceptance")
        self.assertTrue(summary["passed"])
        self.assertTrue(summary["native_preflight_passed"])
        self.assertEqual(summary["test_runs"], 1)
        self.assertEqual(summary["test_passes"], 1)
        self.assertEqual(summary["package_passes"], 1)
        self.assertEqual(summary["skips"], 0)
        self.assertEqual([call["args"][0] for call in calls], ["test", "run"])

    def test_inherited_build_flags_and_connection_defaults_are_disabled(self):
        result, calls = self.run_gate(environment={
            "GOFLAGS": "-skip=TestRestoredDatabaseSafety -overlay=/nonexistent/overlay.json",
            "GOWORK": "/nonexistent/go.work", "GOENV": "/nonexistent/go.env",
            "TEST_DATABASE_URL": "postgres://wrong-database", "PGOPTIONS": "-c search_path=wrong",
        })
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(len(calls), 2)
        for call in calls:
            self.assertEqual(call["env"]["GOFLAGS"], "")
            self.assertEqual(call["env"]["GOWORK"], "off")
            self.assertEqual(call["env"]["GOENV"], "off")
            self.assertEqual(call["env"]["TEST_DATABASE_URL"], "postgres://fixture@127.0.0.1:1/isolated?sslmode=disable")
            self.assertIsNone(call["env"]["PGOPTIONS"])
        arguments = calls[0]["args"]
        self.assertIn("-json", arguments)
        self.assertIn("-count=1", arguments)
        self.assertEqual(arguments[arguments.index("-run") + 1], "^TestRestoredDatabaseSafety$")
        self.assertEqual(arguments[arguments.index("-skip") + 1], "^$")

    def test_any_set_pglite_flag_is_rejected_before_execution(self):
        for value in ("", "0", "false", "1"):
            with self.subTest(value=value):
                result, calls = self.run_gate(environment={"TEST_PGLITE": value})
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(calls, [])

    def test_incomplete_duplicate_skipped_failed_or_wrong_tests_are_rejected(self):
        cases = {
            "missing_run": [event for event in passed_events() if event["Action"] != "run"],
            "missing_package_pass": passed_events()[:-1],
            "duplicate_test": passed_events()[:3] + passed_events()[1:],
            "wrong_test": [dict(event, Test="TestSomethingElse") if "Test" in event else event for event in passed_events()],
            "null_test": [dict(event, Test=None) if "Test" in event else event for event in passed_events()],
            "skip": [dict(event, Action="skip") if event.get("Test") == TEST and event["Action"] == "pass" else event for event in passed_events()],
            "failure": passed_events() + [{"Action": "fail", "Package": PACKAGE}],
            "non_object": passed_events() + ["not an event"],
            "empty": [],
        }
        for name, events in cases.items():
            with self.subTest(name=name):
                result, _ = self.run_gate(events)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_process_failure_cannot_be_masked_by_pass_json(self):
        result, calls = self.run_gate(test_exit=1)
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        # The Go acceptance test validates the TLS/local-host policy before
        # connecting. A rejected DSN must never reach generic nativecheck.
        self.assertEqual([call["args"][0] for call in calls], ["test"])

    def test_native_session_proof_is_required_after_the_validated_test(self):
        for native in ({}, {"kind": "native_postgres_preflight", "passed": True, "backend_pids": [123, 123]}):
            with self.subTest(native=native):
                result, calls = self.run_gate(native=native)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual([call["args"][0] for call in calls], ["test", "run"])


if __name__ == "__main__":
    unittest.main()
