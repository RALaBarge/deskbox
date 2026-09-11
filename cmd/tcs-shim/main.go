// Command tcs-shim adapts a pre-existing CLI — one that knows nothing about
// DeskBox, takes argv flags, and emits text — into a tool that speaks the
// Tool Contract Spec convention (one JSON document on stdin, exactly one
// JSON document on stdout).
//
// A shimmed tool folder looks like any other tool to the desk:
//
//	tools/jq-filter/
//	  tcs.yaml    # the contract, unchanged in concept
//	  shim.yaml   # how to map JSON <-> the real command
//	  run.sh      # one line: exec tcs-shim
//
// The desk needs no new discovery rules, and the wrapped binary is still
// subject to every enforcement a hand-written tool gets: the same bwrap
// sandbox, the same network:false, the same out/ audit, the same per-job
// resource caps. Wrapping a CLI you didn't write doesn't weaken any of it.
//
// The wrapped binary must be reachable inside the sandbox, which binds
// /usr, /bin, /sbin, /lib, /lib64 read-only — an interpreter or binary
// living somewhere else (/opt, a home dir) is invisible there. Vendor it
// into the tool's own folder and call it via $DESKBOX_TOOL_DIR instead.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"text/template/parse"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Config is shim.yaml: the whole mapping, declared rather than coded.
type Config struct {
	// Exec is the command to run. Each element is either a string (a Go
	// template over the input JSON) or a {spread: field} object that
	// expands a JSON array into one argument per element. Rendered
	// elements that come out empty are dropped, which is how an optional
	// flag stays optional.
	//
	// Executed as an argv array with no shell anywhere, so shell
	// metacharacters in a value are inert. That is NOT the same as "a
	// value can't cause execution": plenty of CLIs execute things when
	// given the right *flag* (find -exec, xargs, tar --to-command,
	// ssh, rsync -e). Any element derived from input is therefore
	// rejected if it starts with "-", unless AllowFlagValues opts out.
	// See buildArgv.
	Exec []any `yaml:"exec"`
	// Stdin, when set, is a template rendered and fed to the child's stdin.
	Stdin string `yaml:"stdin"`
	// AllowFlagValues disables the "-" guard above. Only set this when the
	// wrapped command genuinely needs caller-supplied flags AND cannot be
	// made to execute anything — and prefer putting a literal "--" in exec
	// before the caller-supplied values instead.
	AllowFlagValues bool `yaml:"allow_flag_values"`
	// SuccessExitCodes defaults to [0]. Tools like grep exit 1 for "no
	// match", which is a result, not a failure.
	SuccessExitCodes []int `yaml:"success_exit_codes"`
	// MaxOutputBytes caps the wrapped command's stdout. Defaults to
	// defaultMaxOutput. Unbounded buffering here is a memory amplifier:
	// the buffer, the string copy and the JSON encoding of the same bytes
	// coexist, so a tool that prints 600MB can cost several GB of RSS.
	MaxOutputBytes int64 `yaml:"max_output_bytes"`
	Output         struct {
		// Mode: json | raw | lines | jsonl | base64.
		Mode string `yaml:"mode"`
		// Field nests the captured output under this key. Defaults to
		// "data" for the non-json modes; ignored for json (the child's own
		// document is already the result).
		Field string `yaml:"field"`
		// IncludeExitCode adds "exit_code" to the emitted document.
		IncludeExitCode bool `yaml:"include_exit_code"`
	} `yaml:"output"`
}

// defaultMaxOutput bounds a wrapped command's stdout when shim.yaml
// doesn't say otherwise. The per-job cgroup is not a reliable backstop:
// systemd-run's scope is unavailable on plenty of hosts (and fails open by
// design), and the desk buffers the same output again on its side anyway.
const defaultMaxOutput = 32 << 20 // 32 MiB

