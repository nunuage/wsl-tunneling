//go:build windows

package gvisor

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func proxyFixture(t *testing.T, is64 bool, subsystem uint16, signed bool) []byte {
	t.Helper()
	certificate := pe.DataDirectory{}
	if signed {
		certificate = pe.DataDirectory{VirtualAddress: 1024, Size: 16}
	}
	var header any = pe.OptionalHeader32{Magic: 0x10b, Subsystem: subsystem, NumberOfRvaAndSizes: 16}
	machine := uint16(pe.IMAGE_FILE_MACHINE_I386)
	if is64 {
		h := pe.OptionalHeader64{Magic: 0x20b, Subsystem: subsystem, NumberOfRvaAndSizes: 16}
		h.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_SECURITY] = certificate
		header = h
		machine = pe.IMAGE_FILE_MACHINE_AMD64
	} else {
		h := header.(pe.OptionalHeader32)
		h.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_SECURITY] = certificate
		header = h
	}
	dos := make([]byte, 128)
	copy(dos, "MZ")
	binary.LittleEndian.PutUint32(dos[0x3c:], 128)
	var image bytes.Buffer
	image.Write(dos)
	image.WriteString("PE\x00\x00")
	for _, value := range []any{
		pe.FileHeader{Machine: machine, SizeOfOptionalHeader: uint16(binary.Size(header))}, header,
	} {
		if err := binary.Write(&image, binary.LittleEndian, value); err != nil {
			t.Fatal(err)
		}
	}
	return image.Bytes()
}

func TestSetGUISubsystem(t *testing.T) {
	for _, is64 := range []bool{false, true} {
		image := proxyFixture(t, is64, pe.IMAGE_SUBSYSTEM_WINDOWS_CUI, false)
		original := append([]byte(nil), image...)
		changed, err := setGUISubsystem(image)
		if err != nil || !changed {
			t.Fatalf("64bit=%t: changed=%t, err=%v", is64, changed, err)
		}
		original[128+4+20+68] = byte(pe.IMAGE_SUBSYSTEM_WINDOWS_GUI)
		if !bytes.Equal(original, image) {
			t.Fatal("preparation must change only the subsystem")
		}
		if changed, err := setGUISubsystem(image); err != nil || changed {
			t.Fatalf("GUI preparation must be idempotent: %t, %v", changed, err)
		}
	}
}

func TestSetGUISubsystemRejectsInvalidOrSignedImages(t *testing.T) {
	for _, image := range [][]byte{
		nil, []byte("not a PE"),
		proxyFixture(t, true, pe.IMAGE_SUBSYSTEM_WINDOWS_CUI, true),
		proxyFixture(t, false, pe.IMAGE_SUBSYSTEM_NATIVE, false),
	} {
		before := append([]byte(nil), image...)
		if _, err := setGUISubsystem(image); err == nil {
			t.Fatal("expected invalid or signed executable to be rejected")
		}
		if !bytes.Equal(image, before) {
			t.Fatal("rejected executable was modified")
		}
	}
}

func TestConsoleFreeProxyPreservesDownloadAndReusesCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gvproxy.exe")
	original := proxyFixture(t, true, pe.IMAGE_SUBSYSTEM_WINDOWS_CUI, false)
	if err := os.WriteFile(path, original, 0o755); err != nil {
		t.Fatal(err)
	}
	background, err := consoleFreeProxy(path)
	if err != nil {
		t.Fatal(err)
	}
	if background == path {
		t.Fatal("console proxy must use a separate background copy")
	}
	downloaded, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(downloaded, original) {
		t.Fatal("original download changed")
	}
	info, err := os.Stat(background)
	if err != nil {
		t.Fatal(err)
	}
	second, err := consoleFreeProxy(path)
	if err != nil || second != background {
		t.Fatalf("second preparation: %q, %v", second, err)
	}
	infoAfter, err := os.Stat(background)
	if err != nil || !infoAfter.ModTime().Equal(info.ModTime()) {
		t.Fatal("unchanged background copy should not be rewritten")
	}
}

func TestBackgroundExecutableKeepsStdioWithoutConsole(t *testing.T) {
	if os.Getenv("WSL_TUNNELING_PROXY_TEST_CHILD") == "1" {
		hwnd, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
		_, _ = fmt.Fprintf(os.Stdout, "console=%d", hwnd)
		_, _ = fmt.Fprint(os.Stderr, "stderr preserved")
		os.Exit(0)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "proxy.exe")
	if err := os.WriteFile(source, image, 0o755); err != nil {
		t.Fatal(err)
	}
	background, err := consoleFreeProxy(source)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately no HideWindow/CREATE_NO_WINDOW: the PE subsystem alone
	// must prevent a console, including for launches outside our process tree.
	command := exec.Command(background, "-test.run=^TestBackgroundExecutableKeepsStdioWithoutConsole$")
	command.Env = append(os.Environ(), "WSL_TUNNELING_PROXY_TEST_CHILD=1")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "console=0" || stderr.String() != "stderr preserved" {
		t.Fatalf("unexpected stdio: stdout=%q, stderr=%q", stdout.String(), stderr.String())
	}
}