//go:build windows

package tray

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clooooud/wsl-tunneling/internal/config"
)

func TestStartDiagnosticOptIn(t *testing.T) {
	cfg := config.Defaults()
	for _, value := range []string{"", "0", "true", "1"} {
		t.Setenv(startTimingEnv, value)
		diagnostic := newStartDiagnostic(cfg)
		if (diagnostic != nil) != (value == "1") {
			t.Fatalf("diagnostic with %q = %v", value, diagnostic)
		}
	}
	var disabled *startDiagnostic
	if err := disabled.write(time.Second, nil); err != nil {
		t.Fatal(err)
	}
}

func TestStartDiagnosticDurationAndFailure(t *testing.T) {
	t.Setenv(startTimingEnv, "1")
	cfg := config.Defaults()
	cfg.Distro = "test distro"
	cfg.LogDir = t.TempDir()
	var measured []float64
	for _, operationErr := range []error{nil, errors.New("WSL failed")} {
		diagnostic := newStartDiagnostic(cfg)
		var called bool
		elapsed, err := measureOperation(context.Background(), func(ctx context.Context) error {
			called = ctx != nil
			diagnostic.phases.WriteString("start_timing total 4774.000 ms\n")
			return operationErr
		})
		if !called || err != operationErr || elapsed < 0 {
			t.Fatalf("operation = %s, %v, called=%t", elapsed, err, called)
		}
		measured = append(measured, float64(elapsed)/float64(time.Millisecond))
		if err := diagnostic.write(elapsed, err); err != nil {
			t.Fatal(err)
		}
	}
	content, err := os.ReadFile(filepath.Join(cfg.LogDir, startTimingFilename))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(content), []byte{'\n'})
	if len(lines) != 2 {
		t.Fatalf("records = %q", content)
	}
	for i, line := range lines {
		var record startTimingRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record.Success != (i == 0) || record.Distro != cfg.Distro || record.ElapsedMS != measured[i] || record.StartedAt.IsZero() || record.Executable == "" || record.GoVersion == "" || record.Architecture == "" || !strings.Contains(record.Phases, "4774.000") {
			t.Fatalf("record = %+v", record)
		}
		if (record.Error != "") != (i == 1) {
			t.Fatalf("failure information = %+v", record)
		}
	}
}

func TestStartDiagnosticWriteFailureDoesNotChangeResult(t *testing.T) {
	t.Setenv(startTimingEnv, "1")
	cfg := config.Defaults()
	cfg.LogDir = filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(cfg.LogDir, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	diagnostic := newStartDiagnostic(cfg)
	elapsed, operationErr := measureOperation(context.Background(), func(context.Context) error { return nil })
	if diagnostic.write(elapsed, operationErr) == nil || operationErr != nil {
		t.Fatal("expected independent journal error, with operation still successful")
	}
}

func TestStartDiagnosticConcurrentAppend(t *testing.T) {
	t.Setenv(startTimingEnv, "1")
	cfg := config.Defaults()
	cfg.LogDir = t.TempDir()
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := newStartDiagnostic(cfg).write(time.Second, nil); err != nil {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
	content, err := os.ReadFile(filepath.Join(cfg.LogDir, startTimingFilename))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(content), []byte{'\n'})
	if len(lines) != 8 {
		t.Fatalf("record count = %d", len(lines))
	}
	for _, line := range lines {
		if !json.Valid(line) {
			t.Fatalf("partial JSON record: %q", line)
		}
	}
}