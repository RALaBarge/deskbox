#!/usr/bin/env python3
import json
import sys
import uuid

json.load(sys.stdin)  # contract requires a JSON doc on stdin even if unused
json.dump({"uuid": str(uuid.uuid4())}, sys.stdout)
