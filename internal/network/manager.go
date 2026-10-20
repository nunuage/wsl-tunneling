package network

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/clooooud/wsl-tunneling/internal/config"
	"github.com/clooooud/wsl-tunneling/internal/dns"
	"github.com/clooooud/wsl-tunneling/internal/gvisor"
	"github.com/clooooud/wsl-tunneling/internal/state"
	"github.com/clooooud/wsl-tunneling/internal/wsl"
)

type Manager struct {
	Config config.Config
	WSL    wslClient
}

type wslClient interface {
	DistroExists(context.Context, string) (bool, error)
	IsRunning(context.Context, string) (bool, error)
	Bash(context.Context, string, string) (wsl.Result, error)
	Terminate(context.Context, string) error
}

type Status struct {
	DistroRunning bool
	ForwarderUp   bool
	Route         string
	DNS           string
	Raw           string
}

func NewManager(cfg config.Config) Manager {
	return Manager{Config: cfg, WSL: wsl.NewClient()}
}

func (manager Manager) Start(ctx context.Context) error {
	return manager.start(ctx, nil)
}

// StartWithTimings starts the tunnel normally and writes diagnostic phase timings.
// Shell subphases are included in their enclosing WSL phase, not additional time.
func (manager Manager) StartWithTimings(ctx context.Context, output io.Writer) error {
	return manager.start(ctx, newStartProfiler(output))
}

func (manager Manager) start(ctx context.Context, profiler *startProfiler) error {
	defer profiler.finish()
	if err := manager.Config.Validate(); err != nil {
		return err
	}
	if err := config.EnsureDirs(manager.Config); err != nil {
		return err
	}
	lock, err := state.AcquireLock(manager.Config.StateDir)
	if err != nil {
		return err
	}
	defer lock.Release()
	profiler.mark("prepare_local")

	// Cached starts validate the distro in the first real WSL command. Keep
	// the early check for installation, so a typo cannot trigger downloads.
	cached := gvisor.Status(manager.Config)
	if cached["gvproxy"] && cached["gvforwarder"] {
		profiler.skipped("check_distro")
	} else {
		exists, err := manager.WSL.DistroExists(ctx, manager.Config.Distro)
		profiler.mark("check_distro")
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("WSL distro %q does not exist", manager.Config.Distro)
		}
	}

	assets, err := gvisor.Ensure(ctx, manager.Config)
	profiler.mark("ensure_assets")
	if err != nil {
		return err
	}
	gvproxyPath, err := wsl.WindowsPathToWSL(assets.GVProxyPath)
	if err != nil {
		return err
	}
	gvforwarderPath, err := wsl.WindowsPathToWSL(assets.GVForwarderPath)
	if err != nil {
		return err
	}
	profiler.mark("convert_paths")
	dnsSearchSuffixes := dns.NormalizeSearchSuffixes(manager.Config.DNSSearchSuffixes)
	if len(dnsSearchSuffixes) == 0 {
		if suffixes, err := dns.SearchSuffixes(ctx); err == nil {
			dnsSearchSuffixes = suffixes
		}
	}
	profiler.mark("dns_suffixes")

	return manager.startTunnel(ctx, gvproxyPath, gvforwarderPath, dnsSearchSuffixes, profiler)
}

func (manager Manager) startTunnel(ctx context.Context, gvproxyPath, gvforwarderPath string, dnsSearchSuffixes []string, profiler *startProfiler) error {
	result, err := manager.WSL.Bash(ctx, manager.Config.Distro, startScript(manager.Config, gvproxyPath, gvforwarderPath, dnsSearchSuffixes, profiler != nil))
	profiler.mark("wsl_start")
	profiler.shell("wsl_start", result.Stderr)
	if err != nil {
		// Diagnose only a failed first session, never a healthy start or an
		// interop handoff failure. Preserve the original error and errors.Is.
		if ctx.Err() == nil {
			exists, checkErr := manager.WSL.DistroExists(ctx, manager.Config.Distro)
			profiler.mark("diagnose_distro")
			if checkErr == nil && !exists {
				return fmt.Errorf("WSL distro %q does not exist: %w", manager.Config.Distro, err)
			}
		}
		return err
	}
	// Closing the launching wsl.exe session can reconnect Windows interop and
	// recreate the TAP interface. Validate after that handoff, not just before.
	for _, line := range strings.Split(result.Stdout, "\n") {
		if strings.TrimSpace(line) != "forwarder_started=1" {
			continue
		}
		result, err = manager.WSL.Bash(ctx, manager.Config.Distro, stabilizeScript(manager.Config, dnsSearchSuffixes, profiler != nil))
		profiler.mark("wsl_stabilize")
		profiler.shell("wsl_stabilize", result.Stderr)
		if err != nil {
			return err
		}
		break
	}
	return nil
}

