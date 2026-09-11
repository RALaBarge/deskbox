# Example tools

Four tools that exist to make one point each. They live here rather than in
`tools/` on purpose: `tools/` is the space *you* fill with things you wrote
or vetted, and shipping examples that auto-load into it would undercut
that. Copy what's useful:

```bash
cp -r examples/tools/greet-python tools/
```

| Tool | Makes the point that… | Needs |
|---|---|---|
| `greet-python` | a tool is just a program that reads JSON on stdin and writes JSON on stdout — no SDK, no import, no dependency on the desk | `python3` |
| `wordcount-perl` | the desk genuinely does not care about the language; this is Perl next to Python and the desk can't tell | `perl` only |
| `jq-filter` | a pre-existing CLI that never heard of the desk can be adopted with **no glue code at all**, via `tcs-shim` + a `shim.yaml` | `jq` installed |
| `grep-shim` | the shim handles the awkward real-world parts: optional flags, plain-text output, and a tool whose non-zero exit is a legitimate answer | `grep` |

These are meant to run on *any* Linux box, not just the one they were
written on, so the dependency column is deliberately boring. `grep` is
POSIX and `perl` ships in the base of essentially every distribution;
`wordcount-perl` uses `JSON::PP`, which has been a core module since Perl
5.14 and is pure Perl — no XS, no shared object to be missing. `python3`
is everywhere in practice but can be absent from minimal images. `jq` is
the one genuine install.

The same instinct is worth applying to your own tools: prefer a runtime
that is already present and a library with nothing linked, or ship a
static binary that needs neither.

## Wrapping a tool you didn't write (`tcs-shim`)

Most useful CLIs take argv flags and print text. `tcs-shim` adapts one to
the contract convention without you writing a per-tool wrapper:

```
tools/jq-filter/
  tcs.yaml    # the contract, same as any tool
  shim.yaml   # how to map JSON <-> the real command
  run.sh      # one line: exec tcs-shim
```

Build and install it somewhere the sandbox can see (see the path note
below):

```bash
go build -o /usr/local/bin/tcs-shim ./cmd/tcs-shim
```

### shim.yaml

```yaml
exec:
  - grep
  - "{{if .ignore_case}}-i{{end}}"   # renders empty when absent → dropped
  - --                                # everything after is data, not flags
  - "{{.pattern}}"
  - {spread: "extra_paths"}           # a JSON array → one argument each

stdin: "{{.text}}"          # optional; use {{json .field}} to pass an object

success_exit_codes: [0, 1]  # grep's 1 means "no matches", not "failed"
max_output_bytes: 33554432  # optional; 32 MiB default
allow_flag_values: false    # optional; see the injection notes below

output:
  mode: lines               # json | raw | lines | jsonl | base64
  field: matches            # optional, defaults to "data"
  include_exit_code: false
```

**Output modes.** `json` passes the command's own JSON straight through
(the desk then validates it against `tcs.yaml`). `raw` wraps stdout as a
string, `lines` splits it into an array, `jsonl` parses each line, and
`base64` encodes the bytes for a tool that emits binary.

`raw` and `lines` reject output that isn't valid UTF-8 rather than
accepting it: Go's JSON encoder silently rewrites every invalid byte as
U+FFFD, so a binary blob would round-trip as corrupted text that still
satisfies a `type: string` schema. Use `base64` for those tools.

**Output is capped** at `max_output_bytes` (32 MiB default) and the job
fails cleanly past it. This is not belt-and-braces: buffering is a memory
amplifier — the buffer, the string copy and the JSON encoding of the same
bytes coexist, so 600 MB of tool output measured **3.19 GB of RSS** before
the cap existed. The per-job cgroup is not a dependable backstop either,
since `systemd-run --user --scope` is unavailable on plenty of hosts and
the desk fails open when it is. The desk applies the same cap on its own
side.

**On injection — what is and isn't guaranteed.** `exec` is an argv array
executed directly, with no shell anywhere in the path, so a value
containing `$(...)`, backticks or `;` reaches the command as literal text.
Verified: `$(touch /tmp/PWNED)` as a jq filter produces a jq syntax error
and no file.

That is *not* the same as "a caller-supplied value can never cause
execution", and an earlier version of this document wrongly claimed it was.
No shell is needed to run a command if the wrapped tool has a flag that
runs one: `find -exec`, `xargs`, `tar --to-command`, `ssh`, `rsync -e`. A
review broke the original design exactly this way — a `find` shim
spreading caller input accepted `-exec /usr/bin/touch /tmp/PWNED ;` and
created the file, with no shell involved.

So the shim also refuses any caller-supplied value that starts with `-`:

```
tcs-shim: exec[2] (spread "filters"): refusing caller-supplied value "-name"
because it starts with "-" and would reach the command as a flag.
```

Two things deliberately *don't* trip it. A literal in `shim.yaml` is your
own text, so `-n` and `-c` are fine. And a conditional like
`{{if .ignore_case}}-i{{end}}` is also your text — the caller decides
whether it appears, never what it says — so optional flags keep working.
Only values that actually carry caller data are checked, decided by
inspecting the parsed template rather than guessing from the presence of
`{{`.

Once you emit a literal `--`, everything after it is positional and the
guard steps aside, which is what makes the error message's advice actually work.
`allow_flag_values: true` disables the check entirely — only for a command
that genuinely cannot be made to execute anything.

**Enforcement is not weakened.** A shimmed tool is still a tool: same bwrap
sandbox, same `network: false`, same `out/` audit, same per-job memory and
task caps, and its output is still validated against the contract. Point a
shim at a command that returns the wrong shape and the desk rejects the
job, exactly as it would for a hand-written tool.

## The path trap (read this before wondering why your tool won't run)

The sandbox binds `/usr`, `/bin`, `/sbin`, `/lib` and `/lib64` read-only
and **nothing else** — no `/opt`, no `/home`, no `/nix`, no
`/home/linuxbrew`. An interpreter or binary living outside those paths
simply does not exist inside the sandbox, and your tool fails with a
confusing "not found" even though the thing is plainly installed on the
host.

This is easy to trip over because several popular ways of installing
runtimes put them outside `/usr`: tarball installs under `/opt`, Homebrew
on Linux, `nix`, and anything in a home directory. It also means a tool
that works on your machine can fail on another one that installed the same
runtime differently — which is the real argument for keeping a tool's
dependencies boring.

Three ways out, in order of preference:

1. **Depend on nothing unusual.** A runtime that is part of the base system
   is under `/usr` on every distribution. This is why the examples use
   `perl`, `grep` and `python3` rather than something that needs
   installing.
2. **Ship a static binary** and vendor it into the tool's own folder. It
   needs no interpreter and no bound path at all — a Go or Rust tool builds
   to roughly 2 MB with zero shared-library dependencies. Call it through
   `$DESKBOX_TOOL_DIR`, which the desk sets for every run: `/deskbox/tool`
   inside the sandbox, the real folder path when bubblewrap isn't
   available. The tool folder is always bound read-only, so a vendored
   binary is always reachable.
3. **Vendor a whole runtime** the same way, if you must. A bundled
   JavaScript or Python runtime is 60–120 MB, so keep it out of git — a
   `build.sh`/`fetch.sh` in the tool folder plus a `.gitignore` entry beats
   committing the binary.

Check where something actually lives before depending on it:

```bash
command -v perl      # /usr/bin/perl        — inside the sandbox
command -v node      # /opt/node22/bin/node — NOT inside the sandbox
```
