//go:build ghostty

package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creack/pty"
	"github.com/rohanthewiz/herdr-web/internal/detect"
	"github.com/rohanthewiz/herdr-web/internal/terminal"
)

// DefaultFlushInterval coalesces dirty panes into frames at ~60 Hz, mirroring
// the Phase A requestAnimationFrame coalescing.
const DefaultFlushInterval = 16 * time.Millisecond

// detectInterval is how often a pane's foreground process group is probed for
// agent identity.
const detectInterval = 400 * time.Millisecond

// pane is one terminal: a PTY + go-libghostty emulator + child process.
type pane struct {
	id   uint32
	emu  terminal.Emulator
	ptmx *os.File
	cmd  *exec.Cmd

	dirty atomic.Bool

	// emuMu serializes all emulator access (the emulator is not concurrency
	// safe) and guards prev/closed.
	emuMu  sync.Mutex
	prev   *terminal.Snapshot // last snapshot sent, for diffing
	closed bool

	// OSC passthrough state, owned exclusively by this pane's readPump goroutine
	// (libghostty-vt does not surface OSC 7 cwd, so we scan the raw byte stream).
	osc     oscScanner
	lastPwd string // last OSC 7 cwd emitted, for change detection

	// ptyMu serializes writes to the PTY master (user input + the emulator's
	// query-response callback can both write).
	ptyMu sync.Mutex
}

func (p *pane) writePTY(b []byte) error {
	p.ptyMu.Lock()
	defer p.ptyMu.Unlock()
	_, err := p.ptmx.Write(b)
	return err
}

// Host is the Go terminal backend: it owns panes and serves the orchestration
// protocol over a connection. A Host serves one connection at a time.
type Host struct {
	FlushInterval time.Duration

	mu    sync.Mutex
	panes map[uint32]*pane

	out  chan any
	done chan struct{}
}

// NewHost creates an empty Host.
func NewHost() *Host {
	return &Host{
		FlushInterval: DefaultFlushInterval,
		panes:         make(map[uint32]*pane),
		out:           make(chan any, 64),
	}
}

// Serve runs the read/write/flush loops until the connection closes or ctx is
// cancelled, then tears down all panes. It blocks for the lifetime of the link.
func (h *Host) Serve(ctx context.Context, conn io.ReadWriteCloser) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	h.done = make(chan struct{})

	var wg sync.WaitGroup

	// Writer: drain outbound events to the connection.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-h.out:
				if err := WriteMessage(conn, ev); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	// Flusher: coalesce dirty panes into frames.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(h.FlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.flushDirty()
			}
		}
	}()

	// Reader: dispatch inbound commands.
	var readErr error
	for {
		typ, payload, err := ReadMessage(conn)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
		h.dispatch(typ, payload)
	}

	cancel()
	close(h.done) // unblock any emitters
	h.shutdownAll()
	wg.Wait()
	return readErr
}

func (h *Host) dispatch(typ MessageType, payload []byte) {
	switch typ {
	case MsgHello:
		h.emit(NewWelcome(""))
	case MsgCreatePane:
		var c CreatePane
		if err := json.Unmarshal(payload, &c); err != nil {
			h.emit(NewError(0, "bad create_pane: "+err.Error()))
			return
		}
		if err := h.createPane(c); err != nil {
			h.emit(NewError(c.PaneID, err.Error()))
		}
	case MsgInput:
		var c Input
		if err := json.Unmarshal(payload, &c); err != nil {
			h.emit(NewError(0, "bad input: "+err.Error()))
			return
		}
		if p := h.getPane(c.PaneID); p != nil {
			if err := p.writePTY(c.Data); err != nil {
				h.emit(NewError(c.PaneID, err.Error()))
			}
		} else {
			h.emit(NewError(c.PaneID, "no such pane"))
		}
	case MsgResize:
		var c Resize
		if err := json.Unmarshal(payload, &c); err != nil {
			h.emit(NewError(0, "bad resize: "+err.Error()))
			return
		}
		if err := h.resizePane(c); err != nil {
			h.emit(NewError(c.PaneID, err.Error()))
		}
	case MsgClosePane:
		var c ClosePane
		if err := json.Unmarshal(payload, &c); err != nil {
			h.emit(NewError(0, "bad close_pane: "+err.Error()))
			return
		}
		if p := h.removePane(c.PaneID); p != nil {
			h.closePane(p) // read pump observes EOF and emits pane_exited
		}
	default:
		h.emit(NewError(0, "unknown message type: "+string(typ)))
	}
}

