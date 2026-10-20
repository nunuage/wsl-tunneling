//go:build windows

package tray

import (
	"fmt"
	"strings"
	"sync"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/lxn/win"
)

// No test creates a window, starts a timer, or runs the tray/tunnel.
func TestNotificationBounds(t *testing.T) {
	for _, tt := range []struct {
		name string
		work win.RECT
		want win.RECT
	}{
		{"bottom taskbar", win.RECT{Right: 1920, Bottom: 1040}, win.RECT{Left: 1564, Top: 912, Right: 1904, Bottom: 1024}},
		{"right taskbar", win.RECT{Right: 1860, Bottom: 1080}, win.RECT{Left: 1504, Top: 952, Right: 1844, Bottom: 1064}},
		{"left and top taskbars", win.RECT{Left: 60, Top: 40, Right: 1920, Bottom: 1080}, win.RECT{Left: 1564, Top: 952, Right: 1904, Bottom: 1064}},
		{"negative desktop", win.RECT{Left: -1920, Top: -1080, Right: 0, Bottom: -40}, win.RECT{Left: -356, Top: -168, Right: -16, Bottom: -56}},
		{"small work area", win.RECT{Left: 10, Top: 20, Right: 210, Bottom: 100}, win.RECT{Left: 26, Top: 36, Right: 194, Bottom: 84}},
		{"tiny work area", win.RECT{Left: -1, Top: -1, Right: 0, Bottom: 0}, win.RECT{Left: -1, Top: -1, Right: 0, Bottom: 0}},
		{"empty", win.RECT{}, win.RECT{}},
		{"inverted", win.RECT{Left: 100, Right: 50, Bottom: 100}, win.RECT{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := notificationBounds(tt.work)
			if got != tt.want {
				t.Fatalf("bounds = %+v, want %+v", got, tt.want)
			}
			if got != (win.RECT{}) && (got.Left < tt.work.Left || got.Top < tt.work.Top || got.Right > tt.work.Right || got.Bottom > tt.work.Bottom) {
				t.Fatal("popup must remain inside the work area")
			}
		})
	}
}

func TestNotificationDoesNotRequestActivation(t *testing.T) {
	required := uint32(win.WS_EX_NOACTIVATE | win.WS_EX_TOOLWINDOW | win.WS_EX_TOPMOST)
	if popupExStyle&required != required || popupExStyle&win.WS_EX_APPWINDOW != 0 {
		t.Fatal("popup must be non-activating, topmost, and absent from the taskbar")
	}
	if popupStyle != win.WS_POPUP || popupShowCommand != win.SW_SHOWNOACTIVATE {
		t.Fatal("popup must be borderless and shown without activation")
	}
	if popupHideFlags != win.AW_HIDE|win.AW_BLEND || popupHideFlags&win.AW_ACTIVATE != 0 {
		t.Fatal("fade must hide without activation")
	}
	if got := popupWindowProc(0, win.WM_MOUSEACTIVATE, 0, 0); got != 4 {
		t.Fatalf("mouse activation = %d, want MA_NOACTIVATEANDEAT", got)
	}
	if popupDuration != 3000 || popupFadeTime <= 0 || popupFadeTime > 250 {
		t.Fatal("expected three-second display and a quick exit fade")
	}
}

func TestNotificationText(t *testing.T) {
	for _, tt := range []struct {
		name  string
		text  string
		limit int
		want  string
	}{
		{"empty", "", 96, ""},
		{"whitespace", "  Tunnel started \r\n", 96, "Tunnel started"},
		{"NUL", "bad\x00value", 512, "bad\ufffdvalue"},
		{"title limit", strings.Repeat("T", 100), 96, strings.Repeat("T", 95) + "…"},
		{"unicode", strings.Repeat("🚀é", 300), 512, strings.Repeat("🚀é", 255) + "🚀…"},
		{"exact limit", "🚀é", 2, "🚀é"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := notificationText(tt.text, tt.limit)
			if got != tt.want || !utf8.ValidString(got) || strings.ContainsRune(got, '\x00') {
				t.Fatalf("text = %q, want %q", got, tt.want)
			}
			if roundTrip := syscall.UTF16ToString(unsafeStringToUTF16(got)); roundTrip != got {
				t.Fatalf("Win32 text round trip = %q, want %q", roundTrip, got)
			}
		})
	}
}

