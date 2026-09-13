#!/usr/bin/env python3
"""wttr.in weather lookup. Stdlib only. Ported from ormcp's weather.py."""
import json
import sys
import urllib.parse
import urllib.request

# wttr.in sniffs User-Agent to decide plain-text vs full HTML page: a
# browser-looking UA (what every other ported tool here uses) gets the
# HTML page even with ?format=..., silently defeating the format param.
# curl-like UAs get the terminal-friendly text. Caught by actually running
# this against the live endpoint rather than trusting the ported logic.
_HEADERS = {"User-Agent": "curl/8.0.0"}


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    location = payload.get("location", "")
    forecast = bool(payload.get("forecast", False))
    fmt = "?0" if forecast else "?format=%l:+%c+%t,+%w,+%h+humidity"
    url = f"https://wttr.in/{urllib.parse.quote(location)}{fmt}"

    req = urllib.request.Request(url, headers=_HEADERS)
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            text = resp.read().decode("utf-8", errors="replace").strip()
    except Exception as e:
        print(f"weather lookup failed: {e}", file=sys.stderr)
        sys.exit(1)

    json.dump({"report": text or f"No weather data for '{location}'"}, sys.stdout)


if __name__ == "__main__":
    main()
