package gvisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/clooooud/wsl-tunneling/internal/config"
)

type Assets struct {
	GVProxyPath     string
	GVForwarderPath string
}

type release struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
}

func Ensure(ctx context.Context, cfg config.Config) (Assets, error) {
	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		return Assets{}, err
	}

	assets := Paths(cfg)
	if fileExists(assets.GVProxyPath) && fileExists(assets.GVForwarderPath) {
		return prepareAssets(assets)
	}

	gvproxyURL := cfg.GVProxyURL
	gvforwarderURL := cfg.GVForwarderURL
	if gvproxyURL == "" || gvforwarderURL == "" {
		rel, err := fetchRelease(ctx, cfg)
		if err != nil {
			return Assets{}, err
		}
		if gvproxyURL == "" {
			gvproxyURL = findAsset(rel.Assets, "gvproxy", runtime.GOOS, runtime.GOARCH)
		}
		if gvforwarderURL == "" {
			gvforwarderURL = findAsset(rel.Assets, "gvforwarder", "linux", runtime.GOARCH)
		}
	}

	if gvproxyURL == "" {
		return Assets{}, errors.New("could not find a gvproxy release asset; set gvproxyUrl in config")
	}
	if gvforwarderURL == "" {
		return Assets{}, errors.New("could not find a gvforwarder release asset; set gvforwarderUrl in config")
	}

	if !fileExists(assets.GVProxyPath) {
		if err := download(ctx, gvproxyURL, assets.GVProxyPath, cfg.DownloadPermissions); err != nil {
			return Assets{}, fmt.Errorf("download gvproxy: %w", err)
		}
	}
	if !fileExists(assets.GVForwarderPath) {
		if err := download(ctx, gvforwarderURL, assets.GVForwarderPath, cfg.DownloadPermissions); err != nil {
			return Assets{}, fmt.Errorf("download gvforwarder: %w", err)
		}
	}

	return prepareAssets(assets)
}

func prepareAssets(assets Assets) (Assets, error) {
	path, err := consoleFreeProxy(assets.GVProxyPath)
	if err != nil {
		return Assets{}, fmt.Errorf("prepare background gvproxy: %w", err)
	}
	assets.GVProxyPath = path
	return assets, nil
}

func Paths(cfg config.Config) Assets {
	versionDir := strings.TrimPrefix(cfg.GvisorVersion, "v")
	gvproxyName := "gvproxy"
	if runtime.GOOS == "windows" {
		gvproxyName += ".exe"
	}
	return Assets{
		GVProxyPath:     filepath.Join(cfg.CacheDir, versionDir, gvproxyName),
		GVForwarderPath: filepath.Join(cfg.CacheDir, versionDir, "gvforwarder"),
	}
}

func Status(cfg config.Config) map[string]bool {
	assets := Paths(cfg)
	return map[string]bool{
		"gvproxy":     fileExists(assets.GVProxyPath),
		"gvforwarder": fileExists(assets.GVForwarderPath),
	}
}

func fetchRelease(ctx context.Context, cfg config.Config) (release, error) {
	baseURL := strings.TrimRight(cfg.GitHubAPIBaseURL, "/")
	url := fmt.Sprintf("%s/repos/containers/gvisor-tap-vsock/releases/tags/%s", baseURL, cfg.GvisorVersion)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "wsl-tunneling")

	client := http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return release{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return release{}, fmt.Errorf("GitHub release request failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var rel release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return release{}, err
	}
	return rel, nil
}

func findAsset(assets []releaseAsset, binaryName string, goos string, goarch string) string {
	if binaryName == "gvforwarder" {
		for _, asset := range assets {
			if strings.EqualFold(asset.Name, "gvforwarder") {
				return asset.DownloadURL
			}
		}
	}

	if binaryName == "gvproxy" && goos == "windows" && goarch != "arm64" {
		for _, asset := range assets {
			if strings.EqualFold(asset.Name, "gvproxy-windows.exe") {
				return asset.DownloadURL
			}
		}
	}

	for _, asset := range assets {
		name := strings.ToLower(asset.Name)
		if strings.Contains(name, binaryName) && strings.Contains(name, goos) && strings.Contains(name, goarch) {
			return asset.DownloadURL
		}
	}
	for _, asset := range assets {
		name := strings.ToLower(asset.Name)
		if strings.Contains(name, binaryName) && strings.Contains(name, goos) {
			return asset.DownloadURL
		}
	}
	return ""
}

func download(ctx context.Context, url string, destination string, permissions os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "wsl-tunneling")

	client := http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download failed: %s", resp.Status)
	}

	temporary := destination + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, permissions)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, resp.Body)
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(temporary)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(temporary)
		return closeErr
	}
	return os.Rename(temporary, destination)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
