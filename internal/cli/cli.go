package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/clooooud/wsl-tunneling/internal/config"
	"github.com/clooooud/wsl-tunneling/internal/daemon"
	"github.com/clooooud/wsl-tunneling/internal/gvisor"
	"github.com/clooooud/wsl-tunneling/internal/network"
	"github.com/clooooud/wsl-tunneling/internal/service"
	"github.com/clooooud/wsl-tunneling/internal/wsl"
)

func Run(args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		usage(stdout)
		return 0
	}
	if len(args) == 0 {
		usage(stdout)
		return 0
	}

	if !isCommand(args[0]) {
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
	command := args[0]
	commandArgs := args[1:]

	cfg, configPath, timings, err := parseConfig(commandArgs, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if timings && command != "start" {
		fmt.Fprintln(stderr, "--timings is only supported with start")
		return 2
	}

	ctx := context.Background()
	switch command {
	case "start":
		if timings {
			return runErr(stderr, network.NewManager(cfg).StartWithTimings(ctx, stdout))
		}
		return runErr(stderr, network.NewManager(cfg).Start(ctx))
	case "stop":
		return runErr(stderr, network.NewManager(cfg).Stop(ctx))
	case "restart":
		manager := network.NewManager(cfg)
		if err := manager.Stop(ctx); err != nil {
			fmt.Fprintf(stderr, "stop warning: %v\n", err)
		}
		return runErr(stderr, manager.Start(ctx))
	case "status":
		return status(ctx, cfg, stdout, stderr)
	case "doctor":
		return doctor(ctx, cfg, configPath, stdout, stderr)
	case "daemon":
		return runDaemon(cfg, stdout, stderr)
	case "logs":
		return logs(cfg, stdout, stderr)
	case "install-service":
		return runErr(stderr, service.Install(ctx, cfg, configPath))
	case "uninstall-service":
		return runErr(stderr, service.Uninstall(ctx, cfg))
	case "init-config":
		return runErr(stderr, config.SaveExample(configPath))
	}
	return 2
}

func isCommand(command string) bool {
	switch command {
	case "start", "stop", "restart", "status", "doctor", "daemon", "logs", "install-service", "uninstall-service", "init-config":
		return true
	default:
		return false
	}
}

func parseConfig(args []string, stderr io.Writer) (config.Config, string, bool, error) {
	flags := flag.NewFlagSet("wsl-tunneling", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", config.DefaultPath(), "path to config.json")
	distro := flags.String("distro", "", "target WSL distro")
	gvisorVersion := flags.String("gvisor-version", "", "gvisor-tap-vsock release tag")
	cacheDir := flags.String("cache-dir", "", "binary cache directory")
	stateDir := flags.String("state-dir", "", "local state directory")
	interfaceName := flags.String("iface", "", "interface name used by gvforwarder")
	gvproxyURL := flags.String("gvproxy-url", "", "explicit gvproxy download URL")
	gvforwarderURL := flags.String("gvforwarder-url", "", "explicit gvforwarder download URL")
	terminateOnStop := flags.Bool("terminate-on-stop", false, "terminate the target WSL distro after stop")
	timings := flags.Bool("timings", false, "print startup phase timings (start only)")

	if err := flags.Parse(args); err != nil {
		return config.Config{}, "", false, err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return config.Config{}, "", false, err
	}
	if *distro != "" {
		cfg.Distro = *distro
	}
	if *gvisorVersion != "" {
		cfg.GvisorVersion = *gvisorVersion
	}
	if *cacheDir != "" {
		cfg.CacheDir = *cacheDir
	}
	if *stateDir != "" {
		cfg.StateDir = *stateDir
	}
	if *interfaceName != "" {
		cfg.InterfaceName = *interfaceName
	}
	if *gvproxyURL != "" {
		cfg.GVProxyURL = *gvproxyURL
	}
	if *gvforwarderURL != "" {
		cfg.GVForwarderURL = *gvforwarderURL
	}
	if *terminateOnStop {
		cfg.TerminateOnStop = true
	}

	return cfg, *configPath, *timings, nil
}

func status(ctx context.Context, cfg config.Config, stdout io.Writer, stderr io.Writer) int {
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	status, err := network.NewManager(cfg).Status(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "distro: %s\n", cfg.Distro)
	fmt.Fprintf(stdout, "distroRunning: %t\n", status.DistroRunning)
	fmt.Fprintf(stdout, "forwarder: %t\n", status.ForwarderUp)
	fmt.Fprintf(stdout, "route: %s\n", strings.TrimSpace(status.Route))
	fmt.Fprintf(stdout, "dns: %s\n", strings.TrimSpace(status.DNS))
	return 0
}

func doctor(ctx context.Context, cfg config.Config, configPath string, stdout io.Writer, stderr io.Writer) int {
	fmt.Fprintf(stdout, "config: %s\n", configPath)
	fmt.Fprintf(stdout, "distro: %s\n", cfg.Distro)
	fmt.Fprintf(stdout, "cacheDir: %s\n", cfg.CacheDir)
	fmt.Fprintf(stdout, "stateDir: %s\n", cfg.StateDir)
	fmt.Fprintf(stdout, "stateDirWsl: %s\n", cfg.StateDirWSL)
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "config invalid: %v\n", err)
		return 2
	}

	client := wsl.NewClient()
	exists, err := client.DistroExists(ctx, cfg.Distro)
	if err != nil {
		fmt.Fprintf(stderr, "wsl check failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "wslDistroExists: %t\n", exists)

	assetStatus := gvisor.Status(cfg)
	assetPaths := gvisor.Paths(cfg)
	fmt.Fprintf(stdout, "gvproxyPath: %s\n", assetPaths.GVProxyPath)
	fmt.Fprintf(stdout, "gvproxyCached: %t\n", assetStatus["gvproxy"])
	fmt.Fprintf(stdout, "gvforwarderPath: %s\n", assetPaths.GVForwarderPath)
	fmt.Fprintf(stdout, "gvforwarderCached: %t\n", assetStatus["gvforwarder"])
	if !assetStatus["gvproxy"] || !assetStatus["gvforwarder"] {
		fmt.Fprintln(stdout, "downloading missing gvisor binaries...")
		if _, err := gvisor.Ensure(ctx, cfg); err != nil {
			fmt.Fprintf(stderr, "gvisor check failed: %v\n", err)
			return 1
		}
	}
	return 0
}

func runDaemon(cfg config.Config, stdout io.Writer, stderr io.Writer) int {
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := config.EnsureDirs(cfg); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	logFile, err := os.OpenFile(filepath.Join(cfg.LogDir, "wsl-tunneling.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(stderr, "open log file: %v\n", err)
		return 1
	}
	defer logFile.Close()
	output := io.MultiWriter(stdout, logFile)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return runErr(stderr, daemon.Run(ctx, cfg, output))
}

func logs(cfg config.Config, stdout io.Writer, stderr io.Writer) int {
	path := filepath.Join(cfg.LogDir, "wsl-tunneling.log")
	content, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(stderr, "read logs: %v\n", err)
		return 1
	}
	_, _ = stdout.Write(content)
	return 0
}

func runErr(stderr io.Writer, err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	fmt.Fprintln(stderr, err)
	return 1
}

func usage(output io.Writer) {
	exe := filepath.Base(os.Args[0])
	message := strings.Join([]string{
		fmt.Sprintf("%s controls a WSL user-mode network tunnel backed by gvisor-tap-vsock.", exe),
		"",
		"Usage:",
		fmt.Sprintf("  %s [command] [options]", exe),
		"",
		"Use wsl-tunneling.exe to open the Windows tray controller.",
		"",
		"Commands:",
		"  init-config        Write an example config.json",
		"  doctor             Check WSL and gvisor prerequisites",
		"  start              Start the tunnel once",
		"  stop               Stop the tunnel and restore WSL networking",
		"  restart            Stop then start",
		"  status             Print current tunnel status",
		"  daemon             Run supervised in the foreground",
		"  logs               Print the daemon log file",
		"  install-service    Install a logon scheduled task for the tray",
		"  uninstall-service  Remove the logon scheduled task",
		"",
		"Common options:",
		fmt.Sprintf("  --config PATH   default: %s", config.DefaultPath()),
		"  --distro NAME",
		"  --gvisor-version TAG",
		"  --cache-dir PATH",
		"  --state-dir PATH",
		"  --iface NAME",
		"  --gvproxy-url URL",
		"  --gvforwarder-url URL",
		"  --timings       print phase timings (start only)",
		"",
	}, "\n")
	_, _ = fmt.Fprint(output, message)
}
