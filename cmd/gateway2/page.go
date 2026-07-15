package main

import (
	"encoding/json"
	"strings"

	"github.com/rohanthewiz/herdr-web/internal/config"
)

// renderPage bakes the config's front-end settings into the served HTML: a
// <style> that overrides the :root CSS custom properties (theme colours + font)
// and a <script> that publishes the copy-mode keybindings as window.__herdrKeys
// for index.html to consult. Both are injected just before </head> so they win
// the cascade / load before the app script runs. The base page keeps working
// with no injection (its stylesheet has fallback values and the JS has a default
// binding table), so this only ever narrows behaviour toward the config.
//
// The config file is operator-controlled and local, but we still sanitise the
// CSS pieces so a stray value can't break out of <style> into markup; the
// keybindings ride through json.Marshal, whose default HTML escaping keeps a
// "</script>" in a value inert.
func renderPage(base []byte, cfg config.Config) []byte {
	inject := themeStyle(cfg.Theme) + keybindingsScript(cfg.Keybindings)
	html := string(base)
	if i := strings.LastIndex(html, "</head>"); i >= 0 {
		return []byte(html[:i] + inject + html[i:])
	}
	// No </head> (unexpected) — prepend so the settings still take effect.
	return []byte(inject + html)
}

// themeStyle renders the theme as a :root override plus a font rule.
func themeStyle(t config.Theme) string {
	var b strings.Builder
	b.WriteString("<style id=\"herdr-config-theme\">:root{")
	for name, value := range t.Colors {
		n := sanitizeCSSName(name)
		v := sanitizeCSSValue(value)
		if n == "" || v == "" {
			continue
		}
		b.WriteString("--")
		b.WriteString(n)
		b.WriteByte(':')
		b.WriteString(v)
		b.WriteByte(';')
	}
	b.WriteString("}")
	if font := sanitizeCSSValue(t.Font); font != "" {
		b.WriteString("html,body{font-family:")
		b.WriteString(font)
		b.WriteString(";}")
	}
	b.WriteString("</style>\n")
	return b.String()
}

// keybindingsScript publishes the copy-mode bindings for the front-end.
func keybindingsScript(k config.Keybindings) string {
	// json.Marshal escapes <, >, & (SetEscapeHTML default), so a value can't
	// close the <script>. A marshal failure (impossible for a string map) drops
	// the block, and index.html falls back to its built-in table.
	data, err := json.Marshal(map[string]any{"copyMode": k.CopyMode})
	if err != nil {
		return ""
	}
	return "<script id=\"herdr-config-keys\">window.__herdrKeys=" + string(data) + ";</script>\n"
}

// sanitizeCSSName keeps a CSS custom-property name to [A-Za-z0-9-] so it stays a
// valid identifier and can't carry markup.
func sanitizeCSSName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			return r
		default:
			return -1
		}
	}, s)
}

// sanitizeCSSValue drops characters that could end the declaration, the rule, or
// the <style> element, leaving an ordinary colour/font value.
func sanitizeCSSValue(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', '{', '}', ';', '\n', '\r':
			return -1
		default:
			return r
		}
	}, s)
	return strings.TrimSpace(s)
}
