#!/usr/bin/env bash
# Execute against a disposable direct/session-preserving PostgreSQL writer.
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_dir=$(cd -- "$script_dir/../.." && pwd)
go_bin=${GO:-go}
python_bin=${PYTHON:-python3}

evidence_dir=${VERIFY_OUTPUT_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/publisher-native-gate.XXXXXX")}
mkdir -p -- "$evidence_dir"
evidence_dir=$(cd -- "$evidence_dir" && pwd)
# A failed rerun must not leave a previous successful summary authoritative.
printf '%s\n' '{"kind":"native_postgres_gate","passed":false,"status":"started"}' > "$evidence_dir/native-summary.json"
trap 'gate_exit=$?; if [[ $gate_exit -ne 0 ]]; then printf "{\"kind\":\"native_postgres_gate\",\"passed\":false,\"status\":\"failed\",\"exit_code\":%d}\n" "$gate_exit" > "$evidence_dir/native-summary.json"; fi' EXIT
printf 'Native PostgreSQL evidence: %s\n' "$evidence_dir"

if [[ -z ${TEST_DATABASE_URL:-} ]]; then
  printf '%s\n' 'Set TEST_DATABASE_URL to a disposable native PostgreSQL database.' >&2
  exit 1
fi
if [[ ${TEST_PGLITE+x} ]]; then
  printf '%s\n' 'Unset TEST_PGLITE. Native evidence requires independent PostgreSQL sessions.' >&2
  exit 1
fi

cd -- "$repo_dir"
unset TEST_REFRESH_WORKER_MODE TEST_REFRESH_WORKER_DSN TEST_REFRESH_WORKER_ENDPOINT TEST_REFRESH_WORKER_MARKER
# Do not let an inherited overlay, alternate module file, or workspace replace
# the source/dependencies captured by this gate's fingerprint.
export GOFLAGS= GOWORK=off GOENV=off

"$go_bin" version > "$evidence_dir/go-version.txt"
if git rev-parse HEAD > "$evidence_dir/git-head.txt" 2>/dev/null; then
  git diff --binary HEAD | sha256sum > "$evidence_dir/worktree-diff.sha256"
else
  printf '%s\n' 'Source archive; no Git revision is available.' > "$evidence_dir/git-head.txt"
fi
"$python_bin" "$script_dir/snapshot-source.py" > "$evidence_dir/source-before.json"
"$go_bin" run ./scripts/verify/nativecheck > "$evidence_dir/native-preflight.json"
cat "$evidence_dir/native-preflight.json"

set +e
"$go_bin" test -p 1 -race -count=1 -run . -skip '^$' -json ./internal/store ./internal/api ./internal/account ./internal/worker > "$evidence_dir/native-tests.jsonl" 2> "$evidence_dir/native-tests.stderr"
test_status=$?
set -e
cat "$evidence_dir/native-tests.stderr" >&2
set +e
"$python_bin" "$script_dir/check-native-results.py" "$evidence_dir/native-tests.jsonl" "$evidence_dir/source-before.json" > "$evidence_dir/native-result-check.json"
summary_status=$?
set -e
cat "$evidence_dir/native-result-check.json"
"$python_bin" "$script_dir/snapshot-source.py" > "$evidence_dir/source-after.json"
if ! cmp -s "$evidence_dir/source-before.json" "$evidence_dir/source-after.json"; then
  printf '%s\n' 'Source changed during native verification. Freeze the release source and rerun.' >&2
  exit 1
fi
if [[ $test_status -ne 0 || $summary_status -ne 0 ]]; then
  printf 'Native PostgreSQL gate failed (go test=%d, result checks=%d).\n' "$test_status" "$summary_status" >&2
  exit 1
fi
cp -- "$evidence_dir/native-result-check.json" "$evidence_dir/native-summary.json"
