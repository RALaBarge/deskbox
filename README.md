# DeskBox

Enforced tool contracts for AI agents.

Agents don't run tools directly. They call DeskBox, and DeskBox checks a
**Tool Contract Spec (TCS)** before running the tool (input schema) and after
(output schema, side effects). Each run happens in its own bubblewrap
sandbox, so the contract is enforced by the kernel, not by trusting the
agent to follow it.

## Layout

```
deskbox/
├── cmd/
│   ├── agent-desk/          # the enforcing proxy (Go)
│   │   ├── main.go          # HTTP API + routing
│   │   ├── tools.go         # TCS loading + tool discovery
│   │   ├── validator.go     # JSON-schema subset validator
│   │   ├── queue.go         # worker pool, retries, job history
│   │   ├── executor.go      # scratch-dir sandbox, bwrap, side-effect audit
│   │   ├── store.go         # JobStore interface (pluggable durable backend)
│   │   ├── store_sqlite.go  # default backend: one file, no server
│   │   └── store_postgres.go # optional: shared job state across desk instances
│   ├── tcs-shim/            # generic adapter for CLIs that never heard of the desk
│   └── tcs-verify/          # (stub) Rust CLI for offline spec checking
├── examples/tools/          # four sample tools; copy into tools/ to use
└── tools/                   # the space agents read, each tool owns a folder:
    └── <name>/              #   tcs.yaml (contract), run.sh (implementation),
                              #   README.md (skill notes for agents)
```

No tools ship in `tools/` — that space is yours alone, and examples that
auto-loaded into it would undercut the point. Working samples live in
[`examples/tools/`](examples/tools/) (Python, Perl, and two built on
`tcs-shim` that wrap `jq` and `grep` with no glue code); copy what you want.
They depend only on things a stock Linux install already has, so they run
on any box rather than only the one they were written on.

A folder is a tool iff it contains `tcs.yaml` + an executable `run.sh|run.py|run`.

Every run gets `DESKBOX_TOOL_DIR` in its environment, pointing at the tool's
own folder — `/deskbox/tool` inside the sandbox, the real path when
bubblewrap isn't available. Read vendored data or a shim config through it
rather than hardcoding either path.

## Tool Contract Spec (per tool, in `tools/<name>/tcs.yaml`)

```yaml
name: example-tool
summary: What the tool does, how to call it, edge cases. Agents read this.

input:                     # validated on the way in, violations => HTTP 400
  type: object
  additionalProperties: false
  required: [message]
  properties:
    message: { type: string, minLength: 1 }

output:                    # validated on the way out, violations => job fails
  schema:
    type: object
    additionalProperties: false
    required: [result]
    properties:
      result: { type: string }

allowed_side_effects:      # structural, not advisory
  files: []                # empty = no file writes allowed (out/ audit)
  network: false           # false = egress cut by bubblewrap --unshare-net

sandbox:                   # the per-job workspace surface (bwrap layer)
  in: [input.json]         # exact files the pull needs, materialized by the
                           #   desk, bound read-only one by one (the tool can't
                           #   even list siblings)
  out: [result.json]       # files the tool may write, host-tailable live
                           #   (tail -f the real path, or GET /jobs/<id>/out/<f>)

execution:
  mode: queued             # queued (async, retried) | direct (sync)
  max_retries: 2
  timeout_ms: 15000
```

Schema subset: `type, required, properties, additionalProperties, items,
minItems/maxItems, enum, minLength/maxLength, minimum/maximum, oneOf, anyOf`.
Deliberately not supported: `pattern`/`format`/`$ref`/`allOf`/`const`.
Pattern matching especially is left to the tool itself — a regex validator
in the gate is complexity (YAML-escaping quirks, ReDoS exposure) a tool's
own language already handles better downstream. `oneOf`/`anyOf` are
structural (which shape is this?) rather than content-level, so those stay
in the gate; a schema using either is the entire check for that node — it
doesn't compose with a sibling `type` keyword, e.g.:
```yaml
properties:
  amount:
    oneOf:
      - { type: integer }
      - { type: string, enum: ["unspecified"] }
```

