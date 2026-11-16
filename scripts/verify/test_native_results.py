"""Regression checks for release-gate failure paths; no database claims here."""

import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


HERE = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("native_results", HERE / "check-native-results.py")
GATE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GATE)


def passing_events():
    events = []
    for package, names in sorted(GATE.REQUIRED.items()):
        for test in sorted(names):
            events += [
                {"Action": "run", "Package": package, "Test": test},
                {"Action": "pass", "Package": package, "Test": test},
            ]
        events.append({"Action": "pass", "Package": package})
    return events


class NativeResultChecks(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.path = Path(self.temporary.name) / "events.jsonl"

    def result(self, events, snapshot=None):
        self.path.write_text("".join(json.dumps(event) + "\n" for event in events))
        return GATE.check(self.path, snapshot)

    def test_complete_required_run_passes(self):
        result = self.result(passing_events())
        self.assertTrue(result["passed"], result)
        self.assertEqual(result["required_original_native_tests_passed"], 8)

    def test_skip_rejected(self):
        events = passing_events()
        events[1]["Action"] = "skip"
        self.assertFalse(self.result(events)["passed"])

    def test_missing_required_test_rejected(self):
        self.assertFalse(self.result(passing_events()[2:])["passed"])

    def test_failed_test_and_failed_package_rejected(self):
        for failure in (
            {"Action": "fail", "Package": "socialpublisher/internal/store", "Test": "TestNewNative"},
            {"Action": "fail", "Package": "socialpublisher/internal/worker"},
        ):
            with self.subTest(failure=failure):
                self.assertFalse(self.result(passing_events() + [failure])["passed"])

    def test_incomplete_test_rejected(self):
        events = passing_events() + [{"Action": "run", "Package": "socialpublisher/internal/store", "Test": "TestNewNative"}]
        self.assertFalse(self.result(events)["passed"])

    def test_completion_without_test_start_rejected(self):
        events = passing_events() + [{"Action": "pass", "Package": "socialpublisher/internal/api", "Test": "TestAdditionalFlow"}]
        self.assertFalse(self.result(events)["passed"])

    def test_package_only_output_rejected(self):
        events = [{"Action": "pass", "Package": package} for package in GATE.REQUIRED]
        self.assertFalse(self.result(events)["passed"])

    def test_new_native_test_in_source_must_execute(self):
        snapshot = Path(self.temporary.name) / "source.json"
        snapshot.write_text(json.dumps({"native_tests": [{"package": "socialpublisher/internal/store", "test": "TestNativeNewRequiredRegression"}]}))
        result = self.result(passing_events(), snapshot)
        self.assertFalse(result["passed"])
        self.assertTrue(any("TestNativeNewRequiredRegression" in error for error in result["errors"]))

    def test_empty_malformed_and_duplicate_runs_rejected(self):
        self.assertFalse(self.result([])["passed"])
        self.assertFalse(self.result(passing_events() * 2)["passed"])
        self.path.write_text("not JSON\n")
        self.assertFalse(GATE.check(self.path)["passed"])

    def test_real_shell_gate_rejects_source_mutation(self):
        # Synthetic Go output isolates the shell's source-stability guard. It
        # deliberately does not establish native PostgreSQL acceptance.
        events = Path(self.temporary.name) / "passing.jsonl"
        events.write_text("".join(json.dumps(event) + "\n" for event in passing_events()))
        tool = Path(self.temporary.name) / "synthetic-go"
        tool.write_text("""#!/usr/bin/env python3
import os, pathlib, sys
if sys.argv[1] == 'version':
    print('synthetic toolchain for gate failure-path testing')
elif sys.argv[1] == 'run':
    print('{"kind":"synthetic-preflight","passed":true}')
elif sys.argv[1] == 'test':
    pathlib.Path(os.environ['MUTATION_FIXTURE']).write_text('# changed during verification\\n')
    print(pathlib.Path(os.environ['SYNTHETIC_EVENTS']).read_text(), end='')
else:
    sys.exit(2)
""")
        tool.chmod(0o755)
        with tempfile.NamedTemporaryFile(prefix="gate_mutation_", suffix=".py", dir=HERE) as fixture:
            Path(fixture.name).write_text("# before verification\n")
            env = os.environ.copy()
            env.pop("TEST_PGLITE", None)
            env.update({
                "GO": str(tool),
                "TEST_DATABASE_URL": "postgres://synthetic-unused@127.0.0.1/postgres",
                "VERIFY_OUTPUT_DIR": str(Path(self.temporary.name) / "evidence"),
                "MUTATION_FIXTURE": fixture.name,
                "SYNTHETIC_EVENTS": str(events),
            })
            result = subprocess.run([str(HERE / "native-postgres.sh")], env=env, text=True, capture_output=True, timeout=20)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Source changed during native verification", result.stderr)


if __name__ == "__main__":
    unittest.main()
