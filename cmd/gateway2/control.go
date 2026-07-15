//go:build ghostty

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/rohanthewiz/herdr-web/internal/app"
	"github.com/rohanthewiz/herdr-web/internal/ctlproto"
)

// The local control API is the second front-end onto the §7 command table (the
// browser is the first): a CLI/automation client speaks internal/ctlproto over a
// unix socket, and each request runs through the very same app.Dispatcher as a
// browser cmd. orch already implements app.Backend, so the only new server-side
// code is the transport (ctlproto.Server) and the adapter below — no per-command
// logic is duplicated. The socket is owner-only (0600), which is the local trust
// boundary; there is no token (that gates the network-facing browser path).

// controlTimeout bounds a control-API request end-to-end. It sits above the
// orchestrator's read/capture round-trip timeout (reqTimeout) so a normal slow
// command resolves through its own path and only a genuinely wedged dispatch
// trips this backstop.
const controlTimeout = reqTimeout + 3*time.Second

// controlDispatch adapts a control-API request onto the orchestrator: it posts
// the dispatch onto the loop goroutine (the sole owner of the session, which also
// implements app.Backend) and hands the neutral app.Dispatcher the request's JSON
// params and the control responder. This mirrors handleCmd exactly — the browser
// path posts the same app.Dispatcher the same way — which is the whole point of
// the seam.
func (o *orch) controlDispatch(method string, params json.RawMessage, r app.Responder) {
	o.post(func() {
		app.NewDispatcher(o.session, o).Dispatch(method, app.JSONParamDecoder{Raw: params}, r)
	})
}

// ctlSubscriber is one control-API event stream (events.subscribe): a
// ctlproto.Subscriber sink onto the client connection plus the subscription's
// filter. controlStream registers it on the loop; emitEvent pushes matching pane
// events; a full/dead sink is dropped.
type ctlSubscriber struct {
	sub    ctlproto.Subscriber
	filter app.EventsSubscribeParams
}

// controlStream starts a streaming control method (events.subscribe): it decodes
// the optional filter, registers a subscriber on the orchestrator loop, and
// returns a cancel that removes it when the client disconnects. Registration and
// removal both post onto the loop (the sole owner of o.subs); the ctlproto server
// pumps the events the loop pushes onto the subscriber. This is the streaming
// analogue of controlDispatch.
func (o *orch) controlStream(method string, params json.RawMessage, sub ctlproto.Subscriber) (func(), error) {
	if method != ctlproto.MethodEventsSubscribe {
		return nil, fmt.Errorf("unknown streaming method %q", method)
	}
	var f app.EventsSubscribeParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &f); err != nil {
			return nil, fmt.Errorf("bad params: %v", err)
		}
	}
	s := &ctlSubscriber{sub: sub, filter: f}
	o.post(func() { o.subs[s] = struct{}{} })
	return func() { o.post(func() { delete(o.subs, s) }) }, nil
}

// serveControl opens the control socket and serves the local control API until
// the process exits. It removes a stale socket left by a crashed run, restricts
// the socket to the owner (0600), and returns a cleanup that closes the listener
// and unlinks the socket (wired into the server.stop hook). Listen failure is
// returned so main can log it and carry on — the browser front-end works without
// the control API.
func serveControl(o *orch, socket string) (cleanup func(), err error) {
	if socket == "" {
		return func() {}, nil
	}
	if isStaleSocket(socket) {
		_ = os.Remove(socket)
	}
	l, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		log.Printf("gateway2: control socket chmod: %v", err)
	}
	srv := ctlproto.NewServer(o.controlDispatch, controlTimeout, "gateway2")
	srv.SetStreamDispatch(o.controlStream) // events.subscribe
	go func() {
		if err := srv.Serve(l); err != nil {
			log.Printf("gateway2: control server stopped: %v", err)
		}
	}()
	log.Printf("gateway2: control API listening on %s", socket)
	return func() { _ = l.Close(); _ = os.Remove(socket) }, nil
}

// isStaleSocket reports whether socket is a leftover socket file with no live
// listener (a crashed prior run). A real (non-socket) file is left alone so we
// never clobber unrelated data; a socket we can still dial means a server is live
// and we must not steal the path.
func isStaleSocket(socket string) bool {
	info, err := os.Stat(socket)
	if err != nil {
		return false // nothing there
	}
	if info.Mode()&os.ModeSocket == 0 {
		return false // a real file, not our socket
	}
	conn, err := net.DialTimeout("unix", socket, 200*time.Millisecond)
	if err != nil {
		return true // socket file exists but nobody is listening → stale
	}
	_ = conn.Close()
	return false
}