## Tool conventions (the files in the space)

1. Input arrives as one JSON document on **stdin**.
2. Output is exactly one JSON document on **stdout**; nothing else.
3. Errors go to **stderr** with a non-zero exit.
4. Files a tool may write are declared in `allowed_side_effects.files`.
5. `network: false` means no egress. Enforced by bubblewrap, not the code.

### Can the contract be skipped by just running the script directly?

Nothing in the desk itself stops someone from running `tools/<name>/run.sh`
by hand instead of going through the API — that's not a gap a single HTTP
server can close by watching for it (a harness-specific interception layer
only catches that one harness; a plain shell walks right past it). What
*does* close it is the same thing that keeps everything else off the tool's
sandbox: OS file permissions. At load time the desk warns if a run script
is executable by group or other:

```
WARN: tools/example-tool/run.sh is executable by group/other (mode 0755) —
anyone on this box other than the script's owner can run it directly,
bypassing every contract check. If that matters for your deployment, chmod
700 it (or chown tools/ to a dedicated service account that only the desk
runs as).
```

On a single-user dev machine — the primary case this repo is built for,
where your own shell and the desk run as the same account — that warning
is just informational; the guarantee doesn't really apply when "the
operator's shell" and "the desk" are the same user by design. Where it
matters (a shared box, an operator you don't fully trust), `chmod 700` the
scripts or `chown` `tools/` to a dedicated account that only the desk
process runs as, and the warning goes away because the permission itself
now enforces it.

## API

| Endpoint | Purpose |
|---|---|
| `GET /` | Service status + queue summary |
| `GET /tools` | List tools + their contracts (what an agent may invoke) |
| `GET /tools/{name}` | The tool's contract as JSON (authored as `tcs.yaml` on disk, re-decoded for the wire — every API response is JSON, no exceptions) |
| `POST /tools/{name}` | Invoke a tool. Body: `{"input": {...}, "meta": {...}, "idempotency_key": "..."}`. Optional `?wait=<duration>` (e.g. `5s`) — see below. |
| `GET /jobs/{id}` | Poll job status / result / error |
| `GET /jobs/{id}/wait` | Long-poll: blocks until the job is terminal or `?timeout` elapses (default 25s) |
| `DELETE /jobs/{id}` | Cancel a queued or running job |
| `GET /jobs/{id}/out/{file}` | Tail a job's output file (live while running) |
| `GET /jobs` | The inbox: terminal jobs not yet acked. Optional `?limit` (default 50). |
| `GET /jobs/wait-any` | Long-poll the inbox: blocks until at least one job is terminal-and-unacked, or `?timeout` elapses (default 25s) |
| `POST /jobs/{id}/ack` | Mark a terminal job's result seen/handled — empties it from the inbox |
| `POST /batches` | Fan out one job per item. Body: `{"tool": "...", "items": [{...}, {...}], "meta": {...}}` |
| `GET /batches/{id}` | Batch progress: items done/total, broken down by status |
| `GET /queue` | Queue depth, worker count, recent jobs |

Each job gets a stable workspace at `<data>/jobs/<id>/` with `in/` and `out/`.
`out/result.json` (or whatever `sandbox.out` declares) is a real file. Tail it
live from the shell while the job runs, or via `GET /jobs/{id}/out/result.json`.

A conforming request to `POST /tools/example-tool`:

```bash
curl -X POST localhost:8080/tools/example-tool \
  -H 'Content-Type: application/json' \
  -d '{"input": {"message": "hello"}, "meta": {"agent": "operator"}}'
# 202 {"id":"job-…","status":"queued",…}
```

Poll `GET /jobs/{id}` until `status` is `done` (result present), `failed`
(error explains why: retryable after max_retries, or a permanent contract
violation, never retried), or `canceled` (an operator's `DELETE`).

### Waiting for a result instead of polling

Every submission is persisted and queued the same way regardless of the
tool's `execution.mode` — that setting only controls how long `POST`
blocks before responding, not whether the job gets retries, idempotency
dedup, or a durable record. Two ways to avoid a polling loop:

- **`?wait=<duration>` on `POST /tools/{name}`.** Blocks up to that long for
  the job to finish, returning `200` with the final job inline if it does,
  or falling back to today's `202` + `Location` if it doesn't. Capped at
  55s. Omit it (the default) for the original async-first behavior — no
  blocking, immediate `202`.
  ```bash
  curl -X POST 'localhost:8080/tools/example-tool?wait=5s' -d '{"input": {"message": "hi"}}'
  # 200 {"status":"done","result":{...}} if it finished within 5s, else 202 (poll from here)
  ```
- **`GET /jobs/{id}/wait?timeout=<duration>`** — the same long-poll, usable
  any time after submit, not just on the initial call. Also capped at 55s.

A tool declared `execution.mode: direct` always waits for its own result —
that's what "direct" means — bounded by its own timeout × (max_retries+1)
as a safety cap rather than by the caller. It goes through the exact same
queue, worker pool, and store as a queued tool now; only the wait behavior
differs. (This is a behavior change worth knowing: previously "direct" ran
inline in the HTTP handler, bypassing `-workers`, retries, and the store
entirely — a `direct` tool's own `max_retries` was silently ignored, and it
was never queryable afterward via `GET /jobs/{id}`. Both are fixed now, at
the cost of a direct call occasionally waiting on a free worker slot under
heavy concurrent load, same as a queued one would.)

### Canceling a job

```bash
curl -X DELETE localhost:8080/jobs/job-abc123
# 200 {"status":"canceled",...} — queued: skipped before it ever ran;
#                                  running: process killed via its context
# 409 {"error":"job already done, nothing to cancel", ...} — already terminal
# 404 — job doesn't exist
```
A canceled job's process is actually killed (`SIGKILL` via the run's
context being canceled), not just marked canceled while left running in
the background — confirmed with no orphaned process left behind after a
mid-run cancel.

### The inbox: finding out something finished, without a claim/TTL protocol

Earlier designs for "notify the operator when a job finishes" reached for a
shared events table with atomic claims and TTL-based delivery retries — the
right answer when *multiple independent processes* are racing to be the one
that delivers a result. The desk isn't that: it's one always-on daemon, so
there's no race to arbitrate. Multiple callers seeing the same unacked job
is harmless. The inbox is a plain query instead:

```bash
curl localhost:8080/jobs                       # every terminal job not yet acked
curl 'localhost:8080/jobs/wait-any?timeout=30s' # long-poll: blocks until one exists
curl -X POST localhost:8080/jobs/job-abc123/ack # done looking at it — clears the inbox
```

Nothing is lost if nobody polls for a while — unacked rows just sit there,
same durability guarantee the job store already gives every job. Acking an
already-acked job is a no-op; acking one that hasn't finished yet is a
`409`.

### Batches: one job per item, no leases

For a large list of items, `POST /batches` fans out one job per item under
a shared batch id — each going through the exact same queue, store, and
retry path as any other job:

```bash
curl -X POST localhost:8080/batches -d '{
  "tool": "example-tool",
  "items": [{"message": "one"}, {"message": "two"}, {"message": "three"}]
}'
# 202 {"batch_id":"batch-…","job_ids":["job-…","job-…","job-…"]}

curl localhost:8080/batches/batch-abc123
# {"batch_id":"…","total":3,"done":2,"failed":0,"running":1,"queued":0,"jobs":[...]}
```

An invalid item anywhere fails the whole batch at the gate before any job
is created — no partial fan-out with some items silently skipped. There's
no per-item lease to expire: there's one worker (the desk itself), and a
crashed/restarted desk resumes every unfinished item exactly the way it
resumes any other in-flight job (confirmed: killed a desk mid-batch with
one item running and two still queued, restarted against the same data
dir, all three resumed and completed). `GET /batches/{id}`'s items
done/total is also the batch's own progress signal, no separate heartbeat
mechanism needed.

A non-conforming request is rejected at the gate:

```bash
curl -X POST localhost:8080/tools/example-tool -d '{"input": {}}'
# 400 {"error":"contract violation: …","violations":["$: missing required property \"message\""]}
```

## Enforcement summary

| Rule | Enforced by | Never-retried? |
|---|---|---|
| Input matches `input` schema | validator at submit | N/A, rejected at HTTP 400 |
| No forbidden file writes | `out/` audit post-run | yes |
| No network egress when `network:false` | bubblewrap `--unshare-net` | structural |
| Output is one JSON doc | stdout parse or `out/` file | yes |
| Output matches `output` schema | validator | yes |
| Transient tool failure | queue retry w/ exponential backoff | retried up to `max_retries` |
| Tool hangs | per-job timeout | retried (timeout) |

## The bwrap layer (minimal by construction)

Every job runs inside its own bubblewrap sandbox. Inside, the tool sees **only**:

- `/deskbox/in/`: exactly the files declared in `sandbox.in`, each bound
  read-only individually (the tool cannot list siblings it wasn't given)
- `/deskbox/out/`: the writable output dir (host-tailable, real path)
- `/deskbox/tool/`: the tool's own folder, read-only
- `/usr /bin /sbin /lib /lib64`, a bare minimum of `/etc`, `/dev`, `/proc`, tmpfs `/tmp`

The host filesystem is not mounted (no `--ro-bind / /`). Home dirs, `~/.ssh`,
`/var`, service secrets: not visible. Environment is fully controlled
(`cmd.Env`): `OPENROUTER_API_KEY` and the rest of the host shell env never
reach the sandbox. `network: false` adds `--unshare-net`. With egress
allowed, DNS and TLS roots are bound so curl/git work.

Tested on Fedora/Bazzite and Ubuntu 24.04. Both use the usrmerge layout
(`/bin /sbin /lib /lib64` as symlinks into `/usr`), and those toplevel
symlinks must be bound alongside `/usr` or the ELF interpreter can't
resolve and `execvp` fails with ENOENT. See `known.md` for a separate
Ubuntu-only AppArmor gotcha (system config, not a DeskBox bug).

## Build & run

```bash
# requires Go 1.22+
go build -o bin/agent-desk ./cmd/agent-desk
./bin/agent-desk -addr :8080                   # -workers defaults to 10, -tools to ./tools

# optional: the adapter for wrapping pre-existing CLIs. Install it somewhere
# the sandbox can see — /usr is bound read-only, a home dir is not.
go build -o /usr/local/bin/tcs-shim ./cmd/tcs-shim
```

Write a `tcs.yaml` + `run.sh` under `tools/<name>/` following the conventions
above and the desk will pick it up — or start from one of the samples:

```bash
cp -r examples/tools/greet-python tools/
```

## Settings (.env, optional)

The desk reads a `.env` file (`KEY=VALUE` per line, `#` comments allowed) in
the working directory on startup and merges it into the process
environment, without overwriting any variable already set for real (a real
env var, e.g. from systemd, always wins over the file). Missing file is
fine, nothing changes. This is where auth and any future runtime toggle
live, instead of a separate flag for each one.

```bash
# .env
DESKBOX_AUTH_ENABLED=true
DESKBOX_AUTH_TOKEN=some-long-random-string
```

| Variable | Purpose |
|---|---|
| `DESKBOX_AUTH_ENABLED` | `true` to require a bearer token on every request. Default off. |
| `DESKBOX_AUTH_TOKEN` | The token clients must send as `Authorization: Bearer <token>`. Required if auth is enabled. |
| `DESKBOX_STORE` | Same as `-store` below (`sqlite` \| `postgres` \| `memory`); the flag wins if both are set. Default `sqlite`. |
| `DESKBOX_SQLITE_PATH` | Same as `-sqlite-path` below; the flag wins if both are set. |
| `DESKBOX_POSTGRES_DSN` | Same as `-postgres-dsn` below; the flag wins if both are set. Only read when `-store=postgres`. |
| `DESKBOX_JOB_MEMORY_MAX` | Per-job memory cap (systemd `MemoryMax` syntax, e.g. `512M`). Default `512M`. |
| `DESKBOX_JOB_TASKS_MAX` | Per-job cap on forked processes/threads (systemd `TasksMax`), stops fork bombs. Default `64`. |

`.env` is gitignored. Never commit a real token, generate one per
deployment (`openssl rand -hex 32` works fine).

## Config (`deskbox.yaml`, optional)

`.env` is for secrets and is gitignored. `deskbox.yaml` is the opposite:
structural, non-secret desk settings meant to be hand-edited and checked
into version control — the same role `tcs.yaml` plays for a tool's
contract, one level up. The desk reads it once at startup from the working
directory; a missing file changes nothing, every setting still has a
working default.

```yaml
# deskbox.yaml
store:
  kind: sqlite   # sqlite (default) | postgres | memory
  sqlite:
    path: ""     # "" = <data-dir>/deskbox.db
```

Precedence, highest wins, each layer only overrides the next if it actually
set something: **`-store`/`-sqlite-path` flag** > **`DESKBOX_STORE`/
`DESKBOX_SQLITE_PATH` env** > **`deskbox.yaml`** > hardcoded default
(`sqlite`). A Postgres DSN is never read from `deskbox.yaml` — it typically
carries a password, so it stays in `.env`/`-postgres-dsn` only, kept out of
the file you commit.

### Per-job resource limits

`bwrap`'s namespaces isolate what a tool can *see*; they don't cap what it
can *consume*. Without a cap, one runaway or malicious tool (a leak, a fork
bomb, an infinite loop) can degrade the host for every other job running at
the same time — a real risk once real tool traffic runs 10-wide. The desk
closes this by running each job in its own `systemd --user` scope
(`systemd-run --user --scope`) with `MemoryMax`/`TasksMax` set.

This needs a working user D-Bus session, which a plain interactive shell has
but a bare background/service process may not, depending on the distro and
how it's launched. If jobs fail with `Failed to connect to bus`, run:

```bash
loginctl enable-linger $(whoami)
```

so the session persists independent of any active login, then restart the
desk. If `systemd-run` isn't usable at all, the desk detects that (once at
startup, and again if it stops working mid-run) and disables the wrapper
instead of failing every job — logged clearly either way, same as the
`bwrap`-missing and store-unset cases.

## Idempotency and durable jobs (`JobStore`, pluggable)

Job persistence sits behind a `JobStore` interface (`cmd/agent-desk/store.go`):
`Insert`, `Update`, `Get`, `LoadIncomplete`, `ListTerminalUnacked`,
`ListByBatch`, `Close`. Two implementations ship in this repo; `-store`
picks one, and nothing else in the desk knows or cares which:

| `-store` | Backend | When to use it |
|---|---|---|
| `sqlite` (default) | one file, `<data>/deskbox.db` unless `-sqlite-path` overrides it | the normal case: one desk, one box, no server to run |
| `postgres` | needs `-postgres-dsn` / `DESKBOX_POSTGRES_DSN` | more than one desk instance sharing job state over the network |
| `memory` | none — job history and idempotency don't survive a restart | quick/throwaway runs |

```bash
./bin/agent-desk -addr :8080                              # sqlite at ./data/deskbox.db
./bin/agent-desk -addr :8080 -sqlite-path /var/lib/deskbox/jobs.db
./bin/agent-desk -addr :8080 -store postgres -postgres-dsn "postgres://user:pass@host:5432/deskbox?sslmode=disable"
./bin/agent-desk -addr :8080 -store memory
```

Both backends create their own `jobs` table/file on startup (`CREATE TABLE IF
NOT EXISTS`, no migration tool needed). With `sqlite` or `postgres`:

- **Dedup.** Pass `idempotency_key` in the submit body. A repeated submit for
  the same `(tool, idempotency_key)` returns the existing job, whatever its
  status, instead of running it again:
  ```bash
  curl -X POST localhost:8080/tools/example-tool \
    -d '{"input": {"message": "hello"}, "idempotency_key": "operator-run-42"}'
  # first call: 202, status queued/running/done
  # any later call with the same key: 202, the SAME job id, tool not re-run
  ```
  No key means no dedup, same as today: a fresh job every time.
- **Resume.** On startup the desk reloads every job left `queued` or
  `running` by a prior process (crash, redeploy, `kill -9`) and re-enqueues
  it. In-flight work isn't dropped on restart.

With `-store memory` (or no store configured), the desk behaves as before:
in-memory only, `idempotency_key` accepted but ignored.

Want a different backend — Redis, MySQL, a flat file, whatever you actually
run? Implement `JobStore` and pass your type to `NewQueue` instead; the
queue, the HTTP handlers, and everything else in the desk only ever call
through the interface.

## Status / roadmap

- [x] TCS loading, input gate, output schema, out/ file-write audit
- [x] Queued execution, worker pool, retries with backoff, job history
- [x] Minimal per-job bwrap layer: only declared in/ + out/ visible, no host
      mounts, controlled env, live-tailable output files + HTTP tail endpoint
- [x] Durable job log + idempotency behind a pluggable `JobStore`: SQLite by
      default, Postgres opt-in, `idempotency_key` dedup, resume of
      `queued`/`running` jobs after a crash/restart
- [x] Auth token for the desk (optional, `.env`-driven, see Settings above)
- [x] Per-job memory/task-count cgroup limits (`systemd-run --user --scope`,
      fails open with a clear warning if the environment can't support it)
- [x] Unified invoke: `direct` and `queued` both go through the same queue,
      store, and retries; `?wait=`/`GET /jobs/{id}/wait` long-poll instead
      of a tight polling loop; `DELETE /jobs/{id}` cancels a queued or
      running job (process actually killed, not just marked)
- [x] Inbox model (`GET /jobs`, `GET /jobs/wait-any`, `POST /jobs/{id}/ack`):
      finding out something finished without a claim/TTL delivery protocol —
      one daemon means no race to arbitrate, so it's a plain query
- [x] Batches (`POST /batches`, `GET /batches/{id}`): one job per item, no
      per-item leases — a crashed/restarted desk resumes every unfinished
      item the same way it resumes any other job (verified: killed mid-batch,
      restarted, all items completed)
- [x] Bypass-resistance via OS permissions, not a harness-specific hook: the
      desk warns at load time if a run script is executable by group/other
- [x] `tcs-shim`: adapt a pre-existing CLI (argv flags in, text out) to the
      contract with a declarative `shim.yaml` and no per-tool glue code —
      argv-array exec so values can't inject a shell, and the wrapped
      binary still gets the full sandbox/audit/schema enforcement
- [x] Example tools in `examples/tools/` proving the language-agnostic
      claim concretely: Python, Perl, and two shimmed CLIs
- [ ] `tcs-verify` Rust CLI (offline spec linting), stub only
- [ ] `pi` plugin: operator agent that talks to the desk (the original idea)

## License

[PolyForm Noncommercial 1.0.0](LICENSE.md). Free for any noncommercial
purpose. Commercial use needs a separate license from the author.
