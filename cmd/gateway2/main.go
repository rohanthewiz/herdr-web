//go:build ghostty

// Command gateway2 serves the browser front-end: it speaks the browser protocol
// (internal/browserproto, spec ai_docs/phase-c-ws9-protocol.md) over WebSocket
// and sources pane content from the termhost daemon over the β orchestration
// seam. State is owned by the WS2 orchestrator (see gateway.go) — a single-owner
// event loop over an app.Session that starts with one workspace, one tab, and one
// pane; splits, tabs, and workspaces are created at runtime via the §7 command
// table (WS8). Structured key/mouse/paste input is encoded server-side via
// internal/inputenc (D4).
//
// Access control (WS10): a shared password gates the UI. Browsers sign in at
// /login and receive an HMAC-signed session cookie; headless clients present
// the password as an Authorization: Bearer token. --tls serves HTTPS with an
// auto-generated self-signed certificate (override with --tls-cert/--tls-key).
//
// Build (same prerequisite as cmd/termhost — prebuilt libghostty-vt, no Zig):
//
//	PKG_CONFIG_PATH=$HERDR/vendor/libghostty-vt/zig-out/share/pkgconfig \
//	  go build -tags ghostty ./cmd/gateway2
//
// Run a persistent daemon first:
//
//	termhost -socket /tmp/herdr-termhost.sock -persistent
//
// A local control API (WS4) exposes the same §7 command table over a unix socket
// for CLI/automation clients (see cmd/herdrctl, internal/ctlproto). It reuses the
// browser's app.Dispatcher unchanged; the socket is owner-only (0600).
//
// Usage:
//
//	gateway2 [--addr :8421] [--socket /tmp/herdr-termhost.sock] \
//	         [--control-socket /tmp/herdr-control.sock] \
//	         [--auth password|none] [--password SECRET] [--session-ttl 24h] \
//	         [--tls] [--tls-cert cert.pem] [--tls-key key.pem] \
//	         [--persist=false] [--state-dir DIR]
//
// Session persistence (WS3) is on by default: the workspace/tab/pane model is
// saved to $XDG_STATE_HOME/herdr (default ~/.local/state/herdr) on every
// mutation and restored at startup — surviving PTYs are re-adopted from the
// persistent daemon, dead ones re-spawned with their captured scrollback
// replayed.
package main

import (
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/rohanthewiz/rweb"

	"github.com/rohanthewiz/herdr-web/internal/app"
	"github.com/rohanthewiz/herdr-web/internal/config"
	"github.com/rohanthewiz/herdr-web/internal/ctlproto"
	"github.com/rohanthewiz/herdr-web/internal/gwauth"
	"github.com/rohanthewiz/herdr-web/internal/gwtls"
	"github.com/rohanthewiz/herdr-web/internal/persist"
)

//go:embed web/index.html
var webFS embed.FS

