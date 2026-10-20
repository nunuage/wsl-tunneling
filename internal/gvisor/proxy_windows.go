//go:build windows

package gvisor

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
)

// WSL interop launches gvproxy itself, so CREATE_NO_WINDOW on wsl.exe does
// not control its console. Use a private GUI-subsystem copy: the Go proxy
// still receives its inherited stdio pipes, without allocating a console.
// The downloaded executable is never modified.
func consoleFreeProxy(path string) (string, error) {
	image, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	changed, err := setGUISubsystem(image)
	if err != nil {
		return "", err
	}
	if !changed {
		return path, nil
	}
	destination := strings.TrimSuffix(path, ".exe") + "-background.exe"
	if cached, err := os.ReadFile(destination); err == nil && bytes.Equal(cached, image) {
		return destination, nil
	}
	temporary := destination + ".tmp"
	if err := os.WriteFile(temporary, image, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(temporary, destination); err != nil {
		_ = os.Remove(temporary)
		return "", err
	}
	return destination, nil
}

func setGUISubsystem(image []byte) (bool, error) {
	file, err := pe.NewFile(bytes.NewReader(image))
	if err != nil {
		return false, fmt.Errorf("invalid gvproxy executable: %w", err)
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
		return false, fmt.Errorf("gvproxy has no supported PE optional header")
	}
	if subsystem == pe.IMAGE_SUBSYSTEM_WINDOWS_GUI {
		return false, nil
	}
	if subsystem != pe.IMAGE_SUBSYSTEM_WINDOWS_CUI {
		return false, fmt.Errorf("unsupported gvproxy subsystem: %d", subsystem)
	}
	// Never silently invalidate a publisher's Authenticode signature.
	if certificate.VirtualAddress != 0 || certificate.Size != 0 {
		return false, fmt.Errorf("cannot prepare signed gvproxy for background use; provide an unsigned or GUI-subsystem build")
	}
	// Subsystem is at offset 68 in both PE32 and PE32+ optional headers.
	if len(image) < 0x40 {
		return false, fmt.Errorf("truncated gvproxy DOS header")
	}
	peOffset := uint64(binary.LittleEndian.Uint32(image[0x3c:0x40]))
	offset := peOffset + 4 + 20 + 68
	if offset+2 > uint64(len(image)) {
		return false, fmt.Errorf("truncated gvproxy subsystem")
	}
	binary.LittleEndian.PutUint16(image[offset:offset+2], pe.IMAGE_SUBSYSTEM_WINDOWS_GUI)
	return true, nil
}