package api

import "encoding/json"

const (
	FormatJSON  = "json_object"
	FormatLines = "lines"
)

type ErrorBody struct {
	Error APIError `json:"error"`
}

type APIError struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details,omitempty"`
}

type ExecRequest struct {
	Command string `json:"command"`
	Format  string `json:"format"`
}

type ExecResponse struct {
	RequestID string          `json:"request_id"`
	Execution ExecutionResult `json:"execution"`
}

type ExecutionResult struct {
	Success          bool            `json:"success"`
	ExitCode         int             `json:"exit_code"`
	Format           string          `json:"format"`
	Output           json.RawMessage `json:"output"`
	RawOutput        string          `json:"raw_output,omitempty"`
	Stderr           []string        `json:"stderr"`
	Truncated        bool            `json:"truncated"`
	TimedOut         bool            `json:"timed_out"`
	DurationMS       int64           `json:"duration_ms"`
	SessionRestarted bool            `json:"session_restarted,omitempty"`
}

type SessionResponse struct {
	SessionID string `json:"session_id"`
	CreatedAt string `json:"created_at"`
}

type SessionInfoResponse struct {
	SessionID  string `json:"session_id"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at"`
	UptimeSec  int64  `json:"uptime_sec"`
	Busy       bool   `json:"busy"`
}

type HealthResponse struct {
	Status          string `json:"status"`
	Version         string `json:"version"`
	UptimeSec       int64  `json:"uptime_sec"`
	OpenSessions    int    `json:"open_sessions"`
	KillSwitchArmed bool   `json:"kill_switch_armed"`
}

type BlockedIP struct {
	IP             string `json:"ip"`
	BlockedAt      string `json:"blocked_at"`
	FailedAttempts int    `json:"failed_attempts"`
}

type BlocklistResponse struct {
	Blocked []BlockedIP `json:"blocked"`
}
