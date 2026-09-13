#!/usr/bin/env python3
import json
import sys
from difflib import unified_diff


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    a = payload.get("text_a", "").splitlines(keepends=True)
    b = payload.get("text_b", "").splitlines(keepends=True)
    n = int(payload.get("context", 3))

    result = "".join(unified_diff(a, b, fromfile="a", tofile="b", n=n))
    json.dump({"diff": result, "has_differences": bool(result)}, sys.stdout)


if __name__ == "__main__":
    main()
