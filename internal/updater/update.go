package updater

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Updater struct {
	cfg       Config
	publicKey []byte
	logger    *log.Logger
	github    githubClient
}

type Result struct {
	Updated        bool
	SkippedKilled  bool
	CurrentVersion string
	LatestVersion  string
}

func New(cfg Config, publicKey []byte, logger *log.Logger) *Updater {
	return &Updater{
		cfg: cfg, publicKey: append([]byte(nil), publicKey...), logger: logger,
		github: githubClient{httpClient: &http.Client{Timeout: cfg.CheckTimeout()}, owner: cfg.GitHubOwner, repository: cfg.GitHubRepository},
	}
}

func (u *Updater) CheckAndUpdate(ctx context.Context) (Result, error) {
	if u.isKilled() {
		u.logger.Printf("update skipped: kill-switch is armed")
		return Result{SkippedKilled: true}, nil
	}
	current, err := u.currentVersion(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("read installed agent version: %w", err)
	}
	rel, err := u.github.latest(ctx)
	if err != nil {
		return Result{CurrentVersion: current}, err
	}
	result := Result{CurrentVersion: current, LatestVersion: rel.TagName}
	newer, err := newerVersion(current, rel.TagName)
	if err != nil {
		return result, err
	}
	if !newer {
		u.logger.Printf("agent is current: installed=%s latest=%s", current, rel.TagName)
		return result, nil
	}
	assets, err := u.requiredAssets(rel)
	if err != nil {
		return result, err
	}
	paths, cleanup, err := u.downloadAndVerify(ctx, assets)
	if err != nil {
		return result, err
	}
	defer cleanup()
	downloadedVersion, err := u.binaryVersion(ctx, paths.binary)
	if err != nil {
		return result, fmt.Errorf("read downloaded agent version: %w", err)
	}
	if !versionsEqual(downloadedVersion, rel.TagName) {
		return result, fmt.Errorf("signed agent version %q does not match release tag %q", downloadedVersion, rel.TagName)
	}
	if u.isKilled() {
		u.logger.Printf("update aborted after verification: kill-switch became armed")
		result.SkippedKilled = true
		return result, nil
	}
	if err := u.installVerified(ctx, paths.binary); err != nil {
		return result, err
	}
	result.Updated = true
	u.logger.Printf("agent updated successfully: %s -> %s", current, rel.TagName)
	return result, nil
}

type assetURLs struct{ binary, checksum, signature string }
type downloadedPaths struct{ binary, checksum, signature string }

func (u *Updater) requiredAssets(rel release) (assetURLs, error) {
	names := map[string]*string{
		u.cfg.AgentAssetName:             nil,
		u.cfg.AgentAssetName + ".sha256": nil,
		u.cfg.AgentAssetName + ".sig":    nil,
	}
	found := make(map[string]string)
	for _, asset := range rel.Assets {
		if _, needed := names[asset.Name]; needed {
			found[asset.Name] = asset.URL
		}
	}
	for name := range names {
		if found[name] == "" {
			return assetURLs{}, fmt.Errorf("release %s is missing required asset %s", rel.TagName, name)
		}
	}
	return assetURLs{
		binary: found[u.cfg.AgentAssetName], checksum: found[u.cfg.AgentAssetName+".sha256"],
		signature: found[u.cfg.AgentAssetName+".sig"],
	}, nil
}

