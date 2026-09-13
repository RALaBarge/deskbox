#!/usr/bin/env python3
import json
import sys
import urllib.parse


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    mode, text = payload.get("mode"), payload.get("text", "")
    if mode == "encode":
        result = urllib.parse.quote(text)
    elif mode == "decode":
        result = urllib.parse.unquote(text)
    else:
        print(f"unknown mode: {mode!r}. use encode or decode", file=sys.stderr)
        sys.exit(1)

    json.dump({"result": result}, sys.stdout)


if __name__ == "__main__":
    main()