func (h *Host) createPane(c CreatePane) error {
	name := c.Command
	if name == "" {
		name = defaultShell()
	}
	cmd := exec.Command(name, c.Args...)
	cmd.Env = buildEnv(c.Env)
	if c.Cwd != "" {
		cmd.Dir = c.Cwd
	}

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: c.Cols, Rows: c.Rows})
	if err != nil {
		return fmt.Errorf("start pty: %w", err)
	}

	p := &pane{id: c.PaneID, ptmx: ptmx, cmd: cmd}
	emu, err := terminal.New(c.Cols, c.Rows, terminal.WithWritePTY(func(d []byte) {
		_ = p.writePTY(d)
	}))
	if err != nil {
		_ = ptmx.Close()
		_ = cmd.Process.Kill()
		return fmt.Errorf("new emulator: %w", err)
	}
	p.emu = emu

	h.mu.Lock()
	h.panes[p.id] = p
	h.mu.Unlock()

	go h.readPump(p)
	go h.detectPump(p)
	return nil
}

// readPump copies PTY output into the emulator until the child exits, then emits
// a final frame and pane_exited.
func (h *Host) readPump(p *pane) {
	buf := make([]byte, 32*1024)
	for {
		n, err := p.ptmx.Read(buf)
		if n > 0 {
			h.feed(p, buf[:n])
			p.dirty.Store(true)
			// Scan the raw stream for OSC passthrough (cwd) the emulator doesn't surface.
			if cwd, ok := p.osc.scan(buf[:n]); ok && cwd != p.lastPwd {
				p.lastPwd = cwd
				h.emit(NewPaneCwd(p.id, cwd))
			}
		}
		if err != nil { // EOF / EIO when the child exits or the PTY closes
			break
		}
	}

	h.removePane(p.id) // stop the flusher from touching it
	if f, err := h.takeFrame(p); err == nil && f != nil {
		h.emit(NewPaneFrame(p.id, f))
	}
	h.closePane(p)
	h.emit(NewPaneExited(p.id, exitCode(p.cmd.Wait())))
}

// detectPump probes the pane's foreground process group for agent identity and
// runs the agent's detection manifest over the screen to classify state, emitting
// a pane_agent event whenever the result changes. Identity is process-based; state
// (idle/working/blocked) comes from the manifest rules on the screen + OSC title.
func (h *Host) detectPump(p *pane) {
	t := time.NewTicker(detectInterval)
	defer t.Stop()
	last := "\x00sentinel"
	for {
		select {
		case <-h.done:
			return
		case <-t.C:
		}
		if h.getPane(p.id) == nil {
			return // pane closed/removed
		}
		agent := detect.ForegroundAgent(p.ptmx.Fd())
		state := "unknown"
		var vBlocker, vWorking bool
		if agent != "" {
			screen, title := h.paneScreenAndTitle(p)
			d := detect.Detect(agent, detect.Input{Screen: screen, OscTitle: title})
			if d.SkipStateUpdate {
				continue // e.g. transcript viewer / model picker — keep last reported state
			}
			state = string(d.State)
			vBlocker = d.VisibleBlocker
			vWorking = d.VisibleWorking
		}
		key := fmt.Sprintf("%s\x00%s\x00%t\x00%t", agent, state, vBlocker, vWorking)
		if key != last {
			last = key
			h.emit(NewPaneAgent(p.id, agent, state, vBlocker, vWorking))
		}
	}
}

