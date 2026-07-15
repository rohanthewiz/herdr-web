// Package ctlproto is the local control-API wire protocol for driving a running
// herdr server from a CLI or automation client. It is the second front-end onto
// the protocol-neutral §7 command table in internal/app — the browser (WS9,
// internal/browserproto) is the first. A client sends a newline-framed JSON
// Request naming an app command; the server dispatches it through app.Dispatcher
// and replies with a single JSON Response.
//
// The default transport is a per-request round trip over a local (unix) socket:
// one Request in, one Response out, then the connection closes. Two methods layer
// onto that base without disturbing it:
//
//   - pane.wait_for_output is still one Request → one Response, only the response
//     is delayed until the pane's output matches (or the wait times out). It rides
//     the unchanged envelope; the server just grants it a longer backstop.
//   - events.subscribe (MethodEventsSubscribe) is the one streaming method: the
//     server writes an ack Response, then zero or more Event frames on the same
//     newline-framed connection until the client disconnects (see stream.go).
package ctlproto

import (
	"bufio"
	"encoding/json"
	"io"
)

// ProtocolVersion is bumped on any breaking change to the envelope shapes. It is
// independent of browserproto.ProtocolVersion and orchestration.ProtocolVersion.
const ProtocolVersion = 1

// MethodPing is a liveness/handshake check answered directly by the control
// server (no session mutation); its Response.Data is a Pong. Every other Method
// is an app §7 command name (app.Cmd*) routed through the dispatcher.
const MethodPing = "ping"

// MethodEventsSubscribe is the one streaming method: instead of a single Response
// the server sends an ack Response then a stream of Event frames (see stream.go).
// The server routes it to its StreamDispatch rather than the unary dispatcher, so
// it is not an app §7 command name and is absent from app.CommandNames().
const MethodEventsSubscribe = "events.subscribe"

// Request is one control command. Method is an app §7 command name (app.Cmd*) or
// MethodPing. Params is the command's params object, decoded by the dispatcher
// via app.JSONParamDecoder. ID is a client-chosen correlation string echoed back
// in the Response ("" is allowed).
type Request struct {
	ID     string          `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is the reply to a Request. OK reports success; on failure Error carries
// a human-readable message. Data is the command's result payload (e.g. an
// app.ReadResult for "read", a Pong for "ping"), absent when the command yields
// no data.
type Response struct {
	ID    string          `json:"id,omitempty"`
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Pong is the Response.Data for MethodPing: the server's protocol/identity so a
// client can confirm what it is talking to before issuing commands.
type Pong struct {
	Protocol int    `json:"protocol"`
	Service  string `json:"service"`
}

// Event is one server-pushed frame on a streaming connection (events.subscribe).
// After the subscribe ack Response, the server writes zero or more Events on the
// same newline-framed transport until the client disconnects. Name is the event
// type (app.EventPane*); Data is its payload (an app.PaneExitedEvent, etc.). A
// streaming client reads the ack as a Response, then reads Events — the two frame
// shapes never interleave, so each is unambiguous.
type Event struct {
	Name string          `json:"event"`
	Data json.RawMessage `json:"data,omitempty"`
}

// newResponse builds a Response, marshaling data into Data when non-nil. A
// marshal failure degrades to an error Response so a result is always produced.
func newResponse(id string, ok bool, errMsg string, data any) Response {
	r := Response{ID: id, OK: ok, Error: errMsg}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return Response{ID: id, OK: false, Error: "encode result: " + err.Error()}
		}
		r.Data = raw
	}
	return r
}

// writeMessage encodes v as one newline-terminated JSON frame on w.
func writeMessage(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// readRequest reads one newline-framed Request from br. A request written without
// a trailing newline before the peer closes still decodes (ReadBytes returns the
// buffered bytes alongside io.EOF); a truly empty read surfaces the read error.
func readRequest(br *bufio.Reader) (Request, error) {
	line, err := br.ReadBytes('\n')
	if len(line) == 0 {
		return Request{}, err
	}
	var req Request
	if e := json.Unmarshal(line, &req); e != nil {
		return Request{}, e
	}
	return req, nil
}

// readResponse reads one newline-framed Response from br (client side).
func readResponse(br *bufio.Reader) (Response, error) {
	line, err := br.ReadBytes('\n')
	if len(line) == 0 {
		return Response{}, err
	}
	var resp Response
	if e := json.Unmarshal(line, &resp); e != nil {
		return Response{}, e
	}
	return resp, nil
}

// readEvent reads one newline-framed Event from br (streaming client side). A
// clean stream end surfaces the read error (io.EOF) with an empty line.
func readEvent(br *bufio.Reader) (Event, error) {
	line, err := br.ReadBytes('\n')
	if len(line) == 0 {
		return Event{}, err
	}
	var ev Event
	if e := json.Unmarshal(line, &ev); e != nil {
		return Event{}, e
	}
	return ev, nil
}