func main() {
	configPath := flag.String("config", "",
		"config file path (env "+config.EnvVar+"; default ~/.config/herdr/config.yaml)")
	addr := flag.String("addr", ":8421", "listen address")
	socket := flag.String("socket", "/tmp/herdr-termhost.sock", "termhost daemon socket path")
	controlSocket := flag.String("control-socket", "",
		"local control-API socket path (env "+ctlproto.SocketEnvVar+"; default "+ctlproto.DefaultSocket+")")
	authMode := flag.String("auth", "password", `auth mode: "password" (login + session cookie) or "none"`)
	password := flag.String("password", "", "shared access password/token (env HERDR_PASSWORD; generated if unset)")
	sessionTTL := flag.Duration("session-ttl", 24*time.Hour, "session cookie lifetime")
	useTLS := flag.Bool("tls", false, "serve HTTPS (auto self-signed cert unless --tls-cert/--tls-key given)")
	tlsCert := flag.String("tls-cert", "", "TLS certificate PEM (implies --tls)")
	tlsKey := flag.String("tls-key", "", "TLS private key PEM (implies --tls)")
	persistOn := flag.Bool("persist", true, "persist and restore session state (WS3)")
	stateDir := flag.String("state-dir", "", "session state directory (default $XDG_STATE_HOME/herdr)")
	flag.Parse()

	// Config precedence for server settings is flag > config file > default.
	// Start from the config (which starts from the defaults), then let any flag
	// the operator actually passed win. flag.Visit reports only explicitly-set
	// flags, so an unset flag never masks a config value with its default.
	cfg, cfgPath, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("gateway2: %v", err)
	}
	effTTL, err := cfg.Server.TTL()
	if err != nil {
		log.Fatalf("gateway2: %v", err) // Load already validated, but be explicit
	}
	eff := cfg.Server
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["addr"] {
		eff.Addr = *addr
	}
	if set["socket"] {
		eff.TermhostSocket = *socket
	}
	if set["control-socket"] {
		eff.ControlSocket = *controlSocket
	}
	if set["auth"] {
		eff.Auth = *authMode
	}
	if set["session-ttl"] {
		effTTL = *sessionTTL
	}
	if set["tls"] {
		eff.TLS.Enabled = *useTLS
	}
	if set["tls-cert"] {
		eff.TLS.Cert = *tlsCert
	}
	if set["tls-key"] {
		eff.TLS.Key = *tlsKey
	}
	effPersist := cfg.Persistence
	if set["persist"] {
		effPersist.Enabled = *persistOn
	}
	if set["state-dir"] {
		effPersist.StateDir = *stateDir
	}

	indexHTML, err := webFS.ReadFile("web/index.html")
	if err != nil {
		log.Fatalf("gateway2: read embedded page: %v", err)
	}

	cwd, _ := os.Getwd()
	o, err := buildOrch(eff.TermhostSocket, cwd, effPersist)
	if err != nil {
		log.Fatalf("gateway2: %v", err)
	}
	// Wire the config-driven served page: baseHTML + cfgPath let server.reload_config
	// re-render it; the initial render is stored for the "/" handler to serve.
	o.baseHTML = indexHTML
	o.cfgPath = cfgPath
	initialPage := renderPage(indexHTML, cfg)
	o.page.Store(&initialPage)
	if cfgPath != "" {
		log.Printf("gateway2: config %s", cfgPath)
	}

	// Local control API (WS4): a CLI/automation client drives the same §7 command
	// table as the browser over a unix socket. Listen failure is non-fatal — the
	// browser front-end works without it. cleanup unlinks the socket on stop.
	controlCleanup, err := serveControl(o, ctlproto.ResolveSocket(eff.ControlSocket))
	if err != nil {
		log.Printf("gateway2: control API disabled: %v", err)
		controlCleanup = func() {}
	}

	// Process-exit hook, fired by orch.Shutdown (server.stop command or a
	// SIGINT/SIGTERM) after the state save + final capture: rweb has no graceful
	// shutdown, so exit after a short grace period that lets the final
	// cmd_result + shutdown broadcast flush to browsers. The persistent termhost
	// daemon is separate and survives.
	o.stop = func() {
		log.Printf("gateway2: shutting down — session state saved; termhost daemon survives")
		controlCleanup()
		time.AfterFunc(250*time.Millisecond, func() { os.Exit(0) })
	}

	// A clean quit (Ctrl-C / SIGTERM) routes through the same graceful path as
	// server.stop: save the model, run the bounded final scrollback capture,
	// then exit. A second signal force-quits.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigc
		o.post(func() { o.Shutdown() })
		<-sigc
		os.Exit(1)
	}()

	go o.run()        // the orchestrator event loop (sole state owner)
	go o.daemon.run() // dial the termhost daemon
	if o.historyPath != "" {
		go o.runHistoryCapture() // periodic scrollback sweep for cold-restore seeds
	}

	// TLS: operator PEMs, or an auto-generated self-signed pair.
	tlsOn := eff.TLS.Enabled || eff.TLS.Cert != "" || eff.TLS.Key != ""
	var tlsCfg rweb.TLSCfg
	if tlsOn {
		certPath, keyPath, err := resolveTLS(eff.TLS.Cert, eff.TLS.Key)
		if err != nil {
			log.Fatalf("gateway2: tls: %v", err)
		}
		tlsCfg = rweb.TLSCfg{UseTLS: true, TLSAddr: eff.Addr, CertFile: certPath, KeyFile: keyPath}
	}

	// Auth: build the guard unless explicitly disabled.
	guard, err := buildGuard(eff.Auth, *password, effTTL, tlsOn)
	if err != nil {
		log.Fatalf("gateway2: auth: %v", err)
	}

	s := rweb.NewServer(rweb.ServerOptions{Address: eff.Addr, TLS: tlsCfg, Verbose: true})
	if guard != nil {
		s.Use(guard.middleware)
		s.Get("/login", guard.handleLoginGet)
		s.Post("/login", guard.handleLoginPost)
	}
	s.Get("/", func(ctx rweb.Context) error {
		return ctx.WriteHTML(string(*o.page.Load()))
	})
	s.WebSocket("/ws", func(ws *rweb.WSConn) error {
		return o.serve(ws)
	})

	scheme := "http"
	if tlsOn {
		scheme = "https"
	}
	log.Printf("gateway2: serving at %s://localhost%s (termhost socket %s)", scheme, eff.Addr, eff.TermhostSocket)
	if err := s.Run(); err != nil {
		log.Fatalf("gateway2: %v", err)
	}
	// rweb installs its own SIGINT/SIGTERM handler and returns nil from Run on a
	// signal. The signal goroutine above got the same signal and is driving the
	// graceful shutdown (save + final capture → os.Exit); block until it does.
	select {}
}

