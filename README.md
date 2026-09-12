# DeskBox

Enforced tool contracts for AI agents.

Agents don't run tools directly. They call DeskBox, and DeskBox checks a
**Tool Contract Spec (TCS)** before running the tool (input schema) and after
(output schema, side effects). Each run happens in its own bubblewrap
sandbox, so the contract is enforced by the kernel, not by trusting the
agent to follow it.

It ships as a single static binary with no shared-library dependencies and
runs on any Linux. A tool is just a program that reads one JSON document on
stdin and writes one on stdout — any language, no SDK, nothing to import —
so the only thing a given box needs is whatever runtime your own tools name.
`agent-desk -check` tells you whether it has them before you trust it with
work.

## Quickstart

```bash
tar -xzf deskbox-v0.1.0-linux-amd64.tar.gz
cd deskbox-v0.1.0-linux-amd64

mkdir -p tools && cp -r examples/tools/greet-python tools/
./agent-desk -check          # does this box have what that tool needs?
./agent-desk                 # binds 127.0.0.1:8080
```

Then call it. `?wait=` turns invoke-and-poll into one request:

```bash
curl -sS -X POST 'localhost:8080/tools/greet-python?wait=5s' \
  -H 'content-type: application/json' \
  -d '{"input":{"name":"Ryan","excited":true}}'
```

```json
{"id":"job-699c9b85","tool":"greet-python","status":"done","attempt":1,
 "result":{"greeting":"Hello, Ryan!","length":12}, ...}
```

The contract is the point, so try breaking it. The tool never runs:

```bash
curl -sS -X POST localhost:8080/tools/greet-python \
  -H 'content-type: application/json' -d '{"input":{"nam":"typo"}}'
```

```json
{"error":"contract violation: input does not conform to the tool contract",
 "violations":["$: missing required property \"name\"",
               "$: unexpected property \"nam\" is not allowed by the contract"]}
```

That tool is 20 lines of Python that reads one JSON document on stdin and
writes one on stdout. It imports nothing, knows nothing about DeskBox, and
would behave identically piped to by hand — which is the whole design. Write
your own the same way in any language, drop the folder in `tools/`, and it
is a contract-enforced endpoint.

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
| `GET /` | Service status, queue summary, and the `enforcement` block (which guarantees are actually being enforced right now) |
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
| `GET /preflight` | Whether this box actually has what every loaded tool declares it needs (the JSON form of `-check`) |

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

### Fail-open by default, `-strict` when that isn't acceptable

Two of those rows depend on machinery that can simply be absent: without
`bwrap` there is no sandbox, so `network: false` degrades to advisory, and
without a usable `systemd-run --user --scope` there are no per-job memory
and task caps. By default the desk **fails open** — it logs the degradation
loudly and keeps serving, because a desk that refuses to run on a host
missing one optional package is a desk nobody can adopt.

Failing open is the wrong default for some deployments and the whole point
is that enforcement is not a matter of trust, so `-strict` (or
`DESKBOX_STRICT=true`) inverts it:

```bash
./bin/agent-desk -strict
# 2026/09/11 22:24:30 -strict: refusing to start because these enforcement
# mechanisms are unavailable: bubblewrap (the sandbox itself); systemd-run
# --user --scope (per-job memory and task caps). Install them, or drop
# -strict to run with those guarantees downgraded to advisory.
```

It holds at run time too, not only at startup. `systemd-run --user --scope`
can pass the boot probe and stop working later — an SSH session going away,
a container without full session infrastructure — and the desk's fail-open
response is to disable the wrapper for the rest of its life. Under `-strict`
that flag instead makes every subsequent job fail with
`enforcement unavailable` rather than run uncapped. Those refusals are
permanent, never retried: the missing mechanism is process-wide, so a second
attempt would fail identically while burning the tool's retry budget.

`-strict` also refuses two things that are postures rather than missing
mechanisms, because both erase what the mechanisms buy: running as **root**
(every job would get uid 0 inside its sandbox), and listening on a
**routable address with auth off** (anyone who can reach the box could
submit jobs). Auth being off on a loopback bind is just a single-user dev
machine, so that warns without refusing.

### Knowing which guarantees are live

Whether or not you use `-strict`, `GET /` reports what is actually being
enforced, so an operator (or an agent deciding how much to trust a result)
never has to infer it from the logs:

```json
{
  "enforcement": {
    "strict": false,
    "sandbox": false,
    "resource_limits": false,
    "auth": false,
    "listen": "127.0.0.1:8080",
    "user_scoped": false,
    "user": "deskbox (uid 1000)",
    "root": false,
    "degraded": true,
    "warnings": [
      "bubblewrap not found: tools run unsandboxed, and network:false is advisory only rather than enforced by the kernel",
      "systemd-run --user --scope not usable: per-job memory and task-count limits are not enforced",
      "auth disabled: anything that can reach this port can submit jobs"
    ]
  }
}
```

