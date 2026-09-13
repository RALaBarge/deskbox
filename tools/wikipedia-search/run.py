#!/usr/bin/env python3
"""Wikipedia full-text search via the official MediaWiki API. Stdlib only."""
import json
import re
import sys
import urllib.parse
import urllib.request

_URL = "https://en.wikipedia.org/w/api.php"
_HEADERS = {"User-Agent": "Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120.0"}
_TAG_RE = re.compile(r"<[^>]+>")


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    query = payload.get("query", "")
    limit = int(payload.get("limit", 5))

    params = urllib.parse.urlencode({
        "action": "query", "list": "search", "srsearch": query,
        "format": "json", "srlimit": limit,
    })
    req = urllib.request.Request(f"{_URL}?{params}", headers=_HEADERS)

    try:
        with urllib.request.urlopen(req, timeout=8) as resp:
            data = json.loads(resp.read().decode("utf-8", errors="replace"))
    except Exception as e:
        print(f"wikipedia-search request failed: {e}", file=sys.stderr)
        sys.exit(1)

    results = []
    for hit in data.get("query", {}).get("search", []):
        title = hit.get("title", "")
        snippet = _TAG_RE.sub("", hit.get("snippet", ""))
        results.append({
            "title": title,
            "snippet": snippet,
            "url": "https://en.wikipedia.org/wiki/" + urllib.parse.quote(title.replace(" ", "_")),
        })

    json.dump({"results": results}, sys.stdout)


if __name__ == "__main__":
    main()