func (manager Manager) Stop(ctx context.Context) error {
	if err := manager.Config.Validate(); err != nil {
		return err
	}
	lock, err := state.AcquireLock(manager.Config.StateDir)
	if err != nil {
		return err
	}
	defer lock.Release()

	_, stopErr := manager.WSL.Bash(ctx, manager.Config.Distro, stopScript(manager.Config))
	if manager.Config.TerminateOnStop {
		if err := manager.WSL.Terminate(ctx, manager.Config.Distro); err != nil && stopErr == nil {
			stopErr = err
		}
	}
	return stopErr
}

func (manager Manager) Status(ctx context.Context) (Status, error) {
	running, err := manager.WSL.IsRunning(ctx, manager.Config.Distro)
	if err != nil {
		return Status{}, err
	}
	if !running {
		return Status{DistroRunning: false}, nil
	}

	result, err := manager.WSL.Bash(ctx, manager.Config.Distro, statusScript(manager.Config))
	if err != nil {
		return Status{DistroRunning: true, Raw: result.Stdout + result.Stderr}, err
	}

	status := Status{DistroRunning: true, Raw: result.Stdout}
	for _, line := range strings.Split(result.Stdout, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "forwarder":
			status.ForwarderUp = value == "running"
		case "route":
			status.Route = value
		case "dns":
			status.DNS = value
		}
	}
	return status, nil
}

