package tcs

import "testing"

func TestValidateBindsRefusesWhatTheDeskMountsItself(t *testing.T) {
	for _, dst := range []string{
		"/deskbox",
		"/deskbox/tool",
		"/deskbox/tool/run.py", // would replace the script the integrity pin hashed
		"/deskbox/tool/sub/x",
		"/deskbox/in",
		"/deskbox/in/input.json",
		"/deskbox/out",
		"/deskbox/out/result.json",
	} {
		err := ValidateBinds([]BindMount{{Src: "/run/sbx/broker.sock", Dst: dst}})
		if err == nil {
			t.Errorf("dst %q was accepted", dst)
		}
	}
}

func TestValidateBindsAllowsOtherPlacesUnderTheRoot(t *testing.T) {
	for _, dst := range []string{
		"/deskbox/broker.sock",
		"/deskbox/toolbox/x", // shares a prefix with tool, not a path component
		"/deskbox/input",
		"/deskbox/outbox/x",
		"/deskbox/sock/broker.sock",
	} {
		if err := ValidateBinds([]BindMount{{Src: "/run/sbx/broker.sock", Dst: dst}}); err != nil {
			t.Errorf("dst %q was refused: %v", dst, err)
		}
	}
}
