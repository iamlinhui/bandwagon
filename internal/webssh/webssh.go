// Package webssh bridges a browser terminal (xterm.js over a WebSocket) to a
// real SSH PTY on a remote host.
//
// It exists because the KiwiVM "Basic shell" path is request/response only: no
// PTY, no persistent environment, no interactive programs. Here the browser gets
// a genuine login shell — vim, top, tab completion, colours, background jobs and
// window resizing all work — and the VPS credentials never leave the server.
package webssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Target describes where to connect and how to authenticate.
type Target struct {
	// Host and Port are the SSH endpoint. Both are required; callers fall back
	// to the KiwiVM-reported address and ssh_port when the config is silent.
	Host string
	Port int
	// User is the login name (usually "root").
	User string
	// Password and/or PrivateKeyPEM. A key is tried first when both are set.
	Password      string
	PrivateKeyPEM string
	Passphrase    string
	// HostKeyFingerprint pins the server host key ("SHA256:…"). Empty accepts
	// any key but reports it, so the operator can pin it afterwards.
	HostKeyFingerprint string
	// Label names the instance in logs and error text. Never contains secrets.
	Label string
}

// Default timeouts. Callers may override per session.
const (
	DefaultDialTimeout      = 15 * time.Second
	DefaultHandshakeTimeout = 15 * time.Second
	DefaultIdleTimeout      = 30 * time.Minute
	keepaliveInterval       = 30 * time.Second
	defaultCols             = 80
	defaultRows             = 24
	termType                = "xterm-256color"
)

// Session is one live interactive shell on the remote host.
//
// A Session is safe for concurrent use: Write (stdin) and Resize may be called
// from any goroutine while output flows to the writer given to Attach.
type Session struct {
	client *ssh.Client
	sess   *ssh.Session
	in     io.WriteCloser

	mu        sync.Mutex
	hostKeyFP string

	stopOnce sync.Once
	stop     chan struct{}
}

// Dial connects, authenticates, requests a PTY and starts the login shell.
// ctx bounds the TCP dial; the SSH handshake gets its own deadline because
// x/crypto/ssh does not take a context.
func Dial(ctx context.Context, t Target, dialTimeout time.Duration) (*Session, error) {
	if t.Host == "" {
		return nil, errors.New("ssh: 没有可用地址(KiwiVM 未返回 IP,且配置未指定 host)")
	}
	port := t.Port
	if port <= 0 {
		port = 22
	}
	user := t.User
	if user == "" {
		user = "root"
	}
	auth, err := authMethods(t)
	if err != nil {
		return nil, err
	}

	s := &Session{stop: make(chan struct{})}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            auth,
		HostKeyCallback: hostKeyVerifier(t.HostKeyFingerprint, s.setHostKey),
		Timeout:         dialTimeout,
	}

	addr := net.JoinHostPort(t.Host, strconv.Itoa(port))
	if dialTimeout <= 0 {
		dialTimeout = DefaultDialTimeout
	}
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("连接 %s 失败: %w", addr, err)
	}
	// Bound the handshake too: an unresponsive sshd would otherwise hang here.
	_ = conn.SetDeadline(time.Now().Add(DefaultHandshakeTimeout))
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	_ = conn.SetDeadline(time.Time{})
	if err != nil {
		_ = conn.Close()
		return nil, describeAuthErr(err, user, addr)
	}
	s.client = ssh.NewClient(c, chans, reqs)

	sess, err := s.client.NewSession()
	if err != nil {
		_ = s.client.Close()
		return nil, fmt.Errorf("打开 SSH 会话失败: %w", err)
	}
	s.sess = sess
	if s.in, err = sess.StdinPipe(); err != nil {
		s.closeAll()
		return nil, fmt.Errorf("取得 stdin 失败: %w", err)
	}
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := sess.RequestPty(termType, defaultRows, defaultCols, modes); err != nil {
		s.closeAll()
		return nil, fmt.Errorf("请求 PTY 失败: %w", err)
	}
	// The shell itself is started by Attach, not here: x/crypto/ssh discards
	// anything that arrives before Stdout is set, and sshd prints its banner the
	// instant the shell starts.
	return s, nil
}

// Attach wires the shell's output to w, starts the login shell and blocks until
// it exits. It returns the shell's exit status, or an error if the session broke.
// Call it in its own goroutine.
func (s *Session) Attach(w io.Writer) error {
	s.sess.Stdout = w
	s.sess.Stderr = w
	if err := s.sess.Shell(); err != nil {
		return fmt.Errorf("启动 shell 失败: %w", err)
	}
	go s.keepalive()
	return s.sess.Wait()
}

// Write feeds bytes to the remote shell's stdin.
func (s *Session) Write(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	_, err := s.in.Write(p)
	return err
}

// Resize changes the remote PTY size (xterm.js reports it in cells).
func (s *Session) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	return s.sess.WindowChange(rows, cols)
}

// HostKeyFingerprint is the SHA256 fingerprint of the server's host key, e.g.
// "SHA256:AbCd…". Empty until the handshake reports it.
func (s *Session) HostKeyFingerprint() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hostKeyFP
}

