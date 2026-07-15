//go:build windows

package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows/svc"

	"windowsllmmanager/internal/config"
	"windowsllmmanager/internal/server"
	"windowsllmmanager/internal/updatescheduler"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	baseDir := filepath.Dir(exe)
	flags := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	configPath := flags.String("config", filepath.Join(baseDir, "config.json"), "path to config.json")
	genToken := flags.Bool("gen-token", false, "generate a cryptographically random token")
	force := flags.Bool("force", false, "allow --gen-token to overwrite an existing file")
	tokenOutput := flags.String("token-output", filepath.Join(baseDir, "token.txt"), "output path for --gen-token")
	showVersion := flags.Bool("version", false, "print the embedded version")
	console := flags.Bool("console", false, "run in console mode even outside SCM")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if *genToken {
		return generateToken(*tokenOutput, *force)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	logger, closeLog, err := makeLogger(baseDir)
	if err != nil {
		return err
	}
	defer closeLog()

	isService, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("detect Windows service mode: %w", err)
	}
	if isService && !*console {
		return svc.Run(config.ServiceName, &serviceHandler{cfg: cfg, logger: logger})
	}
	return runConsole(cfg, logger)
}

func generateToken(path string, force bool) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return errors.New("token.txt already exists; use --gen-token --force to regenerate")
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(token+"\r\n"), 0600); err != nil {
		return err
	}
	fmt.Println(token)
	return nil
}

func makeLogger(baseDir string) (*log.Logger, func(), error) {
	logDir := filepath.Join(baseDir, "logs")
	if err := os.MkdirAll(logDir, 0750); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(filepath.Join(logDir, "agent.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0640)
	if err != nil {
		return nil, nil, err
	}
	return log.New(f, "", log.LstdFlags|log.LUTC), func() { _ = f.Close() }, nil
}

func runConsole(cfg config.Config, logger *log.Logger) error {
	s, err := server.New(cfg, version, logger)
	if err != nil {
		return err
	}
	errCh, err := s.Start()
	if err != nil {
		return err
	}
	updateCtx, stopUpdates := context.WithCancel(context.Background())
	defer stopUpdates()
	updatescheduler.New(cfg.UpdaterPath, cfg.UpdaterConfigPath, cfg.UpdateCheckInterval(), logger).Start(updateCtx)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case <-sigCh:
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return s.Shutdown(ctx)
	}
}

type serviceHandler struct {
	cfg    config.Config
	logger *log.Logger
}

func (h *serviceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const commandsAccepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}
	s, err := server.New(h.cfg, version, h.logger)
	if err != nil {
		h.logger.Printf("service initialization failed: %v", err)
		return false, 1
	}
	errCh, err := s.Start()
	if err != nil {
		h.logger.Printf("HTTPS listener startup failed: %v", err)
		return false, 1
	}
	changes <- svc.Status{State: svc.Running, Accepts: commandsAccepted}
	updateCtx, stopUpdates := context.WithCancel(context.Background())
	defer stopUpdates()
	go removeLegacyUpdateTask(h.logger)
	updatescheduler.New(h.cfg.UpdaterPath, h.cfg.UpdaterConfigPath, h.cfg.UpdateCheckInterval(), h.logger).Start(updateCtx)
	for {
		select {
		case err := <-errCh:
			if err != nil && !strings.Contains(err.Error(), "Server closed") {
				h.logger.Printf("server stopped: %v", err)
				return false, 1
			}
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				changes <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				_ = s.Shutdown(ctx)
				cancel()
				return false, 0
			}
		}
	}
}

func removeLegacyUpdateTask(logger *log.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	removed, err := updatescheduler.RemoveLegacyScheduledTask(ctx)
	if err != nil {
		logger.Printf("legacy update Scheduled Task cleanup failed: %v", err)
		return
	}
	if removed {
		logger.Printf("removed legacy update Scheduled Task %s; agent-managed scheduler is active", updatescheduler.LegacyTaskName)
	}
}
