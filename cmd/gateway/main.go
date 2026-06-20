// Command gateway serves herdr through the browser. It attaches one herdr
// client connection per browser WebSocket, streams semantic frames to the page
// as JSON, and forwards browser keyboard/resize events back to herdr.
//
// Usage:
//
//	gateway [--addr :8420] [--socket ~/.config/herdr/herdr-client.sock]
package main

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"flag"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rohanthewiz/herdr-web/internal/herdrconn"
	"github.com/rohanthewiz/herdr-web/internal/wire"
	"github.com/rohanthewiz/rweb"
)

//go:embed web/index.html
var webFS embed.FS

// titles broadcasts browser-tab titles to every connected browser pump. herdr
// itself forwards no usable window title, so this is how an external source —
// e.g. a Claude Code Stop hook POSTing the session's ai-title — drives
// document.title in the page.
var titles = newTitleHub()

// titleHub fans a pushed title out to all connected browser pumps and remembers
// the latest so a freshly-connected browser picks it up immediately.
type titleHub struct {
	mu      sync.Mutex
	latest  string
	clients map[chan string]struct{}
}

func newTitleHub() *titleHub {
	return &titleHub{clients: make(map[chan string]struct{})}
}

// subscribe registers a pump and returns its update channel plus an
// unsubscribe func. If a title is already set it is delivered right away.
func (t *titleHub) subscribe() (<-chan string, func()) {
	ch := make(chan string, 1)
	t.mu.Lock()
	t.clients[ch] = struct{}{}
	if t.latest != "" {
		ch <- t.latest
	}
	t.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			t.mu.Lock()
			delete(t.clients, ch)
			close(ch)
			t.mu.Unlock()
		})
	}
}

// broadcast records and delivers a title, coalescing so a slow pump only ever
// sees the newest value.
func (t *titleHub) broadcast(title string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.latest = title
	for ch := range t.clients {
		select {
		case ch <- title:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- title:
			default:
			}
		}
	}
}

func main() {
	addr := flag.String("addr", ":8420", "listen address")
	socket := flag.String("socket", defaultSocket(), "herdr client socket path")
	flag.Parse()

	indexHTML, err := webFS.ReadFile("web/index.html")
	if err != nil {
		log.Fatalf("gateway: read embedded page: %v", err)
	}

	s := rweb.NewServer(rweb.ServerOptions{Address: *addr, Verbose: true})

	s.Get("/", func(ctx rweb.Context) error {
		return ctx.WriteHTML(string(indexHTML))
	})

	s.WebSocket("/ws", func(ws *rweb.WSConn) error {
		return bridge(ws, *socket)
	})

	// POST /title {"title":"..."} sets the browser-tab title for all connected
	// browsers. An empty/missing title restores the default. Intended for a
	// Claude Code hook to advertise the running session's friendly name.
	s.Post("/title", func(ctx rweb.Context) error {
		var body struct {
			Title string `json:"title"`
		}
		if err := json.Unmarshal(ctx.Request().Body(), &body); err != nil {
			return ctx.Status(400).WriteString("bad json")
		}
		titles.broadcast(strings.TrimSpace(body.Title))
		return ctx.WriteString("ok")
	})

	log.Printf("gateway: serving herdr at http://localhost%s (socket %s)", *addr, *socket)
	log.Fatal(s.Run())
}

func defaultSocket() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "herdr", "herdr-client.sock")
}

