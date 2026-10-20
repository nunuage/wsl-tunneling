package network

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/clooooud/wsl-tunneling/internal/config"
)

func readinessBash(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Bash behavior tests run on Linux/WSL; no real network operations")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is unavailable")
	}
	return bash
}

func runReadinessBash(t *testing.T, script string) string {
	t.Helper()
	bash := readinessBash(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, bash, "-c", "set -eu\n"+networkReadinessScript()+"\n"+script).CombinedOutput()
	if err != nil {
		t.Fatalf("readiness shell failed: %v\n%s", err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestWaitForNetwork(t *testing.T) {
	// All OS/network functions and sleep are mocked. Advancing SECONDS makes
	// timeout/retry paths deterministic without waiting or changing networking.
	const mocks = `
probes=0
repairs=0
writes=0
sleeps=0
checks=0
forwarder_alive() { return 0; }
forwarder_running() { return 0; }
network_configured() { return 0; }
gateway_ready() { probes=$((probes + 1)); return 0; }
configure_network() { repairs=$((repairs + 1)); }
write_resolv_conf() { writes=$((writes + 1)); }
sleep() {
	[ "$1" = "0.2" ]
	sleeps=$((sleeps + 1))
	SECONDS=$((SECONDS + 1))
}
`
	tests := []struct {
		name     string
		override string
		timeout  int
		required int
		want     string // result, probes, repairs, writes, sleeps
	}{
		{"healthy existing", "", 10, 1, "0 1 0 0 0"},
		{"new helper stability", "", 10, 6, "0 6 0 0 5"},
		{"route or DNS regresses", `network_configured() { checks=$((checks + 1)); [ "$checks" -ne 3 ]; }`, 10, 3, "0 5 1 1 5"},
		{"proxy regresses", `gateway_ready() { probes=$((probes + 1)); [ "$probes" -ne 2 ]; }`, 10, 3, "0 5 0 0 4"},
		{"proxy unreachable", `gateway_ready() { probes=$((probes + 1)); return 1; }`, 3, 3, "1 3 0 0 3"},
		{"configuration never converges", `network_configured() { return 1; }`, 3, 3, "1 0 3 3 3"},
		{"process exits", `forwarder_alive() { return 1; }`, 10, 3, "2 0 0 0 0"},
		{"child has not execed yet", `forwarder_running() { checks=$((checks + 1)); [ "$checks" -gt 2 ]; }`, 10, 3, "0 3 0 0 4"},
		{"probe finishes after deadline", `gateway_ready() { probes=$((probes + 1)); SECONDS=$((SECONDS + 10)); return 0; }`, 3, 1, "1 1 0 0 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := mocks + "\n" + tt.override + fmt.Sprintf(`
result=0
wait_for_network %d %d || result=$?
printf '%%s %%s %%s %%s %%s\n' "$result" "$probes" "$repairs" "$writes" "$sleeps"
`, tt.timeout, tt.required)
			if got := runReadinessBash(t, script); got != tt.want {
				t.Fatalf("wait_for_network result/probes/repairs/writes/sleeps = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNetworkConfigured(t *testing.T) {
	readinessBash(t)
	tests := []struct {
		name     string
		override string
		want     string
	}{
		{"healthy", "", "0"},
		{"interface down", "LINK=''", "1"},
		{"wrong MAC", "LINK='1: test0: <UP> mtu 1500 link/ether 00:00:00:00:00:00 brd ff:ff:ff:ff:ff:ff'", "1"},
		{"wrong MTU", "LINK='1: test0: <UP> mtu 1400 link/ether 5a:94:ef:e4:0c:ee brd ff:ff:ff:ff:ff:ff'", "1"},
		{"wrong address", "ADDR='1: test0 inet 192.168.127.3/24 scope global test0'", "1"},
		{"wrong prefix", "ADDR='1: test0 inet 192.168.127.2/16 scope global test0'", "1"},
		{"wrong gateway", "ROUTE='default via 192.168.127.10 dev test0'", "1"},
		{"wrong interface", "ROUTE='default via 192.168.127.1 dev test00'", "1"},
		{"no default route", "ROUTE=''", "1"},
		{"another route preferred", "ROUTE=$'default via 172.16.0.1 dev eth0\ndefault via 192.168.127.1 dev test0'", "1"},
		{"wrong DNS", "printf 'nameserver 172.16.0.1\\n' > \"$RESOLV_FILE\"", "1"},
		{"changed search suffix", "RESOLV_CONTENT=$'nameserver 192.168.127.1\nsearch new.example.test'", "1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolv := filepath.Join(t.TempDir(), "resolv.conf")
			if err := os.WriteFile(resolv, []byte("nameserver 192.168.127.1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// Redirect the one read-only system-file access into the test directory.
			library := strings.ReplaceAll(networkReadinessScript(), "/etc/resolv.conf", shellQuote(resolv))
			script := library + `
IFACE=test0
DEVICE=192.168.127.2
GATEWAY=192.168.127.1
RESOLV_CONTENT='nameserver 192.168.127.1'
LINK='1: test0: <UP> mtu 1500 link/ether 5a:94:ef:e4:0c:ee brd ff:ff:ff:ff:ff:ff'
ADDR='1: test0 inet 192.168.127.2/24 scope global test0'
ROUTE='default via 192.168.127.1 dev test0'
ip() {
	case "$*" in
		'-o link show up dev test0') printf '%s\n' "$LINK" ;;
		'-4 -o addr show dev test0') printf '%s\n' "$ADDR" ;;
		'-4 route show default') printf '%s\n' "$ROUTE" ;;
		*) return 1 ;;
	esac
}
` + "\nRESOLV_FILE=" + shellQuote(resolv) + "\n" + tt.override + `
result=0
network_configured || result=$?
printf '%s\n' "$result"
`
			if got := runReadinessBash(t, script); got != tt.want {
				t.Fatalf("network_configured = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGatewayProbeIsLocalAndBounded(t *testing.T) {
	got := runReadinessBash(t, `
GATEWAY=192.168.127.1
called=0
timeout() {
	[ "$#" -eq 8 ] && [ "$1" = '-s' ] && [ "$2" = KILL ] && [ "$3" = 1 ] &&
	[ "$4" = bash ] && [ "$5" = '-c' ] && [ "$6" = 'exec 3<>"/dev/tcp/$1/53"' ] &&
	[ "$7" = '--' ] && [ "$8" = "$GATEWAY" ] || return 1
	called=1
}
gateway_ready
printf '%s\n' "$called"
`)
	if got != "1" {
		t.Fatalf("bounded local probe was not used: %q", got)
	}
}

func TestReadinessToolsRejectIncompatibleTimeout(t *testing.T) {
	for _, exit := range []int{0, 2, 127} {
		t.Run(strconv.Itoa(exit), func(t *testing.T) {
			script := fmt.Sprintf(`
timeout() { return %d; }
if check_readiness_tools 2>/dev/null; then echo supported; else echo rejected; fi
`, exit)
			want := "rejected"
			if exit == 0 {
				want = "supported"
			}
			if got := runReadinessBash(t, script); got != want {
				t.Fatalf("timeout exit %d: got %q, want %q", exit, got, want)
			}
		})
	}
}

func TestStartScriptPreservesRouteBackupOnRepair(t *testing.T) {
	readinessBash(t)
	cfg := config.Defaults()
	script := startScript(cfg, "/proxy", "/forwarder", nil, false)
	begin := strings.Index(script, "ROUTE=$(ip route show default")
	end := strings.Index(script, "timing_mark backup_network")
	if begin < 0 || end <= begin {
		t.Fatal("route backup section not found")
	}
	for _, running := range []bool{false, true} {
		t.Run(strconv.FormatBool(running), func(t *testing.T) {
			stateDir := t.TempDir()
			original := "default via 172.24.96.1 dev eth0 proto kernel\n"
			backup := filepath.Join(stateDir, "route.dat")
			if err := os.WriteFile(backup, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			runningValue := "0"
			if running {
				runningValue = "1"
			}
			runReadinessBash(t, "STATE="+shellQuote(stateDir)+"\nFORWARDER_RUNNING="+runningValue+`
GATEWAY=192.168.127.1
ip() { echo 'default via 172.24.96.254 dev eth0'; }
`+script[begin:end])
			got, err := os.ReadFile(backup)
			if err != nil {
				t.Fatal(err)
			}
			want := "default via 172.24.96.254 dev eth0\n"
			if running {
				want = original
			}
			if string(got) != want {
				t.Fatalf("route backup = %q, want %q", got, want)
			}
		})
	}
}

func TestForwarderIdentity(t *testing.T) {
	readinessBash(t)
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep is unavailable")
	}
	stateDir := filepath.Join(t.TempDir(), "state with spaces")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(sleepPath)
	if err != nil {
		t.Fatal(err)
	}
	helperPath := filepath.Join(stateDir, "gvforwarder")
	if err := os.WriteFile(helperPath, image, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		path string
		want string
	}{{"matching executable", helperPath, "0"}, {"unrelated live PID", sleepPath, "1"}} {
		t.Run(tt.name, func(t *testing.T) {
			child := exec.Command(tt.path, "30")
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
			pidFile := filepath.Join(stateDir, "vm.pid")
			if err := os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			script := "STATE=" + shellQuote(stateDir) + `
result=0
forwarder_running || result=$?
printf '%s\n' "$result"
`
			if got := runReadinessBash(t, script); got != tt.want {
				t.Fatalf("forwarder_running = %q, want %q", got, tt.want)
			}
		})
	}
	for _, pid := range []string{"0", "1", "-1", "not-a-pid"} {
		if err := os.WriteFile(filepath.Join(stateDir, "vm.pid"), []byte(pid+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := runReadinessBash(t, "STATE="+shellQuote(stateDir)+"\nif forwarder_alive; then echo invalid; else echo rejected; fi"); got != "rejected" {
			t.Fatalf("unsafe PID %q was not rejected: %s", pid, got)
		}
	}
}

func TestStartScriptBashSyntax(t *testing.T) {
	bash := readinessBash(t)
	cfg := config.Defaults()
	cfg.StateDirWSL = "/tmp/state with 'quotes'"
	for _, script := range []string{
		startScript(cfg, "/mnt/c/a b/proxy.exe", "/mnt/c/a b/forwarder", []string{"example.test"}, true),
		stabilizeScript(cfg, []string{"example.test"}, true),
	} {
		command := exec.Command(bash, "-n")
		command.Stdin = strings.NewReader(script)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("invalid generated bash: %v\n%s", err, output)
		}
	}
}
