"""
Security scanner — detect secrets, vulnerable dependencies, and dangerous
code patterns (inspired by the Devin !vulnscan playbook).

Works on any project directory. Language-agnostic pattern detection plus
auto-detected dependency audit. No external API keys needed.

Actions:
  secrets       — scan for hardcoded API keys, tokens, credentials
  dependencies  — auto-run pnpm/npm/pip/poetry audit, flag criticals
  patterns      — dangerous code patterns (eval, exec, SQL concat, command
                  injection, hardcoded passwords, debug endpoints, etc.)
  workflows     — audit GitHub Actions workflows for unsafe triggers,
                  unpinned actions, injection risks
  full          — run all scanners and produce a consolidated report
"""

from __future__ import annotations

import json
import logging
import os
import re
import subprocess
import sys
from pathlib import Path
from typing import Any

log = logging.getLogger(__name__)


# ── secret patterns ───────────────────────────────────────────────────────────

_SECRET_PATTERNS: list[tuple[str, str, str]] = [
    # (regex, label, severity)
    (r'(?:sk|api|token|secret|password|passwd)_?(?:[=:]\s*)["\']([A-Za-z0-9+/=_\-]{20,})["\']',
     "Key/token literal assignment", "high"),
    (r'(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9_]{36,}', "GitHub personal access token", "critical"),
    (r'AKIA[0-9A-Z]{16}', "AWS Access Key ID", "critical"),
    (r'sk-(?:or|ant|proj)-[A-Za-z0-9+/=]{32,}', "OpenAI / OpenRouter / Anthropic API key", "critical"),
    (r'xox[bpras]-[0-9A-Za-z\-]{10,}', "Slack token", "critical"),
    (r'sk_live_[0-9a-zA-Z]{24,}', "Stripe live secret key", "critical"),
    (r'eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}', "JWT token", "medium"),
    (r'-----BEGIN (?:RSA|EC|DSA|OPENSSH) PRIVATE KEY-----', "Private key", "critical"),
    (r'(?:DATABASE_URL|MONGODB_URI|REDIS_URL)\s*=\s*["\']?(?:mongodb|postgres|redis)://[^"\'\s]+',
     "Database connection string", "high"),
    (r'(?:heroku|vercel|netlify)_?(?:api|auth)_?(?:key|token)\s*[=:]\s*["\']?\S+["\']?',
     "Platform deployment token", "high"),
    (r'AIza[0-9A-Za-z\-_]{35}', "Google API key", "high"),
]


def _is_test_or_docs(path: str) -> bool:
    """Skip test fixtures, docs, lockfiles, vendor dirs."""
    skip_dirs = {"node_modules", ".git", "__pycache__", "vendor", ".venv", "venv",
                 ".tox", ".eggs", "dist", "build", ".next", ".turbo", "target"}
    parts = Path(path).parts
    return any(p in skip_dirs for p in parts)


def _is_likely_false_positive(line: str) -> bool:
    """Heuristic: skip lines that look like docs/examples/test fixtures."""
    lower = line.lower()
    indicators = [
        "example", "placeholder", "your-key", "your_token", "test_",
        "changeme", "todo", "fake", "mock", "dummy", "replace-me",
        "xxxxxxxx", "****", "<key>", "{key}", "${key", "process.env",
        "os.environ", "from env", "env variable",
    ]
    return any(ind in lower for ind in indicators)


