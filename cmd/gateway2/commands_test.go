//go:build ghostty

package main

import (
	"testing"

	"github.com/rohanthewiz/herdr-web/internal/app"
	"github.com/rohanthewiz/herdr-web/internal/browserproto"
)

// cmd builds a §7 command message for the dispatch tests.
func cmd(t *testing.T, id, name string, params any) *browserproto.Cmd {
	t.Helper()
	c, err := browserproto.NewCmd(id, name, params)
	if err != nil {
		t.Fatalf("NewCmd(%s): %v", name, err)
	}
	return &c
}

// recvDown pops one queued down-message off the client.
func recvDown(t *testing.T, c *client) any {
	t.Helper()
	select {
	case b := <-c.out:
		msg, err := browserproto.DecodeDown(b)
		if err != nil {
			t.Fatalf("decode down: %v", err)
		}
		return msg
	default:
		t.Fatal("no message queued")
		return nil
	}
}

// server.stop replies ok, then tells every browser it is going away, then fires
// the process-shutdown hook. The persistent termhost daemon is untouched.
func TestServerStopDispatch(t *testing.T) {
	o, c := newReadHarness()
	stopped := false
	o.stop = func() { stopped = true }

	o.handleCmd(c, cmd(t, "s1", browserproto.CmdServerStop, nil))

	if r, ok := recvDown(t, c).(*browserproto.CmdResult); !ok || r.ID != "s1" || !r.Ok {
		t.Fatalf("first message should be an ok cmd_result, got %#v", recvDown(t, c))
	}
	if _, ok := recvDown(t, c).(*browserproto.Shutdown); !ok {
		t.Fatal("second message should be a shutdown broadcast")
	}
	if !stopped {
		t.Fatal("stop hook was not invoked")
	}
}

// A nil stop hook (e.g. in tests before main wires it) must not panic: the
// command still acks and broadcasts.
func TestServerStopNilHook(t *testing.T) {
	o, c := newReadHarness()
	o.handleCmd(c, cmd(t, "s2", browserproto.CmdServerStop, nil))
	if r, ok := recvDown(t, c).(*browserproto.CmdResult); !ok || !r.Ok {
		t.Fatal("server.stop should ack even with no stop hook")
	}
	if _, ok := recvDown(t, c).(*browserproto.Shutdown); !ok {
		t.Fatal("server.stop should broadcast shutdown even with no stop hook")
	}
}

// server.reload_config has no config subsystem to act on yet, but is wired to
// ack so browsers get a result.
func TestServerReloadConfigDispatch(t *testing.T) {
	o, c := newReadHarness()
	o.handleCmd(c, cmd(t, "r1", browserproto.CmdServerReloadConfig, nil))
	if r, ok := recvDown(t, c).(*browserproto.CmdResult); !ok || r.ID != "r1" || !r.Ok {
		t.Fatal("server.reload_config should ack ok")
	}
}

// agent.focus for a pane not in the model fails synchronously (before any
// viewport reconciliation), so a bad id never reaches the daemon.
func TestAgentFocusUnknownPane(t *testing.T) {
	o, c := newReadHarness()
	sess, err := app.NewSession(modelSpawner{}, "/tmp")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	o.session = sess

	o.handleCmd(c, cmd(t, "a1", browserproto.CmdAgentFocus, browserproto.PaneParams{Pane: 9999}))
	if r, ok := recvDown(t, c).(*browserproto.CmdResult); !ok || r.Ok || r.Error == "" {
		t.Fatalf("agent.focus on unknown pane should fail with an error result, got %#v", recvDown(t, c))
	}
}
