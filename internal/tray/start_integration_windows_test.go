//go:build windows

package tray

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/clooooud/wsl-tunneling/internal/config"
	"github.com/clooooud/wsl-tunneling/internal/dns"
	"github.com/clooooud/wsl-tunneling/internal/gvisor"
	"github.com/clooooud/wsl-tunneling/internal/network"
	"github.com/clooooud/wsl-tunneling/internal/wsl"
)

// Explicitly destructive opt-in: real stop/start cycles. A separate opt-in is
// required for --shutdown, which stops EVERY WSL distro. Normal tests do neither.
// The shared measurement boundary is identical to notification elapsed; no GUI
// or test-process startup, logging or health verification is inside that time.
func TestTunnelStartIntegration(t *testing.T) {
	if os.Getenv("WSL_TUNNELING_START_INTEGRATION") != "1" {
		t.Skip("set WSL_TUNNELING_START_INTEGRATION=1 to interrupt the real tunnel")
	}
	scenarios, err := startIntegrationScenarios(os.Getenv("WSL_TUNNELING_START_COLD") == "1", os.Getenv("WSL_TUNNELING_START_COLD_ONLY") == "1")
	if err != nil {
		t.Fatal(err)
	}
	cycles := 3
	if value := os.Getenv("WSL_TUNNELING_START_CYCLES"); value != "" {
		var err error
		cycles, err = strconv.Atoi(value)
		if err != nil || cycles < 1 || cycles > 100 {
			t.Fatal("WSL_TUNNELING_START_CYCLES must be between 1 and 100")
		}
	}
	assertSLO := os.Getenv("WSL_TUNNELING_START_ASSERT_SLO") == "1"
	if assertSLO && cycles < 100 {
		t.Fatal("SLO assertion requires 100 samples per scenario; a smoke test cannot establish P95")
	}
	cfg, err := config.Load(os.Getenv("WSL_TUNNELING_START_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	assets := gvisor.Status(cfg)
	if !assets["gvproxy"] || !assets["gvforwarder"] {
		t.Fatal("install assets before measuring; downloads are outside this benchmark")
	}
	deadline, ok := t.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Minute)
	}
	// Honour an explicitly longer campaign timeout, leaving recovery time
	// before the testing package's hard deadline terminates the process.
	reserve := time.Until(deadline) / 4
	if reserve > 90*time.Second {
		reserve = 90 * time.Second
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline.Add(-reserve))
	defer cancel()
	manager := network.NewManager(cfg)
	initial, err := manager.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Restore the user's original running/stopped state, even after a failed
	// sample. This recovery operation is not a benchmark observation.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cleanupCancel()
		if initial.ForwarderUp {
			if err := manager.Start(cleanupCtx); err != nil {
				t.Errorf("restore initially running tunnel: %v", err)
			}
		} else if err := manager.Stop(cleanupCtx); err != nil {
			t.Errorf("restore initially stopped tunnel: %v", err)
		}
		if !initial.DistroRunning {
			if err := wsl.NewClient().Terminate(cleanupCtx, cfg.Distro); err != nil {
				t.Errorf("restore initially stopped distro: %v", err)
			}
		} else if !initial.ForwarderUp && cfg.TerminateOnStop {
			if _, err := wsl.NewClient().Bash(cleanupCtx, cfg.Distro, ":"); err != nil {
				t.Errorf("restore initially running distro: %v", err)
			}
		}
	})
	client := wsl.NewClient()
	for _, scenario := range scenarios {
		var durations []time.Duration
		qualified, failures := 0, 0
		for cycle := 1; cycle <= cycles; cycle++ {
			if err := manager.Stop(ctx); err != nil {
				t.Fatalf("%s/%d stop: %v", scenario, cycle, err)
			}
			stopped, err := manager.Status(ctx)
			if err != nil || stopped.ForwarderUp || strings.Contains(stopped.Route, "via "+cfg.GatewayIP+" ") || stopped.DNS == cfg.GatewayIP {
				t.Fatalf("%s/%d networking not restored: %+v, %v", scenario, cycle, stopped, err)
			}
			if scenario == "cold" {
				if _, err := client.Run(ctx, "--shutdown"); err != nil {
					t.Fatal(err)
				}
				running, err := client.IsRunning(ctx, cfg.Distro)
				if err != nil || running {
					t.Fatalf("cold sample requires stopped WSL: running=%t, err=%v", running, err)
				}
			} else {
				// If terminateOnStop is configured, bring the distro up BEFORE
				// the warm measurement; never do this for a cold sample.
				if _, err := client.Bash(ctx, cfg.Distro, ":"); err != nil {
					t.Fatal(err)
				}
			}
			diagnostic := &startDiagnostic{cfg: cfg, start: time.Now(), scenario: scenario, cycle: cycle}
			elapsed, startErr := measureOperation(ctx, func(ctx context.Context) error {
				return manager.StartWithTimings(ctx, &diagnostic.phases)
			})
			notification := operationNotification("start", "started", elapsed, startErr)
			if !notification.timed || notification.elapsed != elapsed || notification.isError != (startErr != nil) {
				t.Fatal("notification duration/result diverged from operation")
			}
			if err := diagnostic.write(elapsed, startErr); err != nil {
				t.Logf("diagnostic write failed (operation result unchanged): %v", err)
			}
			durations = append(durations, elapsed)
			if startErr == nil && elapsed < 5*time.Second {
				qualified++
			}
			t.Logf("sample scenario=%s cycle=%d elapsed_ms=%.3f success=%t under_5s=%t\n%s",
				scenario, cycle, float64(elapsed)/float64(time.Millisecond), startErr == nil,
				startErr == nil && elapsed < 5*time.Second, diagnostic.phases.String())
			if startErr != nil {
				failures++
				t.Errorf("%s/%d start: %v", scenario, cycle, startErr)
				continue // Keep failed observations; do not retry them into successes.
			}
			requireTunnelLocalReadiness(t, ctx, cfg, client)
			status, err := manager.Status(ctx)
			if err != nil || !status.ForwarderUp || status.DNS != cfg.GatewayIP {
				t.Fatalf("post-start status = %+v, %v", status, err)
			}
			if os.Getenv("WSL_TUNNELING_START_HTTPS") == "1" {
				if _, err := client.Bash(ctx, cfg.Distro, "curl --connect-timeout 5 --max-time 10 --fail --silent --show-error --head https://example.com >/dev/null"); err != nil {
					t.Errorf("%s/%d external HTTPS check (outside elapsed): %v", scenario, cycle, err)
				}
			}
			// A later independent WSL session must still see a healthy tunnel,
			// after status/HTTPS and after all earlier interop sessions exited.
			requireTunnelLocalReadiness(t, ctx, cfg, client)
		}
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		p50 := durations[(len(durations)*50+99)/100-1]
		p95 := durations[(len(durations)*95+99)/100-1]
		t.Logf("summary scenario=%s samples=%d qualified=%d failures=%d p50_ms=%.3f p95_ms=%.3f max_ms=%.3f (nearest-rank; includes failed durations)",
			scenario, cycles, qualified, failures, float64(p50)/float64(time.Millisecond), float64(p95)/float64(time.Millisecond), float64(durations[len(durations)-1])/float64(time.Millisecond))
		if assertSLO && qualified*100 < 95*cycles {
			t.Errorf("%s SLO not met: %d/%d successful starts below 5 seconds", scenario, qualified, cycles)
		}
	}
}

