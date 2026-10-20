package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clooooud/wsl-tunneling/internal/config"
)

func TestRunHelpWritesUsageToStdout(t *testing.T) {
	code, stdout, stderr := runCLI(t, "help")

	if code != 0 {
		t.Fatalf("Run() code = %d, want 0", code)
	}
	assertContains(t, stdout, "Usage:")
	assertContains(t, stdout, "Use wsl-tunneling.exe to open the Windows tray controller.")
	assertContains(t, stdout, "init-config")
	assertNotContains(t, stdout, "  tray")
	assertEmpty(t, "stderr", stderr)
}

func TestRunWithoutCommandWritesUsageToStdout(t *testing.T) {
	code, stdout, stderr := runCLI(t)

	if code != 0 {
		t.Fatalf("Run() code = %d, want 0", code)
	}
	assertContains(t, stdout, "Usage:")
	assertEmpty(t, "stderr", stderr)
}

func TestRunUnknownCommandWritesUsageToStderr(t *testing.T) {
	code, stdout, stderr := runCLI(t, "does-not-exist")

	if code != 2 {
		t.Fatalf("Run() code = %d, want 2", code)
	}
	assertEmpty(t, "stdout", stdout)
	assertContains(t, stderr, `unknown command "does-not-exist"`)
	assertContains(t, stderr, "Usage:")
}

func TestRunUnknownFlagWritesFlagErrorToStderr(t *testing.T) {
	code, stdout, stderr := runCLI(t, "status", "--definitely-unknown")

	if code != 2 {
		t.Fatalf("Run() code = %d, want 2", code)
	}
	assertEmpty(t, "stdout", stdout)
	assertContains(t, stderr, "flag provided but not defined")
	assertContains(t, stderr, "definitely-unknown")
}

func TestRunStatusRequiresDistro(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-config.json")
	code, stdout, stderr := runCLI(t, "status", "--config", path)

	if code != 2 {
		t.Fatalf("Run() code = %d, want 2", code)
	}
	assertEmpty(t, "stdout", stdout)
	assertContains(t, stderr, "distro is required")
}

func TestRunInitConfigWritesExampleConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wsl-tunneling", "config.json")
	code, stdout, stderr := runCLI(t, "init-config", "--config", path)

	if code != 0 {
		t.Fatalf("Run() code = %d, want 0; stderr = %q", code, stderr)
	}
	assertEmpty(t, "stdout", stdout)
	assertEmpty(t, "stderr", stderr)

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	var cfg config.Config
	if err := json.Unmarshal(content, &cfg); err != nil {
		t.Fatalf("example config is invalid JSON: %v", err)
	}
	if cfg.Distro != "Ubuntu" {
		t.Fatalf("example distro = %q, want Ubuntu", cfg.Distro)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("example config Validate() error = %v", err)
	}
}

func TestRunLogsReadsConfiguredLogFile(t *testing.T) {
	root := t.TempDir()
	logDir := filepath.Join(root, "logs")
	configPath := filepath.Join(root, "config.json")
	writeConfig(t, configPath, map[string]any{"logDir": logDir})
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", logDir, err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "wsl-tunneling.log"), []byte("first line\nsecond line\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(log) error = %v", err)
	}

	code, stdout, stderr := runCLI(t, "logs", "--config", configPath)

	if code != 0 {
		t.Fatalf("Run() code = %d, want 0; stderr = %q", code, stderr)
	}
	assertContains(t, stdout, "first line\nsecond line\n")
	assertEmpty(t, "stderr", stderr)
}

func TestRunLogsReportsMissingLogFile(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	writeConfig(t, configPath, map[string]any{"logDir": filepath.Join(root, "logs")})

	code, stdout, stderr := runCLI(t, "logs", "--config", configPath)

	if code != 1 {
		t.Fatalf("Run() code = %d, want 1", code)
	}
	assertEmpty(t, "stdout", stdout)
	assertContains(t, stderr, "read logs:")
}

