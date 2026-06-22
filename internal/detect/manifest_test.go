package detect

import "testing"

func TestManifestsCompile(t *testing.T) {
	manifestsOnce.Do(loadManifests)
	for _, label := range []string{"claude", "codex", "pi", "copilot", "agy", "gemini"} {
		if manifests[label] == nil {
			t.Errorf("manifest for %q did not load", label)
		}
	}
}

func TestDetectClaude(t *testing.T) {
	cases := []struct {
		name      string
		in        Input
		wantState State
		wantIdle  bool
		wantBlock bool
		wantWork  bool
	}{
		{
			name:      "osc_title_working_braille",
			in:        Input{OscTitle: "⠁ building the thing"},
			wantState: StateWorking,
			wantWork:  true,
		},
		{
			name:      "osc_title_idle",
			in:        Input{OscTitle: "✳ ready"},
			wantState: StateIdle,
			wantIdle:  true,
		},
		{
			name:      "live_prompt_box_idle",
			in:        Input{Screen: "──────────\n❯ ask me something\n──────────"},
			wantState: StateIdle,
			wantIdle:  true,
		},
		{
			name: "bash_permission_blocked",
			in: Input{Screen: "Do you want to proceed?\n" +
				"bash command\n" +
				"1. Yes\n" +
				"2. No"},
			wantState: StateBlocked,
			wantBlock: true,
		},
		{
			name:      "no_match_falls_back_idle",
			in:        Input{Screen: "just some plain output\nnothing special here"},
			wantState: StateIdle,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Detect("claude", tc.in)
			if got.State != tc.wantState {
				t.Errorf("state = %q, want %q", got.State, tc.wantState)
			}
			if got.VisibleIdle != tc.wantIdle || got.VisibleBlocker != tc.wantBlock || got.VisibleWorking != tc.wantWork {
				t.Errorf("visible flags = (idle=%v block=%v work=%v), want (idle=%v block=%v work=%v)",
					got.VisibleIdle, got.VisibleBlocker, got.VisibleWorking, tc.wantIdle, tc.wantBlock, tc.wantWork)
			}
		})
	}
}

func TestDetectPiWorking(t *testing.T) {
	got := Detect("pi", Input{Screen: "Working..."})
	if got.State != StateWorking || !got.VisibleWorking {
		t.Errorf("pi working = %+v, want working+visible", got)
	}
}

func TestDetectUnknownAndFallback(t *testing.T) {
	if got := Detect("", Input{Screen: "anything"}); got.State != StateUnknown {
		t.Errorf("empty label = %q, want unknown", got.State)
	}
	// Known agent, empty screen, no rule → idle fallback.
	if got := Detect("codex", Input{}); got.State != StateIdle {
		t.Errorf("codex fallback = %q, want idle", got.State)
	}
}
