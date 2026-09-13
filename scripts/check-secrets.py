#!/usr/bin/env python3
"""Reject secret findings except exact, reviewed synthetic fixture values."""

import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile

root = Path(__file__).resolve().parent.parent
fixtures = json.loads((root / "scripts/secret-fixtures.json").read_text())
allowed = {(f["path"], f["rule"], f["digest"]) for f in fixtures}
with tempfile.TemporaryDirectory(prefix="flint-secret-scan-") as temporary:
    report = Path(temporary) / "report.json"
    report.touch(mode=0o600)
    try:
        result = subprocess.run(
            ["gitleaks", "dir", str(root), "--no-banner", "--report-format", "json",
             "--report-path", str(report)],
            capture_output=True,
            check=False,
        )
    except FileNotFoundError:
        sys.exit("Install Gitleaks v8.30.1 before running this check.")
    if result.returncode not in (0, 1):
        sys.exit("Gitleaks failed; the source has not passed the secret check.")
    findings = json.loads(report.read_text())
    rejected = []
    for finding in findings:
        path = str(Path(finding["File"]).relative_to(root))
        digest = hashlib.sha256(finding["Secret"].encode()).hexdigest()
        if (path, finding["RuleID"], digest) not in allowed:
            rejected.append(f"{path}:{finding['StartLine']}: {finding['RuleID']}")
    if rejected:
        print("Unreviewed secret findings (values withheld):", file=sys.stderr)
        for location in rejected:
            print(location, file=sys.stderr)
        sys.exit(1)
    print(f"Secret check passed: {len(findings)} findings match reviewed fixture hashes.")
