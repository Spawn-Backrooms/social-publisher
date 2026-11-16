# Native PostgreSQL when the host has no unprivileged user

This optional verification environment uses QEMU's software CPU emulator and an
unmodified Linux guest kernel. PostgreSQL runs as guest UID/GID 1000 and forks
real independent backend processes. The host uses ordinary files, process
execution and a loopback TCP listener. No KVM device, host mount, UID spoofing,
PostgreSQL binary patch, user-namespace remap, or host privilege escalation is
involved.

The normal `../native-postgres.sh` gate against an installed PostgreSQL service or
CI container remains the simpler option. This guest was used because the review
host mapped only UID/GID 0 and stock `initdb` correctly refused to run as root.

## Reproduce the guest

Use Linux x86_64 with an Ubuntu 24.04-compatible C runtime, Python 3.12 or later,
`curl`, `dpkg-deb`, `ldd`, and the project's Go toolchain. The preparer downloads
and unpacks pinned archives into the specified directory; it does not install
system packages or execute package installation scripts.

```sh
python3 scripts/verify/qemu/prepare-assets.py --output-dir /tmp/publisher-native-assets
NATIVE_GUEST_ASSETS=/tmp/publisher-native-assets \
  python3 scripts/verify/qemu/with-postgres.py
```

The default child command is the native store/API release gate. For a targeted
command, append its executable and arguments:

```sh
NATIVE_GUEST_ASSETS=/tmp/publisher-native-assets \
  python3 scripts/verify/qemu/with-postgres.py \
  go test -race -count=1 -v -run TestPostgresNative ./internal/store
```

The launcher injects `TEST_DATABASE_URL`, unsets `TEST_PGLITE`, waits for guest
PostgreSQL startup, runs the child, records its status and stops QEMU. It defaults
to port 55440; coordinate concurrent runs with `NATIVE_GUEST_PORT`. Set
`NATIVE_GUEST_OUTPUT_DIR` for serial logs and launcher metadata, and
`VERIFY_OUTPUT_DIR` for gate results. A guest and its test command run in the same
execution environment so tools with isolated loopback namespaces can use them.

Existing trusted tools can be used instead by setting `QEMU_BIN`,
`NATIVE_GUEST_KERNEL`, and `NATIVE_GUEST_INITRAMFS`; relocated QEMU packages may
also need `QEMU_BIOS`, `QEMU_DATA_DIR`, `QEMU_MODULE_DIR`, and `LD_LIBRARY_PATH`.
`build-initramfs.py --help` describes how to create the guest filesystem from
unmodified PostgreSQL files and static BusyBox. Its kernel must include VirtIO
PCI/network support, devtmpfs, tmpfs and the normal PostgreSQL process/IPC facilities.

## Provenance and limits

[environment.json](environment.json) records exact archive URLs, versions and
digests, the Ubuntu/PGDG signing fingerprints used to verify their package
indexes, and the pinned PostgreSQL npm integrity record. The guest uses QEMU
8.2.2, Ubuntu Linux 6.8.0-31, and PostgreSQL 18.4 from
`@embedded-postgres/linux-x64` 18.4.0-beta.17. That suffix belongs to the packaging
project; the executed server reports `PostgreSQL 18.4`. Server executables remain
unmodified. PostgreSQL 18.3 client utilities are included for dump/restore checks.

The initial successful preflight observed distinct backend PIDs 135 and 136,
isolated session settings, and exclusive advisory-lock acquisition and handoff.
That records environment capability. The release gate's fresh test output is the
evidence for a particular source revision.

The guest database is an ephemeral filesystem, with PostgreSQL fsync,
synchronous commit, and full-page writes enabled. It validates concurrent
sessions, SQL/locking semantics and process recovery, but does not establish a
managed database's power-loss durability, backup service, TLS, performance or
production network behavior. Loopback forwarding and restricted QEMU networking
are the only exposed transport. The disposable role uses trust authentication
only inside this isolated fixture; production requires its own verified TLS and
credentials.

Primary references: [QEMU network emulation](https://www.qemu.org/docs/master/system/devices/net.html),
[QEMU invocation](https://www.qemu.org/docs/master/system/invocation.html),
[PostgreSQL Debian packages](https://www.postgresql.org/download/linux/ubuntu/),
and [embedded-postgres source](https://github.com/leinelissen/embedded-postgres).
