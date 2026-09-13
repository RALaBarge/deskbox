#!/usr/bin/env python3
"""DuckDuckGo web search via the public HTML endpoint. Stdlib only.
Ported from ormcp's web_search.py (httpx -> urllib.request; parser unchanged).
"""
import json
import sys
import urllib.parse
import urllib.request
from html import unescape
from html.parser import HTMLParser

_DDG_HTML_URL = "https://html.duckduckgo.com/html/"
_HEADERS = {
    "User-Agent": "Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120.0",
    "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
    "Accept-Language": "en-US,en;q=0.5",
}


class _DDGResultParser(HTMLParser):
    """Extracts result blocks from DDG's HTML response.

    DDG renders each hit as:
        <a class="result__a" href="...">Title</a>
        <a/div/span class="result__snippet">Snippet...</a>
    Redirect URLs (//duckduckgo.com/l/?uddg=<encoded>) are unwrapped.
    """

    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.results = []
        self._cur_url = ""
        self._cur_title = []
        self._cur_snippet = []
        self._capturing_title = False
        self._capturing_snippet = False
        self._has_pending = False

    @staticmethod
    def _has_class(attrs, classname):
        for k, v in attrs:
            if k == "class" and v:
                return classname in v.split()
        return False

    @staticmethod
    def _attr(attrs, name):
        for k, v in attrs:
            if k == name:
                return v
        return None

    @staticmethod
    def _unwrap_ddg_redirect(url):
        if not url:
            return url
        if url.startswith("//"):
            url = "https:" + url
        try:
            parsed = urllib.parse.urlparse(url)
            if parsed.netloc.endswith("duckduckgo.com") and parsed.path.startswith("/l/"):
                qs = urllib.parse.parse_qs(parsed.query)
                target = qs.get("uddg", [None])[0]
                if target:
                    return urllib.parse.unquote(target)
        except Exception:
            pass
        return url

    def _flush_pending(self):
        if not self._has_pending:
            return
        title = " ".join("".join(self._cur_title).split())
        snippet = " ".join("".join(self._cur_snippet).split())
        url = self._cur_url
        if title and url:
            self.results.append({"title": unescape(title), "url": url, "snippet": unescape(snippet)})
        self._has_pending = False
        self._cur_url = ""
        self._cur_title = []
        self._cur_snippet = []

    def handle_starttag(self, tag, attrs):
        if tag == "a" and self._has_class(attrs, "result__a"):
            self._flush_pending()
            href = self._attr(attrs, "href") or ""
            self._cur_url = self._unwrap_ddg_redirect(href)
            self._capturing_title = True
            self._has_pending = True
        elif tag == "a" and self._has_class(attrs, "result__snippet"):
            self._capturing_snippet = True
        elif tag in ("div", "span") and self._has_class(attrs, "result__snippet"):
            self._capturing_snippet = True

    def handle_endtag(self, tag):
        if tag == "a" and self._capturing_title:
            self._capturing_title = False
        elif self._capturing_snippet and tag in ("a", "div", "span"):
            self._capturing_snippet = False

    def handle_data(self, data):
        if self._capturing_title:
            self._cur_title.append(data)
        elif self._capturing_snippet:
            self._cur_snippet.append(data)

    def close(self):
        self._flush_pending()
        super().close()


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    query = payload.get("query", "")
    max_results = int(payload.get("max_results", 5))

    body = urllib.parse.urlencode({"q": query, "b": ""}).encode()
    req = urllib.request.Request(_DDG_HTML_URL, data=body, headers=_HEADERS, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=12) as resp:
            html_text = resp.read().decode("utf-8", errors="replace")
    except Exception as e:
        print(f"duckduckgo-search request failed: {e}", file=sys.stderr)
        sys.exit(1)

    parser = _DDGResultParser()
    try:
        parser.feed(html_text)
        parser.close()
    except Exception:
        pass

    json.dump({"results": parser.results[:max_results]}, sys.stdout)


if __name__ == "__main__":
    main()