`degraded` is true when the sandbox or the resource caps are missing, or
when the desk is running as root — the one field to check if you only check
one. `user` names the account tools will run as, which is the ceiling on
what any of them can do.

### What the sandbox does and does not cover

Worth being precise, because the honest answer is "a lot, with two things
that are yours to get right".

Kernel-enforced, per job, nothing to trust:

- no host filesystem — no home dirs, no `~/.ssh`, no `/var`, no service
  secrets; only `/usr`-and-friends read-only, the declared input files, the
  job's own `out/`, and the tool's folder
- no network at all when `network: false` (`--unshare-net`)
- no view of host processes, no shared memory, no controlling terminal
  (`--unshare-pid`, `--unshare-ipc`, `--new-session`)
- no host environment — `cmd.Env` is built from scratch, so no API key or
  shell variable of yours reaches a tool
- no surviving orphan if the desk dies (`--die-with-parent`, plus
  `Pdeathsig` for the case where the desk is killed hard)
- memory and process-count caps per job, where `systemd-run --user` works

Not covered, by design or by limit:

- **The account the desk runs as is the ceiling.** Nothing sets a
  credential, so a tool runs as exactly the user the desk runs as. That is
  what makes "a tool can't touch anything this account can't" true — and it
  is why running the desk as root throws most of it away: the sandbox still
  limits which paths exist, but everything inside is reached as uid 0, and
  files the tool leaves in `out/` are root-owned. The desk now says so
  loudly at startup, reports it in `GET /`, and refuses to start under
  `-strict`.
- **Syscalls are not filtered.** There is no seccomp profile; a tool can
  make any syscall its uid is allowed to make. Namespaces limit what it can
  *reach*, not what it can *ask for*.
- **`network: true` is all-or-nothing.** A tool that declares egress gets
  DNS and TLS roots bound and can talk to anything. There is no per-host
  allowlist.
- **A contract is a promise about shape, not intent.** The desk enforces
  that a tool takes and returns what it says, writes only what it declared,
  and reaches only what it asked for. It cannot tell you the tool does
  something sensible with that. Vetting the tool is still your job — which
  is the whole reason `tools/` ships empty.
- **The desk itself is not sandboxed.** It is an ordinary process that
  binds a port and runs programs. Which is why it defaults to loopback.
- **"Vetted" is a point in time.** The desk checks that a run script isn't
  writable by other accounts, but nothing pins its *contents* — a tool you
  read last month is whatever is on disk today. The same goes for what it
  pulls in from `/usr`, which your package manager updates underneath it.

### Auth and the listen address

The desk binds `127.0.0.1:8080` by default. It runs programs on request and
auth is off unless you turn it on, so a default reachable from the network
would mean anyone who can route to the box can ask it to run a tool.
Binding wider is a deliberate choice: the desk warns at startup unless auth
is on, and `-strict` refuses outright.

**Loopback stops the network, not other accounts.** Every local user can
reach `127.0.0.1`, and a job any of them submits runs as the user the desk
runs as. On a single-user machine that is the same thing; on a shared box
it is not. Two ways to close it, and the first is stronger:

```bash
./agent-desk -addr /run/user/$(id -u)/deskbox.sock   # 0600 unix socket
```

An `-addr` containing `/` is a unix socket, created mode `0600`. The kernel
checks the file mode on connect, so access is scoped to exactly one OS
account with no shared secret to leak, rotate or forget — which is what
"scoped to the user executing the desk call" actually means. Everything
works unchanged; point clients at the socket:

```bash
curl --unix-socket /run/user/$(id -u)/deskbox.sock http://localhost/tools
```

The other way is a token, which is the right answer when something on
another host legitimately needs to call the desk:

```bash
# .env
DESKBOX_AUTH_ENABLED=true
DESKBOX_AUTH_TOKEN=$(openssl rand -hex 32)
```

`GET /` reports which of these is in force (`listen`, `user_scoped`, `auth`),
and the auth warning is phrased for the binding you actually chose rather
than generically — a warning list you learn to ignore is worse than none.

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

## Build, install, ship

```bash
make build      # static binaries into bin/
make test       # go vet + go test -race
make check      # ask this box whether it can actually run your tools
make install    # into /usr/local/bin (PREFIX= to change)
make dist       # release tarballs for linux/amd64 + linux/arm64, with SHA256SUMS
```

Both binaries are built `CGO_ENABLED=0` and are statically linked. That is
deliberate and it is the whole portability story: the SQLite driver is pure
Go (`modernc.org/sqlite`), so there is no libc to match, no shared object to
be missing, and one binary runs on any Linux of the same architecture —
glibc or musl, old distro or new. `make dist` cross-compiles both
architectures from any machine with a Go toolchain; nothing needs a
container or a matching build host.

Building from source needs **Go 1.25+** — that floor comes from the
dependencies (`modernc.org/sqlite` and `pgx` both declare it), not from the
desk's own code. Running a released binary needs no toolchain at all, which
is the point: `tar -xzf`, then run it.

```bash
tar -xzf deskbox-v0.1.0-linux-amd64.tar.gz
cd deskbox-v0.1.0-linux-amd64
./agent-desk -version
./agent-desk -check                            # before you trust it with work
./agent-desk                                   # 127.0.0.1:8080; -workers 10, -tools ./tools
```

`tcs-shim` must be installed somewhere **under `/usr`** (`/usr/local/bin` is
what `make install` uses). The sandbox binds `/usr` read-only and does not
bind home directories, so a `tcs-shim` in `~/bin` is invisible to every
shimmed tool — `-check` catches exactly this.

Write a `tcs.yaml` + `run.sh` under `tools/<name>/` following the conventions
above and the desk will pick it up — or start from one of the samples:

```bash
cp -r examples/tools/greet-python tools/
```

### `-check`: does this box have what the tools need?

The desk is portable; *tools* are not, and a tool is only as portable as the
runtime it names. `-check` resolves every tool's declared dependencies
against this machine and exits non-zero if any of them is unusable:

```
$ ./agent-desk -check
greet-python
  ok   interpreter python3    /usr/bin/python3
grep-shim
  ok   interpreter /bin/sh    /bin/sh
  ok   shim        tcs-shim   /usr/local/bin/tcs-shim
  ok   command     grep       /usr/bin/grep
jq-filter
  FAIL command     jq         NOT FOUND
       not found on the desk's PATH
       (/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin)

1 of 7 dependencies unusable on this box.
```

Everything it checks is *declared*, never inferred from the body of a shell
script: a run script's shebang, and a `shim.yaml`'s `exec[0]` plus the
`tcs-shim` binary itself. Guessing at shell semantics would produce
confident wrong answers, which is worse than no check.

Two things it resolves that a quick `command -v` does not:

- It looks up against **the PATH the desk hands every job**, not the PATH of
  the shell you ran it from. A runtime in `~/bin` is on your PATH and on no
  job's PATH; checking your own would call a broken box ready.
- It flags a runtime that exists but sits **outside the paths the sandbox
  binds** (`/usr`, `/bin`, `/sbin`, `/lib`, `/lib64`) — a tarball install
  under `/opt`, Homebrew on Linux, `nix`, anything in `$HOME`. It is plainly
  installed and it does not exist inside the sandbox, which without this
  check surfaces as a baffling `ENOENT` on the first job. That is a hard
  failure on a host with bubblewrap and a portability warning on one
  without, and it is reported as whichever it actually is.

The same report is served as JSON at `GET /preflight`, so an agent that just
got a confusing tool failure — or an operator without shell access to the
box — can ask the running desk what it is missing. The desk also logs any
problems at startup rather than waiting for the first job to hit them.

### Running it as a service

```ini
# ~/.config/systemd/user/deskbox.service
[Unit]
Description=DeskBox agent desk
After=network.target

[Service]
ExecStart=/usr/local/bin/agent-desk -addr 127.0.0.1:8080 -tools %h/deskbox/tools
WorkingDirectory=%h/deskbox
Restart=on-failure

[Install]
WantedBy=default.target
```

```bash
systemctl --user enable --now deskbox
loginctl enable-linger $(whoami)   # keeps the user session — and per-job
                                   # resource limits — alive without a login
```

A **user** service, not a system one, is the intended shape: enforcement is
scoped to the OS user running the desk, and per-job cgroup limits go through
`systemd-run --user`, which needs that user's session to exist. `enable-linger`
is what makes it survive logout.

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
| `DESKBOX_STRICT` | `true` to refuse to start, and refuse to run jobs, when the sandbox or per-job resource limits are unavailable, instead of degrading them to advisory. Same as `-strict`; the flag wins if both are set. Default off (fail open). |

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
`bwrap`-missing and store-unset cases. Start with `-strict` if you would
rather the desk refuse the work than run it uncapped.

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
./bin/agent-desk                              # sqlite at ./data/deskbox.db
./bin/agent-desk -sqlite-path /var/lib/deskbox/jobs.db
./bin/agent-desk -store postgres -postgres-dsn "postgres://user:pass@host:5432/deskbox?sslmode=disable"
./bin/agent-desk -store memory
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
- [x] `-strict`: refuse to start, and refuse to run jobs, rather than
      silently downgrading the sandbox or per-job caps to advisory; `GET /`
      reports which guarantees are live either way
- [x] Shippable on any Linux: static CGO-free binaries for amd64/arm64 via
      `make dist`, `-check` to tell an operator up front which runtimes a
      box is missing or has installed where the sandbox can't see them
- [ ] `tcs-verify` Rust CLI (offline spec linting), stub only
- [ ] `pi` plugin: operator agent that talks to the desk (the original idea)

## License

[PolyForm Noncommercial 1.0.0](LICENSE.md). Free for any noncommercial
purpose. Commercial use needs a separate license from the author.
