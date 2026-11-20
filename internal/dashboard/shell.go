package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"bandwagon/internal/kiwivm"
)

// The web shell is KiwiVM's "Basic shell" exposed over HTTP: one command per
// request, executed synchronously inside the VPS as root.
//
// It is not a PTY. Every call is a fresh, stateless shell, so interactive
// programs (top, vim, less), environment variables and background jobs do not
// survive between commands. The working directory is the one piece of state
// worth keeping, so the browser tracks it and sends it back with each command;
// the server re-applies it in front of everything it runs.

const (
	// maxShellCommandLen bounds one command line.
	maxShellCommandLen = 4000
	// maxShellBodyBytes bounds the JSON request body.
	maxShellBodyBytes = 64 << 10
	// shellRateWindow/shellRateLimit pace a single instance so a stuck frontend
	// loop can't burn the account's KiwiVM rate-limit points.
	shellRateWindow = time.Minute
	shellRateLimit  = 60
)

// shellGate serializes shell calls per instance and enforces the rate limit.
type shellGate struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
	hits  map[string][]time.Time
}

func newShellGate() *shellGate {
	return &shellGate{
		locks: make(map[string]*sync.Mutex),
		hits:  make(map[string][]time.Time),
	}
}

// lock returns the per-instance mutex, creating it on first use. Holding it for
// the duration of a request means one VPS never runs two shell commands at once.
func (g *shellGate) lock(id string) *sync.Mutex {
	g.mu.Lock()
	defer g.mu.Unlock()
	l, ok := g.locks[id]
	if !ok {
		l = &sync.Mutex{}
		g.locks[id] = l
	}
	return l
}

// allow records a command for id and reports whether it stays inside
// shellRateLimit per shellRateWindow.
func (g *shellGate) allow(id string) bool {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	kept := g.hits[id][:0]
	for _, t := range g.hits[id] {
		if now.Sub(t) < shellRateWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) >= shellRateLimit {
		g.hits[id] = kept
		return false
	}
	g.hits[id] = append(kept, now)
	return true
}

// handleShell runs one command on an instance.
//
//	POST /api/instances/{id}/shell  {"command": "...", "cwd": "/root"}
//
// An empty command is the "connect" handshake: it resolves the starting
// directory so the UI can render a real prompt. The response always carries the
// cwd to use for the next request.
func (s *Server) handleShell(w http.ResponseWriter, r *http.Request) {
	if !s.shell.Enabled {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "网页 Shell 未启用(在配置里设置 shell.enabled = true)"})
		return
	}
	id := r.PathValue("id")
	cfg, ok := s.store.configFor(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "instance not found"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxShellBodyBytes)
	var body struct {
		Command string `json:"command"`
		Cwd     string `json:"cwd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	command := strings.TrimSpace(body.Command)
	if len(command) > maxShellCommandLen {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("命令过长(上限 %d 字符)", maxShellCommandLen)})
		return
	}
	cwd := strings.TrimSpace(body.Cwd)

	lock := s.gate.lock(id)
	lock.Lock()
	defer lock.Unlock()
	if command != "" && !s.gate.allow(id) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": fmt.Sprintf("命令过于频繁(每 %s 最多 %d 条),请稍后再试", shellRateWindow, shellRateLimit)})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.shell.Timeout())
	defer cancel()

	start := time.Now()
	res, err := runShellCommand(ctx, s.store.client, cfg, command, cwd)
	elapsed := time.Since(start)
	if err != nil {
		// Transport, credential or upstream failure. The VPS may still have run
		// the command if the connection dropped after it started.
		log.Printf("shell %s (%s): %v", cfg.Name, cfg.VEID, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": shellErrMessage(err, s.shell.Timeout())})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"output":     res.Output,
		"exit":       res.Exit,
		"cwd":        res.Cwd,
		"truncated":  res.Truncated,
		"elapsed_ms": elapsed.Milliseconds(),
	})
}

// shellOutcome is what one request produces.
type shellOutcome struct {
	Output    string
	Exit      int
	Cwd       string
	Truncated bool
}

// runShellCommand dispatches the request: the connect handshake, a "cd", or an
// ordinary command run from the tracked directory.
func runShellCommand(ctx context.Context, c *kiwivm.Client, cfg InstanceConfig, command, cwd string) (*shellOutcome, error) {
	if command == "" {
		// Handshake: ask the real shell where it starts (usually /root).
		res, err := c.ShellExec(ctx, cfg.VEID, cfg.APIKey, "pwd")
		if err != nil {
			return nil, err
		}
		pwd := firstLine(res.Output)
		if !strings.HasPrefix(pwd, "/") {
			pwd = cwd // never adopt a stray message as the working directory
		}
		return &shellOutcome{Cwd: pwd}, nil
	}

	if target, isCd := parseCd(command); isCd {
		pwd, msg, err := resolveCd(ctx, c, cfg, cwd, target)
		if err != nil {
			return nil, err
		}
		if msg != "" {
			// A refused cd is ordinary shell output, not a failed request: show
			// it in the terminal with a non-zero exit code and keep the old cwd.
			return &shellOutcome{Output: msg + "\n", Exit: 1, Cwd: cwd}, nil
		}
		if pwd == "" {
			pwd = cwd
		}
		return &shellOutcome{Cwd: pwd}, nil
	}

	// Re-apply the tracked directory. `|| exit 1` keeps the command from
	// running somewhere unexpected if the directory vanished.
	script := command
	if cwd != "" {
		script = "cd " + shellQuote(cwd) + " || exit 1; " + command
	}
	res, err := c.ShellExec(ctx, cfg.VEID, cfg.APIKey, script)
	if err != nil {
		return nil, err
	}
	return &shellOutcome{
		Output:    res.Output,
		Exit:      res.Exit,
		Cwd:       cwd,
		Truncated: res.Truncated,
	}, nil
}

// resolveCd changes directory and returns the new absolute path.
//
// It first tries KiwiVM's purpose-built basicShell/cd, then falls back to the
// VPS's own shell ("cd X && pwd") whenever the simulator refuses or answers
// with something unusable — the real shell also produces the best error text.
// An empty target means "home".
//
// A non-empty msg means the shell itself refused the change (callers show it as
// ordinary output); a non-nil err means the API call never produced a result.
func resolveCd(ctx context.Context, c *kiwivm.Client, cfg InstanceConfig, cwd, target string) (pwd, msg string, err error) {
	newDir := target
	if newDir == "" {
		newDir = "~" // bare cd → home
	}
	if p, cdErr := c.ShellCd(ctx, cfg.VEID, cfg.APIKey, cwd, newDir); cdErr == nil && strings.HasPrefix(p, "/") {
		return p, "", nil
	}

	// Fallback: let the real shell resolve it. A quoted "~" would not expand,
	// so a bare cd is passed through unquoted.
	script := "cd " + shellQuote(target) + " && pwd"
	if target == "" {
		script = "cd && pwd"
	}
	res, err := c.ShellExec(ctx, cfg.VEID, cfg.APIKey, script)
	if err != nil {
		return "", "", err
	}
	pwd = firstLine(res.Output)
	if res.Exit != 0 || !strings.HasPrefix(pwd, "/") {
		msg = strings.TrimSpace(res.Output)
		if msg == "" {
			msg = "cd: " + target + ": 无法进入该目录"
		}
		return "", msg, nil
	}
	return pwd, "", nil
}

// parseCd recognizes a plain "cd [dir]" line and returns its argument. Anything
// more elaborate ("cd a && b", "cd a; ls") is left to basicShell/exec, which
// runs it correctly but without persisting the directory change.
func parseCd(command string) (target string, ok bool) {
	if command != "cd" && !strings.HasPrefix(command, "cd ") && !strings.HasPrefix(command, "cd\t") {
		return "", false
	}
	target = strings.TrimSpace(strings.TrimPrefix(command, "cd"))
	if len(target) >= 2 {
		q := target[0]
		if (q == '\'' || q == '"') && target[len(target)-1] == q && !strings.Contains(target[1:len(target)-1], string(q)) {
			target = target[1 : len(target)-1]
		}
	}
	if strings.ContainsAny(target, ";&|\n") {
		return "", false
	}
	return target, true
}

// shellErrMessage turns a failed call into something worth showing in the
// terminal pane: a timeout or cancellation reads better than the raw error.
func shellErrMessage(err error, timeout time.Duration) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("命令超时(超过 %s)。KiwiVM 也会自行中断长时间运行的命令,可改用 nohup … & 放后台。", timeout)
	case errors.Is(err, context.Canceled):
		return "请求已取消"
	default:
		var apiErr *kiwivm.APIError
		if errors.As(err, &apiErr) && apiErr.Message != "" {
			return fmt.Sprintf("KiwiVM 错误 %d: %s", apiErr.Code, apiErr.Message)
		}
		return kiwivm.FriendlyErr(err)
	}
}

// shellQuote wraps s in single quotes for POSIX shells.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// firstLine returns the first non-empty line, trimmed.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}
