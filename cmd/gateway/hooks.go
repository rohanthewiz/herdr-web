//go:build ghostty

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/rohanthewiz/herdr-web/internal/integration"
)

// The hook-report ingestion seam (the port of herdr's pane.report_agent* API,
// src/app/api/panes.rs): the shell hooks that `herdrctl integration install`
// plants in coding agents dial the unix socket in HERDR_SOCKET_PATH and send
// one newline-terminated JSON request — an agent state transition, a resumable
// session id, or a release. The wire shapes and reply format are herdr's, byte
// for byte, so the installed assets (shared verbatim with the Rust tree) work
// against either server. Env injection is the other half of the seam: createPane
// gives every pane HERDR_ENV/HERDR_PANE_ID/HERDR_SOCKET_PATH via
// integration.PaneEnv, which is what arms the hooks in the first place.
//
// Arbitration (herdr's src/terminal/state.rs, simplified to what the Go side
// models): a hook-reported authority wins over the daemon's process/screen
// detection while it is live; claude-style agents (reserved native sources)
// never get state authority — their hooks only anchor the resume session id and
// detection keeps driving their state; a detected visible blocker overrides a
// non-full-lifecycle hook's non-blocked state; `seq` is a per-source monotonic
// idempotency token; release records a suppression entry so a late duplicate
// report cannot resurrect a finished agent. All state lives on paneRuntime and
// is loop-goroutine only.

// defaultHookSocket is where the hook-report API listens unless overridden by
// config (server.hook_socket) or --hook-socket.
const defaultHookSocket = "/tmp/herdr-hooks.sock"

// hookReadTimeout bounds reading the single request line (herdr's
// INITIAL_REQUEST_TIMEOUT); hookMaxRequest bounds its size (1 MiB).
const (
	hookReadTimeout = 5 * time.Second
	hookMaxRequest  = 1 << 20
)

// Hook API method names (herdr src/api/schema.rs).
const (
	methodReportAgent        = "pane.report_agent"
	methodReportAgentSession = "pane.report_agent_session"
	methodReleaseAgent       = "pane.release_agent"
)

// hookRequest is the one-shot request envelope: {"id","method","params"}.
type hookRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// hookReportParams is the superset of the three methods' params. State is
// required only for pane.report_agent; the session fields ride any of them.
type hookReportParams struct {
	PaneID           string  `json:"pane_id"`
	Source           string  `json:"source"`
	Agent            string  `json:"agent"`
	State            string  `json:"state,omitempty"`
	Message          string  `json:"message,omitempty"`
	CustomStatus     string  `json:"custom_status,omitempty"`
	Seq              *uint64 `json:"seq,omitempty"`
	AgentSessionID   string  `json:"agent_session_id,omitempty"`
	AgentSessionPath string  `json:"agent_session_path,omitempty"`
}

// hookError is a reply error; nil means ok. Codes are herdr's snake_case set.
type hookError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeHookReply writes herdr's reply shape: {"id","result":{"type":"ok"}} on
// success, {"id","error":{"code","message"}} on failure. The hooks ignore the
// reply, but the CLI equivalents (herdr pane report-agent …) parse it, so the
// shape is part of the asset-interop contract.
func writeHookReply(w *net.UnixConn, id string, herr *hookError) {
	var v any
	if herr == nil {
		v = struct {
			ID     string `json:"id"`
			Result struct {
				Type string `json:"type"`
			} `json:"result"`
		}{ID: id, Result: struct {
			Type string `json:"type"`
		}{Type: "ok"}}
	} else {
		v = struct {
			ID    string    `json:"id"`
			Error hookError `json:"error"`
		}{ID: id, Error: *herr}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = w.Write(append(b, '\n'))
}

// serveHooks opens the hook-report socket and serves it until process exit,
// mirroring serveControl: stale-socket cleanup, owner-only 0600 (the hooks run
// as the same user; the path is the capability), non-fatal on failure, and a
// cleanup for the stop hook.
func serveHooks(o *orch, socket string) (cleanup func(), err error) {
	if socket == "" {
		return func() {}, nil
	}
	if isStaleSocket(socket) {
		_ = os.Remove(socket)
	}
	l, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		log.Printf("gateway: hook socket chmod: %v", err)
	}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return // listener closed on shutdown
			}
			go o.serveHookConn(conn.(*net.UnixConn))
		}
	}()
	log.Printf("gateway: hook-report API listening on %s", socket)
	return func() { _ = l.Close(); _ = os.Remove(socket) }, nil
}