func startScript(cfg config.Config, gvproxyPath string, gvforwarderPath string, dnsSearchSuffixes []string, timings bool) string {
	return fmt.Sprintf(`
set -eu
%s
timing_mark begin
STATE=%s
IFACE=%s
GATEWAY=%s
DEVICE=%s
GVPROXY=%s
GVFORWARDER=%s
DNS_SEARCH=%s
%s
check_readiness_tools || exit 4
mkdir -p "$STATE"
FORWARDER_RUNNING=0
if forwarder_running; then
	FORWARDER_RUNNING=1
fi
if [ "$FORWARDER_RUNNING" != "1" ]; then
	pkill -f "$STATE/gvforwarder" >/dev/null 2>&1 || true
	ip link del "$IFACE" >/dev/null 2>&1 || true
	rm -f "$STATE/vm.pid"
fi
timing_mark prepare_state
if [ -f /mnt/wsl/resolv.conf ] && [ ! -e "$STATE/resolv.mnt.orig" ] && ! grep -q "^nameserver $GATEWAY$" /mnt/wsl/resolv.conf 2>/dev/null; then
	cp -f /mnt/wsl/resolv.conf "$STATE/resolv.mnt.orig" 2>/dev/null || true
fi
if [ -e /etc/resolv.conf ] && [ ! -e "$STATE/resolv.etc.orig" ] && ! grep -q "^nameserver $GATEWAY$" /etc/resolv.conf 2>/dev/null; then
  cp -a /etc/resolv.conf "$STATE/resolv.etc.orig" 2>/dev/null || true
fi
if [ -e /etc/wsl.conf ] && [ ! -e "$STATE/wsl.conf.orig" ]; then
	awk '
		/# wsl-tunneling begin/ { skip = 1; next }
		/# wsl-tunneling end/ { skip = 0; next }
		!skip { print }
	' /etc/wsl.conf > "$STATE/wsl.conf.orig" 2>/dev/null || cp -a /etc/wsl.conf "$STATE/wsl.conf.orig" 2>/dev/null || true
fi
ROUTE=$(ip route show default | head -n1 || true)
if [ "$FORWARDER_RUNNING" = "1" ] && [ -f "$STATE/route.dat" ]; then
	# An in-place repair must not replace the original route backup with drift.
	ROUTE=$(cat "$STATE/route.dat")
elif echo "$ROUTE" | grep -q "$GATEWAY" && [ "$FORWARDER_RUNNING" = "1" ]; then
	ROUTE=""
else
	printf '%%s\n' "$ROUTE" > "$STATE/route.dat"
fi
if echo "$ROUTE" | grep -q "$GATEWAY"; then
	if [ "$FORWARDER_RUNNING" != "1" ]; then
		echo "another user-mode network already appears to own the default route" >&2
		exit 2
	fi
fi
if [ -n "$ROUTE" ] && ! echo "$ROUTE" | grep -q '^default '; then
	echo "route state is not a default route: $ROUTE" >&2
	exit 3
fi
timing_mark backup_network
if %s; then
	if ! grep -q '# wsl-tunneling begin' /etc/wsl.conf 2>/dev/null; then
    {
      echo ''
	echo '# wsl-tunneling begin'
      echo '[network]'
      echo 'generateResolvConf = false'
	echo '# wsl-tunneling end'
    } >> /etc/wsl.conf
  fi
fi
%s
write_resolv_conf
if ! ip link show "$IFACE" >/dev/null 2>&1; then
  ip tuntap add dev "$IFACE" mode tap
fi
timing_mark configure_dns_interface
if [ "$FORWARDER_RUNNING" != "1" ]; then
	cp -f "$GVFORWARDER" "$STATE/gvforwarder"
	chmod +x "$STATE/gvforwarder"
	timing_mark copy_forwarder
	STARTED=0
	: > "$STATE/gvforwarder.log"
	: > "$STATE/gvforwarder.err"
	for ATTEMPT in 1 2 3; do
		echo "starting gvforwarder attempt $ATTEMPT" >> "$STATE/gvforwarder.err"
		nohup "$STATE/gvforwarder" -preexisting -iface "$IFACE" -stop-if-exist ignore -url "stdio:$GVPROXY?listen-stdio=accept&ssh-port=-1" >> "$STATE/gvforwarder.log" 2>> "$STATE/gvforwarder.err" < /dev/null &
		echo $! > "$STATE/vm.pid"
		timing_mark launch_forwarder
		# Confirm the connection, then let the launching WSL session exit.
		# The manager validates stability again after the interop handoff.
		if wait_for_network 15 1; then
			STARTED=1
			break
		fi
		pkill -f "$STATE/gvforwarder" >/dev/null 2>&1 || true
		rm -f "$STATE/vm.pid"
	done
	if [ "$STARTED" != "1" ]; then
		echo "tunnel did not become ready after 3 attempts (15s readiness timeout per attempt)" >&2
		cat "$STATE/gvforwarder.err" >&2 2>/dev/null || true
		exit 42
	fi
	echo forwarder_started=1
else
	timing_mark launch_forwarder
	# Reuse a healthy tunnel immediately; repair route/address/DNS drift in place.
	if ! wait_for_network 15 1; then
		echo "running tunnel did not become ready within 15s; stop/start to retry" >&2
		exit 42
	fi
fi
timing_mark wait_ready
`, shellTimingPreamble(timings), shellQuote(cfg.StateDirWSL), shellQuote(cfg.InterfaceName), shellQuote(cfg.GatewayIP), shellQuote(cfg.DeviceIP), shellQuote(gvproxyPath), shellQuote(gvforwarderPath), shellQuote(strings.Join(dnsSearchSuffixes, " ")), networkReadinessScript(), shellBool(cfg.DisableAutoResolv), networkConfigurationScript())
}

