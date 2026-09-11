# Example tools

Four tools that exist to make one point each. They live here rather than in
`tools/` on purpose: `tools/` is the space *you* fill with things you wrote
or vetted, and shipping examples that auto-load into it would undercut
that. Copy what's useful:

```bash
cp -r examples/tools/greet-python tools/
```

| Tool | Makes the point that… |
|---|---|
| `greet-python` | a tool is just a program that reads JSON on stdin and writes JSON on stdout — no SDK, no import, no dependency on the desk |
| `wordcount-node` | the desk genuinely does not care about the language; this is JavaScript next to Python and the desk can't tell |
| `jq-filter` | a pre-existing CLI that never heard of the desk can be adopted with **no glue code at all**, via `tcs-shim` + a `shim.yaml` |
| `grep-shim` | the shim handles the awkward real-world parts: optional flags, plain-text output, and a tool whose non-zero exit is a legitimate answer |

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

output:
  mode: lines               # json | raw | lines | jsonl
  field: matches            # required except for json mode
  include_exit_code: false
```

**Output modes.** `json` passes the command's own JSON straight through
(the desk then validates it against `tcs.yaml`). `raw` wraps stdout as a
string, `lines` splits it into an array, `jsonl` parses each line.

**On injection.** `exec` is an argv array executed directly — there is no
shell anywhere in the path, so a value containing `$(...)`, backticks or
`;` is passed to the command as literal text and cannot start a second
process. Verified, not assumed: feeding `$(touch /tmp/PWNED)` as a jq
filter produces a jq syntax error and no file.

**Enforcement is not weakened.** A shimmed tool is still a tool: same bwrap
sandbox, same `network: false`, same `out/` audit, same per-job memory and
task caps, and its output is still validated against the contract. Point a
shim at a command that returns the wrong shape and the desk rejects the
job, exactly as it would for a hand-written tool.

## The path trap (read this before wondering why your tool won't run)

The sandbox binds `/usr`, `/bin`, `/sbin`, `/lib` and `/lib64` read-only
and **nothing else**. A binary or interpreter living anywhere else is
invisible inside it. This bites in practice: on the machine these examples
were written on, `node` is at `/opt/node22/bin/node`, which a real
sandboxed run cannot see, while `python3`, `jq` and `grep` are all under
`/usr` and work fine.

Two ways out, both already supported:

1. Use an interpreter that lives under `/usr` (the usual case on a normal
   distro install).
2. Vendor the binary into the tool's own folder and call it through
   `$DESKBOX_TOOL_DIR`, which the desk sets for every run — to
   `/deskbox/tool` inside the sandbox, or the real folder path when
   bubblewrap isn't available. The tool folder is always bound read-only,
   so a vendored binary is always reachable.
