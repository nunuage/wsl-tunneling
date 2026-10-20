package wsl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestWindowsPathToWSL(t *testing.T) {
	got, err := WindowsPathToWSL(`C:\Users\me\bin\gvproxy.exe`)
	if err != nil {
		t.Fatalf("WindowsPathToWSL() error = %v", err)
	}
	want := "/mnt/c/Users/me/bin/gvproxy.exe"
	if got != want {
		t.Fatalf("WindowsPathToWSL() = %q, want %q", got, want)
	}
}

func TestWindowsPathToWSLRejectsRelative(t *testing.T) {
	if _, err := WindowsPathToWSL(`bin\gvproxy.exe`); err == nil {
		t.Fatal("WindowsPathToWSL() error = nil, want error")
	}
}

func TestBashArguments(t *testing.T) {
	script := "printf '%s\\n' \"a b 'quoted'\""
	want := []string{"/usr/bin/env", "-u", "BASH_ENV",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"/bin/bash", "--noprofile", "--norc", "-c", script}
	if got := bashArgs(script); !reflect.DeepEqual(got, want) {
		t.Fatalf("Bash arguments = %#v, want %#v", got, want)
	}
}

// Handle the substitute wsl.exe before the testing package parses WSL flags.
func TestMain(m *testing.M) {
	if os.Getenv("WSL_TUNNELING_WSL_TEST_CHILD") == "1" {
		fmt.Fprint(os.Stdout, strings.Join(os.Args[1:], "\n"))
		fmt.Fprint(os.Stderr, "stderr preserved")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// The test executable substitutes for wsl.exe: no real WSL/tunnel operation.
func TestBashProcess(t *testing.T) {
	t.Setenv("WSL_TUNNELING_WSL_TEST_CHILD", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	client := Client{Exe: executable}
	script := "echo 'quotes and spaces'"
	result, err := client.Bash(context.Background(), "Distro with spaces", script)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]string{"--distribution", "Distro with spaces", "--user", "root", "--exec"}, bashArgs(script)...)
	if result.Stdout != strings.Join(want, "\n") || result.Stderr != "stderr preserved" {
		t.Fatalf("Bash result = %+v", result)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Bash(ctx, "test", script); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Bash error = %v", err)
	}
}

func TestBashSkipsStartupFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Bash environment behavior is tested on Linux/WSL")
	}
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("Bash is unavailable")
	}
	home := t.TempDir()
	for _, name := range []string{".bash_profile", ".profile", ".bashrc", "injected-env"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte("echo UNEXPECTED_STARTUP_FILE; exit 97\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("BASH_ENV", filepath.Join(home, "injected-env"))
	t.Setenv("WSL_INTEROP", "/run/WSL/test_interop")
	t.Setenv("PATH", "/deliberately/missing")
	args := bashArgs(`printf '%s\n%s\n' "$WSL_INTEROP" "$PATH"; command -v ip timeout`)
	output, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil || strings.Contains(string(output), "UNEXPECTED") || !strings.HasPrefix(string(output), "/run/WSL/test_interop\n/usr/local/sbin:") {
		t.Fatalf("non-login shell = %q, error = %v", output, err)
	}
}
