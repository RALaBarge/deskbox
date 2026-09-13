#!/usr/bin/env python3
"""YAML <-> JSON conversion using the OS-packaged PyYAML (visible under /usr)."""
import json
import sys

try:
    import yaml
except ImportError:
    print("PyYAML not available on this system (expected at /usr/lib*/python3*/site-packages/yaml)", file=sys.stderr)
    sys.exit(1)


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    mode, text = payload.get("mode"), payload.get("text", "")
    try:
        if mode == "yaml_to_json":
            result = json.dumps(yaml.safe_load(text), indent=2)
        elif mode == "json_to_yaml":
            result = yaml.safe_dump(json.loads(text), sort_keys=False)
        else:
            print(f"unknown mode: {mode!r}. use yaml_to_json or json_to_yaml", file=sys.stderr)
            sys.exit(1)
    except Exception as e:
        print(f"conversion failed: {e}", file=sys.stderr)
        sys.exit(1)

    json.dump({"result": result}, sys.stdout)


if __name__ == "__main__":
    main()
