package network

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

const shellTimingPrefix = "__wsl_tunneling_timing__"

type startProfiler struct {
	output   io.Writer
	started  time.Time
	previous time.Time
}

func newStartProfiler(output io.Writer) *startProfiler {
	if output == nil {
		return nil
	}
	now := time.Now()
	return &startProfiler{output: output, started: now, previous: now}
}

func (profiler *startProfiler) mark(phase string) {
	if profiler == nil {
		return
	}
	now := time.Now()
	profiler.write(phase, now.Sub(profiler.previous))
	profiler.previous = now
}

func (profiler *startProfiler) skipped(phase string) {
	if profiler == nil {
		return
	}
	profiler.mark(phase)
	_, _ = fmt.Fprintf(profiler.output, "start_note %s skipped (assets cached; WSL validates distro on execution)\n", phase)
}

func (profiler *startProfiler) finish() {
	if profiler != nil {
		profiler.write("total", time.Since(profiler.started))
	}
}

func (profiler *startProfiler) write(phase string, duration time.Duration) {
	_, _ = fmt.Fprintf(profiler.output, "start_timing %-42s %10.3f ms\n", phase, float64(duration)/float64(time.Millisecond))
}

func (profiler *startProfiler) shell(parent string, stderr string) {
	if profiler == nil {
		return
	}
	var previous float64
	havePrevious := false
	for _, line := range strings.Split(stderr, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != shellTimingPrefix {
			continue
		}
		seconds, err := strconv.ParseFloat(fields[2], 64)
		if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < previous {
			continue
		}
		if havePrevious {
			profiler.write(parent+"."+fields[1], time.Duration((seconds-previous)*float64(time.Second)))
		}
		previous = seconds
		havePrevious = true
	}
}

func shellTimingPreamble(enabled bool) string {
	if !enabled {
		return "timing_mark() { :; }"
	}
	// /proc/uptime is monotonic (10 ms resolution). Bash read/printf are builtins:
	// no date/awk processes or wall-clock adjustments in the measured path.
	return `timing_mark() {
	local uptime ignored
	read -r uptime ignored < /proc/uptime
	printf '__wsl_tunneling_timing__ %s %s\n' "$1" "$uptime" >&2
}`
}
