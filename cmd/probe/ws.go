// The `ws` subcommand (formerly cmd/wsprobe) is a stdlib-only WebSocket client
// used to verify the gateway end-to-end without a browser: it connects to /ws,
// sends the init message, and prints a summary of the first frame(s) the
// gateway streams back.
package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"
)

func runWSCmd(args []string) error {
	fs := flag.NewFlagSet("probe ws", flag.ExitOnError)
	rawURL := fs.String("url", "ws://localhost:8420/ws", "gateway WebSocket URL")
	cols := fs.Int("cols", 120, "columns")
	rows := fs.Int("rows", 32, "rows")
	msgs := fs.Int("msgs", 8, "total server messages to read")
	sendInput := fs.Bool("send-input", false, "also exercise the browser→herdr input path (sends Ctrl-L; do NOT use against a live session)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runWS(*rawURL, *cols, *rows, *msgs, *sendInput)
}

func runWS(rawURL string, cols, rows, msgs int, sendInput bool) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

	// Minimal RFC6455 client handshake. The key is a fixed nonce; we don't
	// validate Sec-WebSocket-Accept here since this is a local test client.
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", u.RequestURI(), u.Host, key)
	if _, err := conn.Write([]byte(req)); err != nil {
		return err
	}
	br := bufio.NewReader(conn)
	status, _ := br.ReadString('\n')
	if !strings.Contains(status, "101") {
		return fmt.Errorf("expected 101, got %q", strings.TrimSpace(status))
	}
	for { // consume remaining response headers
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		if line == "\r\n" {
			break
		}
	}
	_ = sha1.New // (accept validation omitted intentionally)
	fmt.Println("← 101 Switching Protocols ✓")

	// Send init.
	init, _ := json.Marshal(map[string]any{"t": "init", "cols": cols, "rows": rows})
	if err := writeText(conn, init); err != nil {
		return err
	}
	fmt.Printf("→ init %dx%d\n", cols, rows)

	tally := map[string]int{}
	firstFrame, firstDiff := false, false
	for read := 0; read < msgs; read++ {
		payload, err := readWSFrame(br)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		var msg map[string]json.RawMessage
		if json.Unmarshal(payload, &msg) != nil {
			continue
		}
		var t string
		_ = json.Unmarshal(msg["t"], &t)
		tally[t]++
		switch t {
		case "error":
			var m string
			_ = json.Unmarshal(msg["msg"], &m)
			return fmt.Errorf("gateway error: %s", m)
		case "frame":
			if !firstFrame {
				firstFrame = true
				summarizeFrame(payload, 1)
			}
		case "fdiff":
			if !firstDiff {
				firstDiff = true
				var d struct {
					Cells []json.RawMessage `json:"cells"`
				}
				_ = json.Unmarshal(payload, &d)
				fmt.Printf("← fdiff: %d changed cells, json=%d bytes\n", len(d.Cells), len(payload))
			}
		case "mouse":
			var enabled bool
			_ = json.Unmarshal(msg["enabled"], &enabled)
			fmt.Printf("← mouse capture: %v\n", enabled)
		case "title":
			fmt.Printf("← window title: %s\n", string(msg["title"]))
		}
	}
	fmt.Printf("message tally over %d msgs: %v\n", msgs, tally)

	if sendInput {
		// Confirm the browser→herdr direction is wired. Ctrl-L (redraw) is the
		// least invasive key, but only send it when explicitly requested since
		// it reaches the focused pane of whatever session the gateway targets.
		in, _ := json.Marshal(map[string]string{"t": "input", "data": "\f"})
		if err := writeText(conn, in); err != nil {
			return err
		}
		fmt.Println("→ sent input (Ctrl-L redraw), input path OK")
	}
	return nil
}

func summarizeFrame(payload []byte, n int) {
	var f struct {
		W     int `json:"w"`
		H     int `json:"h"`
		Cells []struct {
			S string `json:"s"`
		} `json:"cells"`
		Cur *struct {
			X   int  `json:"x"`
			Y   int  `json:"y"`
			Vis bool `json:"vis"`
		} `json:"cur"`
	}
	if json.Unmarshal(payload, &f) != nil {
		return
	}
	nonBlank := 0
	for i := range f.Cells {
		if s := f.Cells[i].S; s != "" && s != " " {
			nonBlank++
		}
	}
	fmt.Printf("← frame %d: %dx%d, %d cells (%d non-blank), cursor=%v, json=%d bytes\n",
		n, f.W, f.H, len(f.Cells), nonBlank, f.Cur, len(payload))
	shown := 0
	for y := 0; y < f.H && shown < 5; y++ {
		var b strings.Builder
		for x := 0; x < f.W; x++ {
			s := f.Cells[y*f.W+x].S
			if s == "" {
				s = " "
			}
			b.WriteString(s)
		}
		line := strings.TrimRight(b.String(), " ")
		if line != "" {
			fmt.Printf("   | %s\n", line)
			shown++
		}
	}
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

// readWSFrame reads one server (unmasked) text/binary frame payload; coalescing
// continuation is not needed for our small JSON messages. Named to avoid
// clashing with the wire-protocol frame reader used by the wire subcommand.
func readWSFrame(r *bufio.Reader) ([]byte, error) {
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
	if opcode == 0x8 { // close
		return nil, io.EOF
	}
	return buf, nil
}