type spreadArg struct {
	Spread string `yaml:"spread"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "tcs-shim:", err)
		os.Exit(1)
	}
}

func run() error {
	toolDir := os.Getenv("DESKBOX_TOOL_DIR")
	if toolDir == "" {
		return fmt.Errorf("DESKBOX_TOOL_DIR is not set (is this running under agent-desk?)")
	}
	cfg, err := loadConfig(filepath.Join(toolDir, "shim.yaml"))
	if err != nil {
		return err
	}
	declared, err := declaredInputFields(toolDir)
	if err != nil {
		return err
	}
	if err := validateTemplates(cfg, declared); err != nil {
		return err
	}

	var input map[string]any
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		return fmt.Errorf("reading input JSON on stdin: %w", err)
	}

	argv, err := buildArgv(cfg, input)
	if err != nil {
		return err
	}
	if len(argv) == 0 {
		return fmt.Errorf("shim.yaml: exec produced no command")
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	if cfg.Stdin != "" {
		rendered, err := renderTemplate("stdin", cfg.Stdin, input)
		if err != nil {
			return err
		}
		cmd.Stdin = strings.NewReader(rendered)
	}
	maxOut := cfg.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = defaultMaxOutput
	}
	stdout := &capWriter{max: maxOut}
	stderr := &capWriter{max: 64 << 10} // only ever becomes an error message
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	runErr := cmd.Run()
	if cmd.ProcessState == nil {
		// Never started at all (binary missing inside the sandbox is the
		// usual cause) — say so plainly rather than reporting an exit code
		// that doesn't exist.
		return fmt.Errorf("running %q: %w", argv[0], runErr)
	}
	code := cmd.ProcessState.ExitCode()
	if stdout.overflow {
		return fmt.Errorf("%s produced more than %d bytes on stdout; raise max_output_bytes "+
			"in shim.yaml if this tool is legitimately that chatty", argv[0], maxOut)
	}
	if !isSuccess(code, cfg.SuccessExitCodes) {
		msg := strings.TrimSpace(stderr.buf.String())
		if msg == "" {
			msg = fmt.Sprintf("exit code %d", code)
		}
		return fmt.Errorf("%s failed: %s", argv[0], msg)
	}

	doc, err := buildOutput(cfg, stdout.buf.Bytes(), code)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(doc)
}

// capWriter buffers up to max bytes and then quietly stops accumulating,
// reporting success to the writer so the child isn't killed by EPIPE
// mid-write. The caller checks overflow afterwards and fails cleanly.
// Without this, a tool printing hundreds of MB costs several times that in
// RSS once the buffer, the string copy and the JSON encoding coexist.
type capWriter struct {
	buf      bytes.Buffer
	max      int64
	overflow bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if !w.overflow {
		if remaining := w.max - int64(w.buf.Len()); int64(len(p)) > remaining {
			w.buf.Write(p[:remaining])
			w.overflow = true
		} else {
			w.buf.Write(p)
		}
	}
	return len(p), nil
}

func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading shim.yaml: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parsing shim.yaml: %w", err)
	}
	if len(cfg.Exec) == 0 {
		return nil, fmt.Errorf("shim.yaml: exec is required")
	}
	switch cfg.Output.Mode {
	case "json", "raw", "lines", "jsonl", "base64":
		// output.field defaults to "data" for the non-json modes; see
		// buildOutput. Naming it explicitly reads better at the call site
		// ({"matches": [...]} beats {"data": [...]}), but it shouldn't be
		// mandatory just to get a working tool.
	case "":
		return nil, fmt.Errorf("shim.yaml: output.mode is required (json|raw|lines|jsonl|base64)")
	default:
		return nil, fmt.Errorf("shim.yaml: unknown output.mode %q (want json|raw|lines|jsonl|base64)", cfg.Output.Mode)
	}
	return &cfg, nil
}

// buildArgv renders each exec element against the input document. String
// elements are templates; {spread: field} elements expand a JSON array.
//
// Any element whose value comes from the input is checked for a leading
// "-". A literal in shim.yaml (no template actions) is the config author's
// own text and passes through untouched — that's how "-n" or "--" work —
// but a caller-supplied value becoming a flag is argument injection, and
// for the wrong wrapped command that is remote code execution with no
// shell in sight: a `find` shim spreading caller input happily accepts
// -exec. Callers get a clear refusal instead.
func buildArgv(cfg *Config, input map[string]any) ([]string, error) {
	var argv []string
	// Once the author has emitted a literal "--", everything after it is
	// positional by convention and cannot be read as a flag — which is the
	// very remedy the guard's error message recommends, so it has to
	// actually work. Without this the guard rejects configs that already
	// did the right thing.
	afterDoubleDash := false
	for i, raw := range cfg.Exec {
		switch v := raw.(type) {
		case string:
			rendered, err := renderTemplate(fmt.Sprintf("exec[%d]", i), v, input)
			if err != nil {
				return nil, err
			}
			if rendered == "" {
				continue // an optional flag that didn't apply this call
			}
			// Only values carrying input *data* are suspect. A literal is
			// the author's own argument, and so is `{{if .x}}-i{{end}}`:
			// the input decides whether "-i" appears, never what it says.
			// Checking merely for "{{" would reject every optional flag.
			derived, err := interpolatesInputData(v)
			if err != nil {
				return nil, fmt.Errorf("shim.yaml: exec[%d]: %w", i, err)
			}
			if derived && !afterDoubleDash {
				if err := checkFlagValue(cfg, fmt.Sprintf("exec[%d]", i), rendered); err != nil {
					return nil, err
				}
			}
			if !derived && rendered == "--" {
				afterDoubleDash = true
			}
			argv = append(argv, rendered)
		case map[string]any:
			field, _ := v["spread"].(string)
			if field == "" {
				return nil, fmt.Errorf("shim.yaml: exec[%d] object must be {spread: <field>}", i)
			}
			vals, ok := input[field]
			if !ok {
				continue // absent array field spreads to nothing
			}
			list, ok := vals.([]any)
			if !ok {
				return nil, fmt.Errorf("shim.yaml: exec[%d] spread field %q is not an array", i, field)
			}
			for _, item := range list {
				s := fmt.Sprint(item)
				if !afterDoubleDash {
					if err := checkFlagValue(cfg, fmt.Sprintf("exec[%d] (spread %q)", i, field), s); err != nil {
						return nil, err
					}
				}
				argv = append(argv, s)
			}
		default:
			return nil, fmt.Errorf("shim.yaml: exec[%d] must be a string or {spread: <field>}", i)
		}
	}
	return argv, nil
}

// interpolatesInputData reports whether a template puts input *values* into
// its output, as opposed to only testing them. `{{.pattern}}` does;
// `{{if .ignore_case}}-i{{end}}` does not — the emitted text is the config
// author's, and only its presence depends on the caller. The flag guard
// applies to the first and must not fire on the second.
func interpolatesInputData(text string) (bool, error) {
	tmpl, err := template.New("check").Funcs(templateFuncs).Parse(text)
	if err != nil {
		return false, err
	}
	var walk func(parse.Node) bool
	walk = func(n parse.Node) bool {
		switch t := n.(type) {
		case nil:
			return false
		case *parse.ListNode:
			if t == nil {
				return false
			}
			for _, c := range t.Nodes {
				if walk(c) {
					return true
				}
			}
		case *parse.ActionNode:
			// An action's pipe is emitted, so a field in it is data.
			return pipeReferencesField(t.Pipe)
		case *parse.IfNode:
			// The condition is not emitted; the branches are.
			return walk(t.List) || walk(t.ElseList)
		case *parse.RangeNode, *parse.WithNode:
			// Both rebind dot to caller data and can emit it — assume so.
			return true
		}
		return false
	}
	return walk(tmpl.Tree.Root), nil
}

func pipeReferencesField(pipe *parse.PipeNode) bool {
	if pipe == nil {
		return false
	}
	for _, cmd := range pipe.Cmds {
		for _, arg := range cmd.Args {
			switch arg.(type) {
			case *parse.FieldNode, *parse.VariableNode:
				return true
			}
		}
	}
	return false
}

func checkFlagValue(cfg *Config, where, value string) error {
	if cfg.AllowFlagValues || !strings.HasPrefix(value, "-") {
		return nil
	}
	return fmt.Errorf("%s: refusing caller-supplied value %q because it starts with \"-\" and "+
		"would reach the command as a flag. Put a literal \"--\" in exec before the "+
		"caller-supplied arguments, or set allow_flag_values: true if this command "+
		"cannot be made to execute anything", where, value)
}

// templateFuncs are the helpers available inside shim.yaml templates.
// "json" is not optional in practice: a JSON object or array field
// interpolated bare renders as Go's map[k:v] syntax, which is not JSON and
// would silently feed garbage to the wrapped command.
var templateFuncs = template.FuncMap{
	"json": func(v any) (string, error) {
		b, err := json.Marshal(v)
		return string(b), err
	},
}

func renderTemplate(name, text string, input map[string]any) (string, error) {
	// missingkey=zero rather than =error, because an absent key is normal:
	// the desk validates input against the contract before the tool runs,
	// so anything missing here is an optional field the contract permits,
	// and `{{if .optional}}--flag{{end}}` has to render empty rather than
	// blow up.
	//
	// A typo'd field is caught structurally instead — validateTemplates
	// checks every referenced field against tcs.yaml's declared input
	// properties when the config loads. An earlier version scanned the
	// rendered output for "<no value>", which was unsound in both
	// directions: it rejected legitimate input containing that literal
	// string (a grep for Go program output, say), and it only ever fired
	// at runtime on the one call that happened to hit the bad path.
	tmpl, err := template.New(name).Funcs(templateFuncs).Option("missingkey=zero").Parse(text)
	if err != nil {
		return "", fmt.Errorf("shim.yaml: %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, input); err != nil {
		return "", fmt.Errorf("shim.yaml: %s: %w", name, err)
	}
	return buf.String(), nil
}

// validateTemplates fails the run if shim.yaml references an input field
// the contract never declares — a typo, or a field someone renamed in
// tcs.yaml and forgot here. Checked once at load, against the contract
// itself, so it fires on every invocation rather than only the unlucky one.
func validateTemplates(cfg *Config, declared map[string]bool) error {
	// No declared properties means the contract accepts a free-form object
	// (input: {type: object}); there is nothing to check against.
	if len(declared) == 0 {
		return nil
	}
	check := func(where, text string) error {
		tmpl, err := template.New(where).Funcs(templateFuncs).Parse(text)
		if err != nil {
			return fmt.Errorf("shim.yaml: %s: %w", where, err)
		}
		for _, field := range referencedFields(tmpl.Tree.Root) {
			if !declared[field] {
				return fmt.Errorf("shim.yaml: %s references %q, which the contract's "+
					"input schema does not declare", where, field)
			}
		}
		return nil
	}
	for i, raw := range cfg.Exec {
		switch v := raw.(type) {
		case string:
			if err := check(fmt.Sprintf("exec[%d]", i), v); err != nil {
				return err
			}
		case map[string]any:
			if field, _ := v["spread"].(string); field != "" && !declared[field] {
				return fmt.Errorf("shim.yaml: exec[%d] spreads %q, which the contract's "+
					"input schema does not declare", i, field)
			}
		}
	}
	if cfg.Stdin != "" {
		return check("stdin", cfg.Stdin)
	}
	return nil
}

// referencedFields walks a parsed template for top-level field accesses
// ({{.name}}, {{if .name}}, {{json .name}}), which is what shim.yaml
// templates are made of.
func referencedFields(node parse.Node) []string {
	var out []string
	var walk func(parse.Node)
	walk = func(n parse.Node) {
		switch t := n.(type) {
		case nil:
			return
		case *parse.ListNode:
			if t == nil {
				return
			}
			for _, c := range t.Nodes {
				walk(c)
			}
		case *parse.ActionNode:
			walk(t.Pipe)
		case *parse.PipeNode:
			if t == nil {
				return
			}
			for _, c := range t.Cmds {
				walk(c)
			}
		case *parse.CommandNode:
			for _, a := range t.Args {
				walk(a)
			}
		case *parse.FieldNode:
			if len(t.Ident) > 0 {
				out = append(out, t.Ident[0])
			}
		case *parse.IfNode:
			walk(t.Pipe)
			walk(t.List)
			walk(t.ElseList)
		case *parse.RangeNode:
			walk(t.Pipe)
			walk(t.List)
			walk(t.ElseList)
		case *parse.WithNode:
			walk(t.Pipe)
			walk(t.List)
			walk(t.ElseList)
		}
	}
	walk(node)
	return out
}

// declaredInputFields reads the sibling tcs.yaml for the property names its
// input schema declares. The desk only loads folders that have one, so it
// is always there.
func declaredInputFields(toolDir string) (map[string]bool, error) {
	raw, err := os.ReadFile(filepath.Join(toolDir, "tcs.yaml"))
	if err != nil {
		return nil, fmt.Errorf("reading tcs.yaml: %w", err)
	}
	var contract struct {
		Input struct {
			Properties map[string]any `yaml:"properties"`
		} `yaml:"input"`
	}
	if err := yaml.Unmarshal(raw, &contract); err != nil {
		return nil, fmt.Errorf("parsing tcs.yaml: %w", err)
	}
	declared := map[string]bool{}
	for k := range contract.Input.Properties {
		declared[k] = true
	}
	return declared, nil
}

func isSuccess(code int, allowed []int) bool {
	if len(allowed) == 0 {
		return code == 0
	}
	for _, c := range allowed {
		if c == code {
			return true
		}
	}
	return false
}

// buildOutput turns the child's raw stdout into the one JSON document the
// desk will validate against the tool's declared output schema. The result
// is `any`, not an object: in json mode the wrapped command's own document
// is the result, and a contract is free to declare a scalar or array there.
func buildOutput(cfg *Config, stdout []byte, code int) (any, error) {
	if cfg.Output.Mode == "json" {
		var passthrough any
		if err := json.Unmarshal(bytes.TrimSpace(stdout), &passthrough); err != nil {
			return nil, fmt.Errorf("output.mode is json but the command did not emit valid JSON: %w", err)
		}
		if !cfg.Output.IncludeExitCode {
			return passthrough, nil
		}
		obj, ok := passthrough.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("output.include_exit_code needs a JSON object, but the command emitted %T", passthrough)
		}
		// Silently overwriting the command's own exit_code would corrupt
		// its result to add a field that duplicates what the job status
		// already says.
		if _, taken := obj["exit_code"]; taken {
			return nil, fmt.Errorf("output.include_exit_code would overwrite the command's own " +
				"\"exit_code\" field; drop the option or have the tool rename its field")
		}
		obj["exit_code"] = code
		return obj, nil
	}

	field := cfg.Output.Field
	if field == "" {
		field = "data"
	}
	doc := map[string]any{}
	switch cfg.Output.Mode {
	case "raw":
		if !utf8.Valid(stdout) {
			return nil, fmt.Errorf("output.mode is raw but the command emitted bytes that are not " +
				"valid UTF-8; JSON strings cannot carry them (Go would silently rewrite each one " +
				"as U+FFFD) — use output.mode: base64 for a tool that emits binary")
		}
		doc[field] = string(stdout)
	case "base64":
		doc[field] = base64.StdEncoding.EncodeToString(stdout)
	case "lines":
		if !utf8.Valid(stdout) {
			return nil, fmt.Errorf("output.mode is lines but the command emitted bytes that are " +
				"not valid UTF-8; use output.mode: base64 for a tool that emits binary")
		}
		doc[field] = splitLines(stdout)
	case "jsonl":
		var items []any
		for _, line := range splitLines(stdout) {
			var item any
			if err := json.Unmarshal([]byte(line), &item); err != nil {
				return nil, fmt.Errorf("output.mode is jsonl but a line is not valid JSON: %w", err)
			}
			items = append(items, item)
		}
		if items == nil {
			items = []any{}
		}
		doc[field] = items
	}
	if cfg.Output.IncludeExitCode {
		doc["exit_code"] = code
	}
	return doc, nil
}

// splitLines drops the trailing empty element a final newline produces, so
// "a\nb\n" is two lines rather than three.
func splitLines(b []byte) []string {
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return []string{}
	}
	return strings.Split(s, "\n")
}
