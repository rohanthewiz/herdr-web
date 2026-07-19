// Command probe bundles herdr-web's headless verification tools into a single
// binary, dispatched by subcommand. Each subcommand exercises a different layer
// of the stack, which is why both exist:
//
//	probe wire  — dials the herdr server's Unix socket directly and speaks the
//	              bincode wire protocol (formerly cmd/smoke). Verifies the
//	              codec + handshake with no gateway involved.
//	probe ws    — connects to the gateway's /ws endpoint as a stdlib-only
//	              WebSocket client (formerly cmd/wsprobe). Verifies the full
//	              browser↔gateway↔herdr chain without a browser.
//
//	                 probe wire ─────────────────┐
//	                                             ▼
//	  probe ws ──► gateway (:8420) ──► herdr-client.sock ──► herdr server
//
// Both are read-only against a live session unless explicitly told otherwise
// (see `probe ws --send-input`).
//
// Usage:
//
//	probe wire [--socket ~/.config/herdr/herdr-client.sock] [--cols 120 --rows 32 --frames 3]
//	probe ws   [--url ws://localhost:8420/ws] [--cols 120 --rows 32 --msgs 8] [--send-input]
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	// Each subcommand owns its own flag.FlagSet (see wire.go / ws.go) so their
	// flags stay independent — e.g. --frames belongs to wire, --msgs to ws.
	var err error
	switch os.Args[1] {
	case "wire":
		err = runWireCmd(os.Args[2:])
	case "ws":
		err = runWSCmd(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "probe: unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `probe — headless verification tools for herdr-web

Subcommands:
  wire   dial the herdr server socket directly; handshake + read frames (read-only)
  ws     drive the gateway's /ws WebSocket end-to-end without a browser

Run "probe <subcommand> -h" for that subcommand's flags.
`)
}

// defaultSocket mirrors the gateway's default so the two tools point at the
// same herdr session out of the box.
func defaultSocket() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "herdr", "herdr-client.sock")
}
