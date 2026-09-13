#!/usr/bin/env python3
import json
import re
import sys


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    pattern = payload.get("pattern", "")
    text = payload.get("text", "")
    flags = re.IGNORECASE if payload.get("ignore_case") else 0

    try:
        compiled = re.compile(pattern, flags)
    except re.error as e:
        print(f"invalid regex: {e}", file=sys.stderr)
        sys.exit(1)

    matches = []
    for m in compiled.finditer(text):
        groups = [g if g is not None else "" for g in m.groups()]
        matches.append({"full_match": m.group(0), "groups": groups})

    json.dump({"match_count": len(matches), "matches": matches}, sys.stdout)


if __name__ == "__main__":
    main()
