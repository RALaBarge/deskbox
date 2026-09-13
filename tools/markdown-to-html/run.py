#!/usr/bin/env python3
"""Minimal Markdown -> HTML. Stdlib only, deliberately a subset (see tcs.yaml)."""
import html as _html
import json
import re
import sys

_INLINE_CODE = re.compile(r"`([^`]+)`")
_BOLD = re.compile(r"\*\*([^*]+)\*\*")
_ITALIC = re.compile(r"(?<!\*)\*([^*]+)\*(?!\*)")
_LINK = re.compile(r"\[([^\]]+)\]\(([^)]+)\)")
_HEADER = re.compile(r"^(#{1,6})\s+(.*)$")
_UL_ITEM = re.compile(r"^[-*]\s+(.*)$")
_OL_ITEM = re.compile(r"^\d+\.\s+(.*)$")
_QUOTE = re.compile(r"^>\s?(.*)$")
_HR = re.compile(r"^(-{3,}|\*{3,}|_{3,})$")


def _inline(text: str) -> str:
    text = _html.escape(text)
    text = _INLINE_CODE.sub(r"<code>\1</code>", text)
    text = _BOLD.sub(r"<strong>\1</strong>", text)
    text = _ITALIC.sub(r"<em>\1</em>", text)
    text = _LINK.sub(r'<a href="\2">\1</a>', text)
    return text


def convert(markdown: str) -> str:
    lines = markdown.split("\n")
    out = []
    i = 0
    list_stack = None  # ("ul" | "ol", None) while inside a list
    in_code_block = False
    code_lines = []

    def close_list():
        nonlocal list_stack
        if list_stack:
            out.append(f"</{list_stack}>")
            list_stack = None

    while i < len(lines):
        line = lines[i]

        if line.strip().startswith("```"):
            if not in_code_block:
                in_code_block = True
                code_lines = []
            else:
                close_list()
                out.append("<pre><code>" + _html.escape("\n".join(code_lines)) + "</code></pre>")
                in_code_block = False
            i += 1
            continue
        if in_code_block:
            code_lines.append(line)
            i += 1
            continue

        if not line.strip():
            close_list()
            i += 1
            continue

        m = _HEADER.match(line)
        if m:
            close_list()
            level = len(m.group(1))
            out.append(f"<h{level}>{_inline(m.group(2))}</h{level}>")
            i += 1
            continue

        if _HR.match(line.strip()):
            close_list()
            out.append("<hr>")
            i += 1
            continue

        m = _UL_ITEM.match(line)
        if m:
            if list_stack != "ul":
                close_list()
                out.append("<ul>")
                list_stack = "ul"
            out.append(f"<li>{_inline(m.group(1))}</li>")
            i += 1
            continue

        m = _OL_ITEM.match(line)
        if m:
            if list_stack != "ol":
                close_list()
                out.append("<ol>")
                list_stack = "ol"
            out.append(f"<li>{_inline(m.group(1))}</li>")
            i += 1
            continue

        m = _QUOTE.match(line)
        if m:
            close_list()
            out.append(f"<blockquote>{_inline(m.group(1))}</blockquote>")
            i += 1
            continue

        close_list()
        out.append(f"<p>{_inline(line.strip())}</p>")
        i += 1

    close_list()
    if in_code_block:  # unterminated fence — emit what we have
        out.append("<pre><code>" + _html.escape("\n".join(code_lines)) + "</code></pre>")
    return "\n".join(out)


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    html_out = convert(payload.get("markdown", ""))
    json.dump({"html": html_out}, sys.stdout)


if __name__ == "__main__":
    main()
