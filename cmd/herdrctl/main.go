// Command herdrctl is the local control-API client for a running herdr server
// (cmd/gateway2). It speaks internal/ctlproto over the control unix socket and
// drives the very same §7 command table as the browser front-end — the proof
// that the app.Dispatcher seam serves a non-browser caller with no per-command
// server code. It links no libghostty (untagged): it is a pure socket client.
//
// Usage:
//
//	herdrctl [flags] <verb> [args...]           ergonomic subcommand
//	herdrctl [flags] <method> [--params '<json>']  raw §7 command (escape hatch)
//	herdrctl help                               list the ergonomic verbs
//	herdrctl commands                           list the raw §7 method names
//
// Ergonomic verbs build the params for you from positional args (`herdrctl help`
// lists them). Examples:
//
//	herdrctl split h 2      → pane.split {"direction":"h","pane":2}
//	herdrctl focus 1        → pane.focus {"pane":1}
//	herdrctl panes          → pane.list
//	herdrctl new-tab        → tab.create
//	herdrctl stop           → server.stop
//
// Two verbs block/stream instead of returning at once: `wait <pane> <pattern>`
// resolves when the pane's output contains the pattern (or times out), and
// `events [pane]` streams pane events (exit/agent/title/cwd) until Ctrl-C:
//
//	herdrctl wait 1 "BUILD SUCCESSFUL" 120   → pane.wait_for_output (120s)
//	herdrctl events 1                        → events.subscribe {"pane":1}
//
// The raw form reaches any §7 command directly (and the rarely-scripted options
// like read's rect or capture's ansi/unwrap that the ergonomic verbs omit):
//
//	herdrctl read    --params '{"pane":1,"anchor":[0,0],"cursor":[0,5],"rect":true}'
//	herdrctl capture --params '{"pane":1,"scope":1,"lines":100,"ansi":true}'
//
// Global flags go before the verb (e.g. `herdrctl --socket … split h`). Exit
// status: 0 on success, 1 if the command failed, 2 on a usage/transport error.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"time"

	"github.com/rohanthewiz/herdr-web/internal/app"
	"github.com/rohanthewiz/herdr-web/internal/ctlproto"
)

func main() { os.Exit(run()) }

func run() int {
	socket := flag.String("socket", "",
		"control socket path (env "+ctlproto.SocketEnvVar+"; default "+ctlproto.DefaultSocket+")")
	paramsJSON := flag.String("params", "", "command params as a JSON object")
	timeout := flag.Duration("timeout", 10*time.Second, "round-trip timeout")
	id := flag.String("id", "1", "correlation id echoed in the response")
	rawJSON := flag.Bool("json", false, "print the full JSON response (one line) instead of just the result")
	flag.Usage = usage
	flag.Parse()

	// flag stops at the first positional (the method); re-parse the tail so flags
	// may also appear after the method, e.g. `herdrctl read --params '{...}'`.
	rest := flag.Args()
	if len(rest) == 0 {
		usage()
		return 2
	}
	method := rest[0]
	_ = flag.CommandLine.Parse(rest[1:])
	pos := flag.Args() // positional args after the verb (an ergonomic verb's operands)

	switch method {
	case "help", "-h", "--help":
		usage()
		return 0
	case "commands":
		for _, n := range app.CommandNames() {
			fmt.Println(n)
		}
		return 0
	}

	// Resolve the verb: an ergonomic subcommand (which builds the params from
	// positional args) takes precedence; otherwise the raw `<method> --params`
	// path — the full-coverage escape hatch — carries the request through.
	var params json.RawMessage
	if sc, ok := lookupSubcommand(method); ok {
		if *paramsJSON != "" {
			fmt.Fprintf(os.Stderr, "herdrctl: %s takes positional arguments, not --params\n", method)
			return 2
		}
		built, err := sc.build(pos)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herdrctl: %v\n", err)
			return 2
		}
		method = sc.method
		params = built
	} else {
		// Validate the method locally so a typo lists the vocabulary instead of a
		// round trip to the server's "not supported yet" default. ping and the
		// streaming events.subscribe are transport methods outside the §7 table.
		if method != ctlproto.MethodPing && method != ctlproto.MethodEventsSubscribe &&
			!slices.Contains(app.CommandNames(), method) {
			fmt.Fprintf(os.Stderr, "herdrctl: unknown command %q (try `herdrctl help`)\n", method)
			return 2
		}
		if len(pos) > 0 {
			fmt.Fprintf(os.Stderr, "herdrctl: unexpected extra arguments: %v\n", pos)
			return 2
		}
		if *paramsJSON != "" {
			if !json.Valid([]byte(*paramsJSON)) {
				fmt.Fprintln(os.Stderr, "herdrctl: --params is not valid JSON")
				return 2
			}
			params = json.RawMessage(*paramsJSON)
		}
	}

	socketPath := ctlproto.ResolveSocket(*socket)

	// events.subscribe streams frames until interrupted — a different transport
	// than the unary Call below.
	if method == ctlproto.MethodEventsSubscribe {
		return runEvents(socketPath, *id, params)
	}

	// wait_for_output blocks until its pattern appears; size the round-trip deadline
	// to cover the wait itself (unless an explicit --timeout is already larger).
	callTimeout := *timeout
	if method == app.CmdWaitForOutput {
		var wp app.WaitForOutputParams
		_ = json.Unmarshal(params, &wp)
		if need := app.WaitTimeout(wp.TimeoutMs) + 10*time.Second; callTimeout < need {
			callTimeout = need
		}
	}

	resp, err := ctlproto.Call(socketPath,
		ctlproto.Request{ID: *id, Method: method, Params: params}, callTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herdrctl: %v\n", err)
		return 2
	}

	if *rawJSON {
		b, _ := json.Marshal(resp)
		fmt.Println(string(b))
	} else {
		printResult(resp)
	}
	if !resp.OK {
		return 1
	}
	return 0
}

// printResult renders a Response for a human: the pretty-printed Data payload on
// success (or "ok" when a command yields none), or the error on failure.
func printResult(resp ctlproto.Response) {
	if !resp.OK {
		fmt.Fprintf(os.Stderr, "error: %s\n", resp.Error)
		return
	}
	if len(resp.Data) == 0 {
		fmt.Println("ok")
		return
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, resp.Data, "", "  "); err != nil {
		fmt.Println(string(resp.Data))
		return
	}
	fmt.Println(buf.String())
}

// runEvents opens an events.subscribe stream and prints each event as one line of
// JSON until the server closes it or the user interrupts (Ctrl-C). Exit 0 on a
// clean end (server stop or Ctrl-C), 2 on a transport/subscription error.
func runEvents(socket, id string, params json.RawMessage) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	req := ctlproto.Request{ID: id, Method: ctlproto.MethodEventsSubscribe, Params: params}
	err := ctlproto.Subscribe(ctx, socket, req, func(ev ctlproto.Event) error {
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "herdrctl: %v\n", err)
		return 2
	}
	return 0
}

func usage() {
	fmt.Fprint(os.Stderr, `herdrctl — local control-API client for a running herdr server (cmd/gateway2)

Usage:
  herdrctl [flags] <verb> [args...]            ergonomic subcommand
  herdrctl [flags] <method> [--params '<json>']   raw §7 command (escape hatch)
  herdrctl commands                            list the raw §7 method names

Verbs:
`)
	fmt.Fprint(os.Stderr, subcommandsHelp())
	fmt.Fprint(os.Stderr, `
Global flags (place before the verb):
`)
	flag.PrintDefaults()
}