def _scan_secrets(root: str) -> str:
    findings: list[dict] = []
    scanned = 0
    for dirpath, _, filenames in os.walk(root):
        for fn in filenames:
            path = os.path.join(dirpath, fn)
            if _is_test_or_docs(path):
                continue
            # Skip binary / large
            if fn.endswith(('.pyc', '.o', '.so', '.dll', '.exe', '.bin',
                            '.png', '.jpg', '.jpeg', '.gif', '.ico', '.woff',
                            '.ttf', '.eot', '.map', '.lock', '.lockb', '.db',
                            '.sqlite', '.sqlite3', '.log')):
                continue
            # Skip large files
            try:
                if os.path.getsize(path) > 1_048_576:  # 1 MB
                    continue
            except OSError:
                continue
            try:
                with open(path, errors='ignore') as f:
                    lines = f.readlines()
            except (OSError, PermissionError):
                continue
            scanned += 1
            for i, line in enumerate(lines, 1):
                if _is_likely_false_positive(line):
                    continue
                for pattern, label, severity in _SECRET_PATTERNS:
                    if re.search(pattern, line, re.IGNORECASE):
                        findings.append({
                            "file": os.path.relpath(path, root),
                            "line": i,
                            "label": label,
                            "severity": severity,
                            "snippet": line.strip()[:120],
                        })
                        break  # one finding per line

    if not findings:
        return f"Secret scan: {scanned} files scanned — no secrets found."

    lines = [
        f"Secret scan: {scanned} files scanned — {len(findings)} potential secrets found:\n"
    ]
    by_sev = {"critical": [], "high": [], "medium": []}
    for f in findings:
        by_sev.get(f["severity"], []).append(f)

    for sev in ("critical", "high", "medium"):
        items = by_sev[sev]
        if not items:
            continue
        lines.append(f"  [{sev.upper()}] ({len(items)} findings)")
        for item in items[:20]:
            lines.append(
                f"    {item['file']}:{item['line']}  {item['label']}"
            )
        if len(items) > 20:
            lines.append(f"    ... and {len(items) - 20} more")
    return "\n".join(lines)


# ── dependency audit ──────────────────────────────────────────────────────────

def _detect_package_manager(root: str) -> tuple[str | None, str | None]:
    """Return (manager, lockfile) or (None, None)."""
    checks = [
        ("pnpm", "pnpm-lock.yaml"),
        ("npm", "package-lock.json"),
        ("yarn", "yarn.lock"),
        ("pip", "requirements.txt"),
        ("poetry", "poetry.lock"),
        ("cargo", "Cargo.lock"),
        ("bundler", "Gemfile.lock"),
    ]
    for mgr, lockfile in checks:
        if os.path.isfile(os.path.join(root, lockfile)):
            return mgr, lockfile
    return None, None


def _run_audit(root: str, mgr: str) -> str:
    """Run the package manager's audit command."""
    commands: dict[str, list[str]] = {
        "pnpm": ["pnpm", "audit", "--audit-level=high", "--json"],
        "npm": ["npm", "audit", "--audit-level=high", "--json"],
        "yarn": ["yarn", "npm", "audit", "--json"],
        "pip": [sys.executable, "-m", "pip_audit", "--format=json"],
        "poetry": ["poetry", "run", "pip-audit", "--format=json"],
    }
    cmd = commands.get(mgr)
    if not cmd:
        return f"Dependency audit: {mgr} not supported for auditing."

    try:
        result = subprocess.run(
            cmd, cwd=root, capture_output=True, text=True, timeout=60
        )
    except FileNotFoundError:
        return f"Dependency audit: {mgr} not installed."
    except subprocess.TimeoutExpired:
        return f"Dependency audit: timed out after 60s."

    if result.returncode == 0 and not result.stdout.strip():
        return f"Dependency audit ({mgr}): no vulnerabilities found."
    if result.returncode == 0:
        try:
            parsed = json.loads(result.stdout)
        except json.JSONDecodeError:
            return f"Dependency audit ({mgr}): unable to parse output:\n{result.stdout[:500]}"
        return _format_audit_json(mgr, parsed)
    return f"Dependency audit ({mgr}): audit failed (exit {result.returncode}):\n{result.stderr[:500]}"