func TestNotificationQueueFIFOAndFailedPost(t *testing.T) {
	a := &app{}
	first := notification{title: "Tunnel started", message: "start completed"}
	second := notification{title: "Stop failed", message: "details", isError: true}
	posts := 0
	wake := func() bool { posts++; return true }
	if !a.enqueueNotification(first, wake) {
		t.Fatal("first enqueue failed")
	}
	if a.enqueueNotification(notification{title: "undeliverable"}, func() bool { return false }) {
		t.Fatal("failed PostMessage must roll back the new payload")
	}
	if !a.enqueueNotification(second, wake) || posts != 2 {
		t.Fatal("each accepted payload must wake the UI")
	}
	for _, want := range []notification{first, second} {
		got, ok := a.nextNotification()
		if !ok || got != want {
			t.Fatalf("dequeue = %+v, %t; want %+v", got, ok, want)
		}
	}
	if _, ok := a.nextNotification(); ok || a.notifications != nil {
		t.Fatal("drained queue must release its payloads")
	}
}

func TestNotificationQueueConcurrentWorkers(t *testing.T) {
	a := &app{}
	const count = 64
	var workers sync.WaitGroup
	workers.Add(count)
	for i := 0; i < count; i++ {
		go func(i int) {
			defer workers.Done()
			if !a.enqueueNotification(notification{title: fmt.Sprint(i)}, func() bool { return true }) {
				t.Error("enqueue failed")
			}
		}(i)
	}
	workers.Wait()
	seen := make(map[string]bool)
	for n, ok := a.nextNotification(); ok; n, ok = a.nextNotification() {
		if seen[n.title] {
			t.Fatalf("duplicate payload %q", n.title)
		}
		seen[n.title] = true
	}
	if len(seen) != count {
		t.Fatalf("received %d payloads, want %d", len(seen), count)
	}
}

func TestNotificationQueueShutdown(t *testing.T) {
	a := &app{}
	a.enqueueNotification(notification{title: "already queued"}, func() bool { return true })
	var workers sync.WaitGroup
	for i := 0; i < 64; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			a.enqueueNotification(notification{title: "pending"}, func() bool { return true })
		}()
	}
	a.stopNotifications()
	workers.Wait()
	a.stopNotifications() // Cleanup is idempotent.
	if _, ok := a.nextNotification(); ok || a.notifications != nil {
		t.Fatal("shutdown must discard queued payloads")
	}
	if a.enqueueNotification(notification{}, func() bool {
		t.Error("a late worker must never post to the closed UI")
		return true
	}) {
		t.Fatal("closed queue accepted a payload")
	}
}

func TestNotifyIconTooltipIndependentOfPopup(t *testing.T) {
	a := &app{window: win.HWND(42), tooltip: appName + " - running"}
	a.enqueueNotification(notification{title: "Tunnel started"}, func() bool { return true })
	data := a.notifyIconData(win.NIF_TIP)
	if data.HWnd != a.window || data.UID != trayIconID || data.UFlags != win.NIF_TIP {
		t.Fatal("tooltip must target the tray icon without balloon flags")
	}
	if got := syscall.UTF16ToString(data.SzTip[:]); got != appName+" - running" {
		t.Fatalf("tooltip = %q", got)
	}
	if data.SzInfo[0] != 0 || data.SzInfoTitle[0] != 0 || data.DwInfoFlags != 0 {
		t.Fatal("tray icon must not carry Windows notification data")
	}
}