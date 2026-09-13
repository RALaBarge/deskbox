#!/usr/bin/env python3
import json
import secrets
import string
import sys

POOLS = {
    "alphanumeric": string.ascii_letters + string.digits,
    "hex": string.hexdigits,
    "digits": string.digits,
    "readable": string.ascii_lowercase.replace("l", "").replace("o", "")
    + string.digits.replace("0", "").replace("1", ""),
    "ascii": string.printable.strip(),
}


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    length = int(payload.get("length", 16))
    charset = payload.get("charset", "alphanumeric")
    pool = POOLS.get(charset, POOLS["alphanumeric"])

    result = "".join(secrets.choice(pool) for _ in range(length))
    json.dump({"result": result}, sys.stdout)


if __name__ == "__main__":
    main()
