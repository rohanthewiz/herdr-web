package ctlproto

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// This file is the streaming half of the control protocol: everything the base
// one-request/one-response envelope (proto.go, server.go, client.go) doesn't
// cover. Today its sole method is events.subscribe — the server writes an ack
// Response, then forwards Event frames the app pushes until the client
// disconnects. The app never touches the socket: it holds a Subscriber and calls
// Send; the server owns the connection and drains the events to it.

// Subscriber is the app's handle to one streaming client. The app pushes events
// with Send (from whatever goroutine observes them); the server forwards each to
// the socket. Send is non-blocking: when the client's buffer is full — a slow or
// dead reader — it returns false and the app drops the subscriber, mirroring the
// browser slow-connection drop. It also returns false once the client has gone.
type Subscriber interface {
	Send(event string, data any) bool
}

// StreamDispatch starts a streaming method. It validates method+params, registers
// sub as the sink for matching events, and returns cancel — called when the client
// disconnects — to unregister it. A non-nil error rejects the subscription (bad
// params, unknown method), reported to the client as a failed ack. For gateway2
// this posts the registration onto the orchestrator loop and returns a cancel that
// posts the removal.
type StreamDispatch func(method string, params json.RawMessage, sub Subscriber) (cancel func(), err error)

// SetStreamDispatch installs the handler for streaming methods (events.subscribe).
// Without it, a subscribe request is answered with a "streaming not supported"
// failure. Call once before Serve.
func (s *Server) SetStreamDispatch(sd StreamDispatch) { s.stream = sd }

// handleStream serves one events.subscribe connection: register the subscriber,
// ack, then pump its events to the socket until the client closes the connection
// (its read side reaching EOF) or a write fails. cancel unregisters on the way out.
func (s *Server) handleStream(conn net.Conn, br *bufio.Reader, req Request) {
	if s.stream == nil {
		_ = writeMessage(conn, newResponse(req.ID, false, "streaming not supported", nil))
		return
	}
	sub := newConnSubscriber()
	cancel, err := s.stream(req.Method, req.Params, sub)
	if err != nil {
		_ = writeMessage(conn, newResponse(req.ID, false, err.Error(), nil))
		return
	}
	defer cancel()

	if err := writeMessage(conn, newResponse(req.ID, true, "", nil)); err != nil {
		return // client already gone
	}

	// A streaming client sends nothing after subscribe, so any further read means
	// the connection closed. Watch it in the background and stop the pump when it
	// does; the pump itself also stops on a write error.
	go func() {
		_, _ = br.ReadByte()
		sub.close()
	}()
	sub.pump(conn)
}

// connSubscriber is the server-side Subscriber for one connection: Send enqueues
// onto a bounded buffer, pump drains it to the socket. done cuts both off when the
// client disconnects (or a write fails), so a late Send is dropped, not blocked.
type connSubscriber struct {
	ch        chan Event
	done      chan struct{}
	closeOnce sync.Once
}

// streamBuffer bounds a subscriber's outbound queue. Past this a slow reader is
// dropped rather than back-pressuring the app's event loop.
const streamBuffer = 128

func newConnSubscriber() *connSubscriber {
	return &connSubscriber{ch: make(chan Event, streamBuffer), done: make(chan struct{})}
}

func (s *connSubscriber) Send(event string, data any) bool {
	raw, err := json.Marshal(data)
	if err != nil {
		return true // undeliverable payload isn't the client's fault; skip, keep the sub
	}
	select {
	case s.ch <- Event{Name: event, Data: raw}:
		return true
	case <-s.done:
		return false
	default:
		return false // buffer full → slow/dead reader → drop the subscriber
	}
}

// close stops the subscription: pump returns and further Sends report false.
func (s *connSubscriber) close() { s.closeOnce.Do(func() { close(s.done) }) }

// pump drains queued events to conn until the subscription closes or a write
// fails. Runs on the connection's own goroutine.
func (s *connSubscriber) pump(conn net.Conn) {
	for {
		select {
		case ev := <-s.ch:
			if err := writeMessage(conn, ev); err != nil {
				s.close()
				return
			}
		case <-s.done:
			return
		}
	}
}

// Subscribe opens a streaming subscription and runs it to completion. It dials the
// control socket, sends req (a streaming method — MethodEventsSubscribe), reads the
// ack Response, then invokes onEvent for each pushed Event until the connection
// closes, ctx is cancelled, or onEvent returns an error. It returns nil on a clean
// stream end (server stop / client cancel), the ack error if the server rejects the
// subscription, or the transport/onEvent error that ended the stream.
func Subscribe(ctx context.Context, socket string, req Request, onEvent func(Event) error) error {
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial control socket %s: %w", socket, err)
	}
	defer conn.Close()

	// Unblock the read below when ctx is cancelled (Ctrl-C) by closing the conn.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	if err := writeMessage(conn, req); err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	br := bufio.NewReader(conn)
	ack, err := readResponse(br)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("read ack: %w", err)
	}
	if !ack.OK {
		return fmt.Errorf("subscription rejected: %s", ack.Error)
	}

	for {
		ev, err := readEvent(br)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, io.EOF) {
				return nil // client cancel or clean server-side close
			}
			return fmt.Errorf("read event: %w", err)
		}
		if err := onEvent(ev); err != nil {
			return err
		}
	}
}
