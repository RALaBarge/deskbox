#!/usr/bin/env python3
"""Stdin/stdout wrapper around NetworkAuditTool.run().
The library module is ported verbatim from ormcp's network_audit.py.
NetworkAuditTool.run() takes a space-delimited "command k=v k=v" string
(not JSON) -- this wrapper re-serializes the structured JSON input into
that format before calling it, and passes its JSON output straight through.
"""
import json
import os
import sys

sys.path.insert(0, os.environ.get("DESKBOX_TOOL_DIR", os.path.dirname(os.path.abspath(__file__))))
from _network_audit_lib import NetworkAuditTool  # noqa: E402


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    command = payload.get("command", "")
    if not command:
        print("missing required field: command", file=sys.stderr)
        sys.exit(1)

    kv_fields = ["subnet", "ip", "port", "protocol", "ports", "timeout", "service", "version"]
    parts = [command]
    for key in kv_fields:
        if key in payload and payload[key] not in (None, ""):
            parts.append(f"{key}={payload[key]}")
    command_line = " ".join(parts)

    tool = NetworkAuditTool()
    try:
        result = tool.run(command_line)
    except Exception as e:
        print(f"network-audit failed: {e}", file=sys.stderr)
        sys.exit(1)

    try:
        parsed = json.loads(result)
    except json.JSONDecodeError:
        parsed = {"raw": result}

    json.dump(parsed, sys.stdout)


if __name__ == "__main__":
    main()