def _format_audit_json(mgr: str, data: Any) -> str:
    """Format npm/pnpm audit JSON into a readable summary."""
    lines = [f"Dependency audit ({mgr}):"]

    if isinstance(data, dict):
        advisories = data.get("advisories", {})
        if not advisories:
            lines.append("  No vulnerabilities found.")
            return "\n".join(lines)

        by_severity: dict[str, list] = {}
        for adv_id, adv in advisories.items():
            sev = adv.get("severity", "unknown")
            by_severity.setdefault(sev, []).append(adv)

        total = sum(len(v) for v in by_severity.values())
        lines.append(f"  {total} vulnerabilities across {len(advisories)} advisories:")

        for sev in ("critical", "high", "moderate", "low"):
            items = by_severity.get(sev, [])
            if not items:
                continue
            lines.append(f"    [{sev.upper()}] {len(items)}:")
            for adv in items[:10]:
                title = adv.get("title", "?")
                module = adv.get("module_name", "?")
                vuln = adv.get("vulnerable_versions", "?")
                patched = adv.get("patched_versions", "?")
                lines.append(f"      {module} {vuln} → patched in {patched}")
                lines.append(f"        {title}")
            if len(items) > 10:
                lines.append(f"      ... and {len(items) - 10} more")
    elif isinstance(data, list):
        # pip-audit format
        if not data:
            lines.append("  No vulnerabilities found.")
            return "\n".join(lines)
        lines.append(f"  {len(data)} vulnerabilities:")
        for vuln in data[:20]:
            name = vuln.get("name", "?")
            version = vuln.get("version", "?")
            cve = vuln.get("id", "?")
            lines.append(f"    {name} {version} — {cve}")
        if len(data) > 20:
            lines.append(f"    ... and {len(data) - 20} more")

    return "\n".join(lines)


def _scan_dependencies(root: str) -> str:
    mgr, lockfile = _detect_package_manager(root)
    if not mgr:
        return "Dependency audit: no recognized package manager found (checked: pnpm, npm, yarn, pip, poetry, cargo, bundler)."
    return _run_audit(root, mgr)


# ── dangerous code patterns ───────────────────────────────────────────────────

_PATTERN_CHECKS: list[tuple[str, str, str, str]] = [
    # (regex, label, severity, explanation)
    (r'\beval\s*\(', "eval() call", "high", "Dynamic code execution — potential RCE"),
    (r'\bexec\s*\(', "exec() call", "high", "Dynamic code execution — potential RCE"),
    (r'\bos\.system\s*\(', "os.system() call", "high", "Shell command execution"),
    (r'\bsubprocess\.(?:call|run|Popen|check_output)\s*\(\s*["\']',
     "subprocess with string command", "high", "Prefer list args to avoid shell injection"),
    (r'\bdangerouslySetInnerHTML\b',
     "react dangerouslySetInnerHTML", "high", "XSS vector in React"),
    (r'(?:\braw\s*sql|\bexecute\s*\(\s*f["\'])',
     "raw SQL execution", "high", "Possible SQL injection — use parameterized queries"),
    (r'(?:password|passwd|secret)\s*=\s*["\'][^"\']{4,}["\']',
     "hardcoded password/secret", "critical", "Hardcoded credential in source"),
    (r'console\.log\s*\(', "console.log() in production code", "low",
     "Debug logging — may leak data in production"),
    (r'debug\s*=\s*True', "debug=True in production config", "medium",
     "Debug mode enabled — exposes stack traces"),
    (r'ALLOWED_HOSTS\s*=\s*\[["\']\*["\']\]', "ALLOWED_HOSTS = ['*']", "medium",
     "Wildcard host — Django host header attack risk"),
    (r'\b(?:http|https)://(?:\w+:\w+@)', "URL with embedded credentials", "high",
     "Credentials in URL — may be logged or leaked"),
    (r'\.innerHTML\s*=', "innerHTML assignment", "medium", "XSS vector — prefer textContent"),
    (r'(?:sudo|chmod\s+777|chown)', "privilege escalation / permissive permissions",
     "medium", "Unsafe file permissions or privilege escalation"),
    (r'\bassert\s+.*\s*\(\s*\)', "bare assert in production", "low",
     "asserts are stripped with -O flag — don't use for validation"),
    (r'\b(?:md5|sha1)\s*\(', "weak hash (MD5/SHA1)", "low",
     "Cryptographically weak hash — use SHA-256 or better"),
]


