#!/usr/bin/env python3
import json
import re
import sys

_EMAIL_RE = re.compile(r"^[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}$")
_TYPO_MAP = {
    "gmai.com": "gmail.com", "gmial.com": "gmail.com", "gnail.com": "gmail.com",
    "yahooo.com": "yahoo.com", "yaho.com": "yahoo.com",
    "hotmial.com": "hotmail.com", "hotmai.com": "hotmail.com",
    "outlok.com": "outlook.com", "outllook.com": "outlook.com",
}


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    email = payload.get("email", "").strip()
    format_correct = bool(_EMAIL_RE.match(email))

    domain = email.rsplit("@", 1)[-1].lower() if "@" in email else ""
    suggestion = ""
    has_typo = domain in _TYPO_MAP
    if has_typo:
        local = email.rsplit("@", 1)[0]
        suggestion = f"{local}@{_TYPO_MAP[domain]}"

    json.dump({
        "valid": format_correct and not has_typo,
        "format_correct": format_correct,
        "has_common_typo": has_typo,
        "suggestion": suggestion,
    }, sys.stdout)


if __name__ == "__main__":
    main()
