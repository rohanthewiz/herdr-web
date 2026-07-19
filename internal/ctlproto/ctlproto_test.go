package ctlproto

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/rohanthewiz/herdr-web/internal/app"
)

// syncDispatch resolves the responder inline, echoing method+params — the common
// case (every command except read/capture resolves before Dispatch returns).
func syncDispatch(fail bool) Dispatch {
	return func(method string, params json.RawMessage, r app.Responder) {
		if fail {
			r.Fail("nope: " + method)
			return
		}
		r.OK(map[string]any{"method": method, "params": string(params)})
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	req := Request{ID: "42", Method: app.CmdPaneSplit, Params: json.RawMessage(`{"direction":"h"}`)}
	if err := writeMessage(&buf, req); err != nil {
		t.Fatalf("writeMessage: %v", err)
	}
	if buf.Bytes()[buf.Len()-1] != '\n' {
		t.Fatalf("frame not newline-terminated: %q", buf.String())
	}
	got, err := readRequest(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("readRequest: %v", err)
	}
	if got.ID != req.ID || got.Method != req.Method || string(got.Params) != string(req.Params) {
		t.Fatalf("round-trip mismatch: %+v vs %+v", got, req)
	}
}

func TestReadRequestNoTrailingNewline(t *testing.T) {
	// A request flushed without a trailing newline (peer then closes) must still
	// decode — Call/handleConn rely on ReadBytes surfacing the buffered bytes.
	got, err := readRequest(bufio.NewReader(bytes.NewReader([]byte(`{"method":"ping"}`))))
	if err != nil {
		t.Fatalf("readRequest: %v", err)
	}
	if got.Method != MethodPing {
		t.Fatalf("method = %q, want ping", got.Method)
	}
}

func TestHandlePing(t *testing.T) {
	s := NewServer(syncDispatch(false), time.Second, "test-svc")
	resp := s.handle(Request{ID: "p1", Method: MethodPing})
	if !resp.OK || resp.ID != "p1" {
		t.Fatalf("ping response = %+v", resp)
	}
	var pong Pong
	if err := json.Unmarshal(resp.Data, &pong); err != nil {
		t.Fatalf("decode pong: %v", err)
	}
	if pong.Protocol != ProtocolVersion || pong.Service != "test-svc" {
		t.Fatalf("pong = %+v", pong)
	}
}

func TestDispatchSyncOKAndFail(t *testing.T) {
	ok := NewServer(syncDispatch(false), time.Second, "svc").handle(
		Request{ID: "c1", Method: app.CmdPaneFocus, Params: json.RawMessage(`{"pane":1}`)})
	if !ok.OK || ok.ID != "c1" {
		t.Fatalf("ok response = %+v", ok)
	}
	var data map[string]any
	if err := json.Unmarshal(ok.Data, &data); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if data["method"] != app.CmdPaneFocus {
		t.Fatalf("echoed method = %v", data["method"])
	}

	fail := NewServer(syncDispatch(true), time.Second, "svc").handle(
		Request{ID: "c2", Method: app.CmdPaneFocus})
	if fail.OK || fail.Error == "" {
		t.Fatalf("fail response = %+v", fail)
	}
}

func TestDispatchAsyncResolve(t *testing.T) {
	// Model read/capture: the dispatch returns immediately and another goroutine
	// resolves the responder later. dispatchAndWait must still deliver it.
	async := func(method string, params json.RawMessage, r app.Responder) {
		if !r.WantsReply() {
			t.Errorf("control caller must want a reply")
		}
		go func() {
			time.Sleep(20 * time.Millisecond)
			r.OK(app.ReadResult{Text: "later"})
		}()
	}
	resp := NewServer(async, time.Second, "svc").handle(Request{ID: "r1", Method: app.CmdRead})
	if !resp.OK {
		t.Fatalf("async response = %+v", resp)
	}
	var rr app.ReadResult
	if err := json.Unmarshal(resp.Data, &rr); err != nil || rr.Text != "later" {
		t.Fatalf("async data = %s (%v)", resp.Data, err)
	}
}

func TestDispatchTimeout(t *testing.T) {
	// A dispatch that never resolves its responder must hit the backstop timeout,
	// not hang. A late resolve afterwards is dropped (buffered chan + sync.Once).
	var captured app.Responder
	never := func(method string, params json.RawMessage, r app.Responder) { captured = r }
	resp := NewServer(never, 30*time.Millisecond, "svc").handle(Request{ID: "t1", Method: app.CmdPaneFocus})
	if resp.OK || resp.Error != "command timed out" {
		t.Fatalf("timeout response = %+v", resp)
	}
	captured.OK(nil)  // late resolve: must not panic or block
	captured.Fail("") // second resolve: sync.Once drops it
}

func TestServeAndCallOverSocket(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "ctl.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	s := NewServer(syncDispatch(false), time.Second, "gateway")
	go s.Serve(l)

	// ping over the wire
	pingResp, err := Call(socket, Request{ID: "1", Method: MethodPing}, time.Second)
	if err != nil {
		t.Fatalf("ping Call: %v", err)
	}
	if !pingResp.OK {
		t.Fatalf("ping resp = %+v", pingResp)
	}

	// a command over the wire (separate connection — one request per connection)
	cmdResp, err := Call(socket, Request{ID: "2", Method: app.CmdTabCreate}, time.Second)
	if err != nil {
		t.Fatalf("cmd Call: %v", err)
	}
	if !cmdResp.OK || cmdResp.ID != "2" {
		t.Fatalf("cmd resp = %+v", cmdResp)
	}
}

func TestCallDialError(t *testing.T) {
	_, err := Call(filepath.Join(t.TempDir(), "absent.sock"), Request{Method: MethodPing}, 200*time.Millisecond)
	if err == nil {
		t.Fatalf("expected dial error for absent socket")
	}
}
