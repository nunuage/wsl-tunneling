//go:build windows

package dns

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/clooooud/wsl-tunneling/internal/process"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func suffixQuery(t *testing.T, values ...string) adapterQuery {
	t.Helper()
	// Allocate typed backing memory so the GC can see the synthetic pointers.
	adapters := make([]windows.IpAdapterAddresses, len(values))
	for index, value := range values {
		adapters[index].DnsSuffix = windows.StringToUTF16Ptr(value)
		adapters[index].OperStatus = windows.IfOperStatusDown
		if index+1 < len(adapters) {
			adapters[index].Next = &adapters[index+1]
		}
	}
	return func(family, flags uint32, reserved uintptr, head *windows.IpAdapterAddresses, size *uint32) error {
		if family != windows.AF_UNSPEC || flags != adapterSuffixFlags || reserved != 0 {
			t.Fatalf("unexpected adapter query: family=%d flags=%d reserved=%d", family, flags, reserved)
		}
		if len(adapters) == 0 {
			return windows.ERROR_NO_DATA
		}
		*head = adapters[0]
		runtime.KeepAlive(adapters)
		return nil
	}
}

func TestSearchSuffixesSources(t *testing.T) {
	for _, tt := range []struct {
		name   string
		policy string
		local  string
		want   []string
		reads  int
	}{
		{"policy overrides local", " .corp.test., global.test ,CORP.TEST", "stale.test", []string{"corp.test", "global.test", "vpn.test", "wifi.test"}, 1},
		{"local fallback", "", "local.test,corp.test", []string{"local.test", "corp.test", "vpn.test", "wifi.test"}, 2},
		{"empty policy fallback", " \t", "local.test", []string{"local.test", "CORP.TEST", "vpn.test", "wifi.test"}, 2},
		{"interfaces only", "", "", []string{"CORP.TEST", "vpn.test", "wifi.test"}, 2},
		{"invalid values filtered", "bad suffix,good.test", "stale.test", []string{"good.test", "CORP.TEST", "vpn.test", "wifi.test"}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reads := 0
			read := func(path string) (string, error) {
				reads++
				switch path {
				case dnsPolicyKey:
					return tt.policy, nil
				case tcpipKey:
					return tt.local, nil
				default:
					t.Fatalf("unexpected registry key: %s", path)
					return "", nil
				}
			}
			got, err := searchSuffixes(context.Background(), read, suffixQuery(t, "CORP.TEST", "vpn.test", "", "bad suffix", "wifi.test"))
			if err != nil || !reflect.DeepEqual(got, tt.want) || reads != tt.reads {
				t.Fatalf("suffixes=%v err=%v reads=%d, want %v / %d reads", got, err, reads, tt.want, tt.reads)
			}
		})
	}
}

func TestSearchSuffixesFreshSnapshot(t *testing.T) {
	policy := "before.test"
	read := func(string) (string, error) { return policy, nil }
	for _, want := range []string{"before.test", "after-vpn.test"} {
		policy = want
		got, err := searchSuffixes(context.Background(), read, suffixQuery(t))
		if err != nil || !reflect.DeepEqual(got, []string{want}) {
			t.Fatalf("suffixes=%v error=%v, want fresh %s", got, err, want)
		}
	}
}

func TestSearchSuffixesErrorsAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	neverRead := func(string) (string, error) { t.Fatal("read after cancellation"); return "", nil }
	if _, err := searchSuffixes(ctx, neverRead, suffixQuery(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want cancellation", err)
	}
	denied := func(string) (string, error) { return "", windows.ERROR_ACCESS_DENIED }
	if _, err := searchSuffixes(context.Background(), denied, suffixQuery(t)); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("error=%v, want access denied", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	read := func(string) (string, error) { cancel(); return "corp.test", nil }
	if _, err := searchSuffixes(ctx, read, suffixQuery(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want cancellation before adapter query", err)
	}
	query := func(uint32, uint32, uintptr, *windows.IpAdapterAddresses, *uint32) error {
		return windows.ERROR_INVALID_PARAMETER
	}
	if _, err := searchSuffixes(context.Background(), func(string) (string, error) { return "corp.test", nil }, query); !errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		t.Fatalf("error=%v, want adapter error", err)
	}
}

func TestAdapterSearchSuffixesBufferHandling(t *testing.T) {
	for _, mode := range []string{"grow", "non-growing", "too-large", "changing", "no-data", "empty", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			query := func(_ uint32, _ uint32, _ uintptr, head *windows.IpAdapterAddresses, size *uint32) error {
				calls++
				switch mode {
				case "non-growing":
					return windows.ERROR_BUFFER_OVERFLOW
				case "too-large":
					*size = maxAdapterBuffer + 1
					return windows.ERROR_BUFFER_OVERFLOW
				case "changing":
					*size *= 2
					return windows.ERROR_BUFFER_OVERFLOW
				case "no-data":
					return windows.ERROR_NO_DATA
				case "empty":
					*size = 0
					return nil
				case "cancel":
					cancel()
					return nil
				}
				if calls == 1 {
					if *size != initialAdapterBuffer {
						t.Fatalf("initial size=%d", *size)
					}
					*size *= 2
					return windows.ERROR_BUFFER_OVERFLOW
				}
				// Exercise OS-style pointers into the actual byte buffer, not just
				// typed mock objects, on both 32- and 64-bit Windows test builds.
				text := windows.StringToUTF16("adapter.test")
				ptr := unsafe.Add(unsafe.Pointer(head), unsafe.Sizeof(*head))
				copy(unsafe.Slice((*uint16)(ptr), len(text)), text)
				head.DnsSuffix = (*uint16)(ptr)
				runtime.GC()
				return nil
			}
			got, err := adapterSearchSuffixes(ctx, query)
			switch mode {
			case "grow":
				if err != nil || calls != 2 || !reflect.DeepEqual(got, []string{"adapter.test"}) {
					t.Fatalf("suffixes=%v err=%v calls=%d", got, err, calls)
				}
			case "no-data", "empty":
				if err != nil || len(got) != 0 {
					t.Fatalf("suffixes=%v err=%v", got, err)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error=%v, want canceled", err)
				}
			default:
				if !errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) || calls > 3 {
					t.Fatalf("error=%v calls=%d, want bounded buffer error", err, calls)
				}
			}
		})
	}
}

