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
│   │   └── executor.go      # scratch-dir sandbox, bwrap, side-effect audit
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
  files: []                # empty = no file writes allowed (scratch-dir audit)
  network: false           # false = egress cut by bubblewrap --unshare-net

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
| `POST /tools/{name}` | Invoke a tool. Body: `{"input": {...}, "meta": {...}}` |
| `GET /jobs/{id}` | Poll job status / result / error |
| `GET /queue` | Queue depth, worker count, recent jobs |

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
| No forbidden file writes | scratch-dir audit post-run | ✅ permanent |
| No network egress when `network:false` | bubblewrap `--unshare-net` | structural |
| Output is one JSON doc | stdout parse | ✅ permanent |
| Output matches `output` schema | validator | ✅ permanent |
| Transient tool failure | queue retry w/ exponential backoff | retried up to `max_retries` |
| Tool hangs | per-job timeout | retried (timeout) |

## Build & run

```bash
# requires Go 1.22+ (installed in this env at ~/.local/bin/go)
go build -o bin/agent-desk ./cmd/agent-desk
./bin/agent-desk -addr :8080 -workers 2        # -tools defaults to ./tools
```

Demo tools included: `example-tool` (echo, exercises input gate + retry +
file-write violation) and `net-probe` (proves egress is cut).

## Status / roadmap

- [x] TCS loading, input gate, output schema, scratch-dir file audit
- [x] Queued execution, worker pool, retries with backoff, job history
- [x] bubblewrap network isolation
- [ ] `tcs-verify` Rust CLI (offline spec linting) — stub only
- [ ] Persistent job log (SQLite) + auth token for the desk
- [ ] pi plugin: operator agent that talks to the desk (the original idea)