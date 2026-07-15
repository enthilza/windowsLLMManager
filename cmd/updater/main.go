package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"windowsllmmanager/internal/updater"
)

var (
	version                 = "dev"
	embeddedPublicKeyBase64 = ""
)

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
	configPath := flag.String("config", filepath.Join(baseDir, "updater-config.json"), "path to updater config")
	checkOnly := flag.Bool("check-only", false, "check for and install a newer signed release")
	showVersion := flag.Bool("version", false, "print updater version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if !*checkOnly {
		return fmt.Errorf("no action selected; use --check-only")
	}
	cfg, err := updater.Load(*configPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.LogPath), 0750); err != nil {
		return err
	}
	logFile, err := os.OpenFile(cfg.LogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0640)
	if err != nil {
		return err
	}
	defer logFile.Close()
	logger := log.New(logFile, "", log.LstdFlags|log.LUTC)
	publicKey, err := base64.StdEncoding.DecodeString(embeddedPublicKeyBase64)
	if err != nil || len(publicKey) == 0 {
		return fmt.Errorf("updater was built without a valid embedded cosign public key")
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.CheckTimeout()+2*cfg.ServiceTimeout()+30*time.Second)
	defer cancel()
	result, err := updater.New(cfg, publicKey, logger).CheckAndUpdate(ctx)
	if err != nil {
		logger.Printf("update failed: %v", err)
		return err
	}
	logger.Printf("update check complete: current=%s latest=%s updated=%t killed=%t", result.CurrentVersion, result.LatestVersion, result.Updated, result.SkippedKilled)
	return nil
}
