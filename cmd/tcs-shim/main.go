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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// Config is shim.yaml: the whole mapping, declared rather than coded.
type Config struct {
	// Exec is the command to run. Each element is either a string (a Go
	// template over the input JSON) or a {spread: field} object that
	// expands a JSON array into one argument per element. Rendered
	// elements that come out empty are dropped, which is how an optional
	// flag stays optional. Executed as an argv array — never through a
	// shell, so a template value can't inject an extra command.
	Exec []any `yaml:"exec"`
	// Stdin, when set, is a template rendered and fed to the child's stdin.
	Stdin string `yaml:"stdin"`
	// SuccessExitCodes defaults to [0]. Tools like grep exit 1 for "no
	// match", which is a result, not a failure.
	SuccessExitCodes []int `yaml:"success_exit_codes"`
	Output           struct {
		// Mode: json | raw | lines | jsonl.
		Mode string `yaml:"mode"`
		// Field nests the captured output under this key. Required for
		// raw/lines/jsonl; ignored for json (the child's own document is
		// already the result).
		Field string `yaml:"field"`
		// IncludeExitCode adds "exit_code" to the emitted document.
		IncludeExitCode bool `yaml:"include_exit_code"`
	} `yaml:"output"`
}

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

	var input map[string]any
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		return fmt.Errorf("reading input JSON on stdin: %w", err)
	}

	argv, err := buildArgv(cfg.Exec, input)
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
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	code := cmd.ProcessState.ExitCode()
	if runErr != nil && cmd.ProcessState == nil {
		// Never started at all (binary missing inside the sandbox is the
		// usual cause) — say so plainly rather than reporting an exit code
		// that doesn't exist.
		return fmt.Errorf("running %q: %w", argv[0], runErr)
	}
	if !isSuccess(code, cfg.SuccessExitCodes) {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = fmt.Sprintf("exit code %d", code)
		}
		return fmt.Errorf("%s failed: %s", argv[0], msg)
	}

	doc, err := buildOutput(cfg, stdout.Bytes(), code)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(doc)
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
	case "json":
	case "raw", "lines", "jsonl":
		if cfg.Output.Field == "" {
			return nil, fmt.Errorf("shim.yaml: output.field is required for mode %q", cfg.Output.Mode)
		}
	case "":
		return nil, fmt.Errorf("shim.yaml: output.mode is required (json|raw|lines|jsonl)")
	default:
		return nil, fmt.Errorf("shim.yaml: unknown output.mode %q (want json|raw|lines|jsonl)", cfg.Output.Mode)
	}
	return &cfg, nil
}

// buildArgv renders each exec element against the input document. String
// elements are templates; {spread: field} elements expand a JSON array.
func buildArgv(exec []any, input map[string]any) ([]string, error) {
	var argv []string
	for i, raw := range exec {
		switch v := raw.(type) {
		case string:
			rendered, err := renderTemplate(fmt.Sprintf("exec[%d]", i), v, input)
			if err != nil {
				return nil, err
			}
			if rendered == "" {
				continue // an optional flag that didn't apply this call
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
				argv = append(argv, fmt.Sprint(item))
			}
		default:
			return nil, fmt.Errorf("shim.yaml: exec[%d] must be a string or {spread: <field>}", i)
		}
	}
	return argv, nil
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

// noValue is what text/template renders for an absent key under
// missingkey=zero when the map's element type is `any`.
const noValue = "<no value>"

func renderTemplate(name, text string, input map[string]any) (string, error) {
	// missingkey=zero rather than =error, because an absent key is normal
	// here: the desk validates input against the contract's schema before
	// the tool ever runs, so anything missing at this point is an optional
	// field the contract permits, and `{{if .optional}}--flag{{end}}` has
	// to render empty rather than blow up. =error breaks exactly that.
	//
	// The cost is that a bare `{{.typo}}` renders the literal string
	// "<no value>" instead of failing, which would then be passed to the
	// wrapped command as a real argument. So catch that sentinel below and
	// fail loudly — getting both properties instead of choosing one.
	tmpl, err := template.New(name).Funcs(templateFuncs).Option("missingkey=zero").Parse(text)
	if err != nil {
		return "", fmt.Errorf("shim.yaml: %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, input); err != nil {
		return "", fmt.Errorf("shim.yaml: %s: %w", name, err)
	}
	out := buf.String()
	if strings.Contains(out, noValue) {
		return "", fmt.Errorf("shim.yaml: %s references a field that isn't in the input document; "+
			"for a field the contract marks optional, guard it as {{if .field}}...{{end}}", name)
	}
	return out, nil
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
		obj["exit_code"] = code
		return obj, nil
	}

	doc := map[string]any{}
	switch cfg.Output.Mode {
	case "raw":
		doc[cfg.Output.Field] = string(stdout)
	case "lines":
		doc[cfg.Output.Field] = splitLines(stdout)
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
		doc[cfg.Output.Field] = items
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