def _scan_patterns(root: str) -> str:
    findings: list[dict] = []
    scanned = 0
    ext_allow = {'.py', '.js', '.ts', '.tsx', '.jsx', '.rb', '.go', '.rs',
                 '.java', '.kt', '.php', '.swift', '.c', '.cpp', '.h', '.hpp',
                 '.sh', '.bash', '.yaml', '.yml', '.toml', '.json', '.tf',
                 '.dockerfile', '.dockerignore', 'dockerfile'}
    ext_map = {'.dockerfile', '.dockerignore'}
    for dirpath, _, filenames in os.walk(root):
        for fn in filenames:
            if _is_test_or_docs(os.path.join(dirpath, fn)):
                continue
            ext = Path(fn).suffix.lower()
            if fn.lower() == 'dockerfile':
                ext = '.dockerfile'
            if ext not in ext_allow and fn.lower() not in ext_map:
                continue
            path = os.path.join(dirpath, fn)
            try:
                with open(path, errors='ignore') as f:
                    content = f.read()
            except (OSError, PermissionError):
                continue
            scanned += 1
            for pattern, label, severity, explanation in _PATTERN_CHECKS:
                for m in re.finditer(pattern, content, re.IGNORECASE):
                    line_no = content[:m.start()].count('\n') + 1
                    findings.append({
                        "file": os.path.relpath(path, root),
                        "line": line_no,
                        "label": label,
                        "severity": severity,
                        "explanation": explanation,
                        "snippet": m.group()[:80],
                    })

    if not findings:
        return f"Pattern scan: {scanned} source files scanned — no dangerous patterns found."

    lines = [f"Pattern scan: {scanned} source files scanned — {len(findings)} findings:\n"]
    by_sev = {"critical": [], "high": [], "medium": [], "low": []}
    for f in findings:
        by_sev.get(f["severity"], []).append(f)

    for sev in ("critical", "high", "medium", "low"):
        items = by_sev[sev]
        if not items:
            continue
        lines.append(f"  [{sev.upper()}] ({len(items)} findings)")
        for item in items[:15]:
            lines.append(
                f"    {item['file']}:{item['line']}  {item['label']}"
            )
            lines.append(f"      {item['explanation']}")
        if len(items) > 15:
            lines.append(f"    ... and {len(items) - 15} more")
    return "\n".join(lines)


# ── CI / GitHub Actions workflow audit ────────────────────────────────────────

_WORKFLOW_DANGERS: list[tuple[str, str, str]] = [
    (r'pull_request_target', "pull_request_target trigger",
     "PRs from forks have read access to secrets — dangerous without sandboxing"),
    (r'\$\{\{\s*(?:github\.event\.(?:issue|pull_request|comment)\.(?:title|body)|github\.event\.comment\.body)\s*\}\}',
     "untrusted event input in run step", "Injection via issue/PR/comment body"),
    (r'uses:\s*actions/(?:checkout|setup-node|setup-python)@v\d',
     "unpinned action version (tag, not SHA)",
     "Prefer commit SHA for supply-chain security"),
    (r'permissions:\s*write-all', "write-all permissions",
     "Overly broad — use least-privilege permissions"),
    (r'continue-on-error:\s*true', "continue-on-error: true",
     "May mask CI failures — review necessity"),
    (r'(?:npm|pnpm|yarn)\s+install(?!.*--frozen-lockfile)',
     "install without --frozen-lockfile in CI",
     "CI should use frozen lockfile for reproducibility"),
]


def _scan_workflows(root: str) -> str:
    workflows_dir = os.path.join(root, ".github", "workflows")
    if not os.path.isdir(workflows_dir):
        return "Workflow audit: no .github/workflows directory found."

    findings: list[dict] = []
    for fn in os.listdir(workflows_dir):
        if not fn.endswith(('.yml', '.yaml')):
            continue
        path = os.path.join(workflows_dir, fn)
        try:
            with open(path, errors='ignore') as f:
                content = f.read()
        except (OSError, PermissionError):
            continue
        for pattern, label, explanation in _WORKFLOW_DANGERS:
            for m in re.finditer(pattern, content, re.IGNORECASE):
                findings.append({
                    "file": os.path.relpath(path, root),
                    "line": content[:m.start()].count('\n') + 1,
                    "label": label,
                    "explanation": explanation,
                })

    if not findings:
        return f"Workflow audit: {len(os.listdir(workflows_dir))} workflows scanned — no issues found."

    lines = [f"Workflow audit: {len(os.listdir(workflows_dir))} workflows scanned — {len(findings)} findings:\n"]
    for f in findings[:30]:
        lines.append(f"  {f['file']}:{f['line']}  {f['label']}")
        lines.append(f"    {f['explanation']}")
    if len(findings) > 30:
        lines.append(f"  ... and {len(findings) - 30} more")
    return "\n".join(lines)


# ── configuration file review ─────────────────────────────────────────────────

