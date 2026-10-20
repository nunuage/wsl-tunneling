package network

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/clooooud/wsl-tunneling/internal/config"
)

func TestStartProfilerParsesShellSubphases(t *testing.T) {
	var output bytes.Buffer
	profiler := newStartProfiler(&output)
	profiler.shell("wsl_start", `unrelated stderr
__wsl_tunneling_timing__ begin 100.00
__wsl_tunneling_timing__ prepare_state 100.25
__wsl_tunneling_timing__ invalid not-a-number
__wsl_tunneling_timing__ invalid NaN
__wsl_tunneling_timing__ invalid +Inf
__wsl_tunneling_timing__ backwards 99.00
__wsl_tunneling_timing__ launch_forwarder 103.25
`)
	for _, want := range []string{"wsl_start.prepare_state", "250.000 ms", "wsl_start.launch_forwarder", "3000.000 ms"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("timings = %q, want %q", output.String(), want)
		}
	}
	for _, unwanted := range []string{"invalid", "backwards", "unrelated"} {
		if strings.Contains(output.String(), unwanted) {
			t.Fatalf("timings include %q: %s", unwanted, output.String())
		}
	}
}

func TestStartProfilerReportsPhaseAndTotal(t *testing.T) {
	var output bytes.Buffer
	profiler := newStartProfiler(&output)
	profiler.previous = time.Now().Add(-time.Second)
	profiler.mark("prepare_local")
	profiler.finish()
	if !strings.Contains(output.String(), "start_timing prepare_local") || !strings.Contains(output.String(), "start_timing total") {
		t.Fatalf("missing timings: %s", output.String())
	}
}

func TestDisabledStartProfiler(t *testing.T) {
	profiler := newStartProfiler(nil)
	if profiler != nil {
		t.Fatal("nil output should disable profiling")
	}
	profiler.mark("ignored")
	profiler.shell("ignored", "")
	profiler.finish()
	if got := shellTimingPreamble(false); got != "timing_mark() { :; }" {
		t.Fatalf("disabled preamble = %q", got)
	}
}

func TestStartupScriptTimingsDoNotChangeReadiness(t *testing.T) {
	cfg := config.Defaults()
	for _, enabled := range []bool{false, true} {
		start := startScript(cfg, "/mnt/c/gvproxy.exe", "/mnt/c/gvforwarder", []string{"example.test"}, enabled)
		if strings.Contains(start, "%!") {
			t.Fatalf("invalid script formatting: %s", start)
		}
		if strings.Contains(start, "/proc/uptime") != enabled {
			t.Fatalf("unexpected timing preamble (enabled=%t)", enabled)
		}
		for _, want := range []string{"wait_for_network 15 1", "for ATTEMPT in 1 2 3; do", "forwarder_started=1", "timing_mark wait_ready"} {
			if !strings.Contains(start, want) {
				t.Fatalf("missing adaptive readiness: %q", want)
			}
		}
		if strings.Contains(start, "sleep 1") {
			t.Fatal("startup still has fixed one-second waits")
		}
		stabilize := stabilizeScript(cfg, []string{"example.test"}, enabled)
		if strings.Contains(stabilize, "%!") || !strings.Contains(stabilize, "wait_for_network 15 6") || strings.Contains(stabilize, "sleep 1") {
			t.Fatal("post-interop validation must use adaptive consecutive readiness")
		}
	}
}

func TestStartWithTimingsReportsErrors(t *testing.T) {
	var output bytes.Buffer
	err := NewManager(config.Defaults()).StartWithTimings(context.Background(), &output)
	if err == nil || !strings.Contains(err.Error(), "distro is required") {
		t.Fatalf("StartWithTimings() error = %v", err)
	}
	if !strings.Contains(output.String(), "start_timing total") {
		t.Fatalf("missing total after failure: %s", output.String())
	}
}