func stopScript(cfg config.Config) string {
	return fmt.Sprintf(`
set +e
STATE=%s
IFACE=%s
TUNNEL_GATEWAY=%s
eth0_gateway() {
	CIDR=$(ip -4 -o addr show dev eth0 scope global | awk '{print $4; exit}')
	if [ -z "$CIDR" ]; then
		return 1
	fi
	IP=${CIDR%%/*}
	PREFIX=${CIDR##*/}
	IFS=. read -r A B C D <<EOF
$IP
EOF
	IPNUM=$(( (A << 24) + (B << 16) + (C << 8) + D ))
	MASK=$(( (0xffffffff << (32 - PREFIX)) & 0xffffffff ))
	NETWORK=$(( IPNUM & MASK ))
	GW=$(( NETWORK + 1 ))
	GWA=$(( (GW >> 24) & 255 ))
	GWB=$(( (GW >> 16) & 255 ))
	GWC=$(( (GW >> 8) & 255 ))
	GWD=$(( GW & 255 ))
	echo "$GWA.$GWB.$GWC.$GWD"
}
restore_eth0_default() {
	GW=$(eth0_gateway) || return 1
	ip route replace default via "$GW" dev eth0 >/dev/null 2>&1
}
restore_resolv_conf() {
	RESTORED_MNT=0
	if [ -f "$STATE/resolv.mnt.orig" ] && ! grep -q "^nameserver $TUNNEL_GATEWAY$" "$STATE/resolv.mnt.orig" 2>/dev/null; then
		cp -f "$STATE/resolv.mnt.orig" /mnt/wsl/resolv.conf 2>/dev/null && RESTORED_MNT=1
	fi
	if [ "$RESTORED_MNT" != "1" ]; then
		GW=$(eth0_gateway)
		if [ -n "$GW" ]; then
			printf 'nameserver %%s\n' "$GW" > /mnt/wsl/resolv.conf 2>/dev/null || true
		fi
	fi

	if [ -e "$STATE/resolv.etc.orig" ] && ! grep -q "^nameserver $TUNNEL_GATEWAY$" "$STATE/resolv.etc.orig" 2>/dev/null; then
		rm -f /etc/resolv.conf
		cp -a "$STATE/resolv.etc.orig" /etc/resolv.conf 2>/dev/null || true
	else
		rm -f /etc/resolv.conf
		ln -s /mnt/wsl/resolv.conf /etc/resolv.conf 2>/dev/null || cp -f /mnt/wsl/resolv.conf /etc/resolv.conf 2>/dev/null || true
	fi
}
restore_wsl_conf() {
	SOURCE=/etc/wsl.conf
	if [ -e "$STATE/wsl.conf.orig" ]; then
		SOURCE="$STATE/wsl.conf.orig"
	fi
	if [ -e "$SOURCE" ]; then
		awk '
			/# wsl-tunneling begin/ { skip = 1; next }
			/# wsl-tunneling end/ { skip = 0; next }
			!skip { print }
		' "$SOURCE" > /tmp/wsl-tunneling.wsl.conf 2>/dev/null && cp -f /tmp/wsl-tunneling.wsl.conf /etc/wsl.conf 2>/dev/null
		rm -f /tmp/wsl-tunneling.wsl.conf
	fi
}
if [ -f "$STATE/vm.pid" ]; then
  GPID=$(cat "$STATE/vm.pid")
  kill "$GPID" >/dev/null 2>&1
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    kill -0 "$GPID" >/dev/null 2>&1 || break
    sleep 1
  done
  kill -9 "$GPID" >/dev/null 2>&1 || true
fi
pkill -f "$STATE/gvforwarder" >/dev/null 2>&1 || true
restore_resolv_conf
restore_wsl_conf
if [ -f "$STATE/route.dat" ]; then
  ip route del default >/dev/null 2>&1 || true
  ROUTE=$(cat "$STATE/route.dat")
  if echo "$ROUTE" | grep -q '^default '; then
    ip route add $ROUTE >/dev/null 2>&1 || true
	else
		restore_eth0_default || true
  fi
else
	restore_eth0_default || true
fi
ip link del "$IFACE" >/dev/null 2>&1 || true
rm -rf "$STATE"
`, shellQuote(cfg.StateDirWSL), shellQuote(cfg.InterfaceName), shellQuote(cfg.GatewayIP))
}

func stabilizeScript(cfg config.Config, dnsSearchSuffixes []string, timings bool) string {
	return fmt.Sprintf(`
set -eu
%s
timing_mark begin
STATE=%s
IFACE=%s
GATEWAY=%s
DEVICE=%s
DNS_SEARCH=%s
%s
%s
if ! wait_for_network 15 6; then
	echo "tunnel did not stabilize after WSL interop handoff within 15s" >&2
	cat "$STATE/gvforwarder.err" >&2 2>/dev/null || true
	exit 42
fi
timing_mark wait_ready
`, shellTimingPreamble(timings), shellQuote(cfg.StateDirWSL), shellQuote(cfg.InterfaceName), shellQuote(cfg.GatewayIP), shellQuote(cfg.DeviceIP), shellQuote(strings.Join(dnsSearchSuffixes, " ")), networkReadinessScript(), networkConfigurationScript())
}

func statusScript(cfg config.Config) string {
	return fmt.Sprintf(`
STATE=%s
if [ -f "$STATE/vm.pid" ] && kill -0 "$(cat "$STATE/vm.pid")" >/dev/null 2>&1; then
  echo forwarder=running
else
  echo forwarder=stopped
fi
echo route=$(ip route show default | head -n1)
if [ -f /etc/resolv.conf ]; then
  echo dns=$(awk '/^nameserver / {print $2; exit}' /etc/resolv.conf)
else
  echo dns=
fi
`, shellQuote(cfg.StateDirWSL))
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func shellBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
