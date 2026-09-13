#!/usr/bin/env python3
import hashlib
import hmac
import json
import sys

ALGOS = {"sha256": hashlib.sha256, "sha1": hashlib.sha1, "sha512": hashlib.sha512}


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    message = payload.get("message", "")
    key = payload.get("key", "")
    algo = (payload.get("algorithm") or "sha256").lower()
    if algo not in ALGOS:
        print(f"unknown algorithm: {algo}. try: {', '.join(ALGOS)}", file=sys.stderr)
        sys.exit(1)

    digest = hmac.new(key.encode(), message.encode(), ALGOS[algo]).hexdigest()
    json.dump({"algorithm": algo, "hex_digest": digest}, sys.stdout)


if __name__ == "__main__":
    main()
