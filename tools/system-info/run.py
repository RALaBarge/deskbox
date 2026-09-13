#!/usr/bin/env python3
"""Host stats via an allowlisted command set. Stdlib only.
Ported from ormcp's system_info.py (dropped the optional httpx/Ollama probe).
"""
import json
import platform
import subprocess
import sys

_IS_MACOS = platform.system() == "Darwin"

_ALLOWED = {
    "cat", "ls", "grep", "find", "ps", "df", "uptime", "wc",
    "head", "tail", "cut", "awk", "sed", "sort", "uniq", "stat",
    "du", "nproc", "free", "top", "lsof", "netstat", "ifconfig",
    "sysctl", "vm_stat", "sw_vers", "uname", "id", "whoami",
    "nvidia-smi", "system_profiler",
}
_SHELL_METACHARS = ("|", ";", "&", "`", "$", ">", "<", "\n", "\r", "\\")


def _is_allowed(cmd):
    cmd = cmd.strip()
    if not cmd:
        return False, "empty"
    for ch in _SHELL_METACHARS:
        if ch in cmd:
            return False, f"metachar: {ch!r}"
    base = cmd.split()[0]
    if base not in _ALLOWED:
        return False, f"not in allowlist: {base!r}"
    return True, "ok"


def _run(cmd):
    ok, reason = _is_allowed(cmd)
    if not ok:
        return ""
    return _run_trusted(cmd)


def _run_trusted(cmd):
    # No metachar check: only called with commands hardcoded below in
    # _snapshot(), never with the untrusted `command` field from input.
    # (ormcp's original system_info.py routed these through the same
    # allowlisted _run() as user input, which silently ate every one of
    # these — they all pipe into cut/grep/tail — so _snapshot() always
    # returned close to nothing. Caught by actually running this end to
    # end against the real sandbox rather than trusting it compiled.)
    try:
        r = subprocess.run(["bash", "-c", cmd], capture_output=True, text=True, timeout=5)
        return r.stdout.strip()
    except Exception:
        return ""


def _snapshot():
    sections = []

    if _IS_MACOS:
        cpu_model = _run_trusted("sysctl -n machdep.cpu.brand_string")
        cpu_cores = _run_trusted("sysctl -n hw.logicalcpu")
        load = _run_trusted("uptime")
        if cpu_model:
            load_short = load.split("load averages:")[-1].strip() if "load averages:" in load else load
            sections.append(f"CPU: {cpu_model} ({cpu_cores} cores) — load {load_short}")
        mem_bytes = _run_trusted("sysctl -n hw.memsize")
        if mem_bytes:
            sections.append(f"RAM: {int(mem_bytes) / 1024**3:.1f} GB total")
    else:
        cpu_model = _run_trusted("grep -m1 'model name' /proc/cpuinfo | cut -d: -f2")
        cpu_cores = _run_trusted("nproc")
        load = _run_trusted("cat /proc/loadavg | cut -d' ' -f1-3")
        if cpu_model:
            sections.append(f"CPU: {cpu_model.strip()} ({cpu_cores} cores, load: {load})")
        mem = _run_trusted("free -h | grep Mem")
        if mem:
            parts = mem.split()
            if len(parts) >= 4:
                avail = parts[6] if len(parts) > 6 else "?"
                sections.append(f"RAM: {parts[2]} used / {parts[1]} total (available: {avail})")

    disk = _run_trusted("df -h / | tail -1")
    if disk:
        parts = disk.split()
        if len(parts) >= 5:
            sections.append(f"Disk (/): {parts[2]} used / {parts[1]} total ({parts[4]} full)")

    if not _IS_MACOS:
        gpu = _run_trusted("nvidia-smi --query-gpu=name,memory.used,memory.total,utilization.gpu --format=csv,noheader,nounits")
        if gpu:
            for line in gpu.strip().split("\n"):
                parts = [p.strip() for p in line.split(",")]
                if len(parts) >= 4:
                    sections.append(f"GPU: {parts[0]} — {parts[1]}MB/{parts[2]}MB ({parts[3]}% util)")

    uptime = _run_trusted("uptime")
    if uptime and not _IS_MACOS:
        sections.append(f"Uptime: {uptime}")

    return "\n".join(sections) if sections else "Could not gather system information."


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    command = (payload.get("command") or "").strip()
    if command:
        ok, reason = _is_allowed(command)
        if not ok:
            print(f"command not allowed: {reason}", file=sys.stderr)
            sys.exit(1)
        output = _run(command) or "[no output]"
    else:
        output = _snapshot()

    json.dump({"output": output}, sys.stdout)


if __name__ == "__main__":
    main()
