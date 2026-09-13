#!/usr/bin/env python3
"""Fetch a URL, return clean text. Stdlib only.
Ported from ormcp's web_scraper.py; SSRF guard hand-rolled since
security.safe_url isn't importable inside the bwrap sandbox.
"""
import ipaddress
import json
import socket
import sys
import urllib.parse
import urllib.request
from html import unescape
from html.parser import HTMLParser

_HEADERS = {"User-Agent": "Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120.0"}
_SKIP_TAGS = frozenset({"script", "style", "nav", "footer", "header", "aside", "noscript"})
_BLOCK_TAGS = frozenset({
    "p", "div", "br", "li", "ul", "ol", "tr", "td", "th", "table",
    "h1", "h2", "h3", "h4", "h5", "h6", "section", "article",
    "blockquote", "pre", "hr", "figure", "figcaption", "main",
})


class SsrfRefusedError(Exception):
    pass


def _validate_url(url: str) -> None:
    parsed = urllib.parse.urlparse(url)
    if parsed.scheme not in ("http", "https"):
        raise SsrfRefusedError(f"scheme not allowed: {parsed.scheme!r}")
    host = parsed.hostname
    if not host:
        raise SsrfRefusedError("no hostname in URL")
    try:
        infos = socket.getaddrinfo(host, None)
    except socket.gaierror as e:
        raise SsrfRefusedError(f"could not resolve host: {e}")
    for info in infos:
        ip = ipaddress.ip_address(info[4][0])
        if ip.is_private or ip.is_loopback or ip.is_link_local or ip.is_reserved or ip.is_multicast:
            raise SsrfRefusedError(f"refusing non-public address: {ip}")


class _TextExtractor(HTMLParser):
    def __init__(self):
        super().__init__(convert_charrefs=True)
        self._stack = []
        self._skip_depth = 0
        self._chunks = []

    def handle_starttag(self, tag, attrs):
        tag = tag.lower()
        self._stack.append(tag)
        if tag in _SKIP_TAGS:
            self._skip_depth += 1
        if tag == "br" and self._skip_depth == 0:
            self._chunks.append("\n")

    def handle_startendtag(self, tag, attrs):
        tag = tag.lower()
        if tag == "br" and self._skip_depth == 0:
            self._chunks.append("\n")
        elif tag in _BLOCK_TAGS and self._skip_depth == 0:
            self._chunks.append("\n")

    def handle_endtag(self, tag):
        tag = tag.lower()
        if tag in self._stack:
            while self._stack:
                popped = self._stack.pop()
                if popped in _SKIP_TAGS:
                    self._skip_depth = max(0, self._skip_depth - 1)
                if popped == tag:
                    break
        if tag in _BLOCK_TAGS and self._skip_depth == 0:
            self._chunks.append("\n")

    def handle_data(self, data):
        if self._skip_depth == 0:
            self._chunks.append(data)

    def get_text(self):
        raw = unescape("".join(self._chunks))
        lines = [line.strip() for line in raw.splitlines() if line.strip()]
        return "\n".join(lines)


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    url = payload.get("url", "")
    max_len = int(payload.get("max_content_length", 10000))

    try:
        _validate_url(url)
    except SsrfRefusedError as e:
        print(f"blocked: {e}", file=sys.stderr)
        sys.exit(1)

    req = urllib.request.Request(url, headers=_HEADERS)
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            raw_html = resp.read().decode("utf-8", errors="replace")
    except Exception as e:
        print(f"failed to fetch {url}: {e}", file=sys.stderr)
        sys.exit(1)

    parser = _TextExtractor()
    try:
        parser.feed(raw_html)
        parser.close()
    except Exception:
        pass
    text = parser.get_text()

    truncated = len(text) > max_len
    if truncated:
        text = text[:max_len] + "\n\n[... truncated]"

    json.dump({"text": text, "truncated": truncated}, sys.stdout)


if __name__ == "__main__":
    main()