// paneScreenAndTitle snapshots the pane's screen (rows joined by '\n', trailing
// blanks trimmed) and OSC title for detection — all under emuMu.
func (h *Host) paneScreenAndTitle(p *pane) (screen, title string) {
	p.emuMu.Lock()
	defer p.emuMu.Unlock()
	if p.closed {
		return "", ""
	}
	if t, err := p.emu.Title(); err == nil {
		title = t
	}
	snap, err := p.emu.Snapshot()
	if err != nil {
		return "", title
	}
	rows := make([]string, len(snap.Cells))
	for i, row := range snap.Cells {
		var b strings.Builder
		for _, cell := range row {
			if cell.Rune == "" {
				b.WriteByte(' ')
			} else {
				b.WriteString(cell.Rune)
			}
		}
		rows[i] = strings.TrimRight(b.String(), " ")
	}
	return strings.Join(rows, "\n"), title
}

func (h *Host) feed(p *pane, b []byte) {
	p.emuMu.Lock()
	defer p.emuMu.Unlock()
	if p.closed {
		return
	}
	_, _ = p.emu.Write(b)
}

// takeFrame snapshots the pane, diffs against the last sent snapshot, and
// records the new snapshot — all under emuMu. Returns (nil, nil) if closed.
func (h *Host) takeFrame(p *pane) (*Frame, error) {
	p.emuMu.Lock()
	defer p.emuMu.Unlock()
	if p.closed {
		return nil, nil
	}
	snap, err := p.emu.Snapshot()
	if err != nil {
		return nil, err
	}
	f := FrameFromSnapshot(snap, p.prev)
	p.prev = snap
	return f, nil
}

func (h *Host) resizePane(c Resize) error {
	p := h.getPane(c.PaneID)
	if p == nil {
		return errors.New("no such pane")
	}
	p.ptyMu.Lock()
	err := pty.Setsize(p.ptmx, &pty.Winsize{Cols: c.Cols, Rows: c.Rows})
	p.ptyMu.Unlock()
	if err != nil {
		return fmt.Errorf("pty resize: %w", err)
	}

	p.emuMu.Lock()
	if !p.closed {
		err = p.emu.Resize(c.Cols, c.Rows)
	}
	p.emuMu.Unlock()
	if err != nil {
		return fmt.Errorf("emulator resize: %w", err)
	}
	p.dirty.Store(true) // dimensions changed ⇒ next frame is full
	return nil
}

func (h *Host) flushDirty() {
	h.mu.Lock()
	ps := make([]*pane, 0, len(h.panes))
	for _, p := range h.panes {
		ps = append(ps, p)
	}
	h.mu.Unlock()

	for _, p := range ps {
		if !p.dirty.Swap(false) {
			continue
		}
		f, err := h.takeFrame(p)
		if err != nil {
			h.emit(NewError(p.id, err.Error()))
			continue
		}
		if f != nil {
			h.emit(NewPaneFrame(p.id, f))
		}
	}
}

func (h *Host) closePane(p *pane) {
	p.emuMu.Lock()
	if p.closed {
		p.emuMu.Unlock()
		return
	}
	p.closed = true
	p.emu.Close()
	p.emuMu.Unlock()

	_ = p.ptmx.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

func (h *Host) shutdownAll() {
	h.mu.Lock()
	ps := make([]*pane, 0, len(h.panes))
	for _, p := range h.panes {
		ps = append(ps, p)
	}
	h.panes = make(map[uint32]*pane)
	h.mu.Unlock()
	for _, p := range ps {
		h.closePane(p)
	}
}

func (h *Host) getPane(id uint32) *pane {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.panes[id]
}

func (h *Host) removePane(id uint32) *pane {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.panes[id]
	delete(h.panes, id)
	return p
}

func (h *Host) emit(ev any) {
	select {
	case h.out <- ev:
	case <-h.done:
	}
}

func defaultShell() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/sh"
}

func buildEnv(extra map[string]string) []string {
	env := append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor")
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