// clientCmd is a message from the browser.
type clientCmd struct {
	T    string `json:"t"`    // "init" | "input" | "paste" | "image" | "resize"
	Data string `json:"data"` // raw input/paste text, or base64 image bytes (for "image")
	Ext  string `json:"ext"`  // image file extension (for "image")
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// bridge wires one browser WebSocket to one herdr client connection.
func bridge(ws *rweb.WSConn, socket string) error {
	defer ws.Close(1000, "bye")

	// First message must be init with the browser's grid size.
	cols, rows := uint16(120), uint16(32)
	first, err := ws.ReadMessage()
	if err != nil {
		return nil
	}
	if first.Type == rweb.TextMessage {
		var c clientCmd
		if json.Unmarshal(first.Data, &c) == nil && c.T == "init" {
			if c.Cols > 0 {
				cols = c.Cols
			}
			if c.Rows > 0 {
				rows = c.Rows
			}
		}
	}

	h, err := herdrconn.Dial(socket, cols, rows)
	if err != nil {
		_ = ws.WriteMessage(rweb.TextMessage, mustJSON(map[string]string{"t": "error", "msg": err.Error()}))
		return nil
	}
	defer h.Close()
	defer h.Detach()

	// herdr → browser pump (the only goroutine that writes to the WebSocket).
	// It multiplexes herdr messages with title pushes from the hub; herdr reads
	// happen on a helper goroutine so a quiet herdr doesn't block title updates.
	titleCh, unsubscribe := titles.subscribe()
	go func() {
		defer unsubscribe()
		var st diffState
		herdrMsgs := make(chan *wire.ServerMessage, 8)
		go func() {
			for {
				msg, err := h.Read()
				if err != nil {
					close(herdrMsgs)
					return
				}
				herdrMsgs <- msg
			}
		}()
		for {
			select {
			case title := <-titleCh:
				_ = ws.WriteMessage(rweb.TextMessage, mustJSON(map[string]any{"t": "title", "title": title}))
				continue
			case msg, ok := <-herdrMsgs:
				if !ok {
					ws.Close(1000, "herdr closed")
					return
				}
				switch msg.Kind {
				case wire.SMWelcome:
					if msg.Welcome != nil && msg.Welcome.Error != nil {
						_ = ws.WriteMessage(rweb.TextMessage, mustJSON(map[string]string{"t": "error", "msg": *msg.Welcome.Error}))
						ws.Close(1000, "rejected")
						return
					}
				case wire.SMFrame:
					_ = ws.WriteMessage(rweb.TextMessage, encodeFrameOrDiff(msg.Frame, &st))
				case wire.SMClipboard:
					if msg.Clipboard != nil {
						_ = ws.WriteMessage(rweb.TextMessage, mustJSON(map[string]any{"t": "clipboard", "data": *msg.Clipboard}))
					}
				case wire.SMWindowTitle:
					_ = ws.WriteMessage(rweb.TextMessage, mustJSON(map[string]any{"t": "title", "title": msg.WindowTitle}))
				case wire.SMMouseCapture:
					if msg.Mouse != nil {
						_ = ws.WriteMessage(rweb.TextMessage, mustJSON(map[string]any{"t": "mouse", "enabled": *msg.Mouse}))
					}
				case wire.SMNotify:
					if msg.Notify != nil {
						_ = ws.WriteMessage(rweb.TextMessage, notifyJSON(msg.Notify))
					}
				case wire.SMServerShutdown:
					_ = ws.WriteMessage(rweb.TextMessage, mustJSON(map[string]string{"t": "shutdown"}))
					ws.Close(1000, "server shutdown")
					return
				}
			}
		}
	}()

	// browser → herdr pump.
	for {
		m, err := ws.ReadMessage()
		if err != nil {
			return nil
		}
		if m.Type != rweb.TextMessage {
			continue
		}
		var c clientCmd
		if json.Unmarshal(m.Data, &c) != nil {
			continue
		}
		switch c.T {
		case "input":
			_ = h.SendInput([]byte(c.Data))
		case "paste":
			_ = h.SendPaste(c.Data)
		case "image":
			if c.Ext != "" {
				if raw, err := base64.StdEncoding.DecodeString(c.Data); err == nil && len(raw) > 0 {
					_ = h.SendClipboardImage(c.Ext, raw)
				}
			}
		case "resize":
			if c.Cols > 0 && c.Rows > 0 {
				_ = h.Resize(c.Cols, c.Rows)
			}
		}
	}
}

// wireCell is the compact per-cell JSON sent to the browser. It is comparable so
// the diff encoder can detect changed cells with ==.
type wireCell struct {
	S string `json:"s"`           // symbol (never empty; space for blank)
	F string `json:"f,omitempty"` // fg CSS ("" = default)
	B string `json:"b,omitempty"` // bg CSS ("" = default)
	M uint16 `json:"m,omitempty"` // ratatui modifier bits
	H uint32 `json:"h,omitempty"` // hyperlink index + 1 (0 = none)
}

type wireCursor struct {
	X   uint16 `json:"x"`
	Y   uint16 `json:"y"`
	Vis bool   `json:"vis"`
}

// wireFrame is a full frame: the browser replaces its cell buffer.
type wireFrame struct {
	T     string      `json:"t"` // "frame"
	W     uint16      `json:"w"`
	H     uint16      `json:"h"`
	Cur   *wireCursor `json:"cur,omitempty"`
	Links []string    `json:"links,omitempty"`
	Cells []wireCell  `json:"cells"`
}

// diffCell is one changed cell, identified by its row-major index.
type diffCell struct {
	I int `json:"i"`
	wireCell
}

// wireDiff patches the browser's existing cell buffer.
type wireDiff struct {
	T     string      `json:"t"` // "fdiff"
	Cur   *wireCursor `json:"cur,omitempty"`
	Cells []diffCell  `json:"cells"`
}

// diffState is the gateway's per-connection memory of the last frame sent.
type diffState struct {
	cells []wireCell
	w, h  uint16
	links []string
	has   bool
}

// encodeFrameOrDiff sends a full frame when geometry or hyperlinks change,
// otherwise only the cells that differ from the previous frame.
func encodeFrameOrDiff(f *wire.Frame, st *diffState) []byte {
	cells := make([]wireCell, len(f.Cells))
	for i := range f.Cells {
		cells[i] = toWireCell(&f.Cells[i])
	}
	cur := cursorOf(f)

	full := !st.has || f.Width != st.w || f.Height != st.h ||
		len(cells) != len(st.cells) || !strSliceEq(st.links, f.Hyperlinks)
	if full {
		st.cells, st.w, st.h, st.links, st.has = cells, f.Width, f.Height, f.Hyperlinks, true
		return mustJSON(wireFrame{T: "frame", W: f.Width, H: f.Height, Cur: cur, Links: f.Hyperlinks, Cells: cells})
	}

	var changed []diffCell
	for i := range cells {
		if cells[i] != st.cells[i] {
			changed = append(changed, diffCell{I: i, wireCell: cells[i]})
		}
	}
	st.cells = cells
	return mustJSON(wireDiff{T: "fdiff", Cur: cur, Cells: changed})
}

func toWireCell(c *wire.Cell) wireCell {
	sym := c.Symbol
	if sym == "" {
		sym = " "
	}
	var h uint32
	if c.Hyperlink != nil {
		h = *c.Hyperlink + 1
	}
	return wireCell{S: sym, F: wire.ColorToCSS(c.FG), B: wire.ColorToCSS(c.BG), M: c.Modifier, H: h}
}

func cursorOf(f *wire.Frame) *wireCursor {
	if f.Cursor == nil {
		return nil
	}
	return &wireCursor{X: f.Cursor.X, Y: f.Cursor.Y, Vis: f.Cursor.Visible}
}

func strSliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func notifyJSON(n *wire.Notify) []byte {
	m := map[string]any{"t": "notify", "kind": n.Kind, "message": n.Message}
	if n.Body != nil {
		m["body"] = *n.Body
	}
	return mustJSON(m)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"t":"error","msg":"encode failed"}`)
	}
	return b
}
