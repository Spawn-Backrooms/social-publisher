#!/usr/bin/env python3
"""Fingerprint the Go source, embedded SQL, and verification code being tested."""

import hashlib
import json
import re
from pathlib import Path


root = Path(__file__).resolve().parents[2]
paths = [root / "go.mod", root / "go.sum"]
for directory in ("cmd", "internal", "scripts/verify", "docs"):
    paths.extend(
        path
        for path in (root / directory).rglob("*")
        if path.is_file() and path.suffix in {".go", ".sql", ".sh", ".py", ".json"}
        and not {"guestroot", "hostroot", "archives", "pgclient", "postgres-package", "node_modules", "__pycache__"}.intersection(path.relative_to(root).parts)
    )
files = {
    path.relative_to(root).as_posix(): hashlib.sha256(path.read_bytes()).hexdigest()
    for path in sorted(set(paths))
}
encoded = json.dumps(files, sort_keys=True, separators=(",", ":")).encode()
native_tests = []
for relative in files:
    if relative.endswith("_test.go"):
        package = "socialpublisher/" + str(Path(relative).parent)
        for name in re.findall(r"^func\s+(Test\w+)\s*\(", (root / relative).read_text(), re.MULTILINE):
            if "Native" in name:
                native_tests.append({"package": package, "test": name})
print(json.dumps({"sha256": hashlib.sha256(encoded).hexdigest(), "files": files, "native_tests": native_tests}, indent=2))