// serveHookConn handles one hook connection: one newline-framed request in, one
// reply out (herdr's one-shot transport). A request without a trailing newline
// before EOF still decodes, like ctlproto's reader.
func (o *orch) serveHookConn(conn *net.UnixConn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(hookReadTimeout))
	br := bufio.NewReaderSize(conn, 4096)
	line, err := readHookLine(br)
	if len(line) == 0 && err != nil {
		return
	}
	var req hookRequest
	if err := json.Unmarshal(line, &req); err != nil {
		writeHookReply(conn, "", &hookError{Code: "invalid_request", Message: "invalid request: " + err.Error()})
		return
	}
	switch req.Method {
	case methodReportAgent, methodReportAgentSession, methodReleaseAgent:
	default:
		writeHookReply(conn, req.ID, &hookError{Code: "invalid_request",
			Message: fmt.Sprintf("invalid request: unknown method %q", req.Method)})
		return
	}
	var p hookReportParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeHookReply(conn, req.ID, &hookError{Code: "invalid_request", Message: "invalid request: " + err.Error()})
		return
	}
	done := make(chan *hookError, 1)
	o.post(func() { done <- o.applyHookReport(req.Method, p) })
	select {
	case herr := <-done:
		writeHookReply(conn, req.ID, herr)
	case <-time.After(hookReadTimeout):
		writeHookReply(conn, req.ID, &hookError{Code: "invalid_request", Message: "server busy"})
	}
}

// readHookLine reads one newline-terminated request, bounded by hookMaxRequest.
func readHookLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > hookMaxRequest {
			return nil, fmt.Errorf("request too large")
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return buf, err
	}
}

// --- per-pane hook state (loop-goroutine only) -------------------------------

// hookAuthority is a live hook-reported claim on a pane's agent identity and
// state (herdr's HookAuthority). While present it outranks the daemon's
// detection in effectiveAgent.
type hookAuthority struct {
	source       string
	agent        string
	state        string // idle|working|blocked|unknown
	message      string
	customStatus string
	reportedAt   time.Time
	session      *agentSessionRef
}

// agentSessionRef is a resumable agent-session identity reported by a hook
// (herdr's AgentSessionRef): claude's SessionStart session_id and friends. Kind
// is "id" for every agent except pi, which may report an absolute "path".
type agentSessionRef struct {
	source string
	agent  string
	kind   string // "id" | "path"
	value  string
}

// hookSuppression marks a released/exited agent+session so a late duplicate
// report cannot resurrect it; a report with a different session (a new
// conversation) clears it.
type hookSuppression struct {
	agent   string
	session string
}

// Reserved native state sources (herdr src/agent_resume.rs): these agents'
// hooks report session identity only — their state stays detection-driven, and
// a state or release report from them is downgraded/ignored.
var reservedNativeSources = map[string]bool{
	"herdr:claude": true, "herdr:codex": true, "herdr:copilot": true,
	"herdr:droid": true, "herdr:qodercli": true, "herdr:cursor": true,
}

// Full-lifecycle hook sources (herdr src/detect/mod.rs): agents whose hooks
// report every transition, so a detected visible blocker never overrides them.
var fullLifecycleSources = map[string]bool{
	"herdr:pi": true, "herdr:omp": true, "herdr:hermes": true,
	"herdr:opencode": true, "herdr:kilo": true,
}

// Official agent sources (herdr src/agent_resume.rs is_official_agent_source):
// the only sources whose session refs are recorded (a custom source has no
// resume path). Maps source → the agent label it must report.
var officialAgentSources = map[string]string{
	"herdr:claude": "claude", "herdr:codex": "codex", "herdr:copilot": "copilot",
	"herdr:droid": "droid", "herdr:kimi": "kimi", "herdr:pi": "pi",
	"herdr:hermes": "hermes", "herdr:opencode": "opencode",
	"herdr:qodercli": "qodercli", "herdr:kilo": "kilo", "herdr:cursor": "cursor",
}

func sourcePairIs(m map[string]bool, source, agent string) bool {
	return m[source] && source == "herdr:"+agent
}

