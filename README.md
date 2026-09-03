# DeskBox — enforced tool contracts for AI agents

DeskBox is the desk every tool invocation must pass through. Agents do not run
tools themselves; they ask the desk, and the desk enforces a **Tool Contract
Spec (TCS)** on the way in (input) and on the way out (side effects, output).

The philosophy: *conventions as infrastructure.* You don't trust agents to
behave — you make it structurally impossible for them not to, exactly like the
agent-desk plugin idea. If the only way to do X is through the desk, then
conforming to Y is not optional.

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
│   │   └── store.go         # optional Postgres: durable jobs, idempotency_key
│   └── tcs-verify/          # (stub) Rust CLI for offline spec checking
└── tools/                   # the space agents read — each tool owns its
    ├── example-tool/        #   tcs.yaml (contract)
    │   ├── tcs.yaml         #   run.sh  (implementation)
    │   ├── run.sh           #   README.md (skill notes for agents)
    │   └── README.md
    └── net-probe/           # proves egress cutting works
```

A folder is a tool iff it contains `tcs.yaml` + an executable `run.sh|run.py|run`.

## Tool Contract Spec (per tool, in `tools/<name>/tcs.yaml`)

```yaml
name: example-tool
summary: What the tool does, how to call it, edge cases. Agents read this.

input:                     # validated on the way in — violations => HTTP 400
  type: object
  additionalProperties: false
  required: [message]
  properties:
    message: { type: string, minLength: 1 }

output:                    # validated on the way out — violations => job fails
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
  in: [input.json]         # exact files the pull needs — materialized by the
                           #   desk, bound read-only one by one (the tool can't
                           #   even list siblings)
  out: [result.json]       # files the tool may write — host-tailable live
                           #   (tail -f the real path, or GET /jobs/<id>/out/<f>)

execution:
  mode: queued             # queued (async, retried) | direct (sync)
  max_retries: 2
  timeout_ms: 15000
```

Schema subset: `type, required, properties, additionalProperties, items,
minItems/maxItems, enum, minLength/maxLength, minimum/maximum`.

## Tool conventions (the files in the space)

1. Input arrives as one JSON document on **stdin**.
2. Output is exactly one JSON document on **stdout**; nothing else.
3. Errors go to **stderr** with a non-zero exit.
4. Files a tool may write are declared in `allowed_side_effects.files`.
5. `network: false` means no egress — enforced by bubblewrap, not by the code.

## API

| Endpoint | Purpose |
|---|---|
| `GET /` | Service status + queue summary |
| `GET /tools` | List tools + their contracts (what an agent may invoke) |
| `GET /tools/{name}` | Raw `tcs.yaml` — the contract to conform to |
| `POST /tools/{name}` | Invoke a tool. Body: `{"input": {...}, "meta": {...}, "idempotency_key": "..."}` |
| `GET /jobs/{id}` | Poll job status / result / error |
| `GET /jobs/{id}/out/{file}` | Tail a job's output file (live while running) |
| `GET /queue` | Queue depth, worker count, recent jobs |

Each job gets a stable workspace at `<data>/jobs/<id>/` with `in/` and `out/`.
`out/result.json` (or whatever `sandbox.out` declares) is a real file — tail it
live from the shell while the job runs, or via `GET /jobs/{id}/out/result.json`.

A conforming request to `POST /tools/example-tool`:

```bash
curl -X POST localhost:8080/tools/example-tool \
  -H 'Content-Type: application/json' \
  -d '{"input": {"message": "hello"}, "meta": {"agent": "operator"}}'
