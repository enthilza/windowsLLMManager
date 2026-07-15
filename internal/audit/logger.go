package audit

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Event struct {
	Timestamp        string         `json:"timestamp"`
	Type             string         `json:"type"`
	RequestID        string         `json:"request_id,omitempty"`
	SourceIP         string         `json:"source_ip,omitempty"`
	TokenFingerprint string         `json:"token_fingerprint,omitempty"`
	Mode             string         `json:"mode,omitempty"`
	SessionID        string         `json:"session_id,omitempty"`
	Command          string         `json:"command,omitempty"`
	Success          *bool          `json:"success,omitempty"`
	ExitCode         *int           `json:"exit_code,omitempty"`
	Details          map[string]any `json:"details,omitempty"`
}

type Logger struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
}

func New(path string, maxBytes int64) (*Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return nil, fmt.Errorf("create audit directory: %w", err)
	}
	return &Logger{path: path, maxBytes: maxBytes}, nil
}

func (l *Logger) Write(event Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if event.Timestamp == "" {
		event.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if err := l.rotateIfNeeded(); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0640)
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(event); err != nil {
		return fmt.Errorf("write audit event: %w", err)
	}
	return f.Sync()
}

func (l *Logger) rotateIfNeeded() error {
	info, err := os.Stat(l.path)
	if os.IsNotExist(err) || (err == nil && info.Size() < l.maxBytes) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat audit log: %w", err)
	}
	stamp := time.Now().UTC().Format("20060102-150405.000000000")
	zipPath := filepath.Join(filepath.Dir(l.path), "audit-"+stamp+".zip")
	zf, err := os.OpenFile(zipPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
	if err != nil {
		return fmt.Errorf("create audit archive: %w", err)
	}
	zw := zip.NewWriter(zf)
	w, err := zw.Create(filepath.Base(l.path))
	if err == nil {
		var src *os.File
		src, err = os.Open(l.path)
		if err == nil {
			_, err = io.Copy(w, src)
			src.Close()
		}
	}
	closeErr := zw.Close()
	fileCloseErr := zf.Close()
	if err != nil {
		_ = os.Remove(zipPath)
		return fmt.Errorf("archive audit log: %w", err)
	}
	if closeErr != nil || fileCloseErr != nil {
		_ = os.Remove(zipPath)
		return fmt.Errorf("close audit archive: %v %v", closeErr, fileCloseErr)
	}
	if err := os.Truncate(l.path, 0); err != nil {
		return fmt.Errorf("truncate audit log: %w", err)
	}
	return nil
}