// applyHookReport is the loop-side handler for all three hook methods. A nil
// return is the ok reply — including silent drops (stale seq, suppressed or
// mismatched release), which herdr also answers with ok so a hook never
// distinguishes "applied" from "ignored".
func (o *orch) applyHookReport(method string, p hookReportParams) *hookError {
	source := strings.TrimSpace(p.Source)
	if p.PaneID == "" || source == "" {
		return &hookError{Code: "invalid_request", Message: "invalid request: pane_id and source are required"}
	}
	agent := normalizeAgentLabel(p.Agent)
	if agent == "" {
		return &hookError{Code: "invalid_agent", Message: "agent label must not be empty"}
	}
	var rt *paneRuntime
	if id, ok := o.session.PaneByPublicID(p.PaneID); ok {
		rt = o.panes[uint32(id)]
	}
	if rt == nil {
		return &hookError{Code: "pane_not_found", Message: fmt.Sprintf("pane %s not found", p.PaneID)}
	}
	if !rt.acceptHookSeq(source, p.Seq) {
		return nil // stale/duplicate — silently ok
	}
	ref := sessionRefFromReport(source, agent, p)

	switch method {
	case methodReportAgentSession:
		o.setSessionRef(rt, ref)

	case methodReportAgent:
		// Reserved native agents (claude, codex, …) report state through
		// detection, not hooks: record the session identity, ignore the state.
		if sourcePairIs(reservedNativeSources, source, agent) {
			o.setSessionRef(rt, ref)
			return nil
		}
		switch p.State {
		case "idle", "working", "blocked", "unknown":
		default:
			return &hookError{Code: "invalid_request", Message: fmt.Sprintf("invalid request: unknown state %q", p.State)}
		}
		if sup, ok := rt.hookSuppressed[source]; ok {
			if sup.agent == agent && sup.session == sessionValue(ref) {
				return nil // released/exited; a late duplicate cannot resurrect it
			}
			delete(rt.hookSuppressed, source) // a new conversation clears the suppression
		}
		rt.hook = &hookAuthority{
			source: source, agent: agent, state: p.State,
			message: p.Message, customStatus: normalizeCustomStatus(p.CustomStatus),
			reportedAt: time.Now(), session: ref,
		}
		if ref != nil {
			o.setSessionRef(rt, ref)
		}
		o.publishAgent(rt)

	case methodReleaseAgent:
		// Reserved native agents are never hook-released (herdr no-ops these).
		if sourcePairIs(reservedNativeSources, source, agent) {
			return nil
		}
		if rt.hook != nil && (rt.hook.source != source || rt.hook.agent != agent) {
			return nil // someone else's authority — not yours to release
		}
		rt.suppressHook(source, agent)
		changed := rt.hook != nil
		rt.hook = nil
		if rt.agentSession != nil && rt.agentSession.source == source {
			rt.agentSession = nil
			o.noteSessionRefChanged(rt) // a released conversation must not resume
		}
		if changed {
			o.publishAgent(rt)
		}
	}
	return nil
}

// setSessionRef records a resumable session identity (herdr's
// set_agent_session_ref): dropped when the label conflicts with the currently
// detected agent, or when a different session id arrives for the conversation
// already held (herdr's conflicting_current_session_ref — a sub-agent's or
// nested session's id must not clobber the resumable parent session; the held
// ref stays until detection or release clears it). A changed session clears
// any release suppression for the source (it is a new conversation). An
// actual change to the held ref marks the session state dirty — the ref is
// what resume-on-restore persists.
func (o *orch) setSessionRef(rt *paneRuntime, ref *agentSessionRef) {
	if ref == nil {
		return
	}
	if rt.agent != nil && rt.agent.Agent != "" && rt.agent.Agent != ref.agent {
		return
	}
	if cur := rt.agentSession; cur != nil && cur.kind == "id" && ref.kind == "id" &&
		cur.source == ref.source && cur.agent == ref.agent && cur.value != ref.value {
		return
	}
	if sup, ok := rt.hookSuppressed[ref.source]; ok && sup.session != ref.value {
		delete(rt.hookSuppressed, ref.source)
	}
	if cur := rt.agentSession; cur == nil || *cur != *ref {
		rt.agentSession = ref
		o.noteSessionRefChanged(rt)
	}
}

// noteSessionRefChanged supersedes any restart-restored ref for the pane (the
// live lifecycle owns the identity now — a later clear must not resurrect the
// restored one at save time) and arms the debounced session save, since the
// ref is part of what session.json persists.
func (o *orch) noteSessionRefChanged(rt *paneRuntime) {
	delete(o.restoredAgents, rt.id)
	o.saveSoon()
}