# 202 {"id":"job-…","status":"queued",…}
```

Poll `GET /jobs/{id}` until `status` is `done` (result present) or `failed`
(error explains why — retryable after max_retries, or a permanent contract
violation which is never retried).

A non-conforming request is rejected at the gate:

```bash
curl -X POST localhost:8080/tools/example-tool -d '{"input": {}}'
# 400 {"error":"contract violation: …","violations":["$: missing required property \"message\""]}
```

## Enforcement summary

| Rule | Enforced by | Never-retried? |
|---|---|---|
| Input matches `input` schema | validator at submit | — (rejected, HTTP 400) |
| No forbidden file writes | `out/` audit post-run | ✅ permanent |
| No network egress when `network:false` | bubblewrap `--unshare-net` | structural |
| Output is one JSON doc | stdout parse or `out/` file | ✅ permanent |
| Output matches `output` schema | validator | ✅ permanent |
| Transient tool failure | queue retry w/ exponential backoff | retried up to `max_retries` |
| Tool hangs | per-job timeout | retried (timeout) |

## The bwrap layer (minimal by construction)

Every job runs inside its own bubblewrap sandbox. Inside, the tool sees **only**:

- `/deskbox/in/` — exactly the files declared in `sandbox.in`, each bound
  read-only individually (the tool cannot list siblings it wasn't given)
- `/deskbox/out/` — the writable output dir (host-tailable, real path)
- `/deskbox/tool/` — the tool's own folder, read-only
- `/usr /bin /sbin /lib /lib64`, a bare minimum of `/etc`, `/dev`, `/proc`, tmpfs `/tmp`

**The host filesystem is NOT mounted** — no `--ro-bind / /`. Home dirs,
`~/.ssh`, `/var`, service secrets: invisible. Environment is fully controlled
(`cmd.Env`): `OPENROUTER_API_KEY` and the rest of the host shell env never
reach the sandbox. `network: false` adds `--unshare-net`; with egress allowed,
DNS + TLS roots are bound so curl/git work.

(The toplevel `/bin /sbin /lib /lib64` are Fedora symlinks to `usr/`; they
must be bound alongside `/usr` or the ELF interpreter can't resolve and
`execvp` fails with ENOENT.)

## Build & run

```bash
# requires Go 1.22+ (installed in this env at ~/.local/bin/go)
go build -o bin/agent-desk ./cmd/agent-desk
./bin/agent-desk -addr :8080                   # -workers defaults to 10, -tools to ./tools
```

Demo tools included: `example-tool` (file protocol — input gate, retry,
undeclared-write violation, live tail, host-privacy peek) and `net-probe`
(stdio protocol — proves egress is cut).

## Idempotency (Postgres, optional)

By default job state lives only in memory: a desk restart drops history, and
two submits (e.g. an operator retrying after a dropped connection) run the
tool twice. Point the desk at Postgres to fix both:

```bash
export DESKBOX_POSTGRES_DSN="postgres://user:pass@host:5432/deskbox?sslmode=disable"
./bin/agent-desk -addr :8080          # or: -postgres-dsn "$DESKBOX_POSTGRES_DSN"
```

The desk creates its own `jobs` table on startup (`CREATE TABLE IF NOT
EXISTS`, no migration tool needed). With Postgres configured:

- **Dedup.** Pass `idempotency_key` in the submit body. A repeated submit for
  the same `(tool, idempotency_key)` returns the existing job — whatever its
  current status — instead of running the tool again:
  ```bash
  curl -X POST localhost:8080/tools/example-tool \
    -d '{"input": {"message": "hello"}, "idempotency_key": "operator-run-42"}'
  # first call: 202, status queued/running/done
  # any later call with the same key: 202, the SAME job id, tool not re-run
  ```
  No key = no dedup, same as today (a fresh job every time).
- **Resume.** On startup the desk reloads every job left `queued` or
  `running` by a prior process (crash, redeploy, `kill -9`) and re-enqueues
  it, so in-flight work isn't silently dropped. This is why the resume
  behavior and the idempotency key share one mechanism: both answer "did
  this already happen?" from the same durable row instead of trusting
  in-memory state that a restart just erased.

Without `-postgres-dsn` / `DESKBOX_POSTGRES_DSN` set, the desk behaves exactly
as before — in-memory only, `idempotency_key` accepted but ignored.

## Status / roadmap

- [x] TCS loading, input gate, output schema, out/ file-write audit
- [x] Queued execution, worker pool, retries with backoff, job history
- [x] Minimal per-job bwrap layer: only declared in/ + out/ visible, no host
      mounts, controlled env, live-tailable output files + HTTP tail endpoint
- [x] Durable job log + idempotency (Postgres, optional): `idempotency_key`
      dedup, resume of `queued`/`running` jobs after a crash/restart
- [ ] `tcs-verify` Rust CLI (offline spec linting) — stub only
- [ ] Auth token for the desk
- [ ] pi plugin: operator agent that talks to the desk (the original idea)