//go:build windows

package gvisor

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/windows"
)

var proxyPrepareMu sync.Mutex

// WSL interop launches gvproxy itself, so CREATE_NO_WINDOW on wsl.exe does
// not control its console. Use a private GUI-subsystem copy: the Go proxy
// still receives its inherited stdio pipes, without allocating a console.
// The downloaded executable is never modified.
func consoleFreeProxy(path string) (string, error) {
	proxyPrepareMu.Lock()
	defer proxyPrepareMu.Unlock()
	// Deny writes/replacement while validating and copying the source. A stat
	// cache alone cannot detect corruption or a same-size, same-mtime update.
	source, err := openStableProxy(path)
	if err != nil {
		return "", err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return "", err
	}
	offset, changed, err := guiSubsystemOffset(source, info.Size())
	if err != nil {
		return "", err
	}
	if !changed {
		return path, nil
	}
	destination := strings.TrimSuffix(path, ".exe") + "-background.exe"
	match, err := backgroundProxyMatches(source, destination, info.Size(), offset)
	if err != nil {
		return "", err
	}
	if match {
		return destination, nil
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".gvproxy-background-*.tmp")
	if err != nil {
		return "", err
	}
	defer os.Remove(temporary.Name())
	// ReadAt-based validation leaves source's sequential offset at zero.
	_, copyErr := io.Copy(temporary, source)
	if copyErr == nil {
		_, copyErr = temporary.WriteAt([]byte{byte(pe.IMAGE_SUBSYSTEM_WINDOWS_GUI), 0}, offset)
	}
	closeErr := temporary.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	// Publish only the complete copy; failure leaves the previous file intact.
	if err := os.Rename(temporary.Name(), destination); err != nil {
		return "", err
	}
	return destination, nil
}

func openStableProxy(path string) (*os.File, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// CreateFile needs extended absolute paths to retain os.Open's support for
	// long cache paths. FILE_SHARE_READ deliberately excludes write/delete.
	if !strings.HasPrefix(absolute, `\\?\`) {
		if strings.HasPrefix(absolute, `\\`) {
			absolute = `\\?\UNC\` + absolute[2:]
		} else {
			absolute = `\\?\` + absolute
		}
	}
	name, err := windows.UTF16PtrFromString(absolute)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_SEQUENTIAL_SCAN, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}

// Compare all bytes, masking only the two subsystem bytes in the source. Memory
// use is bounded independently of executable size; there is no metadata cache.
func backgroundProxyMatches(source io.ReaderAt, destination string, size, offset int64) (bool, error) {
	cached, err := openStableProxy(destination)
	if err != nil {
		return false, nil // Absent/unreadable cache: try to regenerate it.
	}
	defer cached.Close()
	info, err := cached.Stat()
	if err != nil || info.Size() != size {
		return false, nil
	}
	var expected, actual [64 * 1024]byte
	for position := int64(0); position < size; {
		count := int64(len(expected))
		if remaining := size - position; remaining < count {
			count = remaining
		}
		if _, err := source.ReadAt(expected[:count], position); err != nil {
			return false, err
		}
		if _, err := cached.ReadAt(actual[:count], position); err != nil {
			return false, nil
		}
		for i, value := range []byte{byte(pe.IMAGE_SUBSYSTEM_WINDOWS_GUI), 0} {
			if index := offset + int64(i) - position; index >= 0 && index < count {
				expected[index] = value
			}
		}
		if !bytes.Equal(expected[:count], actual[:count]) {
			return false, nil
		}
		position += count
	}
	return true, nil
}

func setGUISubsystem(image []byte) (bool, error) {
	offset, changed, err := guiSubsystemOffset(bytes.NewReader(image), int64(len(image)))
	if err != nil || !changed {
		return changed, err
	}
	binary.LittleEndian.PutUint16(image[offset:offset+2], pe.IMAGE_SUBSYSTEM_WINDOWS_GUI)
	return true, nil
}

func guiSubsystemOffset(reader io.ReaderAt, size int64) (int64, bool, error) {
	file, err := pe.NewFile(reader)
	if err != nil {
		return 0, false, fmt.Errorf("invalid gvproxy executable: %w", err)
	}
	defer file.Close()
	var subsystem uint16
	var certificate pe.DataDirectory
	switch header := file.OptionalHeader.(type) {
	case *pe.OptionalHeader32:
		subsystem = header.Subsystem
		certificate = header.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_SECURITY]
	case *pe.OptionalHeader64:
		subsystem = header.Subsystem
		certificate = header.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_SECURITY]
	default:
		return 0, false, fmt.Errorf("gvproxy has no supported PE optional header")
	}
	if subsystem == pe.IMAGE_SUBSYSTEM_WINDOWS_GUI {
		return 0, false, nil
	}
	if subsystem != pe.IMAGE_SUBSYSTEM_WINDOWS_CUI {
		return 0, false, fmt.Errorf("unsupported gvproxy subsystem: %d", subsystem)
	}
	// Never silently invalidate a publisher's Authenticode signature.
	if certificate.VirtualAddress != 0 || certificate.Size != 0 {
		return 0, false, fmt.Errorf("cannot prepare signed gvproxy for background use; provide an unsigned or GUI-subsystem build")
	}
	// Subsystem is at offset 68 in both PE32 and PE32+ optional headers.
	var dos [0x40]byte
	if _, err := reader.ReadAt(dos[:], 0); err != nil {
		return 0, false, fmt.Errorf("truncated gvproxy DOS header: %w", err)
	}
	peOffset := int64(binary.LittleEndian.Uint32(dos[0x3c:0x40]))
	offset := peOffset + 4 + 20 + 68
	if offset+2 > size {
		return 0, false, fmt.Errorf("truncated gvproxy subsystem")
	}
	return offset, true, nil
}