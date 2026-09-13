#!/usr/bin/env python3
import base64
import json
import sys


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    mode, text = payload.get("mode"), payload.get("text", "")
    try:
        if mode == "encode":
            result = base64.b64encode(text.encode()).decode()
        elif mode == "decode":
            result = base64.b64decode(text).decode()
        else:
            print(f"unknown mode: {mode!r}. use encode or decode", file=sys.stderr)
            sys.exit(1)
    except Exception as e:
        print(f"base64 {mode} failed: {e}", file=sys.stderr)
        sys.exit(1)

    json.dump({"result": result}, sys.stdout)


if __name__ == "__main__":
    main()
