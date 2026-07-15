package powershell

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"windowsllmmanager/internal/api"
)

var (
	ErrShellExited = errors.New("PowerShell process exited before returning a complete result")
	ErrTimedOut    = errors.New("PowerShell command timed out")
)

type childProcess struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   *bufio.Reader
	stderr   *boundedBuffer
	killJob  func() error
	waitDone chan error
	killOnce sync.Once
}

func startChild(persistent bool, maxOutput int) (*childProcess, error) {
	args := []string{"-NoLogo", "-NoProfile", "-NonInteractive"}
	if persistent {
		args = append(args, "-NoExit")
	}
	args = append(args, "-Command", "-")
	cmd := exec.Command("powershell.exe", args...)
	prepareCommand(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := newBoundedBuffer(maxOutput)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	killJob, err := attachKillJob(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	p := &childProcess{
		cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), stderr: stderr,
		killJob: killJob, waitDone: make(chan error, 1),
	}
	go func() { p.waitDone <- cmd.Wait() }()
	return p, nil
}

func (p *childProcess) kill() {
	p.killOnce.Do(func() {
		_ = p.stdin.Close()
		if p.killJob != nil {
			_ = p.killJob()
		}
		_ = p.cmd.Process.Kill()
	})
}

type Runner struct {
	MaxOutputBytes int
	mu             sync.Mutex
	active         map[*childProcess]struct{}
}

func NewRunner(maxOutputBytes int) *Runner {
	return &Runner{MaxOutputBytes: maxOutputBytes, active: make(map[*childProcess]struct{})}
}

func (r *Runner) Run(ctx context.Context, command, format string) (api.ExecutionResult, error) {
	started := time.Now()
	id, err := randomID()
	if err != nil {
		return api.ExecutionResult{}, err
	}
	p, err := startChild(false, r.MaxOutputBytes)
	if err != nil {
		return api.ExecutionResult{}, fmt.Errorf("start PowerShell: %w", err)
	}
	r.mu.Lock()
	if r.active == nil {
		r.active = make(map[*childProcess]struct{})
	}
	r.active[p] = struct{}{}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.active, p)
		r.mu.Unlock()
	}()
	defer p.kill()
	wrapper := buildWrapper(command, format, id, r.MaxOutputBytes)
	if _, err := io.WriteString(p.stdin, wrapper+"\r\n"); err != nil {
		return api.ExecutionResult{}, fmt.Errorf("write PowerShell command: %w", err)
	}
	_ = p.stdin.Close()
	frameCh := make(chan struct {
		result framedResult
		err    error
	}, 1)
	go func() {
		result, err := readFrame(p.stdout, id, r.MaxOutputBytes)
		frameCh <- struct {
			result framedResult
			err    error
		}{result, err}
	}()
	select {
	case <-ctx.Done():
		p.kill()
		<-p.waitDone
		return timeoutResult(format, started, false), ErrTimedOut
	case framed := <-frameCh:
		if framed.err != nil {
			p.kill()
			<-p.waitDone
			return api.ExecutionResult{}, fmt.Errorf("%w: %v; stderr=%s", ErrShellExited, framed.err, p.stderr.String())
		}
		select {
		case <-ctx.Done():
			p.kill()
			<-p.waitDone
			return timeoutResult(format, started, false), ErrTimedOut
		case <-p.waitDone:
		}
		return toAPIResult(framed.result, started, false), nil
	}
}

func (r *Runner) KillAll() {
	r.mu.Lock()
	processes := make([]*childProcess, 0, len(r.active))
	for process := range r.active {
		processes = append(processes, process)
	}
	r.mu.Unlock()
	for _, process := range processes {
		process.kill()
	}
}

func timeoutResult(format string, started time.Time, restarted bool) api.ExecutionResult {
	return api.ExecutionResult{
		Success: false, ExitCode: -1, Format: format, Output: json.RawMessage("null"),
		Stderr: []string{}, TimedOut: true, DurationMS: time.Since(started).Milliseconds(),
		SessionRestarted: restarted,
	}
}

func toAPIResult(result framedResult, started time.Time, restarted bool) api.ExecutionResult {
	apiResult := api.ExecutionResult{
		Success: result.Meta.Success, ExitCode: result.Meta.ExitCode, Format: result.Meta.Format,
		Output: json.RawMessage("null"), Stderr: splitLines(string(result.Stderr)),
		Truncated: result.Meta.Truncated, DurationMS: time.Since(started).Milliseconds(),
		SessionRestarted: restarted,
	}
	if result.Meta.Format == api.FormatJSON && !result.Meta.Truncated && json.Valid(result.Output) {
		apiResult.Output = append(json.RawMessage(nil), result.Output...)
	} else if result.Meta.Format == api.FormatLines {
		encoded, _ := json.Marshal(splitLines(string(result.Output)))
		apiResult.Output = encoded
	} else {
		apiResult.RawOutput = string(result.Output)
	}
	return apiResult
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return []string{}
	}
	return strings.Split(s, "\n")
}
