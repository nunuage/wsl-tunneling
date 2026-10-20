//go:build windows

package tray

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/lxn/win"
)

// Normal tests create no windows/timers and never run the tray/tunnel.
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
	if got := popupWindowProc(0, win.WM_MOUSEACTIVATE, 0, 0); got != 3 {
		t.Fatalf("mouse activation = %d, want MA_NOACTIVATE (deliver close click without focus)", got)
	}
	if popupDuration != 3000 || popupFadeTime <= 0 || popupFadeTime > 250 {
		t.Fatal("expected three-second display and a quick exit fade")
	}
}

func TestNotificationOperationDuration(t *testing.T) {
	for _, tt := range []struct {
		elapsed time.Duration
		want    string
	}{
		{-time.Second, "0 ms"}, {0, "0 ms"}, {400 * time.Microsecond, "0 ms"},
		{12500 * time.Microsecond, "13 ms"}, {999 * time.Millisecond, "999 ms"},
		{999600 * time.Microsecond, "1.00 s"}, {time.Second, "1.00 s"},
		{4774215 * time.Microsecond, "4.77 s"}, {65 * time.Second, "65.00 s"},
	} {
		if got := notificationDuration(tt.elapsed); got != tt.want {
			t.Fatalf("duration(%s) = %q, want %q", tt.elapsed, got, tt.want)
		}
	}
	for _, tt := range []struct {
		name, completed, title, message string
		err                             error
	}{
		{"start", "started", "Tunnel started", "start completed", nil},
		{"stop", "stopped", "Tunnel stopped", "stop completed", nil},
		{"start", "started", "start failed", "diagnostic preserved", errors.New("diagnostic preserved")},
		{"stop", "stopped", "stop failed", "restore failed", errors.New("restore failed")},
	} {
		n := operationNotification(tt.name, tt.completed, 4774*time.Millisecond, tt.err)
		if n.title != tt.title || n.message != tt.message || n.isError != (tt.err != nil) || !n.timed || n.elapsed != 4774*time.Millisecond {
			t.Fatalf("operation notification = %+v", n)
		}
	}
}