func TestRunTrayCommandIsUnknown(t *testing.T) {
	code, stdout, stderr := runCLI(t, "tray")

	if code != 2 {
		t.Fatalf("Run() code = %d, want 2", code)
	}
	assertEmpty(t, "stdout", stdout)
	assertContains(t, stderr, `unknown command "tray"`)
}

func TestParseConfigAppliesFileAndFlagInputs(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	writeConfig(t, configPath, map[string]any{
		"distro":                    "FromFile",
		"gvisorVersion":             "v0.1.0",
		"cacheDir":                  filepath.Join(root, "cache-file"),
		"stateDir":                  filepath.Join(root, "state-file"),
		"interfaceName":             "from-file",
		"gvproxyUrl":                "https://example.invalid/gvproxy-file.exe",
		"gvforwarderUrl":            "https://example.invalid/gvforwarder-file",
		"terminateOnStop":           false,
		"supervisorIntervalSeconds": 5,
	})

	var stderr bytes.Buffer
	cfg, gotPath, timings, err := parseConfig([]string{
		"--config", configPath,
		"--distro", "FromFlag",
		"--gvisor-version", "v9.9.9",
		"--cache-dir", filepath.Join(root, "cache-flag"),
		"--state-dir", filepath.Join(root, "state-flag"),
		"--iface", "from-flag",
		"--gvproxy-url", "https://example.invalid/gvproxy-flag.exe",
		"--gvforwarder-url", "https://example.invalid/gvforwarder-flag",
		"--terminate-on-stop",
	}, &stderr)
	if err != nil {
		t.Fatalf("parseConfig() error = %v; stderr = %q", err, stderr.String())
	}
	if gotPath != configPath {
		t.Fatalf("parseConfig() path = %q, want %q", gotPath, configPath)
	}
	if timings {
		t.Fatal("timings = true, want false by default")
	}

	assertEqual(t, "Distro", cfg.Distro, "FromFlag")
	assertEqual(t, "GvisorVersion", cfg.GvisorVersion, "v9.9.9")
	assertEqual(t, "CacheDir", cfg.CacheDir, filepath.Join(root, "cache-flag"))
	assertEqual(t, "StateDir", cfg.StateDir, filepath.Join(root, "state-flag"))
	assertEqual(t, "InterfaceName", cfg.InterfaceName, "from-flag")
	assertEqual(t, "GVProxyURL", cfg.GVProxyURL, "https://example.invalid/gvproxy-flag.exe")
	assertEqual(t, "GVForwarderURL", cfg.GVForwarderURL, "https://example.invalid/gvforwarder-flag")
	if !cfg.TerminateOnStop {
		t.Fatal("TerminateOnStop = false, want true")
	}
}

func TestRunTimingsRequiresStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-config.json")
	code, stdout, stderr := runCLI(t, "stop", "--timings", "--config", path)
	if code != 2 {
		t.Fatalf("Run() code = %d, want 2", code)
	}
	assertEmpty(t, "stdout", stdout)
	assertContains(t, stderr, "--timings is only supported with start")
}

func TestRunStartTimingsReportsValidationFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-config.json")
	code, stdout, stderr := runCLI(t, "start", "--timings", "--config", path)
	if code != 1 {
		t.Fatalf("Run() code = %d, want 1", code)
	}
	assertContains(t, stdout, "start_timing total")
	assertContains(t, stderr, "distro is required")
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func writeConfig(t *testing.T, path string, values map[string]any) {
	t.Helper()
	content, err := json.Marshal(values)
	if err != nil {
		t.Fatalf("Marshal(config) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(config dir) error = %v", err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
}

func assertContains(t *testing.T, got string, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("output = %q, want substring %q", got, want)
	}
}

func assertNotContains(t *testing.T, got string, unwanted string) {
	t.Helper()
	if strings.Contains(got, unwanted) {
		t.Fatalf("output = %q, unwanted substring %q", got, unwanted)
	}
}

func assertEmpty(t *testing.T, name string, got string) {
	t.Helper()
	if got != "" {
		t.Fatalf("%s = %q, want empty", name, got)
	}
}

func assertEqual(t *testing.T, name string, got string, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %q, want %q", name, got, want)
	}
}
