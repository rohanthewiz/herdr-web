//go:build ghostty

package main

import (
	"encoding/json"

	"github.com/rohanthewiz/herdr-web/internal/app"
	"github.com/rohanthewiz/herdr-web/internal/browserproto"
)

// handleCmd runs one §7 command from a browser. The command table itself lives in
// internal/app (app.Dispatcher) so the same vocabulary can serve a CLI/control-API
// too; here we just adapt the browser wire to the neutral seam: a jsonParamDecoder
// over the cmd's params and a browserResponder that marshals the cmd_result back
// on this connection. orch itself implements app.Backend (the runtime effects).
// Loop-goroutine only.
func (o *orch) handleCmd(c *client, m *browserproto.Cmd) {
	d := app.NewDispatcher(o.session, o)
	d.Dispatch(m.Name, jsonParamDecoder{m.Params}, browserResponder{o: o, c: c, id: m.ID})
}

// browserResponder delivers a command's cmd_result to the browser that issued it.
// A command with no id yields no result (WantsReply is false), so async commands
// skip the round-trip and any reply is a no-op.
type browserResponder struct {
	o  *orch
	c  *client
	id string
}

func (r browserResponder) WantsReply() bool   { return r.id != "" }
func (r browserResponder) OK(data any)        { r.reply(true, "", data) }
func (r browserResponder) Fail(errMsg string) { r.reply(false, errMsg, nil) }

func (r browserResponder) reply(ok bool, errMsg string, data any) {
	if r.id == "" {
		return
	}
	if res, err := browserproto.NewCmdResult(r.id, ok, errMsg, data); err == nil {
		r.o.send(r.c, res)
	}
}

// jsonParamDecoder decodes a browser cmd's json params into a typed struct.
// Empty params report app.ErrNoParams so the dispatcher can treat them as the
// zero value for optional commands, or an error for required ones.
type jsonParamDecoder struct{ raw json.RawMessage }

func (d jsonParamDecoder) Decode(v any) error {
	if len(d.raw) == 0 {
		return app.ErrNoParams
	}
	return json.Unmarshal(d.raw, v)
}
