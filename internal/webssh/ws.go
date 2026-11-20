package webssh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

// Subprotocol names the wire format. Browsers that connect without offering it
// still work; it exists so an unrelated WebSocket client can't be mistaken for
// a terminal.
const Subprotocol = "bw-ssh.v1"

// Frame limits. Input frames carry keystrokes and pastes; output is chunked so
// one enormous write cannot monopolize the socket or blow up the browser.
const (
	maxControlFrame = 64 << 10
	maxOutputChunk  = 32 << 10
	writeTimeout    = 20 * time.Second
	pingInterval    = 30 * time.Second
	pingTimeout     = 10 * time.Second
)

// Wire protocol.
//
//	client → server (text)  {"t":"in","d":"ls -l\n"}
//	                        {"t":"resize","cols":120,"rows":40}
//	                        {"t":"ping"}
//	server → client (binary) raw terminal output, byte-exact
//	server → client (text)  {"t":"status","msg":"…"}      progress while dialing
//	                        {"t":"ready","host":"1.2.3.4:22","user":"root",
//	                                    "host_key":"SHA256:…","pinned":false}
//	                        {"t":"err","msg":"…"}          fatal, session is over
//	                        {"t":"exit","code":0}          remote shell exited
//	                        {"t":"pong"}
//
// Binary for output keeps bytes intact (`cat somebinary` must not be mangled by
// a UTF-8 round trip); JSON for control keeps it debuggable.
type clientMsg struct {
	T    string `json:"t"`
	D    string `json:"d,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

type controlMsg struct {
	T       string `json:"t"`
	Msg     string `json:"msg,omitempty"`
	Host    string `json:"host,omitempty"`
	User    string `json:"user,omitempty"`
	HostKey string `json:"host_key,omitempty"`
	Pinned  bool   `json:"pinned,omitempty"`
	Code    *int   `json:"code,omitempty"`
}

// Options configure one bridged session.
type Options struct {
	// Target is where to connect and how to authenticate.
	Target Target
	// DialTimeout bounds TCP connect + SSH handshake. 0 → DefaultDialTimeout.
	DialTimeout time.Duration
	// IdleTimeout closes a session with no input and no output for that long.
	// 0 → DefaultIdleTimeout; negative disables it.
	IdleTimeout time.Duration
	// OriginPatterns whitelists browser Origins for the WebSocket handshake,
	// e.g. ["vps.example.com", "*.example.com"]. Empty keeps the default
	// same-origin check; ["*"] disables it. Needed when a reverse proxy rewrites
	// the Host header, because the Origin then no longer matches it. (Cross-site
	// abuse is still blocked by the SameSite=Lax session cookie.)
	OriginPatterns []string
	// Log records lifecycle events. It must never receive credentials or
	// terminal content.
	Log func(format string, args ...any)
}

// ErrIdleTimeout reports a session closed for inactivity.
var ErrIdleTimeout = errors.New("ssh 会话空闲超时")

// ServeWS upgrades an HTTP request to WebSocket and bridges it to a fresh SSH
// PTY. It blocks for the lifetime of the session and returns the reason it ended
// (nil for a clean exit), so the caller can log it.
func ServeWS(w http.ResponseWriter, r *http.Request, o Options) error {
	logf := o.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}

	conn, err := websocket.Accept(w, r, acceptOptions(o.OriginPatterns))
	if err != nil {
		return fmt.Errorf("websocket upgrade: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxControlFrame)

	ctx := r.Context() // cancelled as soon as the browser goes away
	wc := &wsConn{c: conn}
	addr := targetAddr(o.Target)

	_ = wc.writeText(ctx, controlMsg{T: "status", Msg: "正在连接 " + addr + " …"})

	dialTimeout := o.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = DefaultDialTimeout
	}
	dialCtx, cancelDial := context.WithTimeout(ctx, dialTimeout+DefaultHandshakeTimeout)
	defer cancelDial()

	sess, err := Dial(dialCtx, o.Target, dialTimeout)
	if err != nil {
		_ = wc.writeText(ctx, controlMsg{T: "err", Msg: err.Error()})
		_ = conn.Close(websocket.StatusInternalError, "dial failed")
		return err
	}
	defer sess.Close()

	user := o.Target.User
	if user == "" {
		user = "root"
	}
	fp := sess.HostKeyFingerprint()
	pinned := o.Target.HostKeyFingerprint != ""
	_ = wc.writeText(ctx, controlMsg{T: "ready", Host: addr, User: user, HostKey: fp, Pinned: pinned})
	logf("SSH 终端已连接 %s → %s@%s (host key %s, pinned=%t)", o.Target.Label, user, addr, fp, pinned)

	// Activity tracking drives the idle timeout.
	var lastActive atomic.Int64
	lastActive.Store(time.Now().UnixNano())
	touch := func() { lastActive.Store(time.Now().UnixNano()) }

	sink := &wsSink{wc: wc, ctx: ctx, touch: touch}

	shellDone := make(chan error, 1)
	go func() { shellDone <- sess.Attach(sink) }()

	readerDone := make(chan error, 1)
	go func() { readerDone <- readLoop(ctx, conn, sess, touch) }()

	idle := o.IdleTimeout
	if idle == 0 {
		idle = DefaultIdleTimeout
	}
	tick := time.NewTicker(pingInterval)
	defer tick.Stop()

	var cause error
loop:
	for {
		select {
		case err := <-shellDone:
			code := exitCodeOf(err)
			_ = wc.writeText(ctx, controlMsg{T: "exit", Code: &code})
			if err != nil && !errors.Is(err, io.EOF) {
				cause = err
			}
			break loop
		case err := <-readerDone:
			cause = err
			break loop
		case <-ctx.Done():
			cause = ctx.Err()
			break loop
		case <-tick.C:
			pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
			perr := conn.Ping(pingCtx)
			cancel()
			if perr != nil {
				cause = perr
				break loop
			}
			if idle > 0 && time.Since(time.Unix(0, lastActive.Load())) > idle {
				_ = wc.writeText(ctx, controlMsg{T: "err", Msg: "会话空闲超过 " + idle.String() + ",已断开"})
				cause = ErrIdleTimeout
				break loop
			}
		}
	}

	// Tear down both halves, then tell the browser why.
	_ = sess.Close()
	code := websocket.StatusNormalClosure
	reason := "session closed"
	if cause != nil && !errors.Is(cause, context.Canceled) {
		code = websocket.StatusGoingAway
		reason = truncateReason(cause.Error())
		_ = wc.writeText(context.WithoutCancel(ctx), controlMsg{T: "err", Msg: cause.Error()})
	}
	_ = conn.Close(code, reason)
	logf("SSH 终端断开 %s → %s (%v)", o.Target.Label, addr, cause)
	if cause != nil && (errors.Is(cause, context.Canceled) || errors.Is(cause, ErrIdleTimeout) || websocket.CloseStatus(cause) >= 0) {
		return nil // normal end of life, not worth an error log
	}
	return cause
}

// acceptOptions builds the WebSocket handshake options. A single "*" pattern
// means "don't check the Origin at all", which coder/websocket spells as
// InsecureSkipVerify.
func acceptOptions(patterns []string) *websocket.AcceptOptions {
	opts := &websocket.AcceptOptions{Subprotocols: []string{Subprotocol}}
	for _, p := range patterns {
		if strings.TrimSpace(p) == "*" {
			opts.InsecureSkipVerify = true
			return opts
		}
	}
	if len(patterns) > 0 {
		opts.OriginPatterns = patterns
	}
	return opts
}

// FailWS upgrades the connection only to deliver an error and close it again.
// A plain HTTP error body is invisible to a WebSocket client, so pre-flight
// failures (no credentials, unknown address, too many sessions) are reported this
// way to keep them visible in the terminal pane. originPatterns mirrors Options.
func FailWS(w http.ResponseWriter, r *http.Request, msg string, originPatterns ...string) {
	conn, err := websocket.Accept(w, r, acceptOptions(originPatterns))
	if err != nil {
		http.Error(w, msg, http.StatusBadGateway)
		return
	}
	defer conn.CloseNow()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	data, _ := json.Marshal(controlMsg{T: "err", Msg: msg})
	_ = conn.Write(ctx, websocket.MessageText, data)
	_ = conn.Close(websocket.StatusPolicyViolation, truncateReason(msg))
}

// readLoop pumps browser input into the shell until the socket closes or the
// peer sends something unparseable.
func readLoop(ctx context.Context, conn *websocket.Conn, sess *Session, touch func()) error {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		touch()
		switch typ {
		case websocket.MessageBinary:
			// Tolerate raw keystrokes from non-JSON clients.
			if err := sess.Write(data); err != nil {
				return err
			}
		case websocket.MessageText:
			var m clientMsg
			if err := json.Unmarshal(data, &m); err != nil {
				return fmt.Errorf("协议错误(无法解析控制帧): %w", err)
			}
			switch m.T {
			case "in":
				if m.D == "" {
					continue
				}
				if err := sess.Write([]byte(m.D)); err != nil {
					return err
				}
			case "resize":
				if err := sess.Resize(m.Cols, m.Rows); err != nil {
					return err
				}
			default:
				// Unknown types are ignored so the protocol can grow.
			}
		}
	}
}

// wsConn serializes writes: stdout and stderr write concurrently, and control
// messages come from the pump goroutine.
type wsConn struct {
	c  *websocket.Conn
	mu sync.Mutex
}

func (w *wsConn) writeText(ctx context.Context, m controlMsg) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.c.Write(ctx, websocket.MessageText, data)
}

func (w *wsConn) writeBinary(ctx context.Context, p []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.c.Write(ctx, websocket.MessageBinary, p)
}

// wsSink is the io.Writer handed to the SSH session: every chunk of shell output
// becomes one binary frame.
type wsSink struct {
	wc    *wsConn
	ctx   context.Context
	touch func()
}

func (s *wsSink) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > maxOutputChunk {
			n = maxOutputChunk
		}
		ctx, cancel := context.WithTimeout(s.ctx, writeTimeout)
		err := s.wc.writeBinary(ctx, p[:n])
		cancel()
		if err != nil {
			return total, err
		}
		total += n
		p = p[n:]
	}
	s.touch()
	return total, nil
}

// exitCodeOf maps a session error onto a shell exit status: -1 when the session
// broke rather than the command failing.
func exitCodeOf(err error) int {
	var ee *ssh.ExitError
	if errors.As(err, &ee) {
		return ee.ExitStatus()
	}
	if err == nil || errors.Is(err, io.EOF) {
		return 0
	}
	return -1
}

// targetAddr renders the dial address for logs and the UI status line.
func targetAddr(t Target) string {
	port := t.Port
	if port <= 0 {
		port = 22
	}
	if t.Host == "" {
		return "(未知地址)"
	}
	return net.JoinHostPort(t.Host, strconv.Itoa(port))
}

// truncateReason keeps WebSocket close reasons inside the 125-byte limit.
func truncateReason(s string) string {
	const max = 120
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