// sessionRefFromReport validates a report's session fields into a ref (herdr's
// session_ref_from_report): official sources only; pi prefers the absolute
// path form, everyone else takes the id form only.
func sessionRefFromReport(source, agent string, p hookReportParams) *agentSessionRef {
	if officialAgentSources[source] != agent {
		return nil
	}
	if agent == "pi" && p.AgentSessionPath != "" {
		if v := p.AgentSessionPath; len(v) <= 4096 && strings.HasPrefix(v, "/") && !hasControl(v) {
			return &agentSessionRef{source: source, agent: agent, kind: "path", value: v}
		}
	}
	if v := p.AgentSessionID; v != "" && len(v) <= 512 && !hasControl(v) {
		return &agentSessionRef{source: source, agent: agent, kind: "id", value: v}
	}
	return nil
}

// acceptHookSeq is herdr's accept_hook_report: seq is per-source monotonic; a
// stale or equal seq is dropped, and a missing seq is accepted only as the
// source's first-ever report.
func (rt *paneRuntime) acceptHookSeq(source string, seq *uint64) bool {
	if seq == nil {
		_, seen := rt.hookSeqs[source]
		return !seen
	}
	if last, ok := rt.hookSeqs[source]; ok && *seq <= last {
		return false
	}
	if rt.hookSeqs == nil {
		rt.hookSeqs = make(map[string]uint64)
	}
	rt.hookSeqs[source] = *seq
	return true
}

// suppressHook records the release/exit suppression entry for source, capturing
// the session the agent was on so only that conversation stays dead.
func (rt *paneRuntime) suppressHook(source, agent string) {
	session := ""
	if rt.hook != nil && rt.hook.session != nil {
		session = rt.hook.session.value
	} else if rt.agentSession != nil && rt.agentSession.source == source {
		session = rt.agentSession.value
	}
	if rt.hookSuppressed == nil {
		rt.hookSuppressed = make(map[string]hookSuppression)
	}
	rt.hookSuppressed[source] = hookSuppression{agent: agent, session: session}
}

// sessionValue is ref's value, "" for nil (suppression matching treats a
// ref-less report as the same no-session conversation).
func sessionValue(ref *agentSessionRef) string {
	if ref == nil {
		return ""
	}
	return ref.value
}

// effectiveAgent arbitrates the pane's (agent, state) between the live hook
// authority and the daemon's detection (herdr's recompute_effective_state):
// the hook wins while present, except that a detected visible blocker upgrades
// a non-full-lifecycle hook's non-blocked state for the same agent when the
// detection is not older than the hook report.
func (rt *paneRuntime) effectiveAgent() (agent, state string) {
	if rt.hook != nil {
		agent, state = rt.hook.agent, rt.hook.state
		if state != "blocked" && !sourcePairIs(fullLifecycleSources, rt.hook.source, rt.hook.agent) &&
			rt.agent != nil && rt.agent.VisibleBlocker && rt.agent.Agent == agent &&
			!rt.agentAt.Before(rt.hook.reportedAt) {
			state = "blocked"
		}
		return agent, state
	}
	if rt.agent != nil {
		return rt.agent.Agent, rt.agent.State
	}
	return "", "unknown"
}

// normalizeAgentLabel trims and control-strips a reported agent label, capped
// at 64 chars ("" ⇒ invalid).
func normalizeAgentLabel(s string) string {
	s = stripControl(strings.TrimSpace(s))
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// normalizeCustomStatus trims and control-strips a custom status, capped at 32
// chars (herdr's normalize_custom_status).
func normalizeCustomStatus(s string) string {
	s = stripControl(strings.TrimSpace(s))
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}

func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// paneEnvMap builds a pane's hook environment (HERDR_SOCKET_PATH /
// HERDR_PANE_ID / HERDR_ENV) as the CreatePane.Env map, from
// integration.PaneEnv — the same values `herdrctl integration` documents to
// users, so installed hooks find the socket without any per-agent setup.
func paneEnvMap(socket string, id uint32, publicID string) map[string]string {
	if socket == "" {
		return nil
	}
	env := integration.PaneEnv(socket, uint64(id), publicID)
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

// clearHookOnExit wipes a pane's hook authority and resumable session ref when
// its process exits (herdr: a late in-flight hook packet must not resurrect a
// dead agent, and a dead pane's conversation must not resume on restore — for
// a resumed pane the root process IS the agent) and republishes the arbitrated
// state.
func (o *orch) clearHookOnExit(rt *paneRuntime) {
	if _, restored := o.restoredAgents[rt.id]; restored || rt.agentSession != nil {
		rt.agentSession = nil
		o.noteSessionRefChanged(rt)
	}
	if rt.hook == nil {
		return
	}
	rt.suppressHook(rt.hook.source, rt.hook.agent)
	rt.hook = nil
	o.publishAgent(rt)
}
