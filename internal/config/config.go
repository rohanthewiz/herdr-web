// Package config is gateway2's optional YAML configuration file. It is a second
// source of settings alongside the command-line flags: for the server settings
// the precedence is flag > config > built-in default (main.go applies the flag
// layer via flag.Visit); the front-end settings (theme colours and copy-mode
// keybindings) have no flags and come from the config alone, baked into the
// served page.
//
// A missing file is not an error — every field has a default (Default), so an
// empty or absent config yields the same behaviour gateway2 had before configs
// existed. Absent scalar keys keep their defaults; the theme and keybinding maps
// merge key-wise, so a config that sets one colour or rebinds one action keeps
// the defaults for everything it does not mention.
//
// House style (matching the rest of the repo): stdlib errors/fmt and prefixed
// log messages, no serr.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"time"

	"github.com/goccy/go-yaml"
)

// EnvVar overrides the config file path (after an explicit --config flag, before
// the default location).
const EnvVar = "HERDR_CONFIG"

// Config is the whole gateway2 configuration file.
type Config struct {
	Server      Server      `yaml:"server"`
	Persistence Persistence `yaml:"persistence"`
	Theme       Theme       `yaml:"theme"`
	Keybindings Keybindings `yaml:"keybindings"`
}

// Server mirrors the network/auth flags. Password is deliberately absent — a
// shared secret belongs in the environment (HERDR_PASSWORD) or a flag, never a
// config file that is easy to commit by accident.
type Server struct {
	Addr           string `yaml:"addr"`
	TermhostSocket string `yaml:"termhost_socket"`
	ControlSocket  string `yaml:"control_socket"` // "" ⇒ ctlproto resolves env/default
	Auth           string `yaml:"auth"`           // "password" | "none"
	SessionTTL     string `yaml:"session_ttl"`    // a Go duration string, e.g. "24h"
	TLS            TLS    `yaml:"tls"`
}

// Persistence is session persistence & restore (WS3): the model snapshot that
// survives a gateway restart and the scrollback seeds that survive a termhost
// daemon loss.
type Persistence struct {
	// Enabled turns persistence on (the default): the session model is saved on
	// every mutation and restored at startup.
	Enabled bool `yaml:"enabled"`
	// StateDir overrides where session.json/history.json live ("" ⇒
	// $XDG_STATE_HOME/herdr, falling back to ~/.local/state/herdr).
	StateDir string `yaml:"state_dir"`
	// HistoryLines bounds the scrollback captured per pane for cold-restore
	// seeds (0 = the whole buffer).
	HistoryLines int `yaml:"history_lines"`
}

// TLS is the HTTPS configuration. Enabled alone uses an auto self-signed cert;
// Cert+Key provide operator PEMs (and imply Enabled).
type TLS struct {
	Enabled bool   `yaml:"enabled"`
	Cert    string `yaml:"cert"`
	Key     string `yaml:"key"`
}

// Theme is the front-end appearance: CSS custom-property overrides (without the
// leading "--") and an optional font stack, injected into the served page.
type Theme struct {
	Colors map[string]string `yaml:"colors"`
	Font   string            `yaml:"font"`
}

// Keybindings maps a front-end action to the keyboard keys that trigger it. Only
// copy-mode is configurable today; keys are DOM KeyboardEvent.key values
// ("ArrowLeft", "h", "Escape", …).
type Keybindings struct {
	CopyMode map[string][]string `yaml:"copy_mode"`
}

// TTL parses SessionTTL into a duration.
func (s Server) TTL() (time.Duration, error) {
	d, err := time.ParseDuration(s.SessionTTL)
	if err != nil {
		return 0, fmt.Errorf("session_ttl %q: %w", s.SessionTTL, err)
	}
	return d, nil
}

// --- defaults ----------------------------------------------------------------

// defaultColors are the served page's :root CSS custom properties (index.html).
// Keep in sync with the stylesheet's fallback values.
var defaultColors = map[string]string{
	"bg": "#181818", "fg": "#d4d4d4", "accent": "#5b9dff", "panel": "#141414",
	"panel2": "#1d1d1d", "line": "#2a2a2a", "muted": "#8a8a8a", "chrome": "#202430",
	"chrome-focus": "#26314a", "ok": "#6ac47a", "warn": "#e2b64e", "err": "#ff6b6b",
}

const defaultFont = `ui-monospace, "SF Mono", Menlo, Consolas, monospace`

// defaultCopyMode is the copy-mode action → keys table. Its keys are the full
// set of known copy-mode actions; Validate rejects any others. Keep in sync with
// copyModeKey in index.html.
var defaultCopyMode = map[string][]string{
	"move-left":  {"ArrowLeft", "h"},
	"move-right": {"ArrowRight", "l"},
	"move-up":    {"ArrowUp", "k"},
	"move-down":  {"ArrowDown", "j"},
	"line-start": {"0", "Home"},
	"line-end":   {"$", "End"},
	"top":        {"g"},
	"bottom":     {"G"},
	"select":     {"v"},
	"rect":       {"r"},
	"yank":       {"y", "Enter"},
	"exit":       {"Escape", "q"},
}

