#!/usr/bin/env python3
"""BrowserBox passthrough.

Forwards {"tool": "namespace.method", "input": {...}} to a running
BrowserBox relay (ws_relay.py) and returns its result. One request in, one
response out, one connection per call — this is a Deskbox tool, not a
persistent client, so it doesn't reuse BrowserBox's own client.py (that
needs the third-party `websockets` package, which the sandbox can't see —
see tcs.yaml). Stdlib only.
"""
import base64
import hashlib
import json
import os
import socket
import struct
import sys
import uuid

WS_HOST = os.environ.get("BROWSERBOX_HOST", "127.0.0.1")
WS_PORT = int(os.environ.get("BROWSERBOX_PORT", "9009"))
GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"


class BrowserBoxError(Exception):
    """The relay or extension answered with an error, or the handshake failed."""


def _handshake(sock, host, port):
    key = base64.b64encode(os.urandom(16)).decode()
    req = (
        f"GET / HTTP/1.1\r\n"
        f"Host: {host}:{port}\r\n"
        f"Upgrade: websocket\r\n"
        f"Connection: Upgrade\r\n"
        f"Sec-WebSocket-Key: {key}\r\n"
        f"Sec-WebSocket-Version: 13\r\n\r\n"
    )
    sock.sendall(req.encode())
    resp = b""
    while b"\r\n\r\n" not in resp:
        chunk = sock.recv(4096)
        if not chunk:
            raise BrowserBoxError("relay closed connection during handshake")
        resp += chunk
    header = resp.split(b"\r\n\r\n", 1)[0].decode(errors="replace")
    if " 101 " not in header.split("\r\n", 1)[0]:
        raise BrowserBoxError(f"handshake rejected: {header.splitlines()[0]}")
    expect = base64.b64encode(hashlib.sha1((key + GUID).encode()).digest()).decode()
    accept = None
    for line in header.split("\r\n")[1:]:
        if ":" in line:
            k, v = line.split(":", 1)
            if k.strip().lower() == "sec-websocket-accept":
                accept = v.strip()
    if accept != expect:
        raise BrowserBoxError("handshake accept key mismatch")


def _frame(opcode: int, payload: bytes) -> bytes:
    mask = os.urandom(4)
    masked = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
    header = bytearray([0x80 | opcode])
    length = len(payload)
    if length < 126:
        header.append(0x80 | length)
    elif length < 65536:
        header.append(0x80 | 126)
        header += struct.pack(">H", length)
    else:
        header.append(0x80 | 127)
        header += struct.pack(">Q", length)
    return bytes(header) + mask + masked


def _send_text(sock, payload: str):
    sock.sendall(_frame(0x1, payload.encode()))


def _recv_exact(sock, n):
    buf = b""
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise BrowserBoxError("relay closed connection")
        buf += chunk
    return buf


def _recv_frame(sock):
    """One WS frame as (fin, opcode, payload). Server frames are never
    masked (RFC 6455 5.1)."""
    b0, b1 = _recv_exact(sock, 2)
    fin = b0 & 0x80
    opcode = b0 & 0x0F
    length = b1 & 0x7F
    if length == 126:
        length = struct.unpack(">H", _recv_exact(sock, 2))[0]
    elif length == 127:
        length = struct.unpack(">Q", _recv_exact(sock, 8))[0]
    payload = _recv_exact(sock, length) if length else b""
    return fin, opcode, payload


def _recv_message(sock):
    """Reassembles one logical message across continuation frames,
    answering pings so the relay doesn't close us for going quiet."""
    parts = []
    while True:
        fin, opcode, payload = _recv_frame(sock)
        if opcode == 0x8:
            raise BrowserBoxError("relay closed the connection")
        if opcode == 0x9:
            sock.sendall(_frame(0xA, payload))
            continue
        if opcode in (0x0, 0x1, 0x2):
            parts.append(payload)
        if fin:
            break
    return b"".join(parts)


def call(tool: str, tool_input, timeout_s: float):
    sock = socket.create_connection((WS_HOST, WS_PORT), timeout=timeout_s)
    try:
        sock.settimeout(timeout_s)
        _handshake(sock, WS_HOST, WS_PORT)
        _send_text(sock, json.dumps({"role": "agent"}))
        call_id = str(uuid.uuid4())
        _send_text(sock, json.dumps({"id": call_id, "tool": tool, "input": tool_input}))
        while True:
            msg = json.loads(_recv_message(sock))
            if msg.get("id") != call_id:
                continue  # not our response — e.g. another agent's traffic
            if "error" in msg:
                raise BrowserBoxError(msg["error"])
            return msg.get("result")
    finally:
        try:
            sock.sendall(_frame(0x8, b""))  # close frame — best-effort, tidy relay logs
        except OSError:
            pass
        sock.close()


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    tool = payload.get("tool")
    if not tool:
        print("missing required field: tool", file=sys.stderr)
        sys.exit(1)
    tool_input = payload.get("input")
    timeout_s = float(payload.get("timeout_s", 20))

    try:
        result = call(tool, tool_input, timeout_s)
    except (OSError, socket.timeout) as e:
        print(
            f"cannot reach BrowserBox relay at {WS_HOST}:{WS_PORT}: {e} — "
            "is ws_relay.py running and the extension loaded?",
            file=sys.stderr,
        )
        sys.exit(1)
    except BrowserBoxError as e:
        print(f"browserbox error: {e}", file=sys.stderr)
        sys.exit(1)

    json.dump({"result": result}, sys.stdout)


if __name__ == "__main__":
    main()
