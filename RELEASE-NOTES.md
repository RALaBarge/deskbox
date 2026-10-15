# DeskBox v1.0.0 Release Notes

**DeskBox is a contract-enforced proxy for AI agent tools.** Each tool runs in its own bubblewrap sandbox with kernel-enforced limits on what it can do. Enforcement is not a matter of trust — the kernel says "no" before the tool can misbehave.

## v1.0.0: Hardened, shippable, ready for production

This is the first production-ready release. All enforcement mechanisms are in place and proven, and the design is stable across real workloads. Nothing in this release is "beta" or "experimental" — every feature is either stable or explicitly documented as out-of-scope.

### What's new since v0.1

**Three hardening additions:**
- **Seccomp-BPF syscall denylist.** Dangerous calls (ptrace, bpf, kernel modules, mount, reboot, keyring, clock_settime, …) are blocked at the kernel level, returning `EPERM` before they can execute. The program is assembled pure-Go (no cgo, no libseccomp) and proven to apply at startup. Reported in `GET /` so operators know whether it's live.
- **Interpreter pinning.** Run scripts are pinned with `integrity.run_sha256`. Now the *interpreter* that runs them is pinned too via `integrity.interpreter_sha256` — checked at load and per job. A `dnf upgrade python3` changes the pin and tells you "the thing that runs your tool changed, re-vet it", the exact signal the check exists for.
- **tcs-verify: offline TCS linter.** Validates a contract before it ever ships — schema subset, run-script permissions, and integrity pins — with nothing running. Reuses the same validator package the desk enforces with (`internal/tcs`), so the linter and the gate cannot drift. Drops into CI or a pre-commit hook.

**Stability improvements:**
- Per-job resource limits (memory, task count) now fail with a clear error message when the environment doesn't support them, instead of silently falling back.
- `-strict` mode refuses to start if any enforcement mechanism is missing, for deployments that need guarantees.
- `GET /` reports what guarantees are actually live, so operators never have to infer it from logs.

### Requirements (systemd + bwrap)

**DeskBox requires Linux with:**
- systemd and `systemd-run --user --scope` (for per-job resource limits)
- bubblewrap (`bwrap` on PATH, any recent version)

If your target is Linux without these, DeskBox isn't the right tool — and that's okay. DeskBox does one thing and does it well; it doesn't pretend to work everywhere.

No fallbacks, no degraded modes on other OSes. Use Docker Desktop or colima (macOS/Windows) to run DeskBox inside a Linux VM.

### What enforcement actually covers

Kernel-enforced, per job, nothing to trust:

- No host filesystem (only `/usr`, input files, output dir, tool folder)
- No network at all when `network: false` (bubblewrap `--unshare-net`)
- No view of host processes or environment (separate pid/ipc/uts namespaces)
- No dangerous syscalls (seccomp-BPF denylist — ptrace, kernel modules, mount, reboot, …)
- Per-job memory and task-count caps (via systemd cgroups)
- Integrity checks on the run script *and* its interpreter (at load and per job)

What it doesn't cover (by design, not accidentally):

- **Per-job uid isolation.** Tools run as the user the desk runs as. Set that user to a dedicated unprivileged account and that's your ceiling. Full per-uid isolation needs privileged setup that fights the static-binary portability story.
- **Network egress filtering.** `network: true` means all egress. A per-destination allowlist is the operator's responsibility — add nftables or an authenticated proxy in the job's network namespace.
- **Content-addressed `/usr`.** The interpreter and script are pinned; shared libraries and packages pulled in at runtime are not. Full content-addressed `/usr` would cut off "run with the host's existing runtimes", which is the whole portability story.

### How to use it

#### Install

```bash
tar -xzf deskbox-v1.0.0-linux-amd64.tar.gz
cd deskbox-v1.0.0-linux-amd64
./agent-desk -check          # verify this box has what your tools need
./agent-desk                 # binds 127.0.0.1:8080, default tools/ dir
```

#### Use it

Write one tool:

```bash
mkdir tools/my-tool
cat > tools/my-tool/tcs.yaml << 'EOF'
name: my-tool
summary: What it does
input:
  type: object
  required: [name]
  properties:
    name: { type: string }
output:
  schema:
    type: object
    required: [greeting]
    properties:
      greeting: { type: string }
allowed_side_effects:
  files: []
  network: false
EOF

cat > tools/my-tool/run.sh << 'EOF'
#!/bin/bash
python3 -c "import sys,json; d=json.load(sys.stdin); print(json.dumps({'greeting': f'Hi, {d[\"name\"]}'}))"
EOF

chmod 700 tools/my-tool/run.sh
```

Call it:

```bash
curl -X POST 'localhost:8080/tools/my-tool?wait=5s' \
  -H 'content-type: application/json' \
  -d '{"input":{"name":"Ryan"}}'
```

```json
{"id":"job-…","status":"done","result":{"greeting":"Hi, Ryan"}}
```

#### Real-world patterns

**Idempotent re-runs.** Pass `idempotency_key` in the request. A repeat with the same key gets the same result, even if the network dropped your first request:

```bash
curl -X POST localhost:8080/tools/my-tool \
  -d '{"input":{"name":"Ryan"},"idempotency_key":"req-42"}'
# First call: returns job-abc with status queued/running/done
# Any later call with the same key: returns the SAME job-abc, no re-run
```

**Concurrent tooling.** Multiple agents call the same tool concurrently. Each gets its own sandbox with resource caps (memory, process count). One can't starve the others.

**Audit trail.** Every job is persisted to SQLite (or Postgres). Query `GET /jobs/{id}` anytime to know exactly what ran, when, what it returned, and whether it succeeded or failed.

**Batch processing.** Fan out one job per item with `POST /batches`:

```bash
curl -X POST localhost:8080/batches -d '{
  "tool": "my-tool",
  "items": [{"name":"Alice"},{"name":"Bob"}]
}'
# 202 {"batch_id":"batch-…","job_ids":["job-…","job-…"]}
```

Query progress anytime with `GET /batches/{id}`.

### Production checklist

- [ ] Set `DESKBOX_AUTH_ENABLED=true` and generate a token for any public-facing deployment
- [ ] Use a unix socket (`-addr /run/user/1000/deskbox.sock`) or loopback-only for single-user
- [ ] Run the desk as a dedicated unprivileged account (not root, not your own shell's user)
- [ ] Use `-strict` if enforcement matters (refuse to run rather than degrade on missing mechanisms)
- [ ] Run it as a systemd user service with `enable-linger` so resource limits survive logout
- [ ] Use a durable store (sqlite by default, postgres if you need multiple desk instances)
- [ ] Pin your tool's `integrity.run_sha256` and `integrity.interpreter_sha256` once you've read it
- [ ] Run `tcs-verify tools/` in CI to catch schema and permission problems before they ship

### License

[MIT](LICENSE).
