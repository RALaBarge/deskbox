#!/usr/bin/env python3
import csv
import io
import json
import sys


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    mode, text = payload.get("mode"), payload.get("text", "")

    try:
        if mode == "to_json":
            reader = csv.DictReader(io.StringIO(text))
            result = json.dumps(list(reader), indent=2)
        elif mode == "from_json":
            rows = json.loads(text)
            if not isinstance(rows, list):
                raise ValueError("from_json input must be a JSON array of objects")
            columns = payload.get("columns") or (list(rows[0].keys()) if rows else [])
            buf = io.StringIO()
            writer = csv.DictWriter(buf, fieldnames=columns, extrasaction="ignore")
            writer.writeheader()
            writer.writerows(rows)
            result = buf.getvalue()
        else:
            print(f"unknown mode: {mode!r}. use to_json or from_json", file=sys.stderr)
            sys.exit(1)
    except Exception as e:
        print(f"csv-tool {mode} failed: {e}", file=sys.stderr)
        sys.exit(1)

    json.dump({"result": result}, sys.stdout)


if __name__ == "__main__":
    main()
