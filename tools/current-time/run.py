#!/usr/bin/env python3
"""Current time + static-offset timezone conversion. Stdlib only.
Ported from ormcp's datetime_tool.py.
"""
import json
import sys
from datetime import datetime, timedelta, timezone

TIMEZONE_OFFSETS = {
    "utc": 0, "gmt": 0,
    "est": -5, "edt": -4, "et": -5,
    "cst": -6, "cdt": -5, "ct": -6,
    "mst": -7, "mdt": -6, "mt": -7,
    "pst": -8, "pdt": -7, "pt": -8,
    "cet": 1, "cest": 2, "eet": 2, "eest": 3, "msk": 3,
    "ist": 5.5, "sgt": 8, "jst": 9, "kst": 9,
    "aest": 10, "aedt": 11, "nzst": 12, "nzdt": 13,
    "new york": -5, "los angeles": -8, "chicago": -6, "denver": -7,
    "london": 0, "paris": 1, "berlin": 1, "moscow": 3,
    "dubai": 4, "mumbai": 5.5, "delhi": 5.5,
    "bangkok": 7, "beijing": 8, "shanghai": 8, "hong kong": 8,
    "tokyo": 9, "seoul": 9, "sydney": 10, "auckland": 12,
    "ann arbor": -5, "detroit": -5, "michigan": -5,
}


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    now_utc = datetime.now(timezone.utc)
    out = {
        "utc_iso": now_utc.isoformat(),
        "unix_timestamp": int(now_utc.timestamp()),
    }

    tz_query = (payload.get("timezone") or "").lower().strip()
    if tz_query:
        offset = None
        matched = None
        for tz_name, off in TIMEZONE_OFFSETS.items():
            if tz_name in tz_query:
                offset, matched = off, tz_name
                break
        if offset is None:
            print(f"unrecognized timezone: {tz_query!r}", file=sys.stderr)
            sys.exit(1)
        tz = timezone(timedelta(hours=offset))
        out["requested_tz"] = matched
        out["requested_tz_iso"] = now_utc.astimezone(tz).isoformat()
        out["requested_tz_utc_offset"] = offset

    json.dump(out, sys.stdout)


if __name__ == "__main__":
    main()
