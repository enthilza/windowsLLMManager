package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"windowsllmmanager/internal/api"
	"windowsllmmanager/internal/config"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	token := strings.Repeat("a", 43)
	if err := os.WriteFile(filepath.Join(dir, "token.txt"), []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default(dir)
	cfg.ListenAddress = "127.0.0.1:0"
	cfg.AuditMaxBytes = 1024 * 1024
	s, err := New(cfg, "test", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.jobs.Close()
		s.sessions.Close()
		s.runner.KillAll()
	})
	return s, token
}

func request(t *testing.T, s *Server, token, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, "https://agent"+path, reader)
	req.RemoteAddr = "192.0.2.10:12345"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.httpServer.Handler.ServeHTTP(w, req)
	return w
}

func TestHealthRequiresAuth(t *testing.T) {
	s, token := newTestServer(t)
	if got := request(t, s, "", http.MethodGet, "/health", nil).Code; got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated health returned %d", got)
	}
	w := request(t, s, token, http.MethodGet, "/health", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("authenticated health returned %d: %s", w.Code, w.Body.String())
	}
	var health api.HealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &health); err != nil || health.Status != "ok" || health.Version != "test" {
		t.Fatalf("unexpected health response: %+v err=%v", health, err)
	}
}

func TestKillSwitchBrakesExecution(t *testing.T) {
	s, token := newTestServer(t)
	w := request(t, s, token, http.MethodPost, "/killswitch", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("arm returned %d: %s", w.Code, w.Body.String())
	}
	w = request(t, s, token, http.MethodPost, "/exec", api.ExecRequest{Command: "Get-Date", Format: api.FormatLines})
	if w.Code != http.StatusLocked || !strings.Contains(w.Body.String(), "killswitch_active") {
		t.Fatalf("braked exec returned %d: %s", w.Code, w.Body.String())
	}
	w = request(t, s, token, http.MethodGet, "/health", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"kill_switch_armed":true`) {
		t.Fatalf("braked health returned %d: %s", w.Code, w.Body.String())
	}
}

func TestExecRejectsUnknownFieldsAndFormats(t *testing.T) {
	s, token := newTestServer(t)
	w := request(t, s, token, http.MethodPost, "/exec", map[string]any{"command": "Get-Date", "format": "xml"})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_format") {
		t.Fatalf("invalid format returned %d: %s", w.Code, w.Body.String())
	}
	w = request(t, s, token, http.MethodPost, "/exec", map[string]any{"command": "Get-Date", "format": "lines", "surprise": true})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_json") {
		t.Fatalf("unknown field returned %d: %s", w.Code, w.Body.String())
	}
}

func TestAsyncJobLifecycle(t *testing.T) {
	s, token := newTestServer(t)
	w := request(t, s, token, http.MethodPost, "/jobs", api.JobRequest{Command: "Start-Sleep -Milliseconds 100; 'done'", Format: api.FormatLines, TimeoutSec: 5})
	if w.Code != http.StatusAccepted {
		t.Fatalf("job submit returned %d: %s", w.Code, w.Body.String())
	}
	var created api.JobResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.JobID == "" {
		t.Fatalf("invalid create response: %+v err=%v", created, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		w = request(t, s, token, http.MethodGet, "/jobs/"+created.JobID, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("job status returned %d: %s", w.Code, w.Body.String())
		}
		var status api.JobResponse
		if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if status.Status == "completed" {
			if status.Execution == nil || !status.Execution.Success {
				t.Fatalf("invalid completed result: %+v", status)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("job did not complete")
}

func TestAsyncJobTimeoutValidation(t *testing.T) {
	s, token := newTestServer(t)
	w := request(t, s, token, http.MethodPost, "/jobs", api.JobRequest{Command: "Get-Date", Format: api.FormatLines, TimeoutSec: s.cfg.LongCommandTimeoutSec + 1})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_timeout") {
		t.Fatalf("invalid timeout returned %d: %s", w.Code, w.Body.String())
	}
}
