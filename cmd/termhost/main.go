//go:build ghostty

// Command termhost is the Phase B Go terminal backend daemon: it listens on a
// Unix socket and serves the orchestration protocol (internal/orchestration),
// owning PTYs + VT emulation per pane. The Rust herdr server connects to it as
// the orchestrator (workspace/pane tree, layout, detection, session) and drives
// panes through the seam.
//
// Build requires libghostty-vt on PKG_CONFIG_PATH and -tags ghostty;
// see scripts/build-libghostty-vt.sh.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rohanthewiz/herdr-web/internal/orchestration"
)

func main() {
	socket := flag.String("socket", "/tmp/herdr-termhost.sock", "unix socket path to listen on")
	exitOnDisconnect := flag.Bool("exit-on-disconnect", false,
		"exit after the first client disconnects (managed mode: the orchestrator owns our lifecycle)")
	persistent := flag.Bool("persistent", false,
		"keep panes alive across client disconnects; a restarted/handed-off herdr reconnects and resyncs (overrides -exit-on-disconnect)")
	idleTimeout := flag.Duration("idle-timeout", 10*time.Minute,
		"in persistent mode, exit if no client is attached for this long (0 disables)")
	flag.Parse()

	var err error
	if *persistent {
		err = runPersistent(*socket, *idleTimeout)
	} else {
		err = run(*socket, *exitOnDisconnect)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "termhost:", err)
		os.Exit(1)
	}
}

func run(socket string, exitOnDisconnect bool) error {
	// Remove a stale socket from a previous run.
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale socket: %w", err)
	}

	ln, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer ln.Close()
	defer os.Remove(socket)

	// SIGHUP too: in managed mode the orchestrator is our parent, so its exit (or a
	// closed controlling terminal) hangs us up — treat that as a graceful shutdown
	// so the deferred socket cleanup runs instead of the default terminate.
	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	go func() {
		<-ctx.Done()
		ln.Close() // unblock Accept
	}()

	log.Printf("termhost listening on %s (protocol v%d)", socket, orchestration.ProtocolVersion)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // shutting down
			}
			return fmt.Errorf("accept: %w", err)
		}
		log.Printf("client connected")
		serve := func() {
			defer conn.Close()
			h := orchestration.NewHost()
			if err := h.Serve(ctx, conn); err != nil {
				log.Printf("session ended: %v", err)
			} else {
				log.Printf("client disconnected")
			}
		}
		// Managed mode: the orchestrator spawned us and is our only client, so
		// serve it inline and exit when it disconnects (a backstop against a crashed
		// parent leaving us listening forever). Standalone/dev mode keeps the
		// goroutine-per-connection loop so it can serve reconnects.
		if exitOnDisconnect {
			// Close the connection on shutdown so a blocked Serve read unblocks and
			// the graceful exit path (deferred socket removal) runs even when the
			// signal, not a client EOF, ends the session.
			go func() {
				<-ctx.Done()
				conn.Close()
			}()
			serve()
			log.Printf("exiting after client disconnect (managed mode)")
			return nil
		}
		go serve()
	}
}

// runPersistent serves a single long-lived Host whose panes outlive any one
// client. A herdr that restarts or hands off reconnects to this same daemon and
// resyncs its surviving panes (the create_pane-less path). The daemon exits on a
// clean-quit shutdown command, on the idle timeout, or on a signal.
func runPersistent(socket string, idleTimeout time.Duration) error {
	// Remove a stale socket from a previous run.
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale socket: %w", err)
	}

	ln, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer ln.Close()
	defer os.Remove(socket)

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	h := orchestration.NewHost()
	h.Persistent = true
	h.IdleTimeout = idleTimeout
	h.Start(ctx)
	defer h.Stop()

	// Unblock Accept on a signal or a shutdown command / idle timeout. The Host owns
	// panes, so closing the listener here is a clean exit (deferred Stop tears them
	// down and the deferred socket removal runs).
	go func() {
		select {
		case <-ctx.Done():
		case <-h.Exit():
		}
		ln.Close()
	}()

	log.Printf("termhost listening on %s (persistent, protocol v%d)", socket, orchestration.ProtocolVersion)

	for {
		conn, err := ln.Accept()
		if err != nil {
			// Listener closed by the shutdown goroutine: a clean exit.
			if ctx.Err() != nil {
				return nil
			}
			select {
			case <-h.Exit():
				return nil
			default:
			}
			return fmt.Errorf("accept: %w", err)
		}
		log.Printf("client connected")
		// Serial Attach is the single-writer guarantee: a second client waits in the
		// accept backlog until the current one detaches. Panes survive the gap.
		if err := h.Attach(ctx, conn); err != nil {
			log.Printf("session ended: %v", err)
		} else {
			log.Printf("client disconnected (panes preserved)")
		}
		conn.Close()
	}
}
