package ctlproto

import (
	"bufio"
	"encoding/json"
	"log"
	"net"
	"sync"
	"time"

	"github.com/rohanthewiz/herdr-web/internal/app"
)

// Dispatch runs one §7 command from the control API. An implementation decodes
// params via app.JSONParamDecoder and drives the neutral app.Dispatcher,
// resolving r with the command's result. For gateway2 this posts onto the
// orchestrator loop so the dispatch runs on the single state-owning goroutine
// (which also implements app.Backend); synchronous commands resolve r inline,
// while read/capture resolve it later when the daemon reply arrives.
type Dispatch func(method string, params json.RawMessage, r app.Responder)

// Server accepts control-API connections on a local socket and answers each with
// one dispatched command result. It holds no session state — that lives behind
// the Dispatch func — so the same Server serves any app.Backend.
type Server struct {
	dispatch Dispatch
	// stream handles the streaming method (events.subscribe); nil until
	// SetStreamDispatch is called, in which case a subscribe request is rejected.
	stream StreamDispatch
	// timeout bounds how long a connection waits for the dispatch to resolve its
	// responder — a backstop above the orchestrator's own read/capture timeout so
	// a wedged command can't pin a connection (and its goroutine) forever.
	timeout time.Duration
	svc     string
}

// NewServer builds a control server over dispatch. timeout is the per-request
// backstop (use a value above the orchestrator's read/capture timeout); svc names
// the service in ping responses.
func NewServer(dispatch Dispatch, timeout time.Duration, svc string) *Server {
	return &Server{dispatch: dispatch, timeout: timeout, svc: svc}
}

// Serve accepts connections on l until Accept errors (e.g. l is closed) and
// handles each in its own goroutine. It blocks; run it in a goroutine.
func (s *Server) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		go s.handleConn(conn)
	}
}

// handleConn answers one request on conn. A unary request gets one Response and
// the connection closes; a subscribe request keeps the connection open and streams
// events (handleStream) until the client disconnects.
func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	req, err := readRequest(br)
	if err != nil {
		return // unreadable/closed before a full request — nothing to answer
	}
	if req.Method == MethodEventsSubscribe {
		s.handleStream(conn, br, req)
		return
	}
	if err := writeMessage(conn, s.handle(req)); err != nil {
		log.Printf("ctlproto: write response: %v", err)
	}
}

// handle turns one Request into its Response: ping is answered directly; every
// other method is dispatched through the app command table and awaited.
func (s *Server) handle(req Request) Response {
	if req.Method == MethodPing {
		return newResponse(req.ID, true, "", Pong{Protocol: ProtocolVersion, Service: s.svc})
	}
	return s.dispatchAndWait(req)
}

// awaitBackstop is the connection backstop for pane.wait_for_output, whose own
// timeout can run up to app.MaxWaitTimeout. It sits above that ceiling so the
// backend's waiter always resolves on its own timer first; this only trips if the
// dispatch itself wedges (a bug), keeping the goroutine bounded.
const awaitBackstop = app.MaxWaitTimeout + 15*time.Second

// dispatchAndWait runs the command through the dispatch func and blocks for its
// result. The responder delivers OK/Fail onto a buffered channel, so a command
// that resolves synchronously (most) or asynchronously (read/capture/wait_for_output,
// resolved later on the loop goroutine) both land here; the backstop bounds a
// wedged dispatch — a generous one for wait_for_output, which is meant to block.
func (s *Server) dispatchAndWait(req Request) Response {
	backstop := s.timeout
	if req.Method == app.CmdWaitForOutput {
		backstop = awaitBackstop
	}
	cr := &chanResponder{ch: make(chan Response, 1), id: req.ID}
	s.dispatch(req.Method, req.Params, cr)
	select {
	case resp := <-cr.ch:
		return resp
	case <-time.After(backstop):
		return newResponse(req.ID, false, "command timed out", nil)
	}
}

// chanResponder is the app.Responder for a control request: it converts the
// dispatcher's OK/Fail into a Response and delivers it once onto ch. A control
// caller always wants a reply. OK/Fail may run on a different goroutine than the
// waiter (async read/capture) and, defensively, at most once — a late resolve
// after a timeout is dropped rather than blocking or panicking.
type chanResponder struct {
	ch   chan Response
	id   string
	once sync.Once
}

func (r *chanResponder) WantsReply() bool { return true }

func (r *chanResponder) OK(data any) {
	r.once.Do(func() { r.ch <- newResponse(r.id, true, "", data) })
}

func (r *chanResponder) Fail(errMsg string) {
	r.once.Do(func() { r.ch <- newResponse(r.id, false, errMsg, nil) })
}
