package wsl

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"unicode/utf16"

	"github.com/clooooud/wsl-tunneling/internal/process"
)

type Client struct {
	Exe string
}

type Result struct {
	Stdout string
	Stderr string
}

func NewClient() Client {
	return Client{Exe: "wsl.exe"}
}

func (client Client) Run(ctx context.Context, args ...string) (Result, error) {
	command := exec.CommandContext(ctx, client.Exe, args...)
	process.HideWindow(command)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		return result, fmt.Errorf("wsl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(result.Stderr))
	}
	return result, nil
}

func (client Client) Exec(ctx context.Context, distro string, commandArgs ...string) (Result, error) {
	args := []string{"--distribution", distro, "--user", "root", "--exec"}
	args = append(args, commandArgs...)
	return client.Run(ctx, args...)
}

func (client Client) Bash(ctx context.Context, distro string, script string) (Result, error) {
	return client.Exec(ctx, distro, bashArgs(script)...)
}

// Internal scripts must not run login profiles (which can be slow, interactive,
// or alter stdout). Unset BASH_ENV before starting Bash, not inside the script.
// Keep WSL_INTEROP and the rest of WSL's environment for Windows executables.
func bashArgs(script string) []string {
	return []string{
		"/usr/bin/env", "-u", "BASH_ENV",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"/bin/bash", "--noprofile", "--norc", "-c", script,
	}
}

func (client Client) ListDistros(ctx context.Context, runningOnly bool) ([]string, error) {
	args := []string{"--list", "--quiet"}
	if runningOnly {
		args = append(args, "--running")
	}
	result, err := client.Run(ctx, args...)
	if err != nil {
		return nil, err
	}

	var distros []string
	for _, line := range strings.Split(decodeWSLOutput(result.Stdout), "\n") {
		name := strings.TrimSpace(strings.Trim(line, "\x00"))
		if name != "" {
			distros = append(distros, name)
		}
	}
	return distros, nil
}

func (client Client) DistroExists(ctx context.Context, distro string) (bool, error) {
	distros, err := client.ListDistros(ctx, false)
	if err != nil {
		return false, err
	}
	for _, candidate := range distros {
		if strings.EqualFold(candidate, distro) {
			return true, nil
		}
	}
	return false, nil
}

func (client Client) IsRunning(ctx context.Context, distro string) (bool, error) {
	distros, err := client.ListDistros(ctx, true)
	if err != nil {
		return false, err
	}
	for _, candidate := range distros {
		if strings.EqualFold(candidate, distro) {
			return true, nil
		}
	}
	return false, nil
}

func (client Client) Terminate(ctx context.Context, distro string) error {
	_, err := client.Run(ctx, "--terminate", distro)
	return err
}

func WindowsPathToWSL(path string) (string, error) {
	normalized := strings.ReplaceAll(path, "\\", "/")
	if len(normalized) >= 3 && normalized[1] == ':' && normalized[2] == '/' {
		drive := strings.ToLower(normalized[:1])
		return "/mnt/" + drive + normalized[2:], nil
	}

	uncPattern := regexp.MustCompile(`^//([^/]+)/(.+)$`)
	if match := uncPattern.FindStringSubmatch(normalized); match != nil {
		return "//" + match[1] + "/" + match[2], nil
	}

	if strings.HasPrefix(normalized, "/") {
		return normalized, nil
	}

	return "", fmt.Errorf("cannot convert %q to a WSL path; use an absolute path", path)
}

func decodeWSLOutput(output string) string {
	bytes := []byte(output)
	if len(bytes) < 2 || bytes[1] != 0 {
		return output
	}

	units := make([]uint16, 0, len(bytes)/2)
	for index := 0; index+1 < len(bytes); index += 2 {
		units = append(units, uint16(bytes[index])|uint16(bytes[index+1])<<8)
	}
	return string(utf16.Decode(units))
}
