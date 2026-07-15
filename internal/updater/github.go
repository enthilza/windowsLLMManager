package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
)

type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type release struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

type githubClient struct {
	httpClient *http.Client
	owner      string
	repository string
}

func (g githubClient) latest(ctx context.Context) (release, error) {
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", url.PathEscape(g.owner), url.PathEscape(g.repository))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "WindowsLLMManager-Updater")
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return release{}, fmt.Errorf("GitHub releases API returned %s: %s", resp.Status, body)
	}
	var result release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024)).Decode(&result); err != nil {
		return release{}, err
	}
	return result, nil
}

func (g githubClient) download(ctx context.Context, sourceURL, destination string, maxBytes int64) error {
	u, err := url.Parse(sourceURL)
	if err != nil || u.Scheme != "https" {
		return fmt.Errorf("release asset URL must use HTTPS")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "WindowsLLMManager-Updater")
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s returned %s", filepath.Base(destination), resp.Status)
	}
	f, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(f, io.LimitReader(resp.Body, maxBytes+1))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil || written > maxBytes {
		_ = os.Remove(destination)
		if written > maxBytes {
			return fmt.Errorf("download exceeds %d bytes", maxBytes)
		}
		return fmt.Errorf("write download: %v %v", copyErr, closeErr)
	}
	return nil
}
