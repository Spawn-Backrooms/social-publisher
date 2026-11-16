# Native PostgreSQL release checks

`native-postgres.sh` requires a disposable PostgreSQL database and a connection
path that preserves sessions. It verifies two distinct backend PIDs, isolated
session settings, and exclusive advisory locks before running the complete
store/API/account/worker suite with the Go race detector. The JSON result check rejects every
skip and failure, incomplete runs, and missing passes for the original eight
native concurrency tests, persistent API flow, refresh crash recovery, cross-replica
admission, worker fairness, and provider pacing. Native tests discovered in the
source snapshot are also required to execute. New tests in those
packages also have to pass. A process exit code alone cannot satisfy this gate.
Packages run sequentially (`-p 1`) to keep unrelated fixture setup from competing
for a small or emulated PostgreSQL service. Each test's independent database
sessions, explicit concurrent goroutines/replicas, race detection and interleaving
assertions remain active.

```sh
export TEST_DATABASE_URL='postgres://USER:PASSWORD@HOST:5432/publisher_test?sslmode=verify-full'
unset TEST_PGLITE
scripts/verify/native-postgres.sh
```

The role needs permission to create/drop isolated test schemas and terminate its
own lock backend. Use a direct writer or a confirmed session-preserving pool.
Transaction pooling and PGlite's shared socket backend do not meet this gate.
The local check does not establish behavior through a different production
network path; run the gate through that chosen path before activation.

To create a disposable cluster using installed PostgreSQL server tools, run as a
normal unprivileged OS user:

```sh
# Set PG_BIN when initdb and pg_ctl are not on PATH.
PG_BIN=/usr/lib/postgresql/18/bin scripts/verify/with-local-postgres.sh
```

`with-local-postgres.sh` starts and stops one stock native server on loopback port
55440, with a temporary data directory and no external credentials. Its optional
arguments replace the default gate command, so a server and its tests can run in
the same process environment. `PGPORT` selects a different free port. Root
execution is intentionally refused; a normal container/VM is the alternative in
an environment without an available unprivileged host user.
The optional [QEMU guest instructions](qemu/README.md) reproduce the native
environment used in a host with only UID/GID 0 available.

Both launchers accept `GO` and `PYTHON` executable paths. Set
`VERIFY_OUTPUT_DIR` to retain the preflight result, complete Go JSON output,
summary, toolchain version, Git revision when available, and source fingerprints
in a known directory. The gate also fails if its tested Go/SQL/verification source
changes during execution. Generated results and database contents are local evidence, not source
files. No real provider account or publication is used by these checks.
The three gates clear inherited Go overlays/workspaces and global Go environment
files (`GOFLAGS`, `GOWORK`, `GOENV`) so their commands use the captured module.
Each primary summary is marked non-passing before validation starts; a failed
rerun cannot retain a previous success as its current result. For the native
suite, `native-result-check.json` contains the Go result audit, while
`native-summary.json` reports success only after source stability is established.

The result checker can also verify a previously captured run:

```sh
python3 scripts/verify/check-native-results.py /absolute/path/native-tests.jsonl
```

This only audits that captured file. Fresh release approval still requires a
fresh preflight and test run against the release revision.

## Compiled executable smoke

Run the actual release executable against a disposable native PostgreSQL schema:

```sh
unset TEST_PGLITE
python3 scripts/verify/binary-smoke.py
# Or inspect a previously built release artifact:
python3 scripts/verify/binary-smoke.py --binary /absolute/path/publisher --revision RELEASE_DIGEST
```

The script builds a static executable when `--binary` is omitted. Its default
compiled revision is the verified source fingerprint; `--revision` or
`BUILD_REVISION` may select the expected 40- or 64-character lowercase release
digest. It checks that runtime environment changes cannot replace this revision
and that a mismatched expected revision rejects `migrate`, `api`, `worker`, and
`serve` before a database connection is attempted.

The smoke creates one uniquely named schema, rejects startup before migration,
applies migrations twice, then runs the executable in API-only and combined
serve modes. It checks health/readiness, JWT-protected routes, disabled providers,
separate operations authentication, the local process identity and actual
worker progress, and successful bounded SIGTERM shutdown. Each HTTP port is
selected from an available loopback socket immediately before startup. Generated
signing, encryption and operations secrets stay in process memory. No external
provider is configured or contacted. The schema is removed when the check ends.

Set `VERIFY_OUTPUT_DIR` to retain non-secret command/process logs, source
fingerprints, the executable and its SHA-256, and `binary-summary.json`. The
script also prints that concise result. As with the database suite, changing
source or executable bytes during the run fails the release check and requires a
fresh frozen run.

## Backup and restore smoke

With PostgreSQL 18 client utilities available, run an actual logical backup and
restore against a disposable local server:

```sh
unset TEST_PGLITE
PG_BIN=/usr/lib/postgresql/18/bin python3 scripts/verify/restore-smoke.py
```

The existing `TEST_DATABASE_URL` must name a loopback database whose role may
create disposable databases. The script creates and removes two uniquely named
fixture databases after an independent-session preflight, retains generated old/current encryption keys outside the
dump, and calls `pg_dump`, `pg_restore`, the restore quarantine SQL and the real
restore acceptance test. It requires rejection of an unquarantined database and
the wrong keyring, then checks all fixture ciphertext, retained checkpoints,
receipts, first-start/processing history, original events, idempotency replay,
owner isolation and stale lease/token fencing. No provider is contacted.
The named restore acceptance test must execute exactly once with no skip.
The backup manifest records the exact original-event count and maximum event ID.
The smoke also removes an original publication receipt and inserts two new audit
events; acceptance must still reject the missing original event. After restoring
that row, the valid-key acceptance must pass while inherited Go skip/workspace
inputs are neutralized by the standalone entry point.
Source and restore procedure fingerprints must remain unchanged, and both
fixture databases must be removed before the script records a passing summary.
Executable/restore fixture URLs accept only explicit authorities plus TLS and
connection options: query routing/runtime overrides and multihost authorities
are rejected, and inherited `PG*` connection defaults are cleared. Supply needed
authentication in the explicit fixture URL. `PG_BIN` continues to select client
utilities. The restore smoke additionally requires a loopback authority.

Set `VERIFY_OUTPUT_DIR` for the backup manifest, dump, private fixture keyring,
logs and summary. The keyring is synthetic test material and must not be reused
for deployment. The smoke proves the application's local restore procedure;
`docs/verify-restore.sh` must still run on a real isolated backup with the
independently retained production keyring before production recovery is accepted.
