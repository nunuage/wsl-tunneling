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
	"strings"
	"sync"
	"syscall"
	"testing"
)

func proxyFixture(t testing.TB, is64 bool, subsystem uint16, signed bool) []byte {
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

func TestConsoleFreeProxyDetectsContentChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gvproxy.exe")
	original := append(proxyFixture(t, true, pe.IMAGE_SUBSYSTEM_WINDOWS_CUI, false), []byte("original payload")...)
	if err := os.WriteFile(path, original, 0o755); err != nil {
		t.Fatal(err)
	}
	background, err := consoleFreeProxy(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(background)
	if err != nil {
		t.Fatal(err)
	}
	corrupt, err := os.ReadFile(background)
	if err != nil {
		t.Fatal(err)
	}
	corrupt[len(corrupt)-1] ^= 1
	if err := os.WriteFile(background, corrupt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(background, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	// Corruption must be detected even with identical file size and mtime.
	if _, err := consoleFreeProxy(path); err != nil {
		t.Fatal(err)
	}
	want := append([]byte(nil), original...)
	if _, err := setGUISubsystem(want); err != nil {
		t.Fatal(err)
	}
	assertProxyContents(t, background, want)
	original[len(original)-1] ^= 2
	sourceInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, sourceInfo.ModTime(), sourceInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := consoleFreeProxy(path); err != nil {
		t.Fatal(err)
	}
	want = append([]byte(nil), original...)
	if _, err := setGUISubsystem(want); err != nil {
		t.Fatal(err)
	}
	assertProxyContents(t, path, original)
	assertProxyContents(t, background, want)
	if err := os.Remove(background); err != nil {
		t.Fatal(err)
	}
	if _, err := consoleFreeProxy(path); err != nil {
		t.Fatal(err)
	}
	assertProxyContents(t, background, want)
}

func assertProxyContents(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("unexpected proxy contents at %q: %v", path, err)
	}
}

func TestConsoleFreeProxyGUIAndRejectedImages(t *testing.T) {
	for _, tt := range []struct {
		name      string
		image     []byte
		wantError bool
	}{
		{"GUI", proxyFixture(t, true, pe.IMAGE_SUBSYSTEM_WINDOWS_GUI, false), false},
		{"signed GUI unchanged", proxyFixture(t, false, pe.IMAGE_SUBSYSTEM_WINDOWS_GUI, true), false},
		{"signed console", proxyFixture(t, true, pe.IMAGE_SUBSYSTEM_WINDOWS_CUI, true), true},
		{"invalid", []byte("not a PE"), true},
		{"truncated", proxyFixture(t, false, pe.IMAGE_SUBSYSTEM_WINDOWS_CUI, false)[:180], true},
		{"unsupported", proxyFixture(t, false, pe.IMAGE_SUBSYSTEM_NATIVE, false), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gvproxy.exe")
			if err := os.WriteFile(path, tt.image, 0o755); err != nil {
				t.Fatal(err)
			}
			got, err := consoleFreeProxy(path)
			if (err != nil) != tt.wantError || (!tt.wantError && got != path) {
				t.Fatalf("prepared = %q, error=%v", got, err)
			}
			assertProxyContents(t, path, tt.image)
		})
	}
}

func TestConsoleFreeProxyConcurrentAndFailedPublication(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "gvproxy.exe")
	original := proxyFixture(t, false, pe.IMAGE_SUBSYSTEM_WINDOWS_CUI, false)
	if err := os.WriteFile(path, original, 0o755); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if _, err := consoleFreeProxy(path); err != nil {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
	background := strings.TrimSuffix(path, ".exe") + "-background.exe"
	before, err := os.ReadFile(background)
	if err != nil {
		t.Fatal(err)
	}
	// A reader denying delete/write makes publication fail, without allowing
	// a failed regeneration to truncate or destroy the last complete copy.
	locked, err := openStableProxy(background)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()
	original = append(original, []byte("new version")...)
	if err := os.WriteFile(path, original, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := consoleFreeProxy(path); err == nil {
		t.Fatal("expected publication to fail while old copy is locked")
	}
	assertProxyContents(t, background, before)
	files, err := filepath.Glob(filepath.Join(root, ".gvproxy-background-*.tmp"))
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary files leaked: %v, %v", files, err)
	}
}

func TestStableProxyDeniesSourceChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gvproxy.exe")
	original := proxyFixture(t, true, pe.IMAGE_SUBSYSTEM_WINDOWS_CUI, false)
	if err := os.WriteFile(path, original, 0o755); err != nil {
		t.Fatal(err)
	}
	locked, err := openStableProxy(path)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()
	if err := os.WriteFile(path, []byte("concurrent write"), 0o755); err == nil {
		t.Fatal("source must not be writable during preparation")
	}
	if err := os.Remove(path); err == nil {
		t.Fatal("source must not be replaceable during preparation")
	}
	assertProxyContents(t, path, original)
}

func TestBackgroundComparisonAcrossBufferBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "background.exe")
	source := make([]byte, 2*64*1024+1)
	const offset = 64*1024 - 1
	source[offset] = byte(pe.IMAGE_SUBSYSTEM_WINDOWS_CUI)
	want := append([]byte(nil), source...)
	binary.LittleEndian.PutUint16(want[offset:offset+2], pe.IMAGE_SUBSYSTEM_WINDOWS_GUI)
	if err := os.WriteFile(path, want, 0o755); err != nil {
		t.Fatal(err)
	}
	match, err := backgroundProxyMatches(bytes.NewReader(source), path, int64(len(source)), offset)
	if err != nil || !match {
		t.Fatalf("comparison across two blocks = %t, %v", match, err)
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

// Match the installed 13.2 MB proxy without touching the real asset cache.
func BenchmarkConsoleFreeProxyCached(b *testing.B) {
	path := filepath.Join(b.TempDir(), "gvproxy.exe")
	image := proxyFixture(b, true, pe.IMAGE_SUBSYSTEM_WINDOWS_CUI, false)
	image = append(image, make([]byte, 13210112-len(image))...)
	if err := os.WriteFile(path, image, 0o755); err != nil {
		b.Fatal(err)
	}
	if _, err := consoleFreeProxy(path); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(2 * len(image)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := consoleFreeProxy(path); err != nil {
			b.Fatal(err)
		}
	}
}