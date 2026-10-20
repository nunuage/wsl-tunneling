//go:build windows

package dns

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	dnsPolicyKey = `SOFTWARE\Policies\Microsoft\Windows NT\DNSClient`
	tcpipKey     = `SYSTEM\CurrentControlSet\Services\Tcpip\Parameters`

	adapterSuffixFlags = windows.GAA_FLAG_INCLUDE_ALL_INTERFACES |
		windows.GAA_FLAG_SKIP_UNICAST | windows.GAA_FLAG_SKIP_ANYCAST |
		windows.GAA_FLAG_SKIP_MULTICAST | windows.GAA_FLAG_SKIP_DNS_SERVER |
		windows.GAA_FLAG_SKIP_FRIENDLY_NAME
	initialAdapterBuffer = 15000 // Microsoft's recommended initial working size.
	maxAdapterBuffer     = 16 << 20
)

type adapterQuery func(uint32, uint32, uintptr, *windows.IpAdapterAddresses, *uint32) error

// SearchSuffixes takes a fresh, read-only snapshot without launching PowerShell
// or caching VPN/network settings. Global search-list order has precedence over
// adapter suffixes, as in the previous Get-DnsClient based implementation.
func SearchSuffixes(ctx context.Context) ([]string, error) {
	return searchSuffixes(ctx, readSearchList, windows.GetAdaptersAddresses)
}

func searchSuffixes(ctx context.Context, readList func(string) (string, error), query adapterQuery) ([]string, error) {
	var suffixes []string
	// The policy list overrides the local list. An absent/empty policy is not
	// configured. Do not merge in a stale local list when a policy is set.
	// DnsQueryConfig(DnsConfigSearchList) is documented as unavailable, so read
	// the configured lists directly instead of relying on an undocumented ABI.
	for _, path := range []string{dnsPolicyKey, tcpipKey} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		value, err := readList(path)
		if err != nil {
			return nil, fmt.Errorf("read DNS search list: %w", err)
		}
		if strings.TrimSpace(value) != "" {
			suffixes = strings.Split(value, ",")
			break
		}
	}
	adapters, err := adapterSearchSuffixes(ctx, query)
	if err != nil {
		return nil, err
	}
	return NormalizeSearchSuffixes(append(suffixes, adapters...)), nil
}

func readSearchList(path string) (string, error) {
	// Explicitly use the native registry view for 32-bit builds on 64-bit
	// Windows too. No administrative privileges or registry writes are needed.
	return readRegistrySearchList(registry.LOCAL_MACHINE, path)
}

func readRegistrySearchList(root registry.Key, path string) (string, error) {
	key, err := registry.OpenKey(root, path, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer key.Close()
	value, _, err := key.GetStringValue("SearchList")
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	return value, err
}

func adapterSearchSuffixes(ctx context.Context, query adapterQuery) ([]string, error) {
	size := uint32(initialAdapterBuffer)
	// An adapter can appear/disappear while Windows fills the buffer. Retry a
	// growing buffer, but bound retries and allocations rather than spinning.
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		buffer := make([]byte, size)
		head := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buffer[0]))
		err := query(windows.AF_UNSPEC, adapterSuffixFlags, 0, head, &size)
		// These synchronous local Windows calls cannot be interrupted in flight.
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		if errors.Is(err, windows.ERROR_NO_DATA) || err == nil && size == 0 {
			return nil, nil
		}
		if errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) {
			if size <= uint32(len(buffer)) || size > maxAdapterBuffer {
				return nil, fmt.Errorf("GetAdaptersAddresses: invalid buffer growth (%d bytes): %w", size, err)
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("GetAdaptersAddresses: %w", err)
		}
		var suffixes []string
		for adapter := head; adapter != nil; adapter = adapter.Next {
			// Include disconnected adapters too, matching Get-DnsClient's scope.
			// DnsSuffix is the connection-specific suffix; FirstDnsSuffix contains
			// additional search entries that the previous code did not collect.
			if suffix := windows.UTF16PtrToString(adapter.DnsSuffix); suffix != "" {
				suffixes = append(suffixes, suffix)
			}
		}
		// The OS-owned linked pointers refer into our byte buffer. Copy all text
		// before releasing it; keep the entire buffer live through traversal.
		runtime.KeepAlive(buffer)
		return suffixes, nil
	}
	return nil, fmt.Errorf("GetAdaptersAddresses: adapter list kept changing: %w", windows.ERROR_BUFFER_OVERFLOW)
}
