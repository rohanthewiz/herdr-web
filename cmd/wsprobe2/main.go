// Command wsprobe2 is a stdlib-only WebSocket client for the WS9 browser
// protocol (internal/browserproto), used to verify cmd/gateway2 end-to-end
// without a browser. It connects, sends init, folds pane_frame/pane_diff
// into per-pane grids, and runs a small op script against the live session.
//
// Ops (semicolon-separated, via --script):
//
//	wait:MS                 sleep
//	focus:PANE              cmd pane.focus
//	focusdir:left|right|up|down  cmd pane.focus_direction (nearest neighbour)
//	type:TEXT               structured key events per rune (\n = Enter)
//	key:CODE[:MODS]         one named key, MODS letters c/s/a/m (e.g. key:F10, key:KeyC:c)
//	mouse:PANE:X:Y[:BTN]    click (down+up) at cell x,y (btn default 0 = left)
//	click_text:PANE:TEXT    poll until TEXT appears, then click its first cell
//	wheel:PANE:X:Y:DY       wheel event (negative DY = up)
//	scrollcmd:PANE:DELTA    cmd scroll (viewport scrollback)
//	read:PANE:AROW,ACOL:CROW,CCOL[:rect]  cmd read (selection extract), await result
//	                        (AROW/CROW may be "@TEXT" = the row where TEXT appears)
//	readeq:TEXT             assert the last read's text equals TEXT (\n = newline)
//	split:PANE:h|v          cmd pane.split (h = left/right, v = top/bottom)
//	close:PANE              cmd pane.close
//	cycle[:next|prev]       cmd pane.cycle (default next)   swap:DIR  cmd pane.swap
//	zoom:PANE               cmd pane.zoom (toggle)          resizeborder:BORDER:RATIO
//	last                    cmd pane.last                   rename:PANE:NAME  cmd pane.rename
//	rect:PANE:x|y|w|h:eq|lt|gt:N  poll until a pane rect field matches (PANE may be "f")
//	title:PANE:TEXT         poll until a pane's title equals TEXT (PANE may be "f")
//	tabnew                  cmd tab.create        tabfocus:NUM  cmd tab.focus
//	tabclose[:NUM]          cmd tab.close         wsnew         cmd workspace.create
//	wsfocus:ID              cmd workspace.focus (ID e.g. w1)
//	agentfocus:PANE         cmd agent.focus — reveal+focus a pane (may cross ws/tab)
//	reloadconfig            cmd server.reload_config (awaits ack)
//	serverstop              cmd server.stop (awaits ack; gateway then exits)
//	panes:N|tabs:N|workspaces:N  poll until the layout reports N of that kind
//	expect:PANE:TEXT        poll until TEXT appears (PANE may be "f" = focused pane)
//	absent:PANE:TEXT        assert TEXT is NOT currently in the pane grid
//	dump:PANE               print the pane grid
//	modes:PANE:mouse|nomouse poll until the pane's mouse-capture mode matches
//
// Auth: pass --token to send Authorization: Bearer for a WS10-gated gateway;
// use a wss:// URL for TLS (the probe skips cert verification).
//
// Example:
//
//	wsprobe2 --url ws://localhost:8421/ws --script 'wait:800; type:echo hi\n; expect:1:hi; dump:1'
package main

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rohanthewiz/herdr-web/internal/browserproto"
)

