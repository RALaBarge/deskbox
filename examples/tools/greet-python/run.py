#!/usr/bin/env python3
"""Read one JSON document on stdin, write exactly one on stdout."""
import json
import sys

payload = json.load(sys.stdin)

name = payload["name"]
punctuation = "!" if payload.get("excited") else "."
greeting = f"Hello, {name}{punctuation}"

json.dump({"greeting": greeting, "length": len(greeting)}, sys.stdout)
