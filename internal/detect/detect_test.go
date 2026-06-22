package detect

import "testing"

func TestIdentifyAgent(t *testing.T) {
	cases := map[string]string{
		"claude":               "claude",
		"/usr/local/bin/codex": "codex",
		"CLAUDE-CODE":          "claude",
		"cursor-agent":         "cursor",
		"antigravity":          "agy",
		"node":                 "",
		"bash":                 "",
		"zsh":                  "",
		"":                     "",
		"claude.js":            "claude",
	}
	for in, want := range cases {
		if got := IdentifyAgent(in); got != want {
			t.Errorf("IdentifyAgent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIdentifyFirst(t *testing.T) {
	// node wrapper: comm/path don't match, argv[1] does.
	if got := IdentifyFirst("node", "/usr/local/bin/node", "claude"); got != "claude" {
		t.Errorf("IdentifyFirst(node-wrapped) = %q, want claude", got)
	}
	if got := IdentifyFirst("bash", "sh", "zsh"); got != "" {
		t.Errorf("IdentifyFirst(shells) = %q, want empty", got)
	}
}
