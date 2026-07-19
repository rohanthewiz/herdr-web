//go:build ghostty

package main

import (
	"maps"
	"slices"

	"github.com/rohanthewiz/herdr-web/internal/app"
	"github.com/rohanthewiz/herdr-web/internal/config"
)

// The config commands (app.Backend seam, settings modal). Both run synchronously
// on the loop goroutine — the config file is small and local, like the session
// save. o.cfg holds the config-file state (defaults + file); server settings
// shown here are the file's values, not the flag-overridden effective ones, so
// config.set can marshal o.cfg back to disk without baking flags into the file.

// ConfigGet resolves the live configuration snapshot (config.get).
func (o *orch) ConfigGet(r app.Responder) {
	r.OK(o.configSnapshot())
}

// ConfigSet merges the live-appliable sections (theme colors/font, copy-mode
// keys) onto the current config, validates, writes the YAML (creating the
// default file when none was in use yet), and re-renders the served page so the
// next load carries the change. The issuing page applies the theme client-side
// from the values it just saved; other connected clients pick it up on reload.
func (o *orch) ConfigSet(r app.Responder, p app.ConfigSetParams) {
	cfg := o.cfg
	cfg.Theme.Colors = maps.Clone(cfg.Theme.Colors)
	cfg.Keybindings.CopyMode = maps.Clone(cfg.Keybindings.CopyMode)
	if p.Theme != nil {
		if len(p.Theme.Colors) > 0 && cfg.Theme.Colors == nil {
			cfg.Theme.Colors = map[string]string{}
		}
		maps.Copy(cfg.Theme.Colors, p.Theme.Colors)
		if p.Theme.Font != "" {
			cfg.Theme.Font = p.Theme.Font
		}
	}
	if len(p.CopyMode) > 0 {
		if cfg.Keybindings.CopyMode == nil {
			cfg.Keybindings.CopyMode = map[string][]string{}
		}
		for action, keys := range p.CopyMode {
			cfg.Keybindings.CopyMode[action] = slices.Clone(keys)
		}
	}
	if err := cfg.Validate(); err != nil {
		r.Fail(err.Error())
		return
	}

	path := o.cfgPath
	if path == "" {
		path = config.DefaultPath()
	}
	if path == "" {
		r.Fail("no resolvable config path")
		return
	}
	if err := config.Save(path, cfg); err != nil {
		r.Fail(err.Error())
		return
	}
	o.cfg = cfg
	o.cfgPath = path // a first save adopts the default path for future reloads
	if o.baseHTML != nil {
		page := renderPage(o.baseHTML, cfg)
		o.page.Store(&page)
	}
	r.OK(o.configSnapshot())
}

// configSnapshot builds the wire view of the current config. Maps are cloned so
// a marshalled reply can never alias the live config state.
func (o *orch) configSnapshot() app.ConfigGetResult {
	path := o.cfgPath
	if path == "" {
		path = config.DefaultPath()
	}
	c := o.cfg
	return app.ConfigGetResult{
		Path: path,
		Theme: app.ConfigTheme{
			Colors: maps.Clone(c.Theme.Colors),
			Font:   c.Theme.Font,
		},
		CopyMode: maps.Clone(c.Keybindings.CopyMode),
		Server: app.ConfigServerInfo{
			Addr:           c.Server.Addr,
			Auth:           c.Server.Auth,
			TLS:            c.Server.TLS.Enabled || c.Server.TLS.Cert != "",
			TermhostSocket: c.Server.TermhostSocket,
			ControlSocket:  c.Server.ControlSocket,
			HookSocket:     c.Server.HookSocket,
			SessionTTL:     c.Server.SessionTTL,
		},
	}
}
