package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/clooooud/wsl-tunneling/internal/config"
	"github.com/clooooud/wsl-tunneling/internal/process"
	"github.com/clooooud/wsl-tunneling/internal/tray"
)

func main() {
	process.HideConsoleWindowIfUnshared()
	configPath, err := parseArgs(os.Args[1:])
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := tray.Run(context.Background(), cfg, configPath); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func parseArgs(args []string) (string, error) {
	flags := flag.NewFlagSet("wsl-tunneling", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", config.DefaultPath(), "path to config.json")
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if flags.NArg() > 0 {
		return "", fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	return *configPath, nil
}