// Default is the configuration gateway2 uses with no config file. Every call
// returns fresh maps so callers can mutate the result without affecting the
// package globals or each other.
func Default() Config {
	return Config{
		Server: Server{
			Addr:           ":8421",
			TermhostSocket: "/tmp/herdr-termhost.sock",
			ControlSocket:  "",
			Auth:           "password",
			SessionTTL:     "24h",
		},
		Persistence: Persistence{Enabled: true, HistoryLines: 2000},
		Theme:       Theme{Colors: cloneStrMap(defaultColors), Font: defaultFont},
		Keybindings: Keybindings{CopyMode: cloneKeyMap(defaultCopyMode)},
	}
}

// --- loading -----------------------------------------------------------------

// Load resolves the config path (override flag > HERDR_CONFIG > default location)
// and returns the merged, validated configuration plus the path consulted. A
// missing file at the default location yields Default with no error; a missing
// file at an explicitly requested path (flag or env) is an error, since the user
// named a file that isn't there.
func Load(override string) (Config, string, error) {
	path, explicit := resolvePath(override)
	if path == "" {
		return Default(), "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if explicit {
				return Default(), path, fmt.Errorf("config file %s not found", path)
			}
			return Default(), path, nil
		}
		return Default(), path, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg, err := parse(data)
	if err != nil {
		return Default(), path, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, path, nil
}

// parse decodes YAML onto a defaults copy (so absent scalars keep their
// defaults) and merges the theme/keybinding maps key-wise (which unmarshal would
// otherwise replace wholesale), then validates.
func parse(data []byte) (Config, error) {
	cfg := Default()
	if len(bytes.TrimSpace(data)) == 0 {
		return cfg, nil // empty document ⇒ pure defaults (goccy would zero the struct)
	}
	defColors, defKeys := cfg.Theme.Colors, cfg.Keybindings.CopyMode
	cfg.Theme.Colors, cfg.Keybindings.CopyMode = nil, nil
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Default(), err
	}
	cfg.Theme.Colors = mergeStrMap(defColors, cfg.Theme.Colors)
	cfg.Keybindings.CopyMode = mergeKeyMap(defKeys, cfg.Keybindings.CopyMode)
	if err := cfg.Validate(); err != nil {
		return Default(), err
	}
	return cfg, nil
}

// Validate checks the enum/duration fields and the keybinding action names so a
// bad config fails loudly (at startup or on reload) instead of silently.
func (c Config) Validate() error {
	switch c.Server.Auth {
	case "password", "none":
	default:
		return fmt.Errorf("server.auth %q: want \"password\" or \"none\"", c.Server.Auth)
	}
	if _, err := c.Server.TTL(); err != nil {
		return fmt.Errorf("server.%w", err)
	}
	if (c.Server.TLS.Cert == "") != (c.Server.TLS.Key == "") {
		return errors.New("server.tls: cert and key must be set together")
	}
	if c.Persistence.HistoryLines < 0 {
		return fmt.Errorf("persistence.history_lines %d: must be >= 0", c.Persistence.HistoryLines)
	}
	for action, keys := range c.Keybindings.CopyMode {
		if _, ok := defaultCopyMode[action]; !ok {
			return fmt.Errorf("keybindings.copy_mode: unknown action %q", action)
		}
		if len(keys) == 0 {
			return fmt.Errorf("keybindings.copy_mode.%s: needs at least one key", action)
		}
	}
	return nil
}

// resolvePath picks the config path: an explicit override (flag) wins, then
// HERDR_CONFIG, then the default location. explicit reports whether the path came
// from the flag or env (so a missing file there is an error, not silent
// defaults).
func resolvePath(override string) (path string, explicit bool) {
	if override != "" {
		return override, true
	}
	if v := os.Getenv(EnvVar); v != "" {
		return v, true
	}
	return defaultPath(), false
}

// defaultPath is $XDG_CONFIG_HOME/herdr/config.yaml, falling back to
// ~/.config/herdr/config.yaml (the conventional location for a dev CLI tool, on
// macOS too). Returns "" if neither the env var nor a home dir is available.
func defaultPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "herdr", "config.yaml")
}

// --- map helpers -------------------------------------------------------------

func cloneStrMap(m map[string]string) map[string]string { return maps.Clone(m) }

func cloneKeyMap(m map[string][]string) map[string][]string {
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// mergeStrMap overlays over onto base (base is not mutated).
func mergeStrMap(base, over map[string]string) map[string]string {
	out := cloneStrMap(base)
	maps.Copy(out, over)
	return out
}

// mergeKeyMap overlays over onto base per action (a present action's key list
// replaces the default's; absent actions keep their defaults).
func mergeKeyMap(base, over map[string][]string) map[string][]string {
	out := cloneKeyMap(base)
	for k, v := range over {
		out[k] = append([]string(nil), v...)
	}
	return out
}
