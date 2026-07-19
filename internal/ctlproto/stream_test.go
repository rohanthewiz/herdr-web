package ctlproto

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// streamListener spins up a Server on a fresh unix socket and returns its path.
// It uses a short temp dir (not t.TempDir, whose long test-name path can exceed
// the ~104-char unix socket limit on macOS).
func streamListener(t *testing.T, sd StreamDispatch) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cs")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	s := NewServer(syncDispatch(false), time.Second, "gateway")
	if sd != nil {
		s.SetStreamDispatch(sd)
	}
	go s.Serve(l)
	return socket
}

// An Event survives a newline-framed encode/decode.
func TestEventFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	ev := Event{Name: "pane_agent", Data: json.RawMessage(`{"pane":2,"state":"working"}`)}
	if err := writeMessage(&buf, ev); err != nil {
		t.Fatalf("writeMessage: %v", err)
	}
	got, err := readEvent(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("readEvent: %v", err)
	}
	if got.Name != ev.Name || string(got.Data) != string(ev.Data) {
		t.Fatalf("round-trip mismatch: %+v vs %+v", got, ev)
	}
}

// A subscribe request acks, then the events the app pushes reach the client; when
// the client disconnects, the dispatch's cancel runs and Subscribe returns nil.
func TestStreamSubscribeReceivesEvents(t *testing.T) {
	subCh := make(chan Subscriber, 1)
	cancelled := make(chan struct{})
	socket := streamListener(t, func(method string, params json.RawMessage, sub Subscriber) (func(), error) {
		if method != MethodEventsSubscribe {
			t.Errorf("unexpected method %q", method)
		}
		if string(params) != `{"pane":1}` {
			t.Errorf("params = %s", params)
		}
		subCh <- sub
		return func() { close(cancelled) }, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan Event, 8)
	done := make(chan error, 1)
	go func() {
		done <- Subscribe(ctx, socket,
			Request{ID: "e1", Method: MethodEventsSubscribe, Params: json.RawMessage(`{"pane":1}`)},
			func(ev Event) error { got <- ev; return nil })
	}()

	sub := <-subCh // the dispatch handed us the sink
	if !sub.Send("pane_exited", map[string]any{"pane": 1, "exit_code": 0}) {
		t.Fatalf("Send returned false")
	}
	select {
	case ev := <-got:
		if ev.Name != "pane_exited" {
			t.Fatalf("event name = %q", ev.Name)
		}
		var d struct {
			Pane     int `json:"pane"`
			ExitCode int `json:"exit_code"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil || d.Pane != 1 {
			t.Fatalf("event data = %s (%v)", ev.Data, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}

	cancel() // client disconnects
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch cancel not called on client disconnect")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Subscribe returned %v, want nil on clean cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe did not return after cancel")
	}
}

// A dispatch that rejects the subscription (bad params) surfaces as a failed ack.
func TestStreamSubscribeRejected(t *testing.T) {
	socket := streamListener(t, func(string, json.RawMessage, Subscriber) (func(), error) {
		return nil, errors.New("bad filter")
	})
	err := Subscribe(context.Background(), socket,
		Request{Method: MethodEventsSubscribe}, func(Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "bad filter") {
		t.Fatalf("want rejection error, got %v", err)
	}
}

// A server without a StreamDispatch rejects subscribe with "streaming not supported".
func TestStreamNotSupported(t *testing.T) {
	socket := streamListener(t, nil)
	err := Subscribe(context.Background(), socket,
		Request{Method: MethodEventsSubscribe}, func(Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "streaming not supported") {
		t.Fatalf("want not-supported error, got %v", err)
	}
}

// The per-connection sink buffers up to streamBuffer events, then drops (returns
// false) so a slow reader can't back-pressure the app; a closed sink also drops.
func TestConnSubscriberDropsWhenFull(t *testing.T) {
	s := newConnSubscriber()
	for i := range streamBuffer {
		if !s.Send("e", i) {
			t.Fatalf("Send %d unexpectedly false before buffer full", i)
		}
	}
	if s.Send("e", "overflow") {
		t.Fatalf("Send past buffer should return false")
	}
	s.close()
	if s.Send("e", "after-close") {
		t.Fatalf("Send after close should return false")
	}
}
