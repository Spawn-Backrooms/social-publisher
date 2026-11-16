#!/usr/bin/env python3
"""Run the fixed restore gate and require observed native/session/test evidence.

Stdout contains one safe JSON summary. Failed Go test output is written to
stderr; the acceptance test emits neither connection secrets nor plaintext.
"""

import json
import os
from pathlib import Path
import subprocess
import sys


PACKAGE = "socialpublisher/internal/ops"
TEST = "TestRestoredDatabaseSafety"


def empty_summary():
    return {
        "kind": "native_postgres_restore_acceptance", "passed": False,
        "package": PACKAGE, "test": TEST,
        "test_runs": 0, "test_passes": 0, "package_starts": 0,
        "package_passes": 0, "skips": 0, "failures": 0,
        "native_preflight_passed": False, "errors": [],
    }


def audit_test_output(output, returncode):
    summary = empty_summary()
    package_actions, test_actions = [], []
    for line in output.splitlines():
        if not line.strip():
            continue
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            summary["errors"].append("Go test emitted malformed JSON")
            continue
        if not isinstance(event, dict):
            summary["errors"].append("Go test emitted a non-object event")
            continue
        action = event.get("Action")
        summary["skips"] += action == "skip"
        summary["failures"] += action in {"fail", "build-fail"}
        if event.get("Package") != PACKAGE or ("Test" in event and event["Test"] != TEST):
            summary["errors"].append("Go test emitted an unexpected package or test")
            continue
        if action == "output":
            continue
        if "Test" in event:
            test_actions.append(action)
            summary["test_runs"] += action == "run"
            summary["test_passes"] += action == "pass"
        else:
            package_actions.append(action)
            summary["package_starts"] += action == "start"
            summary["package_passes"] += action == "pass"
    if test_actions != ["run", "pass"]:
        summary["errors"].append("TestRestoredDatabaseSafety must run and pass exactly once with no skips")
    if package_actions != ["start", "pass"]:
        summary["errors"].append("The operations package must start and pass exactly once")
    if summary["skips"] or summary["failures"]:
        summary["errors"].append("Skipped or failed tests cannot establish restore acceptance")
    if returncode != 0:
        summary["errors"].append("Go test exited unsuccessfully")
    return summary


def native_proof(output):
    try:
        proof = json.loads(output)
    except json.JSONDecodeError:
        return False
    if not isinstance(proof, dict):
        return False
    pids = proof.get("backend_pids")
    return (
        proof.get("kind") == "native_postgres_preflight"
        and proof.get("passed") is True
        and proof.get("independent_sessions") is True
        and proof.get("advisory_lock_exclusion") is True
        and isinstance(pids, list) and len(pids) == 2
        and all(type(pid) is int and pid > 0 for pid in pids)
        and pids[0] != pids[1]
    )


def run(go):
    summary = empty_summary()
    if "TEST_PGLITE" in os.environ:
        summary["errors"].append("TEST_PGLITE must be unset")
        return summary
    # The restore DSN is the connection authority. Ambient PostgreSQL defaults
    # and Go overlays/workspaces cannot redirect this source-bound acceptance.
    environment = {key: value for key, value in os.environ.items() if not key.startswith("PG")}
    environment.update(GOFLAGS="", GOWORK="off", GOENV="off", RUN_RESTORE_VERIFY="1")
    environment["TEST_DATABASE_URL"] = environment.get("RESTORE_DATABASE_URL", "")
    repo = Path(__file__).resolve().parents[1]
    try:
        # This test validates the production TLS/local-host DSN policy before
        # its first connection. Generic nativecheck must not run ahead of it.
        result = subprocess.run(
            [go, "test", "-count=1", "-timeout=6m", "-run", "^TestRestoredDatabaseSafety$",
             "-skip", "^$", "-json", "./internal/ops"],
            cwd=repo, env=environment, capture_output=True, text=True, timeout=420,
        )
        summary = audit_test_output(result.stdout, result.returncode)
        if summary["errors"]:
            sys.stderr.write(result.stdout)
            sys.stderr.write(result.stderr)
            return summary
        preflight = subprocess.run(
            [go, "run", "./scripts/verify/nativecheck"], cwd=repo, env=environment,
            capture_output=True, text=True, timeout=120,
        )
        if preflight.returncode != 0 or not native_proof(preflight.stdout):
            # Connection errors can include DSN fragments; do not relay them.
            summary["errors"].append("Native PostgreSQL independent-session preflight failed")
            return summary
        summary["native_preflight_passed"] = True
        summary["passed"] = True
    except subprocess.TimeoutExpired:
        summary["errors"].append("Restore verification exceeded its deadline")
    except OSError:
        summary["errors"].append("Cannot execute the configured Go tool")
    return summary


if __name__ == "__main__":
    result = run(sys.argv[1] if len(sys.argv) == 2 else "go")
    print(json.dumps(result))
    sys.exit(0 if result["passed"] else 1)
