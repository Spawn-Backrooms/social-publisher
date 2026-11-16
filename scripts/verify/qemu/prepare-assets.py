#!/usr/bin/env python3
"""Download pinned verification tools into a local directory without installing.

Digests in environment.json were checked against signed Ubuntu/PGDG repository
metadata and the pinned PostgreSQL npm package integrity record. Maintainer and
npm installation scripts are not executed.
"""

import argparse
import base64
import concurrent.futures
import hashlib
import json
import os
from pathlib import Path
import platform
import subprocess
import sys
import tarfile


def download(url, destination, algorithm, expected):
    if not destination.exists() or hashlib.new(algorithm, destination.read_bytes()).digest() != expected:
        subprocess.run([
            "curl", "--fail", "--location", "--silent", "--show-error",
            "--connect-timeout", "15", "--max-time", "180", url,
            "--output", str(destination),
        ], check=True)
    if hashlib.new(algorithm, destination.read_bytes()).digest() != expected:
        raise RuntimeError("Downloaded archive did not match its pinned digest: " + destination.name)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, required=True)
    args = parser.parse_args()
    if platform.system() != "Linux" or platform.machine() not in {"x86_64", "amd64"}:
        sys.exit("These pinned verification assets are for Linux x86_64.")
    script_dir = Path(__file__).resolve().parent
    metadata = json.loads((script_dir / "environment.json").read_text())
    output = args.output_dir.resolve()
    archives = output / "archives"
    archives.mkdir(parents=True, exist_ok=True)
    hostroot = output / "hostroot"
    hostroot.mkdir(exist_ok=True)

    def unpack_package(item):
        package, destination = item
        archive = archives / Path(package["Filename"]).name
        download(package["URL"], archive, "sha256", bytes.fromhex(package["SHA256"]))
        subprocess.run(["dpkg-deb", "--extract", str(archive), str(destination)], check=True)
        print("Verified and unpacked " + package["Package"] + " " + package["Version"], flush=True)

    items = [(package, hostroot) for package in metadata["ubuntu_packages"]]
    items += [(package, output / "pgclient") for package in metadata["postgres_client_packages"]]
    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
        list(pool.map(unpack_package, items))

    postgres = metadata["postgres_package"]
    algorithm, encoded = postgres["integrity"].split("-", 1)
    archive = archives / "postgres-native.tgz"
    download(postgres["resolved"], archive, algorithm, base64.b64decode(encoded))
    package_dir = output / "postgres-package"
    package_dir.mkdir(exist_ok=True)
    with tarfile.open(archive, "r:gz") as tar:
        tar.extractall(package_dir, filter="data")
    package_root = package_dir / "package"
    native = package_root / "native"
    for link in json.loads((native / "pg-symlinks.json").read_text()):
        source = (package_root / link["source"]).resolve(strict=True)
        destination = package_root / link["target"]
        if not source.is_relative_to(package_root) or not destination.parent.resolve().is_relative_to(package_root):
            raise RuntimeError("PostgreSQL symlink escaped its package directory")
        if not destination.exists():
            destination.symlink_to(os.path.relpath(source, destination.parent))
    for name, expected in metadata["postgres_binary_sha256"].items():
        if hashlib.sha256((native / "bin" / name).read_bytes()).hexdigest() != expected:
            raise RuntimeError("PostgreSQL executable digest mismatch: " + name)
    subprocess.run([
        sys.executable, str(script_dir / "build-initramfs.py"),
        "--postgres-root", str(native),
        "--busybox", str(hostroot / "usr/bin/busybox"),
        "--output-dir", str(output / "initramfs"),
    ], check=True)
    (output / "environment.json").write_text(json.dumps(metadata, indent=2) + "\n")
    print("Prepared native guest assets: " + str(output))
    print("Set NATIVE_GUEST_ASSETS to that directory and run scripts/verify/qemu/with-postgres.py.")


if __name__ == "__main__":
    main()
