//go:build windows

package tray

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"github.com/clooooud/wsl-tunneling/internal/config"
)

const startTimingEnv = "WSL_TUNNELING_START_TIMINGS"
const startTimingFilename = "start-timings.jsonl"

var startTimingMu sync.Mutex

type startDiagnostic struct {
	phases   bytes.Buffer
	cfg      config.Config
	start    time.Time
	scenario string
	cycle    int
}

type startTimingRecord struct {
	StartedAt     time.Time `json:"startedAt"`
	Scenario      string    `json:"scenario,omitempty"`
	Cycle         int       `json:"cycle,omitempty"`
	ElapsedMS     float64   `json:"elapsedMs"`
	Success       bool      `json:"success"`
	Error         string    `json:"error,omitempty"`
	Distro        string    `json:"distro"`
	GvisorVersion string    `json:"gvisorVersion"`
	Executable    string    `json:"executable"`
	GoVersion     string    `json:"goVersion"`
	Architecture  string    `json:"architecture"`
	Version       string    `json:"version,omitempty"`
	Revision      string    `json:"revision,omitempty"`
	Modified      bool      `json:"modified"`
	Phases        string    `json:"phases"`
}

func newStartDiagnostic(cfg config.Config) *startDiagnostic {
	if os.Getenv(startTimingEnv) != "1" {
		return nil
	}
	return &startDiagnostic{cfg: cfg, start: time.Now()}
}

// Shared with the integration harness. No journal, notification, queue or status
// refresh is part of the operation duration (including when the operation fails).
func measureOperation(ctx context.Context, operation func(context.Context) error) (time.Duration, error) {
	started := time.Now()
	err := operation(ctx)
	return time.Since(started), err
}

// Called only after elapsed has been captured and the notification queued.
// Logging is best-effort and cannot turn a successful start into a failure.
func (diagnostic *startDiagnostic) write(elapsed time.Duration, operationErr error) error {
	if diagnostic == nil {
		return nil
	}
	record := startTimingRecord{
		StartedAt: diagnostic.start, ElapsedMS: float64(elapsed) / float64(time.Millisecond),
		Scenario: diagnostic.scenario, Cycle: diagnostic.cycle,
		Success: operationErr == nil, Distro: diagnostic.cfg.Distro,
		GvisorVersion: diagnostic.cfg.GvisorVersion, GoVersion: runtime.Version(),
		Architecture: runtime.GOARCH, Phases: diagnostic.phases.String(),
	}
	if operationErr != nil {
		record.Error = operationErr.Error()
	}
	record.Executable, _ = os.Executable()
	if info, ok := debug.ReadBuildInfo(); ok {
		record.Version = info.Main.Version
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				record.Revision = setting.Value
			case "vcs.modified":
				record.Modified = setting.Value == "true"
			}
		}
	}
	content, err := json.Marshal(record)
	if err != nil {
		return err
	}
	content = append(content, '\n')
	startTimingMu.Lock()
	defer startTimingMu.Unlock()
	if err := os.MkdirAll(diagnostic.cfg.LogDir, 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(diagnostic.cfg.LogDir, startTimingFilename), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(content)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}