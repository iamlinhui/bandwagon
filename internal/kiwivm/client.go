// Package kiwivm is a minimal client for the BandwagonHost (搬瓦工) KiwiVM API.
//
// API base: https://api.64clouds.com/v1
// Auth: every VPS is identified by a VEID + API key pair (key starts with "private_").
// Responses are JSON; the "error" field is 0 on success, non-zero with a "message" on failure.
//
// All calls use POST so credentials never land in URLs (server logs, browser history, etc.).
package kiwivm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// BaseURL is the KiwiVM v1 API endpoint.
const BaseURL = "https://api.64clouds.com/v1"

// Client talks to the KiwiVM API. It is safe for concurrent use.
type Client struct {
	http *http.Client
}

// NewClient returns a Client with a sensible timeout.
func NewClient() *Client {
	return &Client{http: &http.Client{Timeout: 25 * time.Second}}
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
