#!/usr/bin/env python3
"""Exercise an actual pg_dump/pg_restore with private, generated fixture keys.

Use only a disposable local PostgreSQL service. Two fresh databases are created
and removed; production credentials, provider requests and publications are never
part of this verification. This complements the real-backup acceptance script.
"""

import base64
import hashlib
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import sys
import tempfile
from urllib.parse import urlunsplit

from postgres_url import explicit_connection_environment, validated_url


def main():
    directory = Path(os.environ.get("VERIFY_OUTPUT_DIR") or tempfile.mkdtemp(prefix="publisher-restore.")) / "restore"
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    directory = directory.resolve()
    summary_path = directory / "restore-summary.json"
    summary_path.write_text(json.dumps({"kind": "native_postgres_restore_smoke", "passed": False, "status": "started"}) + "\n")
    original = os.environ.get("TEST_DATABASE_URL", "")
    parsed = validated_url(original, local_only=True)
    if "TEST_PGLITE" in os.environ:
        sys.exit("Unset TEST_PGLITE before native restore verification.")
    repo = Path(__file__).resolve().parents[2]
    env = explicit_connection_environment(os.environ)
    env.update(GOFLAGS="", GOWORK="off", GOENV="off")
    env["RESTORE_FIXTURE_DIR"] = str(directory)
    go = env.get("GO", "go")
    pg_bin = Path(env["PG_BIN"]) if env.get("PG_BIN") else None

    def tool(name):
        command = str(pg_bin / name) if pg_bin else shutil.which(name)
        if not command or not os.access(command, os.X_OK):
            raise RuntimeError("Install PostgreSQL 18 client tools or set PG_BIN; missing " + name)
        return command

    psql, pg_dump, pg_restore = (tool(name) for name in ("psql", "pg_dump", "pg_restore"))
    suffix = secrets.token_hex(5)
    source_name = "sp_verify_restore_source_" + suffix
    restored_name = "sp_verify_restore_target_" + suffix

    def url(name):
        return urlunsplit(parsed._replace(path="/" + name))

    source_url, restored_url = url(source_name), url(restored_name)
    created = []
    results = {}

    def run(name, arguments, command_env, expect_failure="", timeout=420):
        try:
            result = subprocess.run(arguments, cwd=repo, env=command_env, text=True, capture_output=True, timeout=timeout)
        except subprocess.TimeoutExpired:
            # Exception text would otherwise include libpq's --dbname argument
            # and any credentials in that URL.
            raise RuntimeError(name + " exceeded its verification deadline") from None
        (directory / (name + ".log")).write_text(result.stdout + result.stderr)
        results[name] = result.returncode
        if expect_failure:
            if result.returncode == 0:
                raise RuntimeError(name + " unexpectedly accepted an invalid restore")
            if expect_failure not in result.stdout + result.stderr:
                raise RuntimeError(name + " failed for an unexpected reason; inspect " + str(directory / (name + ".log")))
        elif result.returncode != 0:
            raise RuntimeError(name + " failed; inspect " + str(directory / (name + ".log")))
        print(name + (": expected rejection" if expect_failure else ": passed"), flush=True)
        return result

    def fingerprint(name):
        snapshot = json.loads(run(name, [sys.executable, str(repo / "scripts/verify/snapshot-source.py")], env).stdout)
        snapshot["restore_procedure"] = {
            path: hashlib.sha256((repo / path).read_bytes()).hexdigest()
            for path in ("docs/restore-quarantine.sql", "docs/verify-restore.sh", "docs/check-restore-results.py")
        }
        (directory / (name + ".json")).write_text(json.dumps(snapshot, indent=2) + "\n")
        return snapshot

    before = fingerprint("source-before")
    preflight = json.loads(run("native-preflight", [go, "run", "./scripts/verify/nativecheck"], env).stdout)
    try:
        for database in (source_name, restored_name):
            run("create-" + database, [psql, "-X", "--dbname", original, "-v", "ON_ERROR_STOP=1", "-c", 'CREATE DATABASE "' + database + '"'], env)
            created.append(database)
        run("seed", [go, "run", "./scripts/verify/restorefixture", "seed"], dict(env, RESTORE_FIXTURE_DATABASE_URL=source_url))
        manifest = json.loads((directory / "backup-manifest.json").read_text())
        backup = directory / "fixture-backup.dump"
        run("pg-dump", [pg_dump, "--dbname", source_url, "--format=custom", "--no-owner", "--no-acl", "--file", str(backup)], env)
        run("pg-restore", [pg_restore, "--exit-on-error", "--no-owner", "--no-acl", "--dbname", restored_url, str(backup)], env)
        restore_env = dict(env,
            RESTORE_DATABASE_URL=restored_url,
            RESTORE_EXPECTED_POSTS=str(manifest["posts"]),
            RESTORE_EXPECTED_TARGETS=str(manifest["targets"]),
            RESTORE_EXPECTED_ENCRYPTED_ACCOUNTS=str(manifest["encrypted_accounts"]),
            RESTORE_EXPECTED_EVENTS=str(manifest["events"]),
            RESTORE_MAX_EVENT_ID=str(manifest["max_event_id"]),
            RESTORE_ISOLATED="true", RESTORE_MAINTENANCE_CONFIRMED="true",
            RESTORE_ALLOW_LOCAL_PLAINTEXT="true", ACTIVE_TOKEN_KEY_ID="current",
            TOKEN_ENCRYPTION_KEYS=(directory / "retained-keyring.json").read_text(),
        )
        verifier = [str(repo / "docs/verify-restore.sh")]
        run("reject-unquarantined", verifier, restore_env, expect_failure="active restored accounts")
        run("quarantine", [psql, "-X", "--dbname", restored_url, "-v", "ON_ERROR_STOP=1", "-v", "restore_isolated=true", "-v", "maintenance_confirmed=true", "-f", str(repo / "docs/restore-quarantine.sql")], env)
        wrong_keys = json.dumps({name: base64.b64encode(secrets.token_bytes(32)).decode() for name in ("old", "current")})
        run("reject-wrong-keyring", verifier, dict(restore_env, TOKEN_ENCRYPTION_KEYS=wrong_keys), expect_failure="credential cannot be authenticated and decrypted")
        missing_receipt = """BEGIN;
            CREATE TABLE sp_verify_restore_saved_event AS
                SELECT * FROM sp_events WHERE kind='target.published' ORDER BY id LIMIT 1;
            DO $$ BEGIN IF (SELECT count(*) FROM sp_verify_restore_saved_event)<>1 THEN
                RAISE EXCEPTION 'Missing publication receipt fixture'; END IF; END $$;
            DELETE FROM sp_events WHERE id IN (SELECT id FROM sp_verify_restore_saved_event);
            INSERT INTO sp_events(post_id,target_id,account_id,kind,detail)
                SELECT post_id,target_id,account_id,'target.restore_quarantined',
                    'Explicit negative fixture: a new audit row cannot replace an original receipt.'
                FROM sp_verify_restore_saved_event CROSS JOIN generate_series(1,2);
            COMMIT;"""
        run("remove-original-receipt-add-new-audits", [psql, "-X", "--dbname", restored_url, "-v", "ON_ERROR_STOP=1", "-c", missing_receipt], env)
        run("reject-lost-original-receipt", verifier, restore_env, expect_failure="retained backup events:")
        run("reinstate-original-receipt", [psql, "-X", "--dbname", restored_url, "-v", "ON_ERROR_STOP=1", "-c",
            "BEGIN; INSERT INTO sp_events OVERRIDING SYSTEM VALUE SELECT * FROM sp_verify_restore_saved_event; DROP TABLE sp_verify_restore_saved_event; COMMIT;"], env)
        # Exercise the real restore entry point with the inputs that previously
        # produced a zero-test success. Its own boundary must neutralize them.
        hostile_go_inputs = dict(restore_env, GOFLAGS="-skip=TestRestoredDatabaseSafety",
                                 GOWORK=str(directory / "must-not-load.work"), GOENV=str(directory / "must-not-load.goenv"))
        verified = run("verify-retained-keyring", verifier, hostile_go_inputs)
        acceptance = json.loads(verified.stdout)
        expected_acceptance = {
            "kind": "native_postgres_restore_acceptance", "passed": True,
            "package": "socialpublisher/internal/ops", "test": "TestRestoredDatabaseSafety",
            "test_runs": 1, "test_passes": 1, "package_passes": 1,
            "skips": 0, "failures": 0, "native_preflight_passed": True, "errors": [],
        }
        if any(acceptance.get(key) != value for key, value in expected_acceptance.items()):
            raise RuntimeError("Restore acceptance did not execute the required native test exactly once with no skips")
        run("verify-preserved-records", [go, "run", "./scripts/verify/restorefixture", "verify"], dict(env, RESTORE_FIXTURE_DATABASE_URL=restored_url))
        if before != fingerprint("source-after"):
            raise RuntimeError("Source or restore procedure changed during verification; freeze the release and rerun")
        summary = {
            "kind": "native_postgres_restore_smoke", "passed": True,
            "source_sha256": before["sha256"], "server_version": preflight["server_version"],
            "independent_backend_pids": preflight["backend_pids"],
            "dump_sha256": hashlib.sha256(backup.read_bytes()).hexdigest(),
            "posts": manifest["posts"], "targets": manifest["targets"],
            "events": manifest["events"], "max_event_id": manifest["max_event_id"],
            "encrypted_accounts": manifest["encrypted_accounts"], "retained_key_ids": ["old", "current"],
            "rejected_unquarantined_restore": True, "rejected_wrong_keyring": True,
            "rejected_missing_original_receipt_after_new_audits": True,
            "standalone_acceptance_ignored_hostile_go_inputs": True,
            "process_exit_codes": results,
        }
    finally:
        cleanup_errors = []
        for database in reversed(created):
            try:
                run("drop-" + database, [psql, "-X", "--dbname", original, "-v", "ON_ERROR_STOP=1", "-c", 'DROP DATABASE "' + database + '" WITH (FORCE)'], env, timeout=30)
            except Exception:
                cleanup_errors.append(database)
        if cleanup_errors:
            raise RuntimeError("Fixture database cleanup failed; inspect its recorded drop logs")
    summary["fixture_databases_removed"] = len(created)
    summary_path.write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary, indent=2), flush=True)
    print("Restore evidence: " + str(directory), flush=True)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(json.dumps({"kind": "native_postgres_restore_smoke", "passed": False, "error": str(error)}))
        sys.exit(1)