// buildOrch constructs the orchestrator, restoring the persisted session when
// persistence is on and a usable snapshot exists (WS3). Any load/restore
// problem beyond "no file yet" is logged and falls back to a fresh session —
// never a dead gateway. Scrollback seeds and saved cwds are installed only
// when the model itself restored: against a fresh session their pane ids would
// collide with newly allocated ones.
func buildOrch(socket, cwd string, pc config.Persistence) (*orch, error) {
	if !pc.Enabled {
		return newOrch(socket, cwd)
	}
	dir := pc.StateDir
	if dir == "" {
		dir = persist.DefaultDir()
	}
	if dir == "" {
		log.Printf("gateway2: persistence disabled — no resolvable state dir")
		return newOrch(socket, cwd)
	}
	sessionPath, historyPath := persist.SessionPath(dir), persist.HistoryPath(dir)

	var sess *app.Session
	var savedCwds map[uint32]string
	snap, cwds, err := persist.LoadSession(sessionPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// first run — start fresh, silently
	case err != nil:
		log.Printf("gateway2: session state unusable, starting fresh: %v", err)
	default:
		if sess, err = app.RestoreSession(modelSpawner{}, snap); err != nil {
			log.Printf("gateway2: session restore failed, starting fresh: %v", err)
			sess = nil
		} else {
			savedCwds = cwds
			total := len(snap.Workspaces)
			log.Printf("gateway2: restored session from %s (%d workspaces, %d panes)",
				sessionPath, total, len(sess.AllPaneIDs()))
		}
	}
	if sess == nil {
		o, err := newOrch(socket, cwd)
		if err != nil {
			return nil, err
		}
		o.sessionPath, o.historyPath = sessionPath, historyPath
		o.histLines = uint32(pc.HistoryLines)
		return o, nil
	}

	o := newOrchWith(socket, cwd, sess)
	o.sessionPath, o.historyPath = sessionPath, historyPath
	o.histLines = uint32(pc.HistoryLines)
	o.restoredCwds = savedCwds
	if seeds, err := persist.LoadHistory(historyPath); err == nil {
		o.seeds = seeds
		o.capturedHist = maps.Clone(seeds) // partial sweeps must not wipe other panes' seeds
	} else if !errors.Is(err, fs.ErrNotExist) {
		log.Printf("gateway2: history state unusable, skipping scrollback seeds: %v", err)
	}
	return o, nil
}

// buildGuard constructs the auth guard for the chosen mode. "none" returns a
// nil guard (no middleware). "password" resolves the shared secret (flag → env
// → generated) and logs a generated one so the operator can find it.
func buildGuard(mode, password string, ttl time.Duration, tlsOn bool) (*authGuard, error) {
	switch mode {
	case "none":
		log.Printf("gateway2: WARNING auth disabled (--auth none) — anyone who can reach the listen address can drive your terminals")
		return nil, nil
	case "password":
		secret, generated, err := resolveSecret(password)
		if err != nil {
			return nil, err
		}
		a, err := gwauth.New(secret, ttl)
		if err != nil {
			return nil, err
		}
		if generated {
			log.Printf("gateway2: no --password/HERDR_PASSWORD set; generated access password: %s", secret)
		}
		return &authGuard{a: a, secure: tlsOn}, nil
	default:
		return nil, fmt.Errorf("unknown --auth %q (want password|none)", mode)
	}
}

// resolveTLS returns the cert/key PEM paths to serve: the operator's files if
// both are given, otherwise an auto-generated self-signed pair cached under the
// user config dir (~/.config/herdr or the platform equivalent).
func resolveTLS(certFlag, keyFlag string) (certPath, keyPath string, err error) {
	if certFlag != "" && keyFlag != "" {
		return certFlag, keyFlag, nil
	}
	if certFlag != "" || keyFlag != "" {
		return "", "", fmt.Errorf("--tls-cert and --tls-key must be given together")
	}
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		return "", "", fmt.Errorf("locate config dir: %w", err)
	}
	dir := filepath.Join(cfgDir, "herdr")
	certPath, keyPath, err = gwtls.EnsureSelfSigned(dir)
	if err != nil {
		return "", "", err
	}
	log.Printf("gateway2: using self-signed TLS certificate in %s (browsers warn on first connect)", dir)
	return certPath, keyPath, nil
}