func (u *Updater) downloadAndVerify(ctx context.Context, assets assetURLs) (downloadedPaths, func(), error) {
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return downloadedPaths{}, func() {}, err
	}
	prefix := filepath.Join(filepath.Dir(u.cfg.AgentPath), ".update-"+hex.EncodeToString(idBytes))
	paths := downloadedPaths{binary: prefix + ".exe", checksum: prefix + ".sha256", signature: prefix + ".sig"}
	cleanup := func() {
		_ = os.Remove(paths.binary)
		_ = os.Remove(paths.checksum)
		_ = os.Remove(paths.signature)
	}
	if err := u.github.download(ctx, assets.binary, paths.binary, 200*1024*1024); err != nil {
		cleanup()
		return downloadedPaths{}, func() {}, err
	}
	if err := u.github.download(ctx, assets.checksum, paths.checksum, 4096); err != nil {
		cleanup()
		return downloadedPaths{}, func() {}, err
	}
	if err := u.github.download(ctx, assets.signature, paths.signature, 1024*1024); err != nil {
		cleanup()
		return downloadedPaths{}, func() {}, err
	}
	if err := verifyChecksum(paths.binary, paths.checksum); err != nil {
		cleanup()
		return downloadedPaths{}, func() {}, err
	}
	if err := verifyCosignBlob(ctx, paths.binary, paths.signature, u.publicKey); err != nil {
		cleanup()
		return downloadedPaths{}, func() {}, err
	}
	return paths, cleanup, nil
}

func (u *Updater) installVerified(parent context.Context, downloaded string) error {
	serviceCtx, cancel := context.WithTimeout(parent, u.cfg.ServiceTimeout())
	defer cancel()
	// Windows may lock a running executable. Stop first, then create the backup.
	if err := stopService(serviceCtx, u.cfg.ServiceName); err != nil {
		return fmt.Errorf("stop service: %w", err)
	}
	if u.isKilled() {
		restartCtx, restartCancel := context.WithTimeout(context.Background(), u.cfg.ServiceTimeout())
		defer restartCancel()
		_ = startService(restartCtx, u.cfg.ServiceName)
		return errors.New("kill-switch became armed; update aborted before replacing agent")
	}
	backup := u.cfg.AgentPath + ".bak"
	_ = os.Remove(backup)
	if err := os.Rename(u.cfg.AgentPath, backup); err != nil {
		u.bestEffortStart()
		return fmt.Errorf("backup current agent after service stop: %w", err)
	}
	if err := os.Rename(downloaded, u.cfg.AgentPath); err != nil {
		_ = os.Rename(backup, u.cfg.AgentPath)
		u.bestEffortStart()
		return fmt.Errorf("install verified agent: %w", err)
	}
	startCtx, startCancel := context.WithTimeout(context.Background(), u.cfg.ServiceTimeout())
	startErr := startService(startCtx, u.cfg.ServiceName)
	startCancel()
	if startErr == nil {
		// Keep one known-good generation. It will be replaced on the next update.
		return nil
	}
	u.logger.Printf("new agent failed to start, rolling back: %v", startErr)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), u.cfg.ServiceTimeout())
	_ = stopService(stopCtx, u.cfg.ServiceName)
	stopCancel()
	failedPath := u.cfg.AgentPath + ".failed-" + time.Now().UTC().Format("20060102-150405")
	_ = os.Rename(u.cfg.AgentPath, failedPath)
	if err := os.Rename(backup, u.cfg.AgentPath); err != nil {
		return fmt.Errorf("rollback restore failed after start error %v: %w", startErr, err)
	}
	rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), u.cfg.ServiceTimeout())
	defer rollbackCancel()
	if err := startService(rollbackCtx, u.cfg.ServiceName); err != nil {
		return fmt.Errorf("new version failed (%v) and rolled-back service failed to start: %w", startErr, err)
	}
	return fmt.Errorf("new version failed to start and was rolled back: %w", startErr)
}

func (u *Updater) bestEffortStart() {
	ctx, cancel := context.WithTimeout(context.Background(), u.cfg.ServiceTimeout())
	defer cancel()
	_ = startService(ctx, u.cfg.ServiceName)
}

func (u *Updater) currentVersion(ctx context.Context) (string, error) {
	return u.binaryVersion(ctx, u.cfg.AgentPath)
}

func (u *Updater) binaryVersion(ctx context.Context, path string) (string, error) {
	cmd := exec.CommandContext(ctx, path, "--version")
	prepareHiddenCommand(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return "", errors.New("agent returned an empty version")
	}
	return value, nil
}

func (u *Updater) isKilled() bool {
	_, err := os.Stat(u.cfg.KillSwitchPath)
	return err == nil || !os.IsNotExist(err)
}
