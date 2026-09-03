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
│   │   └── store.go         # optional Postgres: durable jobs, idempotency_key
│   └── tcs-verify/          # (stub) Rust CLI for offline spec checking
└── tools/                   # the space agents read, each tool owns a folder:
    └── <name>/              #   tcs.yaml (contract), run.sh (implementation),
                              #   README.md (skill notes for agents)
```

No tools ship in this repo yet. `tools/` is where you drop your own.

A folder is a tool iff it contains `tcs.yaml` + an executable `run.sh|run.py|run`.

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
minItems/maxItems, enum, minLength/maxLength, minimum/maximum`.

## Tool conventions (the files in the space)

1. Input arrives as one JSON document on **stdin**.
2. Output is exactly one JSON document on **stdout**; nothing else.
3. Errors go to **stderr** with a non-zero exit.
4. Files a tool may write are declared in `allowed_side_effects.files`.
5. `network: false` means no egress. Enforced by bubblewrap, not the code.

## API

| Endpoint | Purpose |
|---|---|
| `GET /` | Service status + queue summary |
| `GET /tools` | List tools + their contracts (what an agent may invoke) |
| `GET /tools/{name}` | Raw `tcs.yaml`, the contract to conform to |
| `POST /tools/{name}` | Invoke a tool. Body: `{"input": {...}, "meta": {...}, "idempotency_key": "..."}` |
| `GET /jobs/{id}` | Poll job status / result / error |
| `GET /jobs/{id}/out/{file}` | Tail a job's output file (live while running) |
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

Poll `GET /jobs/{id}` until `status` is `done` (result present) or `failed`
(error explains why: retryable after max_retries, or a permanent contract
violation, never retried).

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
```

No demo tools ship in this repo. Write a `tcs.yaml` + `run.sh` under
`tools/<name>/` following the conventions above and the desk will pick it up.

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
| `DESKBOX_POSTGRES_DSN` | Same as `-postgres-dsn` below; the flag wins if both are set. |
| `DESKBOX_JOB_MEMORY_MAX` | Per-job memory cap (systemd `MemoryMax` syntax, e.g. `512M`). Default `512M`. |
| `DESKBOX_JOB_TASKS_MAX` | Per-job cap on forked processes/threads (systemd `TasksMax`), stops fork bombs. Default `64`. |

`.env` is gitignored. Never commit a real token, generate one per
deployment (`openssl rand -hex 32` works fine).

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
`bwrap`-missing and Postgres-unset cases.

## Idempotency (Postgres, optional)

By default job state lives only in memory. A desk restart drops history, and
two submits (e.g. an operator retrying after a dropped connection) run the
tool twice. Point the desk at Postgres to fix both:

```bash
export DESKBOX_POSTGRES_DSN="postgres://user:pass@host:5432/deskbox?sslmode=disable"
./bin/agent-desk -addr :8080          # or: -postgres-dsn "$DESKBOX_POSTGRES_DSN"
```

The desk creates its own `jobs` table on startup (`CREATE TABLE IF NOT
EXISTS`, no migration tool needed). With Postgres configured:

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

Without `-postgres-dsn` / `DESKBOX_POSTGRES_DSN` set, the desk behaves exactly
as before: in-memory only, `idempotency_key` accepted but ignored.

## Status / roadmap

- [x] TCS loading, input gate, output schema, out/ file-write audit
- [x] Queued execution, worker pool, retries with backoff, job history
- [x] Minimal per-job bwrap layer: only declared in/ + out/ visible, no host
      mounts, controlled env, live-tailable output files + HTTP tail endpoint
- [x] Durable job log + idempotency (Postgres, optional): `idempotency_key`
      dedup, resume of `queued`/`running` jobs after a crash/restart
- [ ] `tcs-verify` Rust CLI (offline spec linting), stub only
- [x] Auth token for the desk (optional, `.env`-driven, see Settings above)
- [x] Per-job memory/task-count cgroup limits (`systemd-run --user --scope`,
      fails open with a clear warning if the environment can't support it)
- [ ] `pi` plugin: operator agent that talks to the desk (the original idea)

## License

[PolyForm Noncommercial 1.0.0](LICENSE.md). Free for any noncommercial
purpose. Commercial use needs a separate license from the author.
