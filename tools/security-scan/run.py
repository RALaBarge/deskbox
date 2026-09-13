#!/usr/bin/env python3
"""Stdin/stdout wrapper around _security_scan_lib.security_scan().
The library module is ported verbatim from ormcp's security_scan.py.
Project arrives as an inline base64 zip (see tcs.yaml) and is extracted
into /deskbox/out/project before scanning.
"""
import base64
import io
import json
import os
import shutil
import sys
import zipfile

sys.path.insert(0, os.environ.get("DESKBOX_TOOL_DIR", os.path.dirname(os.path.abspath(__file__))))
from _security_scan_lib import security_scan  # noqa: E402

_PROJECT_DIR = "/deskbox/out/project"


def _safe_extract(zf: zipfile.ZipFile, dest: str) -> None:
    dest_abs = os.path.abspath(dest)
    for member in zf.infolist():
        target = os.path.abspath(os.path.join(dest, member.filename))
        if not (target == dest_abs or target.startswith(dest_abs + os.sep)):
            raise ValueError(f"zip entry escapes extraction dir: {member.filename!r}")
    zf.extractall(dest)


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    action = payload.get("action", "")
    try:
        raw = base64.b64decode(payload.get("project_zip_base64", ""))
    except Exception as e:
        print(f"invalid base64: {e}", file=sys.stderr)
        sys.exit(1)

    os.makedirs(_PROJECT_DIR, exist_ok=True)
    try:
        try:
            with zipfile.ZipFile(io.BytesIO(raw)) as zf:
                _safe_extract(zf, _PROJECT_DIR)
        except (zipfile.BadZipFile, ValueError) as e:
            print(f"could not extract project zip: {e}", file=sys.stderr)
            sys.exit(1)

        report = security_scan(json.dumps({"action": action, "path": _PROJECT_DIR}))
        json.dump({"report": report}, sys.stdout)
    finally:
        # /deskbox/out/project is scratch space, not a declared result file
        # — an undeclared leftover trips the desk's "no undeclared writes"
        # contract check (auditOutDir in executor.go). Same fix as
        # mcp-probe's script cleanup, same reason.
        shutil.rmtree(_PROJECT_DIR, ignore_errors=True)


if __name__ == "__main__":
    main()