func main() {
	rawURL := flag.String("url", "ws://localhost:8421/ws", "gateway2 WebSocket URL")
	cols := flag.Int("cols", 120, "grid columns")
	rows := flag.Int("rows", 32, "grid rows")
	script := flag.String("script", "wait:1000; dump:0", "op script (see doc comment)")
	timeout := flag.Duration("timeout", 8*time.Second, "expect/modes poll timeout")
	life := flag.Duration("life", 120*time.Second, "connection lifetime limit")
	token := flag.String("token", "", "shared access token sent as Authorization: Bearer (WS10 auth)")
	flag.Parse()

	if err := run(*rawURL, *cols, *rows, *script, *timeout, *life, *token); err != nil {
		fmt.Fprintf(os.Stderr, "wsprobe2: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("wsprobe2: PASS")
}

// paneGrid is the probe's fold of one pane's frame stream.
type paneGrid struct {
	W, H  int
	Cells []browserproto.Cell
	Mouse bool
	Alt   bool
	Title string
	Cwd   string
	Agent string
	State string
	Exit  *int
	Fulls int
	Diffs int
}

// probe is the live session state, updated by the reader goroutine.
type probe struct {
	mu     sync.Mutex
	conn   net.Conn
	br     *bufio.Reader
	panes  map[uint32]*paneGrid
	layout *browserproto.Layout
	tally  map[string]int
	errs   []string
	dead   error
	// reads correlates read cmd_results by their command id; lastRead holds the
	// most recent read op's extracted text (asserted by readeq); seq mints ids.
	reads    map[string]*browserproto.CmdResult
	lastRead string
	seq      int
}

func run(rawURL string, cols, rows int, script string, timeout, life time.Duration, token string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	var conn net.Conn
	if u.Scheme == "wss" {
		// Self-signed dev certs: skip verification (the probe is a test client).
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", u.Host,
			&tls.Config{InsecureSkipVerify: true})
	} else {
		conn, err = net.DialTimeout("tcp", u.Host, 5*time.Second)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(life))

	br := bufio.NewReader(conn)
	if err := handshake(conn, br, u, token); err != nil {
		return err
	}

	p := &probe{conn: conn, br: br, panes: map[uint32]*paneGrid{}, tally: map[string]int{},
		reads: map[string]*browserproto.CmdResult{}}

	init := browserproto.Init{T: browserproto.MsgInit, V: browserproto.ProtocolVersion,
		Cols: uint16(cols), Rows: uint16(rows), DPR: 1, CellWPx: 8, CellHPx: 16}
	if err := p.send(init); err != nil {
		return err
	}
	fmt.Printf("→ init v%d %dx%d\n", browserproto.ProtocolVersion, cols, rows)

	go p.reader()

	for i, op := range strings.Split(script, ";") {
		op = strings.TrimSpace(op)
		if op == "" {
			continue
		}
		if err := p.exec(op, timeout); err != nil {
			return fmt.Errorf("op %d %q: %w", i+1, op, err)
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Printf("message tally: %v\n", p.tally)
	if len(p.errs) > 0 {
		fmt.Printf("gateway errors seen (non-fatal): %v\n", p.errs)
	}
	return nil
}

func handshake(conn net.Conn, br *bufio.Reader, u *url.URL, token string) error {
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	auth := ""
	if token != "" {
		auth = "Authorization: Bearer " + token + "\r\n"
	}
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"%sSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", u.RequestURI(), u.Host, auth, key)
	if _, err := conn.Write([]byte(req)); err != nil {
		return err
	}
	status, _ := br.ReadString('\n')
	if !strings.Contains(status, "101") {
		return fmt.Errorf("expected 101, got %q", strings.TrimSpace(status))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		if line == "\r\n" {
			return nil
		}
	}
}

// --- Session state fold --------------------------------------------------------

func (p *probe) reader() {
	for {
		payload, err := readFrame(p.br)
		if err != nil {
			p.mu.Lock()
			p.dead = err
			p.mu.Unlock()
			return
		}
		msg, err := browserproto.DecodeDown(payload)
		if err != nil {
			continue
		}
		p.mu.Lock()
		p.apply(msg)
		p.mu.Unlock()
	}
}

func (p *probe) grid(id uint32) *paneGrid {
	g := p.panes[id]
	if g == nil {
		g = &paneGrid{}
		p.panes[id] = g
	}
	return g
}

func (p *probe) apply(msg any) {
	switch m := msg.(type) {
	case *browserproto.Welcome:
		p.tally["welcome"]++
		if m.Error != "" {
			p.dead = fmt.Errorf("welcome rejected: %s", m.Error)
		}
	case *browserproto.Layout:
		p.tally["layout"]++
		p.layout = m
	case *browserproto.PaneFrame:
		p.tally["pane_frame"]++
		g := p.grid(m.Pane)
		g.W, g.H = int(m.W), int(m.H)
		g.Cells = append([]browserproto.Cell(nil), m.Cells...)
		g.Fulls++
	case *browserproto.PaneDiff:
		p.tally["pane_diff"]++
		g := p.grid(m.Pane)
		for _, dc := range m.Cells {
			if dc.I >= 0 && dc.I < len(g.Cells) {
				g.Cells[dc.I] = dc.Cell
			}
		}
		g.Diffs++
	case *browserproto.PaneModes:
		p.tally["pane_modes"]++
		g := p.grid(m.Pane)
		g.Mouse, g.Alt = m.Mouse, m.AltScreen
	case *browserproto.PaneTitle:
		p.tally["pane_title"]++
		p.grid(m.Pane).Title = m.Title
	case *browserproto.PaneCwd:
		p.tally["pane_cwd"]++
		p.grid(m.Pane).Cwd = m.Cwd
	case *browserproto.PaneAgent:
		p.tally["pane_agent"]++
		g := p.grid(m.Pane)
		g.Agent, g.State = m.Agent, m.State
		fmt.Printf("← pane_agent pane=%d agent=%q state=%s\n", m.Pane, m.Agent, m.State)
	case *browserproto.PaneExited:
		p.tally["pane_exited"]++
		code := m.Code
		p.grid(m.Pane).Exit = &code
	case *browserproto.Error:
		p.tally["error"]++
		p.errs = append(p.errs, m.Msg)
		fmt.Printf("← error: %s\n", m.Msg)
	case *browserproto.CmdResult:
		p.tally["cmd_result"]++
		if m.ID != "" {
			p.reads[m.ID] = m
		}
		if !m.Ok {
			p.errs = append(p.errs, "cmd "+m.ID+": "+m.Error)
		}
	default:
		p.tally[fmt.Sprintf("%T", msg)]++
	}
}

func (g *paneGrid) text() string {
	var b strings.Builder
	for y := 0; y < g.H; y++ {
		var line strings.Builder
		for x := 0; x < g.W; x++ {
			i := y*g.W + x
			s := " "
			if i < len(g.Cells) && g.Cells[i].S != "" {
				s = g.Cells[i].S
			}
			line.WriteString(s)
		}
		b.WriteString(strings.TrimRight(line.String(), " "))
		b.WriteByte('\n')
	}
	return b.String()
}

// find returns the 0-based cell coords of the first occurrence of want
// (matched cell-per-rune, so coords stay column-accurate), or -1,-1.
func (g *paneGrid) find(want string) (int, int) {
	w := []rune(want)
	if len(w) == 0 {
		return -1, -1
	}
	for y := 0; y < g.H; y++ {
		row := make([]rune, g.W)
		for x := 0; x < g.W; x++ {
			row[x] = ' '
			if i := y*g.W + x; i < len(g.Cells) && g.Cells[i].S != "" {
				row[x] = []rune(g.Cells[i].S)[0]
			}
		}
		for x := 0; x+len(w) <= g.W; x++ {
			match := true
			for j, wr := range w {
				if row[x+j] != wr {
					match = false
					break
				}
			}
			if match {
				return x, y
			}
		}
	}
	return -1, -1
}

// --- Ops ------------------------------------------------------------------------

func (p *probe) exec(op string, timeout time.Duration) error {
	name, arg, _ := strings.Cut(op, ":")
	switch name {
	case "wait":
		ms, err := strconv.Atoi(arg)
		if err != nil {
			return err
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
		return nil

	case "focus":
		pane, err := strconv.Atoi(arg)
		if err != nil {
			return err
		}
		cmd, err := browserproto.NewCmd("", browserproto.CmdPaneFocus,
			browserproto.PaneParams{Pane: uint32(pane)})
		if err != nil {
			return err
		}
		fmt.Printf("→ cmd pane.focus %d\n", pane)
		return p.send(cmd)

	case "focusdir":
		if _, ok := browserproto.NavDirection(arg); !ok {
			return fmt.Errorf("focusdir needs left|right|up|down, got %q", arg)
		}
		cmd, err := browserproto.NewCmd("", browserproto.CmdPaneFocusDirection,
			browserproto.DirParams{Dir: arg})
		if err != nil {
			return err
		}
		fmt.Printf("→ cmd pane.focus_direction %s\n", arg)
		return p.send(cmd)

	case "type":
		text := strings.ReplaceAll(arg, `\n`, "\n")
		fmt.Printf("→ type %q\n", text)
		for _, r := range text {
			code, key, mods, ok := keyFor(r)
			if !ok {
				return fmt.Errorf("no key mapping for %q", r)
			}
			for _, kind := range []string{browserproto.KeyDown, browserproto.KeyUp} {
				if err := p.send(browserproto.Key{T: browserproto.MsgKey, Code: code, Key: key, Mods: mods, Kind: kind}); err != nil {
					return err
				}
			}
			time.Sleep(15 * time.Millisecond)
		}
		return nil

	case "key":
		code, modStr, _ := strings.Cut(arg, ":")
		mods := parseMods(modStr)
		key := keyNameFor(code, mods)
		fmt.Printf("→ key %s mods=%d\n", code, mods)
		for _, kind := range []string{browserproto.KeyDown, browserproto.KeyUp} {
			if err := p.send(browserproto.Key{T: browserproto.MsgKey, Code: code, Key: key, Mods: mods, Kind: kind}); err != nil {
				return err
			}
		}
		return nil

	case "mouse":
		f := strings.Split(arg, ":")
		if len(f) < 3 {
			return fmt.Errorf("mouse needs PANE:X:Y[:BTN]")
		}
		pane, _ := strconv.Atoi(f[0])
		x, _ := strconv.Atoi(f[1])
		y, _ := strconv.Atoi(f[2])
		btn := 0
		if len(f) > 3 {
			btn, _ = strconv.Atoi(f[3])
		}
		fmt.Printf("→ mouse click pane=%d cell=%d,%d btn=%d\n", pane, x, y, btn)
		for _, kind := range []string{browserproto.MouseDown, browserproto.MouseUp} {
			m := browserproto.Mouse{T: browserproto.MsgMouse, Pane: uint32(pane),
				X: uint16(x), Y: uint16(y), Btn: uint8(btn), Kind: kind}
			if err := p.send(m); err != nil {
				return err
			}
			time.Sleep(30 * time.Millisecond)
		}
		return nil

	case "click_text":
		pane, want, err := p.paneText(arg)
		if err != nil {
			return err
		}
		deadline := time.Now().Add(timeout)
		for {
			p.mu.Lock()
			g := p.panes[pane]
			x, y := -1, -1
			if g != nil {
				x, y = g.find(want)
			}
			p.mu.Unlock()
			if x >= 0 {
				fmt.Printf("→ click_text pane=%d %q at cell=%d,%d\n", pane, want, x, y)
				for _, kind := range []string{browserproto.MouseDown, browserproto.MouseUp} {
					m := browserproto.Mouse{T: browserproto.MsgMouse, Pane: pane,
						X: uint16(x), Y: uint16(y), Btn: 0, Kind: kind}
					if err := p.send(m); err != nil {
						return err
					}
					time.Sleep(30 * time.Millisecond)
				}
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("timeout: %q not found in pane %d", want, pane)
			}
			time.Sleep(100 * time.Millisecond)
		}

	case "wheel":
		f := strings.Split(arg, ":")
		if len(f) != 4 {
			return fmt.Errorf("wheel needs PANE:X:Y:DY")
		}
		pane, _ := strconv.Atoi(f[0])
		x, _ := strconv.Atoi(f[1])
		y, _ := strconv.Atoi(f[2])
		dy, _ := strconv.Atoi(f[3])
		fmt.Printf("→ wheel pane=%d cell=%d,%d dy=%d\n", pane, x, y, dy)
		return p.send(browserproto.Mouse{T: browserproto.MsgMouse, Pane: uint32(pane),
			X: uint16(x), Y: uint16(y), Btn: browserproto.BtnNone,
			Kind: browserproto.MouseWheel, DY: dy})

	case "scrollcmd":
		f := strings.Split(arg, ":")
		if len(f) != 2 {
			return fmt.Errorf("scrollcmd needs PANE:DELTA")
		}
		pane, _ := strconv.Atoi(f[0])
		delta, _ := strconv.Atoi(f[1])
		cmd, err := browserproto.NewCmd("", browserproto.CmdScroll,
			browserproto.ScrollParams{Pane: uint32(pane), Delta: delta})
		if err != nil {
			return err
		}
		fmt.Printf("→ cmd scroll pane=%d delta=%d\n", pane, delta)
		return p.send(cmd)

	case "read":
		// read:PANE:AROW,ACOL:CROW,CCOL[:rect] — issue read, await its cmd_result,
		// store + print the extracted text (assert exact value with readeq). Anchor
		// and cursor are screen-buffer coordinates (row from the top of scrollback).
		f := strings.Split(arg, ":")
		if len(f) < 3 {
			return fmt.Errorf("read needs PANE:AROW,ACOL:CROW,CCOL[:rect]")
		}
		pane, err := strconv.Atoi(f[0])
		if err != nil {
			return fmt.Errorf("bad pane %q: %w", f[0], err)
		}
		anchor, err := p.parsePoint(uint32(pane), f[1])
		if err != nil {
			return err
		}
		cursor, err := p.parsePoint(uint32(pane), f[2])
		if err != nil {
			return err
		}
		rect := len(f) > 3 && f[3] == "rect"
		id := p.nextID()
		cmd, err := browserproto.NewCmd(id, browserproto.CmdRead,
			browserproto.ReadParams{Pane: uint32(pane), Anchor: anchor, Cursor: cursor, Rect: rect})
		if err != nil {
			return err
		}
		fmt.Printf("→ cmd read id=%s pane=%d anchor=%v cursor=%v rect=%v\n", id, pane, anchor, cursor, rect)
		if err := p.send(cmd); err != nil {
			return err
		}
		deadline := time.Now().Add(timeout)
		for {
			p.mu.Lock()
			res, ok := p.reads[id]
			dead := p.dead
			p.mu.Unlock()
			if ok {
				if !res.Ok {
					return fmt.Errorf("read failed: %s", res.Error)
				}
				var rr browserproto.ReadResult
				if len(res.Data) > 0 {
					if err := json.Unmarshal(res.Data, &rr); err != nil {
						return fmt.Errorf("bad read result data: %w", err)
					}
				}
				p.mu.Lock()
				p.lastRead = rr.Text
				p.mu.Unlock()
				fmt.Printf("← read result %q\n", rr.Text)
				return nil
			}
			if dead != nil {
				return fmt.Errorf("connection died: %w", dead)
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("timeout waiting for read result id=%s", id)
			}
			time.Sleep(50 * time.Millisecond)
		}

	case "readeq":
		// readeq:TEXT — assert the last read op's extracted text equals TEXT (\n
		// for newlines). Exact match proves the daemon selection round-trip.
		want := strings.ReplaceAll(arg, `\n`, "\n")
		p.mu.Lock()
		got := p.lastRead
		p.mu.Unlock()
		if got != want {
			return fmt.Errorf("read text = %q, want %q", got, want)
		}
		fmt.Printf("✓ read text = %q\n", want)
		return nil

	case "split":
		id, dir, ok := strings.Cut(arg, ":")
		if !ok {
			return fmt.Errorf("split needs PANE:h|v (PANE empty or 'f' = focused)")
		}
		pane, err := optPane(id)
		if err != nil {
			return err
		}
		cmd, err := browserproto.NewCmd("", browserproto.CmdPaneSplit,
			browserproto.SplitParams{Pane: pane, Direction: dir})
		if err != nil {
			return err
		}
		fmt.Printf("→ cmd pane.split pane=%s dir=%s\n", id, dir)
		return p.send(cmd)

	case "close":
		pane, err := optPane(arg)
		if err != nil {
			return err
		}
		cmd, err := browserproto.NewCmd("", browserproto.CmdPaneClose,
			browserproto.OptPaneParams{Pane: pane})
		if err != nil {
			return err
		}
		fmt.Printf("→ cmd pane.close pane=%s\n", arg)
		return p.send(cmd)

	case "cycle":
		next := arg != "prev" // anything but "prev" cycles forward
		cmd, err := browserproto.NewCmd("", browserproto.CmdPaneCycle,
			browserproto.CycleParams{Next: next})
		if err != nil {
			return err
		}
		fmt.Printf("→ cmd pane.cycle next=%v\n", next)
		return p.send(cmd)

	case "swap":
		if _, ok := browserproto.NavDirection(arg); !ok {
			return fmt.Errorf("swap needs left|right|up|down, got %q", arg)
		}
		cmd, err := browserproto.NewCmd("", browserproto.CmdPaneSwap,
			browserproto.DirParams{Dir: arg})
		if err != nil {
			return err
		}
		fmt.Printf("→ cmd pane.swap %s\n", arg)
		return p.send(cmd)

	case "zoom":
		pane, err := optPane(arg) // empty/f = focused
		if err != nil {
			return err
		}
		cmd, err := browserproto.NewCmd("", browserproto.CmdPaneZoom,
			browserproto.OptPaneParams{Pane: pane})
		if err != nil {
			return err
		}
		fmt.Printf("→ cmd pane.zoom pane=%s\n", arg)
		return p.send(cmd)

	case "resizeborder":
		bid, ratioStr, ok := strings.Cut(arg, ":")
		if !ok {
			return fmt.Errorf("resizeborder needs BORDER:RATIO (e.g. r:0.3)")
		}
		ratio, err := strconv.ParseFloat(ratioStr, 32)
		if err != nil {
			return fmt.Errorf("bad ratio %q: %w", ratioStr, err)
		}
		cmd, err := browserproto.NewCmd("", browserproto.CmdPaneResizeBorder,
			browserproto.ResizeBorderParams{Border: bid, Ratio: float32(ratio)})
		if err != nil {
			return err
		}
		fmt.Printf("→ cmd pane.resize_border border=%s ratio=%s\n", bid, ratioStr)
		return p.send(cmd)

	case "last":
		cmd, err := browserproto.NewCmd("", browserproto.CmdPaneLast, struct{}{})
		if err != nil {
			return err
		}
		fmt.Println("→ cmd pane.last")
		return p.send(cmd)

	case "rename":
		id, newName, ok := strings.Cut(arg, ":")
		if !ok {
			return fmt.Errorf("rename needs PANE:NAME (PANE numeric or 'f' = focused)")
		}
		pane, err := optPane(id)
		if err != nil {
			return err
		}
		if pane == nil { // resolve "f"/empty to the focused pane id
			fp, ok := p.focusedPane()
			if !ok {
				return fmt.Errorf("no focused pane in layout yet")
			}
			pane = &fp
		}
		cmd, err := browserproto.NewCmd("", browserproto.CmdPaneRename,
			browserproto.RenamePaneParams{Pane: *pane, Name: newName})
		if err != nil {
			return err
		}
		fmt.Printf("→ cmd pane.rename pane=%d name=%q\n", *pane, newName)
		return p.send(cmd)

	case "panes", "tabs", "workspaces":
		want, err := strconv.Atoi(arg)
		if err != nil {
			return err
		}
		deadline := time.Now().Add(timeout)
		for {
			n := p.layoutCount(name)
			if n == want {
				fmt.Printf("✓ %s = %d\n", name, want)
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("timeout: want %d %s, have %d", want, name, n)
			}
			time.Sleep(100 * time.Millisecond)
		}

	case "rect":
		// rect:PANE:FIELD:CMP:VALUE — poll a pane rect from the last layout.
		// PANE may be "f" (focused); FIELD x|y|w|h; CMP eq|lt|gt.
		f := strings.Split(arg, ":")
		if len(f) != 4 {
			return fmt.Errorf("rect needs PANE:x|y|w|h:eq|lt|gt:VALUE")
		}
		fieldIdx := map[string]int{"x": 0, "y": 1, "w": 2, "h": 3}
		fi, ok := fieldIdx[f[1]]
		if !ok {
			return fmt.Errorf("rect field must be x|y|w|h, got %q", f[1])
		}
		want, err := strconv.Atoi(f[3])
		if err != nil {
			return fmt.Errorf("bad rect value %q: %w", f[3], err)
		}
		deadline := time.Now().Add(timeout)
		for {
			var pane uint32
			if f[0] == "f" {
				pane, ok = p.focusedPane()
			} else {
				n, e := strconv.Atoi(f[0])
				pane, ok = uint32(n), e == nil
			}
			var got int
			have := false
			if ok {
				if r, okr := p.paneRect(pane); okr {
					got, have = int(r[fi]), true
				}
			}
			pass := have && ((f[2] == "eq" && got == want) ||
				(f[2] == "lt" && got < want) || (f[2] == "gt" && got > want))
			if pass {
				fmt.Printf("✓ rect pane=%s %s %s %d (got %d)\n", f[0], f[1], f[2], want, got)
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("timeout: rect pane=%s %s %s %d, got %d (have=%v)", f[0], f[1], f[2], want, got, have)
			}
			time.Sleep(100 * time.Millisecond)
		}

	case "title":
		// title:PANE:TEXT — poll until a pane's title equals TEXT exactly (PANE
		// may be "f" = focused). Exact match proves a custom name (pane.rename)
		// overrides — and isn't overwritten by later terminal title events.
		id, want, err := p.paneText(arg)
		if err != nil {
			return err
		}
		deadline := time.Now().Add(timeout)
		for {
			p.mu.Lock()
			g := p.panes[id]
			got := ""
			if g != nil {
				got = g.Title
			}
			p.mu.Unlock()
			if got == want {
				fmt.Printf("✓ title pane=%d = %q\n", id, want)
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("timeout: want title %q in pane %d, got %q", want, id, got)
			}
			time.Sleep(100 * time.Millisecond)
		}

	case "tabnew":
		cmd, err := browserproto.NewCmd("", browserproto.CmdTabCreate, struct{}{})
		if err != nil {
			return err
		}
		fmt.Println("→ cmd tab.create")
		return p.send(cmd)

	case "tabfocus":
		num, err := strconv.Atoi(arg)
		if err != nil {
			return err
		}
		cmd, err := browserproto.NewCmd("", browserproto.CmdTabFocus, browserproto.TabParams{Num: num})
		if err != nil {
			return err
		}
		fmt.Printf("→ cmd tab.focus %d\n", num)
		return p.send(cmd)

	case "tabclose":
		var params browserproto.OptTabParams
		if arg != "" {
			n, err := strconv.Atoi(arg)
			if err != nil {
				return err
			}
			params.Num = &n
		}
		cmd, err := browserproto.NewCmd("", browserproto.CmdTabClose, params)
		if err != nil {
			return err
		}
		fmt.Printf("→ cmd tab.close %s\n", arg)
		return p.send(cmd)

	case "wsnew":
		cmd, err := browserproto.NewCmd("", browserproto.CmdWorkspaceCreate, struct{}{})
		if err != nil {
			return err
		}
		fmt.Println("→ cmd workspace.create")
		return p.send(cmd)

	case "wsfocus":
		cmd, err := browserproto.NewCmd("", browserproto.CmdWorkspaceFocus, browserproto.WorkspaceParams{ID: arg})
		if err != nil {
			return err
		}
		fmt.Printf("→ cmd workspace.focus %s\n", arg)
		return p.send(cmd)

	case "agentfocus":
		// agentfocus:PANE — reveal+focus a pane by id, which (unlike focus) may
		// live in another workspace/tab. Awaits the cmd_result ack.
		pane, err := strconv.Atoi(arg)
		if err != nil {
			return fmt.Errorf("agentfocus needs PANE, got %q", arg)
		}
		fmt.Printf("→ cmd agent.focus pane=%d\n", pane)
		if err := p.awaitCmd(browserproto.CmdAgentFocus, browserproto.PaneParams{Pane: uint32(pane)}, timeout); err != nil {
			return err
		}
		fmt.Printf("✓ agent.focus pane=%d acked\n", pane)
		return nil

	case "reloadconfig":
		fmt.Println("→ cmd server.reload_config")
		if err := p.awaitCmd(browserproto.CmdServerReloadConfig, struct{}{}, timeout); err != nil {
			return err
		}
		fmt.Println("✓ server.reload_config acked")
		return nil

	case "serverstop":
		// server.stop acks, then the gateway broadcasts shutdown and exits ~250ms
		// later; awaiting the ack proves the command round-trips before teardown.
		fmt.Println("→ cmd server.stop")
		if err := p.awaitCmd(browserproto.CmdServerStop, struct{}{}, timeout); err != nil {
			return err
		}
		fmt.Println("✓ server.stop acked")
		return nil

	case "expect":
		pane, want, err := p.paneText(arg)
		if err != nil {
			return err
		}
		deadline := time.Now().Add(timeout)
		for {
			p.mu.Lock()
			g, dead := p.panes[pane], p.dead
			var got string
			if g != nil {
				got = g.text()
			}
			p.mu.Unlock()
			if strings.Contains(got, want) {
				fmt.Printf("✓ expect pane=%d %q\n", pane, want)
				return nil
			}
			if dead != nil {
				return fmt.Errorf("connection died: %w", dead)
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("timeout waiting for %q in pane %d; grid:\n%s", want, pane, got)
			}
			time.Sleep(100 * time.Millisecond)
		}

	case "absent":
		pane, want, err := p.paneText(arg)
		if err != nil {
			return err
		}
		p.mu.Lock()
		g := p.panes[pane]
		var got string
		if g != nil {
			got = g.text()
		}
		p.mu.Unlock()
		if strings.Contains(got, want) {
			return fmt.Errorf("%q unexpectedly present in pane %d", want, pane)
		}
		fmt.Printf("✓ absent pane=%d %q\n", pane, want)
		return nil

	case "modes":
		f := strings.Split(arg, ":")
		if len(f) != 2 {
			return fmt.Errorf("modes needs PANE:mouse|nomouse")
		}
		pane, _ := strconv.Atoi(f[0])
		want := f[1] == "mouse"
		deadline := time.Now().Add(timeout)
		for {
			p.mu.Lock()
			g := p.panes[uint32(pane)]
			ok := g != nil && g.Mouse == want
			p.mu.Unlock()
			if ok {
				fmt.Printf("✓ modes pane=%d mouse=%v\n", pane, want)
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("timeout waiting for pane %d mouse=%v", pane, want)
			}
			time.Sleep(100 * time.Millisecond)
		}

	case "dump":
		pane, _ := strconv.Atoi(arg)
		p.mu.Lock()
		defer p.mu.Unlock()
		if pane == 0 {
			for id, g := range p.panes {
				fmt.Printf("--- pane %d (%dx%d, %d fulls, %d diffs, mouse=%v alt=%v title=%q cwd=%q agent=%q/%s)\n%s",
					id, g.W, g.H, g.Fulls, g.Diffs, g.Mouse, g.Alt, g.Title, g.Cwd, g.Agent, g.State, g.text())
			}
			if p.layout != nil {
				for _, pr := range p.layout.Panes {
					fmt.Printf("layout: pane=%d pub=%s rect=%v inner=%v focused=%v\n",
						pr.Pane, pr.Pub, pr.Rect, pr.Inner, pr.Focused)
				}
			}
			return nil
		}
		g := p.panes[uint32(pane)]
		if g == nil {
			return fmt.Errorf("no pane %d", pane)
		}
		fmt.Printf("--- pane %d (%dx%d, %d fulls, %d diffs)\n%s", pane, g.W, g.H, g.Fulls, g.Diffs, g.text())
		return nil
	}
	return fmt.Errorf("unknown op %q", name)
}

// parsePoint parses "ROW,COL" into a [2]uint32{row, col} selection endpoint.
// ROW may be "@TEXT", resolved to the viewport row where TEXT first appears —
// which, with no scrollback yet, equals the screen-buffer (absolute) row the
// daemon selects on. That lets a test anchor to on-screen content it just typed
// without hardcoding a prompt-dependent row.
func (p *probe) parsePoint(pane uint32, s string) ([2]uint32, error) {
	rowSpec, colSpec, ok := strings.Cut(s, ",")
	if !ok {
		return [2]uint32{}, fmt.Errorf("point needs ROW,COL, got %q", s)
	}
	col, err := strconv.Atoi(colSpec)
	if err != nil {
		return [2]uint32{}, fmt.Errorf("bad col %q: %w", colSpec, err)
	}
	var row int
	if text, isFind := strings.CutPrefix(rowSpec, "@"); isFind {
		p.mu.Lock()
		g := p.panes[pane]
		y := -1
		if g != nil {
			_, y = g.find(text)
		}
		p.mu.Unlock()
		if y < 0 {
			return [2]uint32{}, fmt.Errorf("read anchor text %q not found in pane %d", text, pane)
		}
		row = y
	} else if row, err = strconv.Atoi(rowSpec); err != nil {
		return [2]uint32{}, fmt.Errorf("bad row %q: %w", rowSpec, err)
	}
	return [2]uint32{uint32(row), uint32(col)}, nil
}

// awaitCmd sends a command under a fresh id and blocks until its cmd_result
// arrives, failing on a not-ok result, a dead connection, or timeout. Used by
// ops that assert a command was accepted (agent.focus, server.*).
func (p *probe) awaitCmd(name string, params any, timeout time.Duration) error {
	id := p.nextID()
	cmd, err := browserproto.NewCmd(id, name, params)
	if err != nil {
		return err
	}
	if err := p.send(cmd); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for {
		p.mu.Lock()
		res, ok := p.reads[id]
		dead := p.dead
		p.mu.Unlock()
		if ok {
			if !res.Ok {
				return fmt.Errorf("%s failed: %s", name, res.Error)
			}
			return nil
		}
		if dead != nil {
			return fmt.Errorf("connection died awaiting %s: %w", name, dead)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout awaiting %s result id=%s", name, id)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// nextID mints a unique command id so read results don't collide across ops.
func (p *probe) nextID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	return fmt.Sprintf("read-%d", p.seq)
}

// optPane parses a pane id, returning nil (meaning "the focused pane") for an
// empty id or "f" — used by split/close whose pane param is optional.
func optPane(id string) (*uint32, error) {
	if id == "" || id == "f" {
		return nil, nil
	}
	n, err := strconv.Atoi(id)
	if err != nil {
		return nil, err
	}
	pp := uint32(n)
	return &pp, nil
}

// paneText parses PANE:TEXT. PANE may be "f" for the currently-focused pane
// (from the last layout), so content checks survive pane-id churn.
func (p *probe) paneText(arg string) (uint32, string, error) {
	id, rest, ok := strings.Cut(arg, ":")
	if !ok {
		return 0, "", fmt.Errorf("need PANE:TEXT")
	}
	if id == "f" {
		if pane, ok := p.focusedPane(); ok {
			return pane, rest, nil
		}
		return 0, "", fmt.Errorf("no focused pane in layout yet")
	}
	pane, err := strconv.Atoi(id)
	if err != nil {
		return 0, "", err
	}
	return uint32(pane), rest, nil
}

// layoutCount returns the number of panes/tabs/workspaces in the last layout.
func (p *probe) layoutCount(kind string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.layout == nil {
		return 0
	}
	switch kind {
	case "tabs":
		return len(p.layout.Tabs)
	case "workspaces":
		return len(p.layout.Workspaces)
	default:
		return len(p.layout.Panes)
	}
}

// focusedPane resolves the focused pane id from the last layout.
func (p *probe) focusedPane() (uint32, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.layout != nil {
		for _, pr := range p.layout.Panes {
			if pr.Focused {
				return pr.Pane, true
			}
		}
	}
	return 0, false
}

// paneRect returns a pane's [x,y,w,h] rect from the last layout.
func (p *probe) paneRect(id uint32) (browserproto.Rect, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.layout != nil {
		for _, pr := range p.layout.Panes {
			if pr.Pane == id {
				return pr.Rect, true
			}
		}
	}
	return browserproto.Rect{}, false
}

// --- Key mapping (probe-side convenience; the browser sends real W3C values) ----

var punctCodes = map[rune]string{
	'-': "Minus", '=': "Equal", '[': "BracketLeft", ']': "BracketRight",
	'\\': "Backslash", ';': "Semicolon", '\'': "Quote", '`': "Backquote",
	',': "Comma", '.': "Period", '/': "Slash",
}

var shiftedPunct = map[rune]rune{
	'!': '1', '@': '2', '#': '3', '$': '4', '%': '5', '^': '6', '&': '7',
	'*': '8', '(': '9', ')': '0', '_': '-', '+': '=', '{': '[', '}': ']',
	'|': '\\', ':': ';', '"': '\'', '~': '`', '<': ',', '>': '.', '?': '/',
}

func keyFor(r rune) (code, key string, mods uint8, ok bool) {
	switch {
	case r >= 'a' && r <= 'z':
		return "Key" + strings.ToUpper(string(r)), string(r), 0, true
	case r >= 'A' && r <= 'Z':
		return "Key" + string(r), string(r), browserproto.ModShift, true
	case r >= '0' && r <= '9':
		return "Digit" + string(r), string(r), 0, true
	case r == ' ':
		return "Space", " ", 0, true
	case r == '\n' || r == '\r':
		return "Enter", "Enter", 0, true
	case r == '\t':
		return "Tab", "Tab", 0, true
	}
	if c, found := punctCodes[r]; found {
		return c, string(r), 0, true
	}
	if base, found := shiftedPunct[r]; found {
		if c, f2 := punctCodes[base]; f2 {
			return c, string(r), browserproto.ModShift, true
		} else if base >= '0' && base <= '9' {
			return "Digit" + string(base), string(r), browserproto.ModShift, true
		}
	}
	return "", "", 0, false
}

func parseMods(s string) uint8 {
	var m uint8
	for _, c := range s {
		switch c {
		case 'c':
			m |= browserproto.ModCtrl
		case 's':
			m |= browserproto.ModShift
		case 'a':
			m |= browserproto.ModAlt
		case 'm':
			m |= browserproto.ModMeta
		}
	}
	return m
}

// keyNameFor derives the W3C .key value for a named .code the way a browser
// would for the common probe cases (letters/digits; everything else keeps
// the code name, which matches named keys like Enter/F10/ArrowUp).
func keyNameFor(code string, mods uint8) string {
	if rest, ok := strings.CutPrefix(code, "Key"); ok && len(rest) == 1 {
		if mods&browserproto.ModShift != 0 {
			return rest
		}
		return strings.ToLower(rest)
	}
	if rest, ok := strings.CutPrefix(code, "Digit"); ok && len(rest) == 1 && mods&browserproto.ModShift == 0 {
		return rest
	}
	return code
}

// --- Wire helpers (RFC6455 client frames, mirrors cmd/wsprobe) -------------------

func (p *probe) send(m any) error {
	b, err := browserproto.Marshal(m)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dead != nil {
		return fmt.Errorf("connection died: %w", p.dead)
	}
	return writeText(p.conn, b)
}

// writeText writes a masked client text frame (RFC6455 §5).
func writeText(w io.Writer, payload []byte) error {
	var hdr []byte
	hdr = append(hdr, 0x81) // FIN + text
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, byte(0x80|n))
	case n < 65536:
		hdr = append(hdr, 0x80|126)
		var ext [2]byte
		binary.BigEndian.PutUint16(ext[:], uint16(n))
		hdr = append(hdr, ext[:]...)
	default:
		hdr = append(hdr, 0x80|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		hdr = append(hdr, ext[:]...)
	}
	mask := []byte{0x12, 0x34, 0x56, 0x78}
	hdr = append(hdr, mask...)
	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(masked)
	return err
}

// readFrame reads one server frame payload (control frames skipped).
func readFrame(r *bufio.Reader) ([]byte, error) {
	for {
		b0, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		opcode := b0 & 0x0f
		b1, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		masked := b1&0x80 != 0
		n := int(b1 & 0x7f)
		switch n {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(r, ext[:]); err != nil {
				return nil, err
			}
			n = int(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(r, ext[:]); err != nil {
				return nil, err
			}
			n = int(binary.BigEndian.Uint64(ext[:]))
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(r, mask[:]); err != nil {
				return nil, err
			}
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		if masked {
			for i := range buf {
				buf[i] ^= mask[i%4]
			}
		}
		switch opcode {
		case 0x8: // close
			return nil, io.EOF
		case 0x9, 0xa: // ping/pong: ignore
			continue
		}
		return buf, nil
	}
}