func startIntegrationScenarios(allowCold, coldOnly bool) ([]string, error) {
	if coldOnly {
		if !allowCold {
			return nil, fmt.Errorf("WSL_TUNNELING_START_COLD_ONLY requires WSL_TUNNELING_START_COLD=1 to authorize global WSL shutdown")
		}
		return []string{"cold"}, nil
	}
	if allowCold {
		return []string{"warm", "cold"}, nil
	}
	return []string{"warm"}, nil
}

func TestStartIntegrationScenarios(t *testing.T) {
	for _, tt := range []struct {
		allowCold, coldOnly bool
		want               string
		wantError          bool
	}{
		{false, false, "warm", false},
		{true, false, "warm,cold", false},
		{true, true, "cold", false},
		{false, true, "", true},
	} {
		scenarios, err := startIntegrationScenarios(tt.allowCold, tt.coldOnly)
		if (err != nil) != tt.wantError || strings.Join(scenarios, ",") != tt.want {
			t.Fatalf("scenarios(%t,%t) = %v, %v", tt.allowCold, tt.coldOnly, scenarios, err)
		}
	}
}

func requireTunnelLocalReadiness(t *testing.T, ctx context.Context, cfg config.Config, client wsl.Client) {
	t.Helper()
	suffixes := dns.NormalizeSearchSuffixes(cfg.DNSSearchSuffixes)
	if len(suffixes) == 0 {
		var err error
		suffixes, err = dns.SearchSuffixes(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	resolv := "nameserver " + cfg.GatewayIP
	if len(suffixes) != 0 {
		resolv += "\nsearch " + strings.Join(suffixes, " ")
	}
	script := fmt.Sprintf(`set -eu
IFACE=%s
GATEWAY=%s
DEVICE=%s
EXPECTED_DNS=%s
link=$(ip -o link show up dev "$IFACE")
case "$link" in *" mtu 1500 "*"link/ether 5a:94:ef:e4:0c:ee "*) ;; *) exit 1 ;; esac
ip -4 -o addr show dev "$IFACE" | awk -v address="$DEVICE/24" '$4 == address { found=1 } END { exit !found }'
ip -4 route show default | awk -v gateway="$GATEWAY" -v iface="$IFACE" 'NR == 1 { good=($1 == "default" && $2 == "via" && $3 == gateway && $4 == "dev" && $5 == iface) } END { exit !good }'
[ "$(cat /etc/resolv.conf)" = "$EXPECTED_DNS" ]
timeout -s KILL 1 bash -c 'exec 3<>"/dev/tcp/$1/53"' -- "$GATEWAY"
`, quote(cfg.InterfaceName), quote(cfg.GatewayIP), quote(cfg.DeviceIP), quote(resolv))
	if _, err := client.Bash(ctx, cfg.Distro, script); err != nil {
		t.Fatalf("independent local readiness after handoff: %v", err)
	}
}