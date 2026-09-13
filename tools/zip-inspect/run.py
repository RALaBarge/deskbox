#!/usr/bin/env python3
import base64
import io
import json
import sys
import zipfile


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    try:
        raw = base64.b64decode(payload.get("zip_base64", ""))
    except Exception as e:
        print(f"invalid base64: {e}", file=sys.stderr)
        sys.exit(1)

    try:
        entries = []
        with zipfile.ZipFile(io.BytesIO(raw)) as zf:
            for info in zf.infolist():
                entries.append({
                    "name": info.filename,
                    "size": info.file_size,
                    "compressed_size": info.compress_size,
                    "is_dir": info.is_dir(),
                })
    except zipfile.BadZipFile as e:
        print(f"not a valid zip file: {e}", file=sys.stderr)
        sys.exit(1)

    json.dump({"entry_count": len(entries), "entries": entries}, sys.stdout)


if __name__ == "__main__":
    main()
