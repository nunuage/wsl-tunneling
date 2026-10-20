package network

import (
	"bytes"
	"context"
	"debug/pe"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clooooud/wsl-tunneling/internal/config"
	"github.com/clooooud/wsl-tunneling/internal/gvisor"
	"github.com/clooooud/wsl-tunneling/internal/wsl"
)

type scriptClient struct {
	wsl.Client    // Other methods are unused by startTunnel.
	scripts       []string
	result        wsl.Result
	failCall      int
	err           error
	distroChecks  int
	distroMissing bool
	distroErr     error
}

func (client *scriptClient) DistroExists(context.Context, string) (bool, error) {
	client.distroChecks++
	return !client.distroMissing, client.distroErr
}

func (client *scriptClient) Bash(_ context.Context, _ string, script string) (wsl.Result, error) {
	client.scripts = append(client.scripts, script)
	if len(client.scripts) == client.failCall {
		return wsl.Result{}, client.err
	}
	if len(client.scripts) == 1 {
		return client.result, nil
	}
	return wsl.Result{}, nil
}

func TestStartTunnelInteropHandoff(t *testing.T) {
	sentinel := errors.New("script failure")
	for _, tt := range []struct {
		name     string
		stdout   string
		failCall int
		calls    int
	}{
		{"new helper", "forwarder_started=1\n", 0, 2},
		{"new helper with profile output", "shell profile\nforwarder_started=1\n", 0, 2},
		{"healthy existing", "", 0, 1},
		{"unrelated stdout", "not_forwarder_started=1\n", 0, 1},
		{"start failure", "", 1, 1},
		{"post-handoff failure", "forwarder_started=1\n", 2, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &scriptClient{result: wsl.Result{Stdout: tt.stdout}, failCall: tt.failCall, err: sentinel}
			manager := Manager{Config: config.Defaults(), WSL: client}
			err := manager.startTunnel(context.Background(), "/proxy", "/forwarder", []string{"corp.example.test"}, nil)
			if (tt.failCall != 0 && !errors.Is(err, sentinel)) || (tt.failCall == 0 && err != nil) {
				t.Fatalf("startTunnel error = %v, failCall = %d", err, tt.failCall)
			}
			if len(client.scripts) != tt.calls {
				t.Fatalf("Bash calls = %d, want %d", len(client.scripts), tt.calls)
			}
			for _, script := range client.scripts {
				if !strings.Contains(script, "DNS_SEARCH='corp.example.test'") {
					t.Fatal("DNS suffixes must be retained through handoff")
				}
			}
			if tt.calls == 2 && !strings.Contains(client.scripts[1], "wait_for_network 15 6") {
				t.Fatal("new helper must be validated for consecutive stability after handoff")
			}
			wantChecks := 0
			if tt.failCall == 1 {
				wantChecks = 1
			}
			if client.distroChecks != wantChecks {
				t.Fatalf("distro diagnostic calls = %d, want %d", client.distroChecks, wantChecks)
			}
		})
	}
}

func TestStartTunnelDistroDiagnostic(t *testing.T) {
	sentinel := errors.New("original WSL failure")
	for _, tt := range []struct {
		name       string
		missing    bool
		diagnostic error
		canceled   bool
	}{
		{"existing distro", false, nil, false},
		{"missing distro", true, nil, false},
		{"diagnostic fails", true, errors.New("diagnostic failure"), false},
		{"canceled", true, nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.canceled {
				cancel()
			}
			client := &scriptClient{failCall: 1, err: sentinel, distroMissing: tt.missing, distroErr: tt.diagnostic}
			manager := Manager{Config: config.Defaults(), WSL: client}
			err := manager.startTunnel(ctx, "/proxy", "/forwarder", nil, nil)
			if !errors.Is(err, sentinel) {
				t.Fatalf("lost original WSL error: %v", err)
			}
			wantMissing := tt.missing && tt.diagnostic == nil && !tt.canceled
			if strings.Contains(err.Error(), "does not exist") != wantMissing {
				t.Fatalf("unexpected diagnostic: %v", err)
			}
			wantChecks := 1
			if tt.canceled {
				wantChecks = 0
			}
			if client.distroChecks != wantChecks {
				t.Fatalf("distro diagnostic calls = %d, want %d", client.distroChecks, wantChecks)
			}
		})
	}
}

func TestStartChecksDistroOnlyBeforeDownloads(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(map[bool]string{false: "uncached", true: "cached"}[cached], func(t *testing.T) {
			root := t.TempDir()
			cfg := config.Defaults()
			cfg.Distro = "test distro"
			cfg.CacheDir = filepath.Join(root, "bin")
			cfg.StateDir = filepath.Join(root, "state")
			cfg.LogDir = filepath.Join(root, "logs")
			cfg.DNSSearchSuffixes = []string{"test.example"}
			if cached {
				writeTestAssets(t, cfg)
			}
			client := &scriptClient{distroMissing: !cached}
			manager := Manager{Config: cfg, WSL: client}
			var timings bytes.Buffer
			err := manager.StartWithTimings(context.Background(), &timings)
			if !cached {
				if err == nil || !strings.Contains(err.Error(), "does not exist") || client.distroChecks != 1 || len(client.scripts) != 0 {
					t.Fatalf("uncached start = %v, checks=%d, scripts=%d", err, client.distroChecks, len(client.scripts))
				}
				if _, err := os.Stat(cfg.CacheDir); err != nil {
					t.Fatal(err)
				}
				files, _ := os.ReadDir(cfg.CacheDir)
				if len(files) != 0 {
					t.Fatal("missing distro must be rejected before assets are downloaded")
				}
				return
			}
			if err != nil || client.distroChecks != 0 || len(client.scripts) != 1 {
				t.Fatalf("cached start = %v, checks=%d, scripts=%d", err, client.distroChecks, len(client.scripts))
			}
			if !strings.Contains(timings.String(), "start_note check_distro skipped") || !strings.Contains(timings.String(), "start_timing total") {
				t.Fatalf("missing skip annotation or total: %s", &timings)
			}
		})
	}
}

// A minimal GUI-subsystem PE is accepted without modification on Windows;
// other platforms treat these fixtures as ordinary cached assets.
func writeTestAssets(t *testing.T, cfg config.Config) {
	t.Helper()
	paths := gvisor.Paths(cfg)
	if err := os.MkdirAll(filepath.Dir(paths.GVProxyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	dos := make([]byte, 128)
	copy(dos, "MZ")
	binary.LittleEndian.PutUint32(dos[0x3c:], 128)
	var image bytes.Buffer
	image.Write(dos)
	image.WriteString("PE\x00\x00")
	header := pe.OptionalHeader32{Magic: 0x10b, Subsystem: pe.IMAGE_SUBSYSTEM_WINDOWS_GUI, NumberOfRvaAndSizes: 16}
	for _, value := range []any{pe.FileHeader{Machine: pe.IMAGE_FILE_MACHINE_I386, SizeOfOptionalHeader: uint16(binary.Size(header))}, header} {
		if err := binary.Write(&image, binary.LittleEndian, value); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{paths.GVProxyPath, paths.GVForwarderPath} {
		if err := os.WriteFile(path, image.Bytes(), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}
