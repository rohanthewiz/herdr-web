package main

import (
	"strings"
	"testing"

	"github.com/rohanthewiz/herdr-web/internal/config"
)

const baseHead = "<!DOCTYPE html><html><head><title>x</title></head><body>hi</body></html>"

// renderPage injects the theme style and keybindings script, both before
// </head>, and preserves the rest of the document.
func TestRenderPageInjects(t *testing.T) {
	out := string(renderPage([]byte(baseHead), config.Default()))

	for _, want := range []string{
		`<style id="herdr-config-theme">`,
		`--bg:#181818;`,
		`window.__herdrKeys=`,
		`"copyMode"`,
		"<body>hi</body>", // original body intact
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered page missing %q", want)
		}
	}
	// Both injections land before </head>.
	head := strings.Index(out, "</head>")
	if head < 0 {
		t.Fatal("no </head> in output")
	}
	if strings.Index(out, "herdr-config-theme") > head || strings.Index(out, "__herdrKeys") > head {
		t.Fatal("injections must precede </head>")
	}
}

// A rebound keybinding from config reaches the injected script.
func TestRenderPageReboundKey(t *testing.T) {
	cfg := config.Default()
	cfg.Keybindings.CopyMode["yank"] = []string{"c"}
	out := string(renderPage([]byte(baseHead), cfg))
	if !strings.Contains(out, `"yank":["c"]`) {
		t.Errorf("rebound yank not in injected keys:\n%s", out)
	}
}

// CSS values that try to break out of <style> into markup are stripped.
func TestRenderPageSanitizesTheme(t *testing.T) {
	cfg := config.Default()
	cfg.Theme.Colors["bg"] = "#000</style><script>alert(1)</script>"
	cfg.Theme.Colors["ev;il"] = "red}body{display:none"
	out := string(renderPage([]byte(baseHead), cfg))

	if strings.Contains(out, "<script>alert(1)") {
		t.Error("theme value broke out of <style>")
	}
	if strings.Contains(out, "}body{display:none") {
		t.Error("theme value injected an extra CSS rule")
	}
}

// A keybinding key value containing </script> can't close the injected script —
// json.Marshal escapes it.
func TestRenderPageEscapesKeys(t *testing.T) {
	cfg := config.Default()
	cfg.Keybindings.CopyMode["yank"] = []string{"</script><script>alert(1)</script>"}
	out := string(renderPage([]byte(baseHead), cfg))
	if strings.Contains(out, "</script><script>alert(1)") {
		t.Error("keybinding value broke out of <script>")
	}
}

// With no </head>, the injection still lands (prepended) so settings take effect.
func TestRenderPageNoHead(t *testing.T) {
	out := string(renderPage([]byte("<body>only</body>"), config.Default()))
	if !strings.Contains(out, "herdr-config-theme") || !strings.Contains(out, "<body>only</body>") {
		t.Fatalf("no-head render dropped content:\n%s", out)
	}
}
