#!/usr/bin/env python3
"""DuckDuckGo Instant Answer API — official public JSON endpoint. Stdlib only."""
import json
import sys
import urllib.parse
import urllib.request

_URL = "https://api.duckduckgo.com/"
_HEADERS = {"User-Agent": "Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120.0"}


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    query = payload.get("query", "")
    params = urllib.parse.urlencode({"q": query, "format": "json", "no_html": "1", "skip_disambig": "1"})
    req = urllib.request.Request(f"{_URL}?{params}", headers=_HEADERS)

    try:
        with urllib.request.urlopen(req, timeout=8) as resp:
            data = json.loads(resp.read().decode("utf-8", errors="replace"))
    except Exception as e:
        print(f"duckduckgo-instant-answer request failed: {e}", file=sys.stderr)
        sys.exit(1)

    related = []
    for item in data.get("RelatedTopics", []):
        text = item.get("Text")
        if text:
            related.append(text)
        elif "Topics" in item:
            for sub in item["Topics"]:
                if sub.get("Text"):
                    related.append(sub["Text"])

    json.dump({
        "heading": data.get("Heading", ""),
        "abstract": data.get("AbstractText", ""),
        "abstract_url": data.get("AbstractURL", ""),
        "abstract_source": data.get("AbstractSource", ""),
        "related_topics": related[:10],
    }, sys.stdout)


if __name__ == "__main__":
    main()
