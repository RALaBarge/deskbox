#!/usr/bin/env python3
"""Probe a stdio MCP server: initialize + tools/list. Stdlib only.
Ported from ormcp's mcp_probe.py.
"""
import base64
import json
import os
import stat
import subprocess
import sys
import threading

_OUT_DIR = "/deskbox/out"


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    command = payload.get("command", "")
    args = list(payload.get("args") or [])
    timeout = int(payload.get("timeout_sec", 15))

    script_path = None
    script_b64 = payload.get("script_base64")
    if script_b64:
        script_name = payload.get("script_name")
        if not script_name or "/" in script_name:
            print("script_name is required (single path segment) when script_base64 is set", file=sys.stderr)
            sys.exit(1)
        try:
            script_bytes = base64.b64decode(script_b64)
        except Exception as e:
            print(f"invalid base64 in script_base64: {e}", file=sys.stderr)
            sys.exit(1)
        script_path = os.path.join(_OUT_DIR, script_name)
        with open(script_path, "wb") as f:
            f.write(script_bytes)
        os.chmod(script_path, os.stat(script_path).st_mode | stat.S_IXUSR)
        args.append(script_path)

    proc = subprocess.Popen(
        [command] + args,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
    )

    timed_out = threading.Event()

    def kill():
        timed_out.set()
        try:
            proc.kill()
        except OSError:
            pass

    timer = threading.Timer(timeout, kill)
    timer.start()

    def send(msg):
        proc.stdin.write((json.dumps(msg) + "\n").encode())
        proc.stdin.flush()

    def recv():
        line = proc.stdout.readline()
        if not line:
            if timed_out.is_set():
                raise RuntimeError(f"probe timed out after {timeout}s")
            raise RuntimeError("server closed stdout unexpectedly")
        return line.decode().strip()

    init_resp, tools_resp = "", ""
    try:
        send({
            "jsonrpc": "2.0", "id": 1, "method": "initialize",
            "params": {"protocolVersion": "2024-11-05", "capabilities": {},
                       "clientInfo": {"name": "deskbox-mcp-probe", "version": "0.1"}},
        })
        init_resp = recv()

        send({"jsonrpc": "2.0", "method": "notifications/initialized", "params": {}})

        send({"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}})
        tools_resp = recv()
    except Exception as e:
        print(f"mcp-probe failed: {e}", file=sys.stderr)
        sys.exit(1)
    finally:
        timer.cancel()
        try:
            proc.terminate()
            proc.wait(timeout=3)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()
        # /deskbox/out is scratch space for the script, not a declared
        # result file — an undeclared leftover trips the desk's "no
        # undeclared writes" contract check (auditOutDir in executor.go),
        # so clean it up regardless of how the probe went. Caught by
        # actually running this against the real server and audit.
        if script_path and os.path.exists(script_path):
            os.remove(script_path)

    json.dump({"initialize_response": init_resp, "tools_list_response": tools_resp}, sys.stdout)


if __name__ == "__main__":
    main()
