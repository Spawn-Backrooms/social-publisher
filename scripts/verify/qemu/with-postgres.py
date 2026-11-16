#!/usr/bin/env python3
"""Run a command with genuine PostgreSQL sessions in a software-emulated guest.

The host never changes identity, mounts a guest filesystem, or uses /dev/kvm.
PostgreSQL runs as guest UID/GID 1000 under an unmodified Linux guest kernel.
"""

import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import time


def main():
    assets = os.environ.get("NATIVE_GUEST_ASSETS")
    if assets:
        root = Path(assets).resolve()
        defaults = {
            "QEMU_BIN": root / "hostroot/usr/bin/qemu-system-x86_64",
            "QEMU_BIOS": root / "hostroot/usr/share/seabios/bios-256k.bin",
            "QEMU_DATA_DIR": root / "hostroot/usr/share/qemu",
            "QEMU_MODULE_DIR": root / "hostroot/usr/lib/x86_64-linux-gnu/qemu",
            "NATIVE_GUEST_KERNEL": root / "hostroot/boot/vmlinuz-6.8.0-31-generic",
            "NATIVE_GUEST_INITRAMFS": root / "initramfs/native-postgres-initramfs.cpio.gz",
        }
        for name, value in defaults.items():
            os.environ.setdefault(name, str(value))
        libraries = str(root / "hostroot/usr/lib/x86_64-linux-gnu")
        os.environ["LD_LIBRARY_PATH"] = libraries + (":" + os.environ["LD_LIBRARY_PATH"] if os.environ.get("LD_LIBRARY_PATH") else "")
    kernel = os.environ.get("NATIVE_GUEST_KERNEL")
    initramfs = os.environ.get("NATIVE_GUEST_INITRAMFS")
    qemu_bin = os.environ.get("QEMU_BIN") or shutil.which("qemu-system-x86_64")
    if not qemu_bin or not kernel or not initramfs:
        sys.exit("Set NATIVE_GUEST_KERNEL and NATIVE_GUEST_INITRAMFS; install QEMU or set QEMU_BIN.")
    port = int(os.environ.get("NATIVE_GUEST_PORT", "55440"))
    if not 1024 <= port <= 65535:
        sys.exit("NATIVE_GUEST_PORT must be between 1024 and 65535.")
    with socket.socket() as reservation:
        reservation.bind(("127.0.0.1", port))
    output = Path(os.environ.get("NATIVE_GUEST_OUTPUT_DIR") or tempfile.mkdtemp(prefix="publisher-native-guest."))
    output.mkdir(parents=True, exist_ok=True)
    output = output.resolve()
    serial = output / "guest-serial.log"
    qemu = [
        qemu_bin, "-accel", "tcg,thread=multi", "-cpu", "max", "-smp", "4",
        "-m", "2048", "-nodefaults", "-display", "none", "-no-reboot",
        "-kernel", kernel, "-initrd", initramfs,
        "-append", "console=ttyS0 rdinit=/init loglevel=4 panic=-1",
        "-serial", "file:" + str(serial), "-monitor", "none",
        "-device", "virtio-net-pci,netdev=net0,romfile=",
        "-netdev", f"user,id=net0,restrict=on,hostfwd=tcp:127.0.0.1:{port}-:5432",
    ]
    if os.environ.get("QEMU_BIOS"):
        qemu += ["-bios", os.environ["QEMU_BIOS"]]
    if os.environ.get("QEMU_DATA_DIR"):
        qemu += ["-L", os.environ["QEMU_DATA_DIR"]]
    (output / "qemu-command.json").write_text(json.dumps(qemu, indent=2) + "\n")
    with (output / "qemu-stderr.log").open("wb") as log:
        process = subprocess.Popen(qemu, stdout=log, stderr=subprocess.STDOUT)
        try:
            deadline = time.monotonic() + 180
            while time.monotonic() < deadline:
                text = serial.read_text(errors="replace") if serial.exists() else ""
                if "NATIVE_POSTGRES_STARTED" in text:
                    break
                if process.poll() is not None:
                    raise RuntimeError(f"QEMU exited {process.returncode}; inspect {output / 'qemu-stderr.log'}")
                if "Kernel panic" in text:
                    raise RuntimeError("Guest kernel panicked: " + text[-4000:])
                time.sleep(0.2)
            else:
                raise RuntimeError("Guest readiness timed out: " + text[-4000:])
            print(json.dumps({"guest_ready": True, "host_port": port, "output_directory": str(output)}), flush=True)
            for line in text.splitlines():
                if "GUEST_" in line:
                    print(line, flush=True)
            child_env = os.environ.copy()
            child_env.pop("TEST_PGLITE", None)
            child_env["TEST_DATABASE_URL"] = f"postgres://publisher@127.0.0.1:{port}/postgres?sslmode=disable"
            child_env.setdefault("VERIFY_OUTPUT_DIR", str(output / "evidence"))
            child_env["NATIVE_GUEST_RUN_DIR"] = str(output)
            command = sys.argv[1:] or [str(Path(__file__).resolve().parents[1] / "native-postgres.sh")]
            result = subprocess.run(command, env=child_env)
            (output / "command-status.json").write_text(json.dumps({"argv": command, "returncode": result.returncode}) + "\n")
            return result.returncode
        finally:
            process.terminate()
            try:
                process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
            print("Guest evidence: " + str(output), flush=True)


if __name__ == "__main__":
    sys.exit(main())
