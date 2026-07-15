package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"windowsllmmanager/internal/api"
	"windowsllmmanager/internal/audit"
	"windowsllmmanager/internal/config"
	jobmanager "windowsllmmanager/internal/jobs"
	"windowsllmmanager/internal/powershell"
	"windowsllmmanager/internal/security"
)

type contextKey string

const (
	clientIPKey  contextKey = "client-ip"
	requestIDKey contextKey = "request-id"
)

type Server struct {
	cfg        config.Config
	version    string
	startedAt  time.Time
	auth       *security.TokenAuth
	resolver   security.ClientIPResolver
	blocklist  *security.Blocklist
	limiter    *security.RateLimiter
	audit      *audit.Logger
	runner     *powershell.Runner
	jobs       *jobmanager.Manager
	sessions   *powershell.SessionManager
	httpServer *http.Server
	logger     *log.Logger
}

func New(cfg config.Config, version string, logger *log.Logger) (*Server, error) {
	auth, err := security.LoadToken(cfg.TokenPath)
	if err != nil {
		return nil, err
	}
	auditLogger, err := audit.New(cfg.AuditLogPath, cfg.AuditMaxBytes)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = log.Default()
	}
	runner := powershell.NewRunner(cfg.MaxOutputBytes)
	s := &Server{
		cfg: cfg, version: version, startedAt: time.Now().UTC(), auth: auth,
		resolver:  security.NewClientIPResolver(cfg.TrustedProxyIP),
		blocklist: security.NewBlocklist(cfg.AuthFailuresBeforeBlock),
		limiter:   security.NewRateLimiter(cfg.RateLimitPerSec, cfg.RateLimitBurst),
		audit:     auditLogger, runner: runner,
		sessions: powershell.NewSessionManager(cfg.MaxSessions, cfg.MaxOutputBytes, cfg.IdleTimeout()),
		logger:   logger,
	}
	s.jobs = jobmanager.NewManager(runner, cfg.MaxAsyncJobs, cfg.MaxJobResults, cfg.JobRetention())
	if _, err := os.Stat(cfg.KillSwitchPath); err == nil || !os.IsNotExist(err) {
		s.jobs.BlockAndCancelAll("kill_switch")
		s.runner.BlockAndKillAll()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /exec", s.handleExec)
	mux.HandleFunc("POST /jobs", s.handleJobCreate)
	mux.HandleFunc("GET /jobs/{id}", s.handleJobGet)
	mux.HandleFunc("DELETE /jobs/{id}", s.handleJobCancel)
	mux.HandleFunc("POST /session", s.handleSessionCreate)
	mux.HandleFunc("POST /session/{id}/exec", s.handleSessionExec)
	mux.HandleFunc("POST /session/{id}/restart", s.handleSessionRestart)
	mux.HandleFunc("GET /session/{id}/info", s.handleSessionInfo)
	mux.HandleFunc("DELETE /session/{id}", s.handleSessionDelete)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /blocklist", s.handleBlocklistGet)
	mux.HandleFunc("DELETE /blocklist/{ip}", s.handleBlocklistDelete)
	mux.HandleFunc("POST /killswitch", s.handleKillSwitch)
	s.httpServer = &http.Server{
		Addr: cfg.ListenAddress, Handler: s.securityMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: cfg.CommandTimeout() + 30*time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 32 * 1024,
		TLSConfig:      &tls.Config{MinVersion: tls.VersionTLS12},
		// HTTP/1.1 permits an already-blocklisted connection to be closed without a response.
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
	}
	return s, nil
}

func (s *Server) Run() error {
	errCh, err := s.Start()
	if err != nil {
		return err
	}
	return <-errCh
}

// Start loads the TLS keypair and binds the port before returning. Windows SCM
// may therefore report RUNNING only after the HTTPS endpoint is genuinely ready.
func (s *Server) Start() (<-chan error, error) {
	certificate, err := tls.LoadX509KeyPair(s.cfg.TLSCertPath, s.cfg.TLSKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load TLS keypair: %w", err)
	}
	listener, err := net.Listen("tcp", s.cfg.ListenAddress)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", s.cfg.ListenAddress, err)
	}
	tlsConfig := s.httpServer.TLSConfig.Clone()
	tlsConfig.Certificates = []tls.Certificate{certificate}
	tlsListener := tls.NewListener(listener, tlsConfig)
	s.logger.Printf("WindowsLLMManager %s listening on %s", s.version, s.cfg.ListenAddress)
	errCh := make(chan error, 1)
	go func() {
		err := s.httpServer.Serve(tlsListener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()
	return errCh, nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.jobs.Close()
	s.sessions.Close()
	s.runner.KillAll()
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := s.resolver.Resolve(r)
		requestID := newRequestID()
		ctx := context.WithValue(r.Context(), clientIPKey, ip)
		ctx = context.WithValue(ctx, requestIDKey, requestID)
		r = r.WithContext(ctx)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Request-ID", requestID)
		w.Header().Set("Cache-Control", "no-store")
		if s.blocklist.IsBlocked(ip) {
			s.writeAudit(audit.Event{Type: "blocked_request_dropped", RequestID: requestID, SourceIP: ip})
			if hijacker, ok := w.(http.Hijacker); ok {
				conn, _, err := hijacker.Hijack()
				if err == nil {
					_ = conn.Close()
					return
				}
			}
			return
		}
		if !s.limiter.Allow(ip, time.Now()) {
			writeError(w, http.StatusTooManyRequests, "rate_limited", "request rate limit exceeded", true, nil)
			return
		}
		if !s.auth.Authenticate(r.Header.Get("Authorization")) {
			count, blocked := s.blocklist.RecordFailure(ip, time.Now())
			s.writeAudit(audit.Event{Type: "auth_failed", RequestID: requestID, SourceIP: ip, Details: map[string]any{"failed_attempts": count, "blocked": blocked}})
			if blocked {
				s.writeAudit(audit.Event{Type: "ip_auto_blocked", RequestID: requestID, SourceIP: ip, Details: map[string]any{"failed_attempts": count}})
			}
			writeError(w, http.StatusUnauthorized, "auth_failed", "invalid or missing bearer token", false, map[string]any{"attempts_before_block": max(0, s.cfg.AuthFailuresBeforeBlock-count)})
			return
		}
		s.blocklist.RecordSuccess(ip)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	if s.rejectIfKilled(w) {
		return
	}
	request, ok := s.decodeExecRequest(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.CommandTimeout())
	defer cancel()
	result, err := s.runner.Run(ctx, request.Command, request.Format)
	s.auditCommand(r, "exec", "", request.Command, result)
	if errors.Is(err, powershell.ErrBlocked) {
		writeError(w, http.StatusLocked, "killswitch_active", "execution is disabled; disarm requires local administrator intervention", false, nil)
		return
	}
	if errors.Is(err, powershell.ErrTimedOut) {
		writeError(w, http.StatusGatewayTimeout, "command_timeout", "command timed out; target state is unknown and must be verified", false, map[string]any{"execution": result})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "shell_failure", err.Error(), true, nil)
		return
	}
	writeJSON(w, http.StatusOK, api.ExecResponse{RequestID: requestID(r), Execution: result})
}

func (s *Server) handleJobCreate(w http.ResponseWriter, r *http.Request) {
	if s.rejectIfKilled(w) {
		return
	}
	request, ok := s.decodeJobRequest(w, r)
	if !ok {
		return
	}
	timeoutSec := request.TimeoutSec
	if timeoutSec == 0 {
		timeoutSec = s.cfg.LongCommandTimeoutSec
	}
	if timeoutSec < 1 || timeoutSec > s.cfg.LongCommandTimeoutSec {
		writeError(w, http.StatusBadRequest, "invalid_timeout", fmt.Sprintf("timeout_sec must be between 1 and %d", s.cfg.LongCommandTimeoutSec), false, nil)
		return
	}
	sourceIP := clientIP(r)
	snapshot, err := s.jobs.Submit(request.Command, request.Format, time.Duration(timeoutSec)*time.Second, func(final jobmanager.Snapshot) {
		result := final.Execution
		var success *bool
		var exitCode *int
		if result != nil {
			success, exitCode = &result.Success, &result.ExitCode
		}
		s.writeAudit(audit.Event{
			Type: "job_finished", SourceIP: sourceIP, TokenFingerprint: s.auth.Fingerprint(), Mode: "job", Success: success, ExitCode: exitCode,
			Details: map[string]any{"job_id": final.JobID, "status": final.Status},
		})
	})
	if errors.Is(err, jobmanager.ErrLimit) {
		writeError(w, http.StatusConflict, "job_limit", "maximum number of asynchronous jobs reached", true, map[string]any{"max_async_jobs": s.cfg.MaxAsyncJobs})
		return
	}
	if errors.Is(err, jobmanager.ErrBlocked) {
		writeError(w, http.StatusLocked, "killswitch_active", "execution is disabled; disarm requires local administrator intervention", false, nil)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "job_start_failed", err.Error(), true, nil)
		return
	}
	s.writeAudit(audit.Event{
		Type: "job_submitted", RequestID: requestID(r), SourceIP: clientIP(r), TokenFingerprint: s.auth.Fingerprint(),
		Mode: "job", Command: request.Command, Details: map[string]any{"job_id": snapshot.JobID, "timeout_sec": timeoutSec},
	})
	writeJSON(w, http.StatusAccepted, jobResponse(snapshot))
}

func (s *Server) handleJobGet(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.jobs.Get(r.PathValue("id"))
	if errors.Is(err, jobmanager.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job_not_found", "job does not exist or its retained result expired", false, nil)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "job_status_failed", err.Error(), true, nil)
		return
	}
	writeJSON(w, http.StatusOK, jobResponse(snapshot))
}

func (s *Server) handleJobCancel(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.jobs.Cancel(r.PathValue("id"), "operator")
	if errors.Is(err, jobmanager.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job_not_found", "job does not exist or its retained result expired", false, nil)
		return
	}
	if errors.Is(err, jobmanager.ErrNotRunning) {
		writeError(w, http.StatusConflict, "job_not_running", "job has already reached a terminal state", false, map[string]any{"job": jobResponse(snapshot)})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "job_cancel_failed", err.Error(), true, nil)
		return
	}
	s.writeAudit(audit.Event{
		Type: "job_cancel_requested", RequestID: requestID(r), SourceIP: clientIP(r), TokenFingerprint: s.auth.Fingerprint(),
		Mode: "job", Details: map[string]any{"job_id": snapshot.JobID},
	})
	writeJSON(w, http.StatusAccepted, jobResponse(snapshot))
}

func (s *Server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	if s.rejectIfKilled(w) {
		return
	}
	session, err := s.sessions.Create()
	if errors.Is(err, powershell.ErrSessionLimit) {
		writeError(w, http.StatusConflict, "session_limit", "maximum number of sessions reached", false, map[string]any{"max_sessions": s.cfg.MaxSessions})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_start_failed", err.Error(), true, nil)
		return
	}
	info := session.Info()
	writeJSON(w, http.StatusCreated, api.SessionResponse{SessionID: info.SessionID, CreatedAt: info.CreatedAt})
}

func (s *Server) handleSessionExec(w http.ResponseWriter, r *http.Request) {
	if s.rejectIfKilled(w) {
		return
	}
	session, err := s.sessions.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "session_not_found", "session does not exist or was reaped", false, nil)
		return
	}
	request, ok := s.decodeExecRequest(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.CommandTimeout())
	defer cancel()
	result, execErr := session.Exec(ctx, request.Command, request.Format)
	s.auditCommand(r, "session", r.PathValue("id"), request.Command, result)
	if errors.Is(execErr, powershell.ErrTimedOut) {
		writeError(w, http.StatusGatewayTimeout, "command_timeout", "command timed out; session was restarted and target state is unknown", false, map[string]any{"execution": result})
		return
	}
	if errors.Is(execErr, powershell.ErrShellExited) {
		writeError(w, http.StatusConflict, "session_process_exited", "command terminated the PowerShell process; session was restarted if possible and its state was lost", false, map[string]any{"execution": result})
		return
	}
	if execErr != nil {
		writeError(w, http.StatusInternalServerError, "session_exec_failed", execErr.Error(), true, nil)
		return
	}
	writeJSON(w, http.StatusOK, api.ExecResponse{RequestID: requestID(r), Execution: result})
}

func (s *Server) handleSessionRestart(w http.ResponseWriter, r *http.Request) {
	if s.rejectIfKilled(w) {
		return
	}
	session, err := s.sessions.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "session_not_found", "session does not exist or was reaped", false, nil)
		return
	}
	if err := session.Restart(); err != nil {
		writeError(w, http.StatusInternalServerError, "session_restart_failed", err.Error(), true, nil)
		return
	}
	writeJSON(w, http.StatusOK, session.Info())
}

func (s *Server) handleSessionInfo(w http.ResponseWriter, r *http.Request) {
	session, err := s.sessions.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "session_not_found", "session does not exist or was reaped", false, nil)
		return
	}
	writeJSON(w, http.StatusOK, session.Info())
}

func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	if !s.sessions.Delete(r.PathValue("id")) {
		writeError(w, http.StatusNotFound, "session_not_found", "session does not exist or was reaped", false, nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	killed := s.isKilled()
	status := "ok"
	if killed {
		status = "braked"
	}
	writeJSON(w, http.StatusOK, api.HealthResponse{
		Status: status, Version: s.version, UptimeSec: int64(time.Since(s.startedAt).Seconds()),
		OpenSessions: s.sessions.Count(), ActiveJobs: s.jobs.ActiveCount(), UpdateCheckIntervalMin: s.cfg.UpdateCheckIntervalMin, KillSwitchArmed: killed,
	})
}

func (s *Server) handleBlocklistGet(w http.ResponseWriter, _ *http.Request) {
	entries := s.blocklist.List()
	result := make([]api.BlockedIP, 0, len(entries))
	for _, entry := range entries {
		result = append(result, api.BlockedIP{IP: entry.IP, BlockedAt: entry.BlockedAt.Format(time.RFC3339Nano), FailedAttempts: entry.FailedAttempts})
	}
	writeJSON(w, http.StatusOK, api.BlocklistResponse{Blocked: result})
}

func (s *Server) handleBlocklistDelete(w http.ResponseWriter, r *http.Request) {
	ip := net.ParseIP(r.PathValue("ip"))
	if ip == nil {
		writeError(w, http.StatusBadRequest, "invalid_ip", "path value is not an IP address", false, nil)
		return
	}
	if !s.blocklist.Remove(ip.String()) {
		writeError(w, http.StatusNotFound, "ip_not_blocked", "IP address is not blocklisted", false, nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleKillSwitch(w http.ResponseWriter, r *http.Request) {
	f, err := os.OpenFile(s.cfg.KillSwitchPath, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "killswitch_write_failed", err.Error(), false, nil)
		return
	}
	_ = f.Close()
	jobsKilled := s.jobs.BlockAndCancelAll("kill_switch")
	s.runner.BlockAndKillAll()
	s.sessions.KillAll()
	s.writeAudit(audit.Event{Type: "killswitch_armed", RequestID: requestID(r), SourceIP: clientIP(r), TokenFingerprint: s.auth.Fingerprint()})
	writeJSON(w, http.StatusOK, map[string]any{"armed": true, "sessions_killed": true, "jobs_killed": jobsKilled, "disarm": "local_only"})
}

func (s *Server) decodeExecRequest(w http.ResponseWriter, r *http.Request) (api.ExecRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var request api.ExecRequest
	if err := dec.Decode(&request); err != nil {
		status := http.StatusBadRequest
		code := "invalid_json"
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			status, code = http.StatusRequestEntityTooLarge, "request_too_large"
		}
		writeError(w, status, code, err.Error(), false, nil)
		return api.ExecRequest{}, false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body contains trailing JSON data", false, nil)
		return api.ExecRequest{}, false
	}
	if strings.TrimSpace(request.Command) == "" {
		writeError(w, http.StatusBadRequest, "empty_command", "command must not be empty", false, nil)
		return api.ExecRequest{}, false
	}
	if request.Format != api.FormatJSON && request.Format != api.FormatLines {
		writeError(w, http.StatusBadRequest, "invalid_format", "format must be json_object or lines", false, nil)
		return api.ExecRequest{}, false
	}
	return request, true
}

func (s *Server) decodeJobRequest(w http.ResponseWriter, r *http.Request) (api.JobRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var request api.JobRequest
	if err := dec.Decode(&request); err != nil {
		status := http.StatusBadRequest
		code := "invalid_json"
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			status, code = http.StatusRequestEntityTooLarge, "request_too_large"
		}
		writeError(w, status, code, err.Error(), false, nil)
		return api.JobRequest{}, false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body contains trailing JSON data", false, nil)
		return api.JobRequest{}, false
	}
	if strings.TrimSpace(request.Command) == "" {
		writeError(w, http.StatusBadRequest, "empty_command", "command must not be empty", false, nil)
		return api.JobRequest{}, false
	}
	if request.Format != api.FormatJSON && request.Format != api.FormatLines {
		writeError(w, http.StatusBadRequest, "invalid_format", "format must be json_object or lines", false, nil)
		return api.JobRequest{}, false
	}
	return request, true
}

func jobResponse(snapshot jobmanager.Snapshot) api.JobResponse {
	response := api.JobResponse{
		JobID: snapshot.JobID, Status: snapshot.Status, CreatedAt: snapshot.CreatedAt.Format(time.RFC3339Nano),
		StartedAt: snapshot.StartedAt.Format(time.RFC3339Nano), TimeoutSec: int(snapshot.Timeout / time.Second),
		Execution: snapshot.Execution, Error: snapshot.Error, CancelReason: snapshot.CancelReason,
	}
	if !snapshot.CompletedAt.IsZero() {
		response.CompletedAt = snapshot.CompletedAt.Format(time.RFC3339Nano)
	}
	return response
}

func (s *Server) rejectIfKilled(w http.ResponseWriter) bool {
	if !s.isKilled() {
		return false
	}
	writeError(w, http.StatusLocked, "killswitch_active", "execution is disabled; disarm requires local administrator intervention", false, nil)
	return true
}

func (s *Server) isKilled() bool {
	_, err := os.Stat(s.cfg.KillSwitchPath)
	return err == nil || !os.IsNotExist(err)
}

func (s *Server) auditCommand(r *http.Request, mode, sessionID, command string, result api.ExecutionResult) {
	success, exitCode := result.Success, result.ExitCode
	s.writeAudit(audit.Event{
		Type: "command_executed", RequestID: requestID(r), SourceIP: clientIP(r), TokenFingerprint: s.auth.Fingerprint(),
		Mode: mode, SessionID: sessionID, Command: command, Success: &success, ExitCode: &exitCode,
		Details: map[string]any{"timed_out": result.TimedOut, "truncated": result.Truncated, "duration_ms": result.DurationMS, "session_restarted": result.SessionRestarted},
	})
}

func (s *Server) writeAudit(event audit.Event) {
	if err := s.audit.Write(event); err != nil {
		s.logger.Printf("audit write failed: %v", err)
	}
}

func requestID(r *http.Request) string { return valueString(r.Context(), requestIDKey) }
func clientIP(r *http.Request) string  { return valueString(r.Context(), clientIPKey) }
func valueString(ctx context.Context, key contextKey) string {
	value, _ := ctx.Value(key).(string)
	return value
}

func newRequestID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func writeError(w http.ResponseWriter, status int, code, message string, retryable bool, details map[string]any) {
	writeJSON(w, status, api.ErrorBody{Error: api.APIError{Code: code, Message: message, Retryable: retryable, Details: details}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
