#!/usr/bin/env bash
set -euo pipefail

# All configuration comes from the invoking environment. Never echo the DSN,
# encryption keyring, OAuth verifier, or decrypted account token.
for name in RESTORE_DATABASE_URL TOKEN_ENCRYPTION_KEYS ACTIVE_TOKEN_KEY_ID RESTORE_EXPECTED_POSTS RESTORE_EXPECTED_TARGETS RESTORE_EXPECTED_ENCRYPTED_ACCOUNTS RESTORE_EXPECTED_EVENTS RESTORE_MAX_EVENT_ID; do
  if [[ -z "${!name:-}" ]]; then
    printf 'Restore verification requires %s from the isolated restore or backup manifest.\n' "$name" >&2
    exit 2
  fi
done
if [[ "${RESTORE_ISOLATED:-}" != true || "${RESTORE_MAINTENANCE_CONFIRMED:-}" != true ]]; then
  printf '%s\n' 'Set RESTORE_ISOLATED=true and RESTORE_MAINTENANCE_CONFIRMED=true only after isolating the restored database and stopping publication workers.' >&2
  exit 2
fi
if [[ -v TEST_PGLITE ]]; then
  printf '%s\n' 'Unset TEST_PGLITE before restore acceptance; a PGlite fixture is insufficient.' >&2
  exit 2
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
cd -- "$script_dir/.."
export RUN_RESTORE_VERIFY=1 GOFLAGS= GOWORK=off GOENV=off
exec python3 "$script_dir/check-restore-results.py" "${GO:-go}"
