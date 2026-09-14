// tcs-verify lints Tool Contract Specs offline — no running desk, no network,
// nothing executed. It answers "is this contract well-formed and will the desk
// enforce what it says" before a tool is ever deployed, using the same schema
// package (deskbox/agent-desk/internal/tcs) the desk validates against, so a
// contract this passes is exactly one the desk will honor.
//
// For each tool folder (or tcs.yaml) it checks:
//   - the YAML parses
//   - input and output schemas use only the supported subset (no silently
//     ignored keywords, no typos, no malformed oneOf/anyOf)
//   - a run script exists, is executable, and is not readable/executable by
//     group or other (the bypass check the desk warns about at load)
//   - any integrity pin matches what is on disk right now
//
// Exit code is non-zero if any tool has an error, so it drops into CI as-is.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"deskbox/agent-desk/internal/tcs"
	"gopkg.in/yaml.v3"
)

// contract is the subset of tcs.yaml this linter reads. It is intentionally
// its own small struct rather than the desk's runtime Tool type: the linter
// checks the file on disk, it does not load a tool to run, so it needs none of
// the runtime machinery.
type contract struct {
	Name   string      `yaml:"name"`
	Input  *tcs.Schema `yaml:"input"`
	Output struct {
		Schema tcs.Schema `yaml:"schema"`
	} `yaml:"output"`
	Integrity struct {
		RunSHA256         string `yaml:"run_sha256"`
		InterpreterSHA256 string `yaml:"interpreter_sha256"`
	} `yaml:"integrity"`
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: tcs-verify <tool-dir|tcs.yaml> [...]\n\n"+
			"Lint one or more Tool Contract Specs offline. A directory is treated as a\n"+
			"tool folder (its tcs.yaml is read); a file is read directly. Exit non-zero\n"+
			"if any contract has an error.\n")
	}
	flag.Parse()
	targets := flag.Args()
	if len(targets) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	failed := 0
	for _, t := range targets {
		path, err := resolveTarget(t)
		if err != nil {
			fmt.Printf("%s\n  ERROR %v\n", t, err)
			failed++
			continue
		}
		problems := lintFile(path)
		errs, warns := 0, 0
		for _, p := range problems {
			if p.warn {
				warns++
			} else {
				errs++
			}
		}
		if errs == 0 && warns == 0 {
			fmt.Printf("%s\n  ok\n", path)
			continue
		}
		fmt.Printf("%s\n", path)
		for _, p := range problems {
			mark := "FAIL"
			if p.warn {
				mark = "warn"
			}
			fmt.Printf("  %-4s %s\n", mark, p.msg)
		}
		if errs > 0 {
			failed++
		}
	}

	if failed > 0 {
		fmt.Printf("\n%d of %d contract(s) have errors.\n", failed, len(targets))
		os.Exit(1)
	}
	fmt.Printf("\nall %d contract(s) OK.\n", len(targets))
}

type finding struct {
	msg  string
	warn bool
}

// resolveTarget turns a tool-dir or file argument into the tcs.yaml path to read.
func resolveTarget(target string) (string, error) {
	fi, err := os.Stat(target)
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		p := filepath.Join(target, "tcs.yaml")
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("no tcs.yaml in %s", target)
		}
		return p, nil
	}
	return target, nil
}

func lintFile(path string) []finding {
	raw, err := os.ReadFile(path)
	if err != nil {
		return []finding{{msg: fmt.Sprintf("cannot read: %v", err)}}
	}
	var c contract
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return []finding{{msg: fmt.Sprintf("invalid YAML: %v", err)}}
	}

	var out []finding
	if strings.TrimSpace(c.Name) == "" {
		out = append(out, finding{msg: "name is empty (the folder name would be used, but declaring it is clearer)", warn: true})
	}

	// The schemas: exactly the desk's subset, so anything flagged here would
	// otherwise validate but not constrain what the author intended.
	if c.Input != nil {
		for _, e := range tcs.Lint(*c.Input) {
			out = append(out, finding{msg: "input" + trimDollar(e)})
		}
	}
	for _, e := range tcs.Lint(c.Output.Schema) {
		out = append(out, finding{msg: "output.schema" + trimDollar(e)})
	}

	// The run script: present, executable, and not bypassable by other
	// accounts — the same conditions the desk checks when it loads a tool.
	dir := filepath.Dir(path)
	out = append(out, lintRunScript(dir, c)...)

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].warn != out[j].warn {
			return !out[i].warn // errors first
		}
		return out[i].msg < out[j].msg
	})
	return out
}

func lintRunScript(dir string, c contract) []finding {
	var out []finding
	var runPath string
	for _, cand := range []string{"run.sh", "run.py", "run"} {
		p := filepath.Join(dir, cand)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			runPath = p
			if fi.Mode()&0o111 == 0 {
				out = append(out, finding{msg: fmt.Sprintf("%s exists but is not executable (chmod +x it)", cand)})
			}
			// Read OR execute by group/other is enough to run a script's
			// contents directly, bypassing the desk — the same check the
			// daemon warns about at load time.
			if fi.Mode().Perm()&0o055 != 0 {
				out = append(out, finding{warn: true, msg: fmt.Sprintf(
					"%s is readable or executable by group/other (mode %04o) — anyone on the box can "+
						"run it directly, bypassing the contract; chmod 700 it on a shared host", cand, fi.Mode().Perm())})
			}
			break
		}
	}
	if runPath == "" {
		out = append(out, finding{msg: "no executable run script (run.sh / run.py / run) found"})
		return out
	}

	// Run-script pin, if present, must match the file on disk now.
	if pin := strings.ToLower(strings.TrimSpace(c.Integrity.RunSHA256)); pin != "" {
		sum, err := tcs.HashFile(runPath)
		if err != nil {
			out = append(out, finding{msg: fmt.Sprintf("cannot hash run script to check its pin: %v", err)})
		} else if sum != pin {
			out = append(out, finding{msg: fmt.Sprintf(
				"integrity.run_sha256 does not match the run script on disk\n       pinned:  %s\n       on disk: %s", pin, sum)})
		}
	}

	// Interpreter pin, if present, must match the interpreter the shebang
	// resolves to on the sandbox PATH — resolved via the same shared code the
	// desk uses, so the linter and the runtime check agree. This is the stale
	// pin (a package upgrade moved the interpreter) tcs-verify exists to catch
	// in CI before deploy.
	if pin := strings.ToLower(strings.TrimSpace(c.Integrity.InterpreterSHA256)); pin != "" {
		name, ipath, ok := tcs.ResolveInterpreter(runPath, tcs.SandboxPath)
		switch {
		case !ok:
			out = append(out, finding{msg: fmt.Sprintf(
				"integrity.interpreter_sha256 is set but the interpreter (%s) could not be resolved "+
					"on the sandbox PATH to hash", orNone(name))})
		default:
			sum, err := tcs.HashFile(ipath)
			if err != nil {
				out = append(out, finding{msg: fmt.Sprintf("cannot hash interpreter %s to check its pin: %v", ipath, err)})
			} else if sum != pin {
				out = append(out, finding{msg: fmt.Sprintf(
					"integrity.interpreter_sha256 does not match %s (%s) on disk — often a package upgrade; re-pin with agent-desk -pin\n       pinned:  %s\n       on disk: %s",
					name, ipath, pin, sum)})
			}
		}
	}
	return out
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "none — no shebang"
	}
	return s
}

// trimDollar rewrites the leading "$" the schema linter uses for the document
// root into a readable "input"/"output.schema" prefix set by the caller.
func trimDollar(e string) string {
	return strings.TrimPrefix(e, "$")
}