func TestReadRegistrySearchList(t *testing.T) {
	// Use a disposable HKCU key only: never write system DNS or policy settings.
	path := fmt.Sprintf(`Software\wsl-tunneling-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	key, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.ALL_ACCESS|registry.WOW64_64KEY)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.DeleteKey(registry.CURRENT_USER, path)
	defer key.Close()
	if got, err := readRegistrySearchList(registry.CURRENT_USER, path); err != nil || got != "" {
		t.Fatalf("missing value=%q err=%v", got, err)
	}
	if got, err := readRegistrySearchList(registry.CURRENT_USER, path+`\absent`); err != nil || got != "" {
		t.Fatalf("missing key=%q err=%v", got, err)
	}
	for _, value := range []string{"corp.test,second.test", "new-vpn.test", ""} {
		if err := key.SetStringValue("SearchList", value); err != nil {
			t.Fatal(err)
		}
		if got, err := readRegistrySearchList(registry.CURRENT_USER, path); err != nil || got != value {
			t.Fatalf("value=%q err=%v, want %q", got, err, value)
		}
	}
	if err := key.SetDWordValue("SearchList", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegistrySearchList(registry.CURRENT_USER, path); !errors.Is(err, registry.ErrUnexpectedType) {
		t.Fatalf("error=%v, want unexpected registry type", err)
	}
}

// This is the retired implementation, retained only for an opt-in read-only
// comparison. Normal tests and production never spawn PowerShell for DNS.
func legacySearchSuffixes(ctx context.Context) ([]string, error) {
	script := "$items = @(); $global = (Get-DnsClientGlobalSetting).SuffixSearchList; if ($global) { $items += $global }; Get-DnsClient | ForEach-Object { if ($_.ConnectionSpecificSuffix) { $items += $_.ConnectionSpecificSuffix } }; $items | Where-Object { $_ } | Select-Object -Unique"
	command := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	process.HideWindow(command)
	output, err := command.Output()
	return NormalizeSearchSuffixes(strings.Split(string(output), "\n")), err
}

func TestSearchSuffixesWindowsIntegration(t *testing.T) {
	if os.Getenv("WSL_TUNNELING_DNS_INTEGRATION") != "1" {
		t.Skip("set WSL_TUNNELING_DNS_INTEGRATION=1 for read-only native/PowerShell comparison")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	legacy, err := legacySearchSuffixes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for sample := 0; sample < 5; sample++ {
		started := time.Now()
		native, err := SearchSuffixes(ctx)
		elapsed := time.Since(started)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("sample=%d native_ms=%.3f suffix_count=%d exact_legacy_order=%t", sample+1, float64(elapsed)/float64(time.Millisecond), len(native), reflect.DeepEqual(native, legacy))
		// Global list order is preserved. Interface enumeration order can differ
		// between CIM and IP Helper; require all the same normalized suffixes.
		nativeSet := append([]string(nil), native...)
		legacySet := append([]string(nil), legacy...)
		for index := range nativeSet {
			nativeSet[index] = strings.ToLower(nativeSet[index])
		}
		for index := range legacySet {
			legacySet[index] = strings.ToLower(legacySet[index])
		}
		sort.Strings(nativeSet)
		sort.Strings(legacySet)
		if !reflect.DeepEqual(nativeSet, legacySet) {
			t.Fatal("native and legacy suffix sets differ (network may have changed during comparison)")
		}
	}
}

func BenchmarkSearchSuffixes(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()
	for index := 0; index < b.N; index++ {
		if _, err := SearchSuffixes(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