_CONFIG_CHECKS: dict[str, list[tuple[str, str, str]]] = {
    "Dockerfile": [
        (r'FROM\s+\S+\s+AS\s+development', "Development stage in production Dockerfile",
         "Ensure production image doesn't include dev tooling"),
        (r'COPY\s+\.\s+\.\s*$', "COPY . . (entire context)",
         "Copies entire build context — may include secrets"),
    ],
    ".env": [
        (r'^(?!\s*#)[A-Z_]+=(?!\s*$)', "env file with values",
         ".env files should be in .gitignore — verify"),
    ],
    "docker-compose": [
        (r'ports:\s*\n\s*-\s*["\']?\d+:\d+["\']?', "Exposed ports",
         "Review if all exposed ports are necessary"),
    ],
}


def _scan_configs(root: str) -> str:
    findings: list[dict] = []
    config_files = {"Dockerfile": [], "docker-compose.yml": [],
                    "docker-compose.yaml": [], ".env": []}
    for dirpath, _, filenames in os.walk(root):
        for fn in filenames:
            if fn in config_files:
                config_files[fn].append(os.path.join(dirpath, fn))

    for fn, paths in config_files.items():
        base = fn.replace('.yml', '').replace('.yaml', '')
        checks = _CONFIG_CHECKS.get(fn, []) + _CONFIG_CHECKS.get(base, [])
        if not checks:
            continue
        for path in paths:
            try:
                with open(path, errors='ignore') as f:
                    content = f.read()
            except (OSError, PermissionError):
                continue
            for pattern, label, explanation in checks:
                for m in re.finditer(pattern, content, re.MULTILINE):
                    findings.append({
                        "file": os.path.relpath(path, root),
                        "line": content[:m.start()].count('\n') + 1,
                        "label": label,
                        "explanation": explanation,
                    })

    if not findings:
        return "Config review: no issues found in Dockerfiles, compose files, or env files."

    lines = [f"Config review: {len(findings)} findings:\n"]
    for f in findings:
        lines.append(f"  {f['file']}:{f['line']}  {f['label']}")
        lines.append(f"    {f['explanation']}")
    return "\n".join(lines)


# ── dispatch ──────────────────────────────────────────────────────────────────

def security_scan(input: str) -> str:
    """Run security scanners on a project directory.

    JSON actions:
      {"action": "secrets", "path": "/path/to/project"}
        Scan for hardcoded API keys, tokens, credentials

      {"action": "dependencies", "path": "/path/to/project"}
        Auto-detect package manager and run audit (pnpm/npm/pip/poetry)

      {"action": "patterns", "path": "/path/to/project"}
        Dangerous code patterns: eval, exec, SQL injection, XSS, etc.

      {"action": "workflows", "path": "/path/to/project"}
        Audit GitHub Actions workflows for unsafe triggers and practices

      {"action": "configs", "path": "/path/to/project"}
        Review Dockerfiles, compose files, and env files

      {"action": "full", "path": "/path/to/project"}
        Run all scanners and produce a consolidated report
    """
    try:
        params = json.loads(input) if isinstance(input, str) else input
    except json.JSONDecodeError as e:
        return f"security_scan: invalid JSON ({e})"

    action = (params.get("action") or "").strip().lower()
    root = params.get("path", ".")

    if not os.path.isdir(root):
        return f"security_scan: path not found or not a directory: {root}"

    try:
        if action == "secrets":
            return _scan_secrets(root)
        elif action == "dependencies":
            return _scan_dependencies(root)
        elif action == "patterns":
            return _scan_patterns(root)
        elif action == "workflows":
            return _scan_workflows(root)
        elif action == "configs":
            return _scan_configs(root)
        elif action == "full":
            sections = [
                _scan_secrets(root),
                _scan_dependencies(root),
                _scan_patterns(root),
                _scan_workflows(root),
                _scan_configs(root),
            ]
            summary = "SECURITY SCAN — FULL REPORT\n" + "=" * 50 + "\n"
            return summary + "\n\n".join(sections)
        return (
            "Unknown action: {}. Try: secrets, dependencies, patterns, "
            "workflows, configs, full".format(action)
        )
    except Exception as e:
        log.exception("security_scan %s failed", action)
        return f"security_scan {action} failed: {type(e).__name__}: {e}"