func TestNotificationCloseInteraction(t *testing.T) {
	type event struct {
		message uint32
		hit     bool
	}
	for _, tt := range []struct {
		name   string
		events []event
		want   int
	}{
		{"click cross", []event{{win.WM_LBUTTONDOWN, true}, {win.WM_LBUTTONUP, true}}, 1},
		{"body click", []event{{win.WM_LBUTTONDOWN, false}, {win.WM_LBUTTONUP, false}}, 0},
		{"release only", []event{{win.WM_LBUTTONUP, true}}, 0},
		{"drag into cross", []event{{win.WM_LBUTTONDOWN, false}, {win.WM_LBUTTONUP, true}}, 0},
		{"drag out", []event{{win.WM_LBUTTONDOWN, true}, {win.WM_LBUTTONUP, false}}, 0},
		{"drag out and back", []event{{win.WM_LBUTTONDOWN, true}, {win.WM_MOUSEMOVE, false}, {win.WM_LBUTTONUP, true}}, 0},
		{"leave window", []event{{win.WM_LBUTTONDOWN, true}, {win.WM_MOUSELEAVE, false}, {win.WM_LBUTTONUP, true}}, 0},
		{"cancel", []event{{win.WM_LBUTTONDOWN, true}, {win.WM_CANCELMODE, false}, {win.WM_LBUTTONUP, true}}, 0},
		{"lost capture", []event{{win.WM_LBUTTONDOWN, true}, {win.WM_CAPTURECHANGED, false}, {win.WM_LBUTTONUP, true}}, 0},
		{"right click", []event{{win.WM_RBUTTONDOWN, true}, {win.WM_RBUTTONUP, true}}, 0},
		{"duplicate release", []event{{win.WM_LBUTTONDOWN, true}, {win.WM_LBUTTONUP, true}, {win.WM_LBUTTONUP, true}}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var state notificationCloseState
			dismissals := 0
			for _, e := range tt.events {
				if _, dismiss := state.update(e.message, e.hit); dismiss {
					dismissals++
				}
			}
			if dismissals != tt.want || state.pressed {
				t.Fatalf("dismissals=%d state=%+v, want %d dismissals and no stuck press", dismissals, state, tt.want)
			}
		})
	}
	var state notificationCloseState
	if changed, _ := state.update(win.WM_MOUSEMOVE, true); !changed || !state.hovered {
		t.Fatal("entering the close button must update hover")
	}
	if changed, _ := state.update(win.WM_MOUSEMOVE, true); changed {
		t.Fatal("moving inside the same button must not repaint")
	}
	if changed, _ := state.update(win.WM_MOUSELEAVE, false); !changed || state.hovered {
		t.Fatal("leaving the popup must clear hover")
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
	first := operationNotification("start", "started", 4774*time.Millisecond, nil)
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

// Opt-in native message-routing test: short-lived, non-activating popups;
// no mouse movement, desktop click, tray icon or tunnel operation.
func TestNotificationPopupIntegration(t *testing.T) {
	if os.Getenv("WSL_TUNNELING_NOTIFICATION_INTEGRATION") != "1" {
		t.Skip("set WSL_TUNNELING_NOTIFICATION_INTEGRATION=1 for native popup routing checks")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	previousApp := currentApp
	defer func() { currentApp = previousApp }()
	a := &app{}
	currentApp = a
	defer a.popup.cleanup()
	first := operationNotification("start", "started", 4774*time.Millisecond, nil)
	second := operationNotification("stop", "stopped", 315*time.Millisecond, nil)
	third := notification{title: "Diagnostic", message: "Error details", isError: true}
	a.notifications = []notification{first, second, third}
	foreground := win.GetForegroundWindow()
	a.showNextNotification()
	if a.popup.window == 0 || a.popup.content != first {
		t.Fatal("first popup not displayed")
	}
	firstWindow, firstTimer := a.popup.window, a.popup.timer
	if got := win.SendMessage(firstWindow, win.WM_MOUSEACTIVATE, 0, 0); got != popupMouseActivate {
		t.Fatalf("mouse activation result=%d", got)
	}
	// A click in the body is inert.
	win.SendMessage(firstWindow, win.WM_LBUTTONDOWN, win.MK_LBUTTON, 0)
	win.SendMessage(firstWindow, win.WM_LBUTTONUP, 0, 0)
	if a.popup.window != firstWindow || a.popup.content != first {
		t.Fatal("body click dismissed the notification")
	}
	var rect win.RECT
	if !win.GetClientRect(firstWindow, &rect) {
		t.Fatal("popup client rectangle unavailable")
	}
	close := notificationLayout(rect.Right, rect.Bottom).close
	point := uintptr(uint16((close.Left+close.Right)/2)) | uintptr(uint16((close.Top+close.Bottom)/2))<<16
	win.SendMessage(firstWindow, win.WM_LBUTTONDOWN, win.MK_LBUTTON, point)
	win.SendMessage(firstWindow, win.WM_LBUTTONUP, 0, point)
	if a.popup.window == 0 || a.popup.content != second || a.popup.timer == firstTimer || len(a.notifications) != 1 {
		t.Fatal("cross must close immediately and show exactly the next queued notification")
	}
	// Even if an HWND is reused, the previous timer must not close the next card.
	win.SendMessage(a.popup.window, win.WM_TIMER, firstTimer, 0)
	if a.popup.content != second {
		t.Fatal("stale timer dismissed the new notification")
	}
	// Exercise the normal expiry/fade path without waiting three seconds.
	win.SendMessage(a.popup.window, win.WM_TIMER, a.popup.timer, 0)
	if a.popup.window == 0 || a.popup.content != third || len(a.notifications) != 0 {
		t.Fatal("expiry must continue the queue after a manually dismissed popup")
	}
	win.SendMessage(a.popup.window, win.WM_CLOSE, 0, 0)
	if a.popup.window != 0 || a.popup.content != (notification{}) || a.popup.close != (notificationCloseState{}) {
		t.Fatal("close did not release popup state")
	}
	a.dismissNotification(false) // A second dismissal must be harmless.
	if win.GetForegroundWindow() != foreground {
		t.Fatal("showing or dismissing the popup changed foreground focus")
	}
	var message win.MSG
	if win.PeekMessage(&message, 0, win.WM_QUIT, win.WM_QUIT, win.PM_NOREMOVE) {
		t.Fatal("closing a notification must not quit the tray message loop")
	}
}