// Package kiwivm is a minimal client for the BandwagonHost (搬瓦工) KiwiVM API.
//
// API base: https://api.64clouds.com/v1
// Auth: every VPS is identified by a VEID + API key pair (key starts with "private_").
// Responses are JSON; the "error" field is 0 on success, non-zero with a "message" on failure.
//
// All calls use POST so credentials never land in URLs (server logs, browser history, etc.).
package kiwivm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// BaseURL is the KiwiVM v1 API endpoint. It is a variable (not a constant) so
// tests can point a Client at a local stub server.
var BaseURL = "https://api.64clouds.com/v1"

// Client talks to the KiwiVM API. It is safe for concurrent use.
type Client struct {
	http *http.Client
	// shell has no fixed client timeout: a basicShell command can legitimately
	// run for a minute, so those calls are bounded by the caller's context.
	shell *http.Client
}

// NewClient returns a Client with a sensible timeout.
func NewClient() *Client {
	return &Client{
		http:  &http.Client{Timeout: 25 * time.Second},
		shell: &http.Client{},
	}
}

// rawResponse is the common envelope every KiwiVM call returns.
type rawResponse struct {
	Error   int    `json:"error"`
	Message string `json:"message"`
}

func (r rawResponse) failed() bool { return r.Error != 0 }

// ServiceInfo is the data returned by getServiceInfo. Only fields the dashboard
// uses are captured; ve_status/ssh_port/uptime are not in this endpoint — they
// come from getLiveServiceInfo (see LiveInfo).
type ServiceInfo struct {
	rawResponse
	Hostname        string   `json:"hostname"`
	IPAddresses     []string `json:"ip_addresses"`
	OS              string   `json:"os"`
	NodeAlias       string   `json:"node_alias"`
	Location        string   `json:"node_datacenter"`
	Plan            string   `json:"plan"`
	PlanMonthlyData int64    `json:"plan_monthly_data"`
	PlanDisk        int64    `json:"plan_disk"`
	PlanRAM         int64    `json:"plan_ram"`
	PlanSwap        int64    `json:"plan_swap"`
	DataCounter     int64    `json:"data_counter"`
	DataNextReset   int64    `json:"data_next_reset"`
	Suspended       bool     `json:"suspended"`
}

// LiveInfo is the real-time data returned by getLiveServiceInfo.
//
// KiwiVM returns load_average as a space-delimited string
// "0.00 0.00 0.00 2/97 24982" (1/5/15-min load, running/total procs, last pid),
// not a JSON array — so we decode it raw and parse it with ParseLoadAverage.
// ve_status, ssh_port and the used-disk/available-memory counters are only
// available from this endpoint.
type LiveInfo struct {
	rawResponse
	VEStatus        string `json:"ve_status"`
	SSHPort         int    `json:"ssh_port"`
	VEUptime        int64  `json:"ve_uptime"`
	LoadAverage     string `json:"load_average"`
	VEUsedDiskSpace int64  `json:"ve_used_disk_space_b"`
	MemAvailableKB  int64  `json:"mem_available_kb"`
}

// ParseLoadAverage turns the KiwiVM load string into its numeric parts,
// keeping the 1/5/15-min averages. Returns nil for an empty/unparseable string.
func ParseLoadAverage(s string) []float64 {
	fields := strings.Fields(s)
	out := make([]float64, 0, 3)
	for i := 0; i < 3 && i < len(fields); i++ {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return out
		}
		out = append(out, v)
	}
	return out
}

// APIError wraps a non-zero KiwiVM error response.
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("kiwivm error %d: %s", e.Code, e.Message)
}

// call posts to the given action with veid/api_key plus any extra params and
// unmarshals the response into dst. It retries transient network errors.
func (c *Client) call(action, veid, apiKey string, extra url.Values, dst any) error {
	if veid == "" || apiKey == "" {
		return fmt.Errorf("veid and api_key are required")
	}

	form := url.Values{}
	form.Set("veid", veid)
	form.Set("api_key", apiKey)
	for k, vs := range extra {
		for _, v := range vs {
			form.Set(k, v)
		}
	}
	endpoint := BaseURL + "/" + action

	var lastErr error
	// Two retries on transient network failures; KiwiVM rate limits are handled
	// by the store's pacing, not by hammering here.
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(1500 * time.Millisecond)
		}
		resp, err := c.http.PostForm(endpoint, form)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("kiwivm upstream %d for %s", resp.StatusCode, action)
			continue
		}
		if err := json.Unmarshal(body, dst); err != nil {
			return fmt.Errorf("decode %s response: %w", action, err)
		}
		// Surface a typed error if KiwiVM flagged the request.
		if r, ok := dst.(interface{ failed() bool }); ok && r.failed() {
			// Extract the common fields without losing typed data already decoded.
			var env rawResponse
			_ = json.Unmarshal(body, &env)
			return &APIError{Code: env.Error, Message: env.Message}
		}
		return nil
	}
	return fmt.Errorf("kiwivm %s failed after retries: %w", action, lastErr)
}

// GetServiceInfo fetches basic service information for a VPS.
func (c *Client) GetServiceInfo(veid, apiKey string) (*ServiceInfo, error) {
	var info ServiceInfo
	if err := c.call("getServiceInfo", veid, apiKey, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// GetLiveInfo fetches real-time status (status, ssh_port, load, used disk and
// available memory) for a VPS. Uptime is not provided by KiwiVM.
func (c *Client) GetLiveInfo(veid, apiKey string) (*LiveInfo, error) {
	var info LiveInfo
	if err := c.call("getLiveServiceInfo", veid, apiKey, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// Action performs a power action. Allowed: start, stop, restart, kill.
func (c *Client) Action(veid, apiKey, action string) error {
	switch action {
	case "start", "stop", "restart", "kill":
	default:
		return fmt.Errorf("unsupported action %q", action)
	}
	var r rawResponse
	if err := c.call(action, veid, apiKey, nil, &r); err != nil {
		return err
	}
	return nil
}

// --------------------------- Basic shell ---------------------------------

// KiwiVM's "Basic shell" runs commands inside the VPS over the API. Two quirks
// shape the code below:
//
//   - For basicShell/exec the envelope's "error" field doubles as the command's
//     exit status and "message" as its console output, so a non-zero error is
//     normally not an API failure. KiwiVM's own error codes sit far above any
//     possible exit status, so ShellExec only reports an *APIError when the
//     code exceeds maxExitStatus.
//   - Each call is a fresh, stateless shell: no PTY, no environment carried
//     over, no interactive programs (top/vim/less). The working directory is
//     emulated with basicShell/cd, which is why callers track it themselves.
//
// Shell calls are never retried — a retry could execute a command twice.

// MaxShellOutput bounds the console output kept from a single command.
const MaxShellOutput = 512 * 1024

// maxExitStatus is the highest value a POSIX exit status can have. KiwiVM's own
// API error codes are far larger (700005 = authentication failure, observed
// live), which is how basicShell/exec's overloaded "error" field can still be
// told apart: <= 255 is the command's exit status, above that the call failed.
const maxExitStatus = 255

// maxShellBody bounds the raw HTTP response so a runaway command can't exhaust
// memory; it is deliberately larger than MaxShellOutput.
const maxShellBody = 8 << 20

// ansiRE matches CSI sequences (colors, cursor moves) and the handful of other
// escapes a shell may emit. Without a PTY they are noise, so they are stripped.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b[()][A-Za-z0-9]|\x1b[>=]`)

// ShellResult is the outcome of one basicShell/exec call.
type ShellResult struct {
	// Exit is the command's exit status as reported by KiwiVM.
	Exit int `json:"exit"`
	// Output is the combined console output, cleaned of ANSI escapes and
	// normalized to \n line endings.
	Output string `json:"output"`
	// Truncated reports that Output was cut at MaxShellOutput.
	Truncated bool `json:"truncated,omitempty"`
}

// shellEnvelope is the raw basicShell response.
type shellEnvelope struct {
	Error   int    `json:"error"`
	Message string `json:"message"`
	Pwd     string `json:"pwd"`
}

// ShellExec runs a single command inside the VPS and waits for it to finish.
// ctx bounds the call; KiwiVM itself gives up on commands that run too long.
//
// A non-nil error means the call never produced a result (network, upstream or
// credential problem). A command that simply failed returns Exit != 0 with its
// output, and err == nil.
func (c *Client) ShellExec(ctx context.Context, veid, apiKey, command string) (*ShellResult, error) {
	var env shellEnvelope
	if err := c.shellCall(ctx, "basicShell/exec", veid, apiKey, url.Values{"command": {command}}, &env); err != nil {
		return nil, err
	}
	if env.Error > maxExitStatus {
		// Not an exit status — the API itself rejected the call.
		msg := strings.TrimSpace(env.Message)
		if msg == "" {
			msg = fmt.Sprintf("kiwivm error %d", env.Error)
		}
		return nil, &APIError{Code: env.Error, Message: msg}
	}
	out := cleanShellOutput(env.Message)
	res := &ShellResult{Exit: env.Error, Output: out}
	if len(out) > MaxShellOutput {
		cut := MaxShellOutput
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut-- // never split a multi-byte rune
		}
		res.Output = out[:cut]
		res.Truncated = true
	}
	return res, nil
}

// ShellCd resolves a directory change and returns the resulting absolute path.
// KiwiVM keeps no state between calls, so currentDir has to be supplied by the
// caller ("" means "wherever the shell starts", usually the root home).
func (c *Client) ShellCd(ctx context.Context, veid, apiKey, currentDir, newDir string) (string, error) {
	extra := url.Values{"newDir": {newDir}}
	if currentDir != "" {
		extra.Set("currentDir", currentDir)
	}
	var env shellEnvelope
	if err := c.shellCall(ctx, "basicShell/cd", veid, apiKey, extra, &env); err != nil {
		return "", err
	}
	if env.Error != 0 {
		// Here a non-zero error really is a failure (e.g. no such directory).
		msg := strings.TrimSpace(env.Message)
		if msg == "" {
			msg = "cannot change directory"
		}
		return "", &APIError{Code: env.Error, Message: msg}
	}
	return env.Pwd, nil
}

// shellCall posts one basicShell request. It deliberately does not retry and
// does not apply the rawResponse.failed() check, because for basicShell/exec a
// non-zero "error" is the command's exit status.
func (c *Client) shellCall(ctx context.Context, action, veid, apiKey string, extra url.Values, dst *shellEnvelope) error {
	if veid == "" || apiKey == "" {
		return fmt.Errorf("veid and api_key are required")
	}
	form := url.Values{"veid": {veid}, "api_key": {apiKey}}
	for k, vs := range extra {
		for _, v := range vs {
			form.Set(k, v)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseURL+"/"+action, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.shell.Do(req)
	if err != nil {
		return fmt.Errorf("kiwivm %s: %w", action, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxShellBody))
	if err != nil {
		return fmt.Errorf("kiwivm %s: read response: %w", action, err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("kiwivm upstream %d for %s", resp.StatusCode, action)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("decode %s response: %w", action, err)
	}
	return nil
}

// cleanShellOutput strips ANSI escapes and normalizes line endings so the UI
// (and any other API consumer) can render the text as-is.
func cleanShellOutput(s string) string {
	if s == "" {
		return ""
	}
	s = ansiRE.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// FriendlyErr removes the leading "kiwivm error N: " noise for UI display
// when the message alone is clearer.
func FriendlyErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if i := strings.Index(s, ": "); i >= 0 {
		// Keep authentication/credential hints intact.
		if strings.Contains(s, "Authentication") {
			return s
		}
	}
	return s
}

// FormatBytes converts a byte count into a compact human string.
func FormatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return strconv.FormatInt(b, 10) + " B"
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	if exp >= len(units) {
		exp = len(units) - 1
	}
	return fmt.Sprintf("%.2f %s", float64(b)/float64(div), units[exp])
}
