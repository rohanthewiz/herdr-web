// The `wire` subcommand (formerly cmd/smoke) connects to a running herdr
// server's client socket, performs the Hello handshake, reads a few semantic
// frames, and prints a summary. It sends no input and Detaches cleanly, so it
// is safe against a live session.
package main

import (
	"flag"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/rohanthewiz/herdr-web/internal/wire"
)

func runWireCmd(args []string) error {
	fs := flag.NewFlagSet("probe wire", flag.ExitOnError)
	socket := fs.String("socket", defaultSocket(), "path to herdr-client.sock")
	cols := fs.Int("cols", 120, "terminal columns to request")
	rows := fs.Int("rows", 32, "terminal rows to request")
	frames := fs.Int("frames", 3, "number of frames to read before detaching")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runWire(*socket, uint16(*cols), uint16(*rows), *frames)
}

func runWire(socket string, cols, rows uint16, wantFrames int) error {
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial %s: %w", socket, err)
	}
	defer conn.Close()

	// Handshake.
	if err := wire.WriteFrame(conn, wire.EncodeHello(cols, rows, 0, 0)); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}
	fmt.Printf("→ sent Hello (proto %d, %dx%d, SemanticFrame)\n", wire.ProtocolVersion, cols, rows)

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	gotWelcome := false
	frameCount := 0
	for frameCount < wantFrames {
		payload, err := wire.ReadFrame(conn)
		if err != nil {
			return fmt.Errorf("read frame: %w", err)
		}
		msg, err := wire.DecodeServerMessage(payload)
		if err != nil {
			return fmt.Errorf("decode (%d bytes): %w", len(payload), err)
		}
		switch msg.Kind {
		case 0: // Welcome
			w := msg.Welcome
			gotWelcome = true
			if w.Error != nil {
				return fmt.Errorf("server rejected handshake: %s (server proto %d)", *w.Error, w.Version)
			}
			fmt.Printf("← Welcome: server proto %d, encoding %d, no error ✓\n", w.Version, w.Encoding)
		case 1: // Frame
			f := msg.Frame
			frameCount++
			printFrameSummary(f, frameCount)
		default:
			fmt.Printf("← (server message kind %d, %d bytes, skipped)\n", msg.Kind, len(payload))
		}
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	}

	if err := wire.WriteFrame(conn, wire.EncodeDetach()); err != nil {
		return fmt.Errorf("send detach: %w", err)
	}
	fmt.Println("→ sent Detach, closing")
	if !gotWelcome {
		return fmt.Errorf("never received Welcome")
	}
	return nil
}

func printFrameSummary(f *wire.Frame, n int) {
	nonBlank := 0
	for i := range f.Cells {
		if s := f.Cells[i].Symbol; s != "" && s != " " {
			nonBlank++
		}
	}
	fmt.Printf("← Frame %d: %dx%d, %d cells (%d non-blank), %d hyperlinks, cursor=%v\n",
		n, f.Width, f.Height, len(f.Cells), nonBlank, len(f.Hyperlinks), f.Cursor)

	// Reconstruct the first few non-empty rows as plain text, as a sanity check
	// that the cell grid decoded coherently.
	if f.Width == 0 || f.Height == 0 || len(f.Cells) != int(f.Width)*int(f.Height) {
		return
	}
	shown := 0
	for y := 0; y < int(f.Height) && shown < 6; y++ {
		var b strings.Builder
		for x := 0; x < int(f.Width); x++ {
			s := f.Cells[y*int(f.Width)+x].Symbol
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
