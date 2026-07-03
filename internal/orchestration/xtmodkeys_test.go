package orchestration

import "testing"

func TestXtmodkeysScanner(t *testing.T) {
	cases := []struct {
		name  string
		feeds []string
		want  bool
	}{
		{"enable level 1", []string{"\x1b[>4;1m"}, true},
		{"enable level 2", []string{"\x1b[>4;2m"}, true},
		{"explicit zero disables", []string{"\x1b[>4;2m", "\x1b[>4;0m"}, false},
		{"bare reset disables", []string{"\x1b[>4;2m", "\x1b[>4m"}, false},
		{"split across reads", []string{"text\x1b[>", "4;", "2m more"}, true},
		{"other resource ignored", []string{"\x1b[>0m\x1b[>1;5m"}, false},
		{"plain csi ignored", []string{"\x1b[4;2m\x1b[2J"}, false},
		{"kitty u sequence ignored", []string{"\x1b[>5u"}, false},
		{"mixed stream", []string{"a\x1b[31mred\x1b[>4;2mb\r\n"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s xtmodkeysScanner
			var enabled bool
			for _, chunk := range tc.feeds {
				enabled, _ = s.scan([]byte(chunk))
			}
			if enabled != tc.want {
				t.Fatalf("enabled = %v, want %v", enabled, tc.want)
			}
		})
	}
}

func TestXtmodkeysScannerReportsChange(t *testing.T) {
	var s xtmodkeysScanner
	if _, changed := s.scan([]byte("plain output")); changed {
		t.Fatal("no sequence should report no change")
	}
	if _, changed := s.scan([]byte("\x1b[>4;2m")); !changed {
		t.Fatal("enabling should report a change")
	}
	if _, changed := s.scan([]byte("\x1b[>4;1m")); changed {
		t.Fatal("still-enabled should not report a change")
	}
	if _, changed := s.scan([]byte("\x1b[>4m")); !changed {
		t.Fatal("reset should report a change")
	}
}
