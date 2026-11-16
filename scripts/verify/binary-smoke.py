#!/usr/bin/env python3
"""Verify the compiled publisher process against disposable native PostgreSQL.

Generated signing/encryption/operations secrets remain only in process memory.
No provider is configured. The final output is a concise non-secret JSON result.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
from urllib.parse import parse_qsl, urlencode, urlunsplit
import urllib.request

from postgres_url import explicit_connection_environment, validated_url


def main():
    root = Path(__file__).resolve().parents[2]
    directory = Path(os.environ.get("VERIFY_OUTPUT_DIR") or tempfile.mkdtemp(prefix="publisher-binary-smoke.")) / "binary-smoke"
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    directory = directory.resolve()
    summary_path = directory / "binary-summary.json"
    summary_path.write_text(json.dumps({"kind": "native_executable_smoke", "passed": False, "status": "started"}) + "\n")
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, default=os.environ.get("PUBLISHER_BINARY"), help="Existing release binary; otherwise build from this source")
    parser.add_argument("--revision", default=os.environ.get("BUILD_REVISION"), help="Expected 40/64-character release digest; defaults to the source fingerprint when building")
    args = parser.parse_args()
    dsn = os.environ.get("TEST_DATABASE_URL", "")
    parsed = validated_url(dsn)
    if "TEST_PGLITE" in os.environ:
        sys.exit("Unset TEST_PGLITE before native executable verification.")
    go = os.environ.get("GO", "go")
    environment = explicit_connection_environment(os.environ)
    environment.update(GOFLAGS="", GOWORK="off", GOENV="off")
    for name in list(environment):
        if name.startswith(("TIKTOK_", "X_", "INSTAGRAM_", "TEST_REFRESH_WORKER_")):
            environment.pop(name)
    protected_values = []

    def command(name, argv, env, expect_message=None, log=True, timeout=90):
        result = subprocess.run(argv, cwd=root, env=env, text=True, capture_output=True, timeout=timeout)
        text = result.stdout + result.stderr
        if any(value and value in text for value in protected_values):
            raise RuntimeError(name + " exposed generated secret material")
        if log:
            (directory / (name + ".log")).write_text(text)
        if expect_message is not None:
            if result.returncode == 0 or expect_message not in text:
                raise RuntimeError(name + " did not produce the expected startup rejection")
        elif result.returncode != 0:
            raise RuntimeError(name + " failed; inspect its smoke log")
        return result

    snapshot_command = [sys.executable, str(root / "scripts/verify/snapshot-source.py")]
    before = json.loads(command("source-before", snapshot_command, environment).stdout)
    command("native-preflight", [go, "run", "./scripts/verify/nativecheck"], environment)
    if args.binary:
        binary = args.binary.resolve()
        revision = args.revision
    else:
        revision = args.revision or before["sha256"]
        if not re.fullmatch(r"(?:[a-f0-9]{40}|[a-f0-9]{64})", revision):
            raise RuntimeError("--revision/BUILD_REVISION must be a 40/64-character lowercase hexadecimal digest")
        binary = directory / "publisher"
        command("build", [go, "build", "-trimpath", "-ldflags=-X socialpublisher/internal/buildinfo.Revision=" + revision, "-o", str(binary), "./cmd/publisher"], dict(environment, CGO_ENABLED="0"), timeout=300)
    binary_digest = hashlib.sha256(binary.read_bytes()).hexdigest()
    actual = json.loads(command("version", [str(binary), "version"], environment).stdout).get("build_revision")
    if not isinstance(actual, str) or not re.fullmatch(r"(?:[a-f0-9]{40}|[a-f0-9]{64})", actual):
        raise RuntimeError("Executable smoke requires a release digest embedded with BUILD_REVISION")
    if revision is not None and actual != revision:
        raise RuntimeError("Provided executable does not match the expected compiled revision")
    revision = actual
    different = ("1" if revision[0] != "1" else "2") + revision[1:]
    spoofed = json.loads(command("immutable-version", [str(binary), "version"], dict(environment, BUILD_REVISION=different)).stdout)
    if spoofed.get("build_revision") != revision:
        raise RuntimeError("Runtime environment replaced the compiled revision")
    for mode in ("migrate", "api", "worker", "serve"):
        command("reject-revision-" + mode, [str(binary), mode], dict(environment, EXPECTED_BUILD_REVISION=different, DATABASE_URL="postgres://unused@127.0.0.1:1/unused?sslmode=disable"), expect_message="container build revision does not match the expected release")
    generated = json.loads(command("keygen", [str(binary), "keygen"], environment, log=False).stdout)
    protected_values += [generated["JWT_HMAC_KEY"], generated["OPERATIONS_TOKEN"], generated["CONTROL_TOKEN"]]
    protected_values += list(generated["TOKEN_ENCRYPTION_KEYS"].values())
    schema = "sp_verify_smoke_" + secrets.token_hex(10)
    query = dict(parse_qsl(parsed.query, keep_blank_values=True))
    query["search_path"] = schema
    scoped = urlunsplit(parsed._replace(query=urlencode(query)))
    environment.update({
        "DATABASE_URL": scoped, "DEV_MODE": "true", "PUBLIC_BASE_URL": "http://127.0.0.1:18080", "PORT": "18080",
        "EXPECTED_BUILD_REVISION": revision, "JWT_HMAC_KEY": generated["JWT_HMAC_KEY"],
        "JWT_ISSUER": "native-executable-smoke", "JWT_AUDIENCE": "native-executable-smoke",
        "TOKEN_ENCRYPTION_KEYS": json.dumps(generated["TOKEN_ENCRYPTION_KEYS"]),
        "ACTIVE_TOKEN_KEY_ID": generated["ACTIVE_TOKEN_KEY_ID"], "OPERATIONS_TOKEN": generated["OPERATIONS_TOKEN"],
        "WORKER_CONCURRENCY": "1", "DB_MAX_CONNS": "4", "JOB_TIMEOUT_SECONDS": "30", "MEDIA_ALLOWED_HOSTS": "",
        "API_REQUESTS_PER_MINUTE": "60", "API_OWNER_CONCURRENCY": "4", "API_ACCOUNT_CONCURRENCY": "1",
        "PENDING_TARGETS_PER_OWNER": "1000", "RETENTION_BATCH_SIZE": "500", "EVENT_RETENTION_DAYS": "90",
        "MAX_MEDIA_BYTES": "1048576",
    })
    command("create-schema", [go, "run", "./scripts/verify/testdb", "create-schema", schema], environment)
    observations = {}
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    previous_instance = None
    try:
        command("reject-unmigrated", [str(binary), "serve"], environment, expect_message="database schema is not ready")
        command("migrate-initial", [str(binary), "migrate"], environment)
        command("migrate-replay", [str(binary), "migrate"], environment)
        token = command("mint-user-token", [str(binary), "token", "--tenant", "smoke-tenant", "--user", "smoke-user", "--ttl", "5m"], environment, log=False).stdout.strip()
        protected_values.append(token)
        for mode in ("api", "serve"):
            with socket.socket() as reservation:
                reservation.bind(("127.0.0.1", 0))
                port = reservation.getsockname()[1]
            origin = "http://127.0.0.1:" + str(port)
            mode_env = dict(environment, PORT=str(port), PUBLIC_BASE_URL=origin)
            logfile = directory / (mode + "-process.log")
            with logfile.open("w") as output:
                process = subprocess.Popen([str(binary), mode], cwd=root, env=mode_env, stdout=output, stderr=subprocess.STDOUT)

                def request(path, auth=None, operations=None, body=None):
                    headers = {"Content-Type": "application/json"}
                    if auth is not None:
                        headers["Authorization"] = "Bearer " + auth
                    if operations is not None:
                        headers["X-Operations-Token"] = operations
                    req = urllib.request.Request(origin + path, data=body, headers=headers)
                    try:
                        response = opener.open(req, timeout=5)
                    except urllib.error.HTTPError as error:
                        response = error
                    with response:
                        return response.status, json.load(response)

                try:
                    deadline = time.monotonic() + 45
                    while time.monotonic() < deadline:
                        if process.poll() is not None:
                            raise RuntimeError(mode + " exited before HTTP startup")
                        try:
                            if request("/healthz") == (200, {"status": "ok"}):
                                break
                        except (urllib.error.URLError, TimeoutError):
                            pass
                        time.sleep(0.1)
                    else:
                        raise RuntimeError(mode + " did not become healthy")
                    if request("/readyz") != (200, {"status": "ready"}):
                        raise RuntimeError(mode + " readiness failed")
                    for path in ("/v1/accounts", "/v1/posts", "/v1/platforms"):
                        if request(path)[0] != 401 or request(path, auth="invalid.fixture.jwt")[0] != 401:
                            raise RuntimeError("Private route accepted missing/invalid signed identity")
                    if request("/v1/accounts", auth=token) != (200, {"accounts": []}) or request("/v1/posts", auth=token) != (200, {"posts": []}):
                        raise RuntimeError("Signed user did not receive its empty owned collections")
                    code, platforms = request("/v1/platforms", auth=token)
                    if code != 200 or len(platforms.get("platforms", [])) != 3 or any(item["configured"] for item in platforms["platforms"]):
                        raise RuntimeError("A provider was unexpectedly configured")
                    if request("/v1/connections/x/authorize", auth=token, body=b"{}")[0] != 400:
                        raise RuntimeError("Disabled provider OAuth was not rejected")
                    if request("/internal/ops")[0] != 401 or request("/internal/ops", auth=token)[0] != 401 or request("/internal/ops", operations="invalid.fixture.ops")[0] != 401:
                        raise RuntimeError("Operations route accepted insufficient authority")
                    deadline = time.monotonic() + 40
                    while time.monotonic() < deadline:
                        code, snapshot = request("/internal/ops", operations=generated["OPERATIONS_TOKEN"])
                        local = snapshot.get("local_process") or {}
                        # Startup grace is useful readiness, but this empty
                        # fixture must also demonstrate a completed dispatch
                        # iteration persisted by a real heartbeat.
                        progressed = mode == "api" or (local.get("worker_progress_sequence", 0) > 0 and local.get("worker_progress_fresh"))
                        if code == 200 and snapshot.get("healthy") and progressed:
                            break
                        time.sleep(0.2)
                    else:
                        raise RuntimeError(mode + " never reported healthy process/worker operations")
                    snapshot_text = json.dumps(snapshot, indent=2) + "\n"
                    if any(value and value in snapshot_text for value in protected_values):
                        raise RuntimeError(mode + " operations response exposed generated secret material")
                    (directory / (mode + "-operations.json")).write_text(snapshot_text)
                    local = snapshot.get("local_process") or {}
                    if snapshot.get("role") != mode or snapshot.get("build_revision") != revision or local.get("role") != mode or local.get("build_revision") != revision or not local.get("heartbeat_fresh"):
                        raise RuntimeError("Operations identity did not match this executable process")
                    if previous_instance == snapshot.get("instance_id"):
                        raise RuntimeError("New process reused the previous operations identity")
                    previous_instance = snapshot.get("instance_id")
                    if mode == "api" and (local.get("worker_progress_sequence") != 0 or local.get("worker_progress_at") is not None or local.get("worker_progress_fresh")):
                        raise RuntimeError("API-only process fabricated worker progress")
                    if mode == "serve" and (local.get("worker_progress_sequence", 0) <= 0 or not local.get("worker_progress_fresh")):
                        raise RuntimeError("Combined process has no real fresh worker progress")
                    observations[mode] = {"health": True, "readiness": True, "operations_identity": True, "worker_progress_fresh": local.get("worker_progress_fresh", False), "jwt_private_routes": True, "providers_disabled": 3}
                finally:
                    if process.poll() is None:
                        process.send_signal(signal.SIGTERM)
                    try:
                        status = process.wait(timeout=25)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait()
                        raise RuntimeError(mode + " did not stop within the SIGTERM deadline")
                    if status != 0:
                        raise RuntimeError(mode + " exited unsuccessfully during SIGTERM shutdown")
                if any(value and value in logfile.read_text() for value in protected_values):
                    raise RuntimeError(mode + " process logged generated secret material")
                observations[mode]["sigterm_exit"] = 0
    finally:
        command("drop-schema", [go, "run", "./scripts/verify/testdb", "drop-schema", schema], environment)
    after = json.loads(command("source-after", snapshot_command, environment).stdout)
    if before != after:
        raise RuntimeError("Source changed during executable smoke; freeze the release and rerun")
    if hashlib.sha256(binary.read_bytes()).hexdigest() != binary_digest:
        raise RuntimeError("Executable changed during verification; freeze the release artifact and rerun")
    result = {
        "kind": "native_executable_smoke", "passed": True,
        "build_revision": revision, "binary_sha256": binary_digest,
        "compiled_revision_immutable": True, "mismatch_guards": ["migrate", "api", "worker", "serve"],
        "unmigrated_startup_rejected": True, "migration_runs": 2,
        "modes": observations, "generated_secrets_logged": False, "live_provider_requests": 0,
    }
    summary_path.write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(json.dumps({"kind": "native_executable_smoke", "passed": False, "error": str(error)}))
        sys.exit(1)
