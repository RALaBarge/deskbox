#!/usr/bin/env python3
import hashlib
import json
import sys

ALGOS = {"sha256": hashlib.sha256, "sha1": hashlib.sha1, "md5": hashlib.md5, "sha512": hashlib.sha512}


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    text = payload.get("text", "")
    algo = (payload.get("algorithm") or "sha256").lower()
    if algo not in ALGOS:
        print(f"unknown algorithm: {algo}. try: {', '.join(ALGOS)}", file=sys.stderr)
        sys.exit(1)

    digest = ALGOS[algo](text.encode()).hexdigest()
    json.dump({"algorithm": algo, "hex_digest": digest}, sys.stdout)


if __name__ == "__main__":
    main()