func (s *Session) setHostKey(fp string) {
	s.mu.Lock()
	s.hostKeyFP = fp
	s.mu.Unlock()
}

// Close tears the session down. It is safe to call more than once.
func (s *Session) Close() error {
	s.stopOnce.Do(func() { close(s.stop) })
	return s.closeAll()
}

func (s *Session) closeAll() error {
	s.stopOnce.Do(func() { close(s.stop) })
	var err error
	if s.sess != nil {
		err = s.sess.Close()
	}
	if s.client != nil {
		if cerr := s.client.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

// keepalive pokes the SSH connection so idle sessions survive NAT timeouts.
func (s *Session) keepalive() {
	t := time.NewTicker(keepaliveInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			if s.client == nil {
				return
			}
			// wantReply=true so a dead peer surfaces as an error promptly.
			if _, _, err := s.client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				return
			}
		}
	}
}

// authMethods builds the credential chain: key first, then password.
func authMethods(t Target) ([]ssh.AuthMethod, error) {
	var out []ssh.AuthMethod
	if strings.TrimSpace(t.PrivateKeyPEM) != "" {
		signer, err := ParsePrivateKey(t.PrivateKeyPEM, t.Passphrase)
		if err != nil {
			return nil, err
		}
		if signer != nil {
			out = append(out, ssh.PublicKeys(signer))
		}
	}
	if t.Password != "" {
		out = append(out, ssh.Password(t.Password))
		// Some sshd builds only offer keyboard-interactive. Same secret,
		// different exchange — offer it as a fallback so password logins work
		// regardless of how sshd is configured.
		out = append(out, ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
			answers := make([]string, len(questions))
			for i := range answers {
				answers[i] = t.Password
			}
			return answers, nil
		}))
	}
	if len(out) == 0 {
		return nil, errors.New("ssh: 没有配置凭据(需要 password 或 private_key)")
	}
	return out, nil
}

// ParsePrivateKey turns a PEM block into a signer, decrypting it when a
// passphrase is given. An empty PEM returns (nil, nil) so config validation can
// call it unconditionally.
func ParsePrivateKey(pemBlock, passphrase string) (ssh.Signer, error) {
	if strings.TrimSpace(pemBlock) == "" {
		return nil, nil
	}
	raw := []byte(pemBlock)
	if passphrase != "" {
		key, err := ssh.ParseRawPrivateKeyWithPassphrase(raw, []byte(passphrase))
		if err != nil {
			return nil, fmt.Errorf("解密私钥失败: %w", err)
		}
		return ssh.NewSignerFromKey(key)
	}
	key, err := ssh.ParseRawPrivateKey(raw)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "encrypted") {
			return nil, fmt.Errorf("私钥已加密,请在 ssh.passphrase 里给出口令 (%w)", err)
		}
		return nil, fmt.Errorf("解析私钥失败: %w", err)
	}
	return ssh.NewSignerFromKey(key)
}

// hostKeyVerifier pins the host key when configured, otherwise it records the
// fingerprint and accepts it (trust on first use — the UI shows it so the
// operator can pin it in config afterwards).
func hostKeyVerifier(pin string, report func(string)) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		fp := ssh.FingerprintSHA256(key)
		if report != nil {
			report(fp)
		}
		if pin == "" {
			return nil
		}
		if !SameFingerprint(fp, pin) {
			return &HostKeyMismatchError{Got: fp, Pinned: pin}
		}
		return nil
	}
}

// HostKeyMismatchError reports a server whose host key differs from the pinned
// fingerprint. It survives the handshake wrapping so callers can detect it.
type HostKeyMismatchError struct {
	Got    string
	Pinned string
}

func (e *HostKeyMismatchError) Error() string {
	return fmt.Sprintf("主机密钥不匹配:服务器为 %s,配置固定为 %s(可能是中间人,也可能机器重装过系统)", e.Got, e.Pinned)
}

// SameFingerprint compares SHA256 fingerprints, tolerating a missing "SHA256:"
// prefix, padding and case differences.
func SameFingerprint(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimSpace(s)
		if len(s) > 7 && strings.EqualFold(s[:7], "sha256:") {
			s = s[7:]
		}
		return strings.TrimRight(s, "=")
	}
	return strings.EqualFold(norm(a), norm(b))
}

// describeAuthErr turns the opaque handshake failure into something the operator
// can act on, without echoing any credential.
func describeAuthErr(err error, user, addr string) error {
	// A rejected host key is already actionable and must stay identifiable.
	var mismatch *HostKeyMismatchError
	if errors.As(err, &mismatch) {
		return err
	}
	msg := err.Error()
	if strings.Contains(msg, "主机密钥") || strings.Contains(msg, "host key") {
		return err
	}
	switch {
	case strings.Contains(msg, "unable to authenticate"),
		strings.Contains(msg, "no supported methods remain"),
		strings.Contains(msg, "handshake failed"):
		return fmt.Errorf("认证失败(%s@%s):请检查 ssh.user 与 password/private_key", user, addr)
	case strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "deadline"):
		return fmt.Errorf("连接 %s 超时:确认机器在线、SSH 端口正确、防火墙放行", addr)
	default:
		return fmt.Errorf("SSH 握手失败(%s): %w", addr, err)
	}
}
