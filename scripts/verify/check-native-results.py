#!/usr/bin/env python3
"""Fail the release gate on skips, missing required tests, or non-passing runs."""

import json
import sys
from pathlib import Path


ORIGINAL_NATIVE = {
        "TestPostgresNativeConcurrentIdempotency",
        "TestPostgresNativeOAuthConsumeIsAtomic",
        "TestPostgresNativeIdentityLocksCancelAndRelease",
        "TestPostgresNativeClaimsAreExclusive",
        "TestPostgresNativeClaimCancelAndRescheduleRaces",
        "TestPostgresNativeDisconnectDoesNotDeadlockAuditForeignKey",
        "TestPostgresNativeAccountDisableSerializesWithRetryAndResume",
        "TestPostgresNativeLockBackendLossCancelsGuard",
}
REQUIRED = {
    "socialpublisher/internal/store": ORIGINAL_NATIVE | {
        "TestPostgresNativeAdmissionSharedAcrossReplicas",
        "TestPostgresNativeTryAccountAdmissionIsBoundedAndGuardSurvives",
        "TestPostgresNativeProviderPacingFirstReservationRace",
        "TestPostgresNativeTryAccountGuardCancelsAfterBackendLoss",
        "TestPostgresNativeConcurrentRefreshIntentHasOneWinner",
        "TestPostgresNativeRefreshCompletionCannotDisableReconnect",
    },
    "socialpublisher/internal/api": {"TestDatabaseBackedPostFlow"},
    "socialpublisher/internal/account": {
        "TestPostgresRefreshCrashRecovery",
        "TestPostgresMalformedRefreshNeverReusesOldGrant",
    },
    "socialpublisher/internal/worker": {
        "TestNativeWorkerOtherAccountFinishesWhileBusyAccountIsBlocked",
        "TestNativeWorkerTikTokStatusPacingAcrossTargetsAndReplicas",
    },
}


def check(path, source_snapshot=None):
    required = {package: set(tests) for package, tests in REQUIRED.items()}
    if source_snapshot:
        snapshot = json.loads(Path(source_snapshot).read_text())
        for item in snapshot.get("native_tests", []):
            if item["package"] in required:
                required[item["package"]].add(item["test"])
    actions = {}
    package_actions = {}
    errors = []
    events = 0
    for number, line in enumerate(Path(path).read_text().splitlines(), 1):
        try:
            event = json.loads(line)
        except (json.JSONDecodeError, TypeError):
            errors.append(f"line {number}: malformed go test JSON")
            continue
        if not isinstance(event, dict):
            errors.append(f"line {number}: go test event is not an object")
            continue
        events += 1
        action = event.get("Action", "")
        package = event.get("Package", "")
        test = event.get("Test", "")
        if test:
            actions.setdefault((package, test), []).append(action)
        else:
            package_actions.setdefault(package, []).append(action)
        if action in {"skip", "fail"}:
            errors.append(f"{action}: {package}/{test}".rstrip("/"))
    if not events:
        errors.append("no go test events were recorded")
    for package, tests in required.items():
        if package_actions.get(package, []).count("pass") != 1:
            errors.append(f"package did not complete exactly one passing run: {package}")
        for test in sorted(tests):
            seen = actions.get((package, test), [])
            if seen.count("run") != 1 or seen.count("pass") != 1:
                errors.append(f"required test did not run and pass exactly once: {package}/{test}")
    for (package, test), seen in actions.items():
        if seen.count("run") != 1 or seen.count("pass") != 1:
            errors.append(f"test has an incomplete or non-passing run: {package}/{test}")
    summary = {
        "kind": "native_postgres_gate",
        "passed": not errors,
        "required_original_native_tests_passed": sum(
            actions.get(("socialpublisher/internal/store", test), []).count("pass") == 1
            for test in ORIGINAL_NATIVE
        ),
        "top_level_tests_passed": sum(
            "/" not in test and seen.count("pass") == 1
            for (_, test), seen in actions.items()
        ),
        "skips": sum(seen.count("skip") for seen in actions.values()),
        "errors": errors,
    }
    return summary


if __name__ == "__main__":
    if len(sys.argv) not in {2, 3}:
        sys.exit("usage: check-native-results.py GO_TEST_JSON_FILE [SOURCE_SNAPSHOT_JSON]")
    result = check(sys.argv[1], sys.argv[2] if len(sys.argv) == 3 else None)
    print(json.dumps(result, indent=2))
    sys.exit(0 if result["passed"] else 1)
