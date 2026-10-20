//go:build windows

package tray

import (
	"image"
	"image/png"
	"os"
	"runtime"
	"syscall"
	"testing"
	"unsafe"

	"github.com/lxn/win"
)

// These tests never create a HWND, install capture, focus the desktop, start
// the tray/tunnel or query services. Only the opt-in preview allocates GDI.
func TestTrayMenuBounds(t *testing.T) {
	for _, tt := range []struct {
		name string
		work win.RECT
		pt   win.POINT
		want win.RECT
	}{
		{"bottom right", win.RECT{Right: 1920, Bottom: 1040}, win.POINT{X: 1900, Y: 1060}, win.RECT{Left: 1556, Top: 732, Right: 1900, Bottom: 1040}},
		{"top left taskbar", win.RECT{Left: 60, Top: 40, Right: 1920, Bottom: 1080}, win.POINT{X: 20, Y: 20}, win.RECT{Left: 60, Top: 40, Right: 404, Bottom: 348}},
		{"negative monitor", win.RECT{Left: -1920, Top: -1080, Right: 0, Bottom: -40}, win.POINT{X: -10, Y: -20}, win.RECT{Left: -354, Top: -348, Right: -10, Bottom: -40}},
		{"small", win.RECT{Left: 10, Top: 20, Right: 110, Bottom: 100}, win.POINT{X: 500, Y: 500}, win.RECT{Left: 10, Top: 20, Right: 110, Bottom: 100}},
		{"one pixel", win.RECT{Left: -1, Top: -1}, win.POINT{}, win.RECT{Left: -1, Top: -1}},
		{"empty", win.RECT{}, win.POINT{}, win.RECT{}},
		{"invalid", win.RECT{Left: 10, Right: 5, Bottom: 10}, win.POINT{}, win.RECT{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := menuBounds(tt.work, tt.pt)
			if got != tt.want {
				t.Fatalf("bounds = %+v, want %+v", got, tt.want)
			}
			if got != (win.RECT{}) && (got.Left < tt.work.Left || got.Top < tt.work.Top || got.Right > tt.work.Right || got.Bottom > tt.work.Bottom) {
				t.Fatal("panel escaped monitor work area")
			}
		})
	}
}

func TestTrayMenuLayoutAndHit(t *testing.T) {
	s := (&app{startEnabled: true, configEnabled: true, bootEnabled: true}).menuSnapshot()
	for _, size := range [][2]int32{{menuWidth, menuHeight}, {200, 100}, {1, 1}, {0, 0}, {-10, -20}} {
		l := menuLayout(size[0], size[1])
		rects := append([]win.RECT{l.brand, l.title, l.badge}, l.rows[:]...)
		for _, r := range rects {
			if r.Left < 0 || r.Top < 0 || r.Right > max(0, size[0]) || r.Bottom > max(0, size[1]) || r.Right < r.Left || r.Bottom < r.Top {
				t.Fatalf("invalid layout for %v: %+v", size, r)
			}
		}
		for i := 1; i < len(l.rows); i++ {
			if l.rows[i].Top < l.rows[i-1].Bottom {
				t.Fatal("rows overlap")
			}
		}
	}
	l := menuLayout(menuWidth, menuHeight)
	if l.rows[0].Top != 80 || l.rows[4].Bottom != 296 || l.title.Bottom > l.rows[0].Top || l.brand.Right > l.badge.Left {
		t.Fatalf("unexpected full-size layout: %+v", l)
	}
	for i, row := range l.rows {
		want := i
		if !s.items[i].enabled {
			want = -1
		}
		if got := menuHit(l, s, win.POINT{X: row.Left, Y: row.Top}); got != want {
			t.Fatalf("hit row %d = %d, want %d", i, got, want)
		}
		if got := menuHit(l, s, win.POINT{X: row.Right, Y: row.Top}); got != -1 {
			t.Fatal("right boundary must be exclusive")
		}
	}
	for _, pt := range []win.POINT{{X: -1, Y: 120}, {X: 30, Y: 30}, {X: 30, Y: 164}, {X: menuWidth, Y: menuHeight}} {
		if menuHit(l, s, pt) != -1 {
			t.Fatalf("header, separator or exterior hit: %+v", pt)
		}
	}
	if got := menuPoint(uintptr(0xfffeffff)); got != (win.POINT{X: -1, Y: -2}) {
		t.Fatalf("capture coordinates not signed: %+v", got)
	}
}

func TestTrayMenuSnapshot(t *testing.T) {
	a := &app{statusText: "Status: running", statusLabel: "running", running: true,
		startEnabled: false, stopEnabled: true, bootEnabled: true, configEnabled: true, bootChecked: true}
	s := a.menuSnapshot()
	if s.badge != "RUNNING" || s.isError || !s.items[3].checked {
		t.Fatalf("running snapshot = %+v", s)
	}
	for i, id := range []uint32{menuStart, menuStop, menuConfig, menuBoot, menuQuit} {
		if s.items[i].id != id {
			t.Fatal("existing command IDs changed")
		}
	}
	a.busy.Store(true)
	busy := a.menuSnapshot()
	if busy.badge != "BUSY" || busy.items[0].enabled || busy.items[1].enabled || busy.items[3].enabled || !busy.items[2].enabled || !busy.items[4].enabled {
		t.Fatalf("busy snapshot = %+v", busy)
	}
	if !a.stopEnabled || !a.bootEnabled || a.statusText != "Status: running" || s != (menuSnapshot{badge: "RUNNING", items: [menuRows]menuItem{
		{menuStart, "Start", false, false}, {menuStop, "Stop", true, false},
		{menuConfig, "Open config folder", true, false}, {menuBoot, "Start on boot", true, true}, {menuQuit, "Quit", true, false},
	}}) {
		t.Fatal("snapshot mutated app fields or previously returned values")
	}
	a.statusError = "failed"
	if got := a.menuSnapshot(); got.badge != "BUSY" || got.isError {
		t.Fatal("busy must take priority over stale errors")
	}
	a.busy.Store(false)
	if got := a.menuSnapshot(); got.badge != "ERROR" || !got.isError {
		t.Fatal("status error must be visible")
	}
	a.statusError, a.bootError = "", "boot failed"
	if got := a.menuSnapshot(); got.badge != "ERROR" || !got.isError {
		t.Fatal("boot error must be visible")
	}
	a.closing = true
	for _, item := range a.menuSnapshot().items {
		if item.enabled {
			t.Fatal("shutdown snapshot must disable all commands")
		}
	}
	initial := (&app{}).menuSnapshot()
	if initial.badge != "CHECKING" || initial.items[0].enabled || initial.items[1].enabled || initial.items[3].enabled {
		t.Fatal("initial state must not enable service commands")
	}
	stopped := (&app{statusLabel: "stopped"}).menuSnapshot()
	if stopped.badge != "STOPPED" {
		t.Fatal("expected stopped badge")
	}
}

func TestTrayMenuStatusOnlyInBadge(t *testing.T) {
	for _, tt := range []struct {
		name string
		app  *app
		want string
	}{
		{"running", &app{running: true, statusLabel: "running", statusText: "Status: running"}, "RUNNING"},
		{"cached", &app{running: true, statusLabel: "running", statusText: "Status: running (cached)"}, "RUNNING"},
		{"stopped", &app{statusLabel: "stopped", statusText: "Status: stopped"}, "STOPPED"},
		{"checking", &app{statusText: "Status: checking..."}, "CHECKING"},
		{"error", &app{statusError: "network failure", statusText: "Status: error"}, "ERROR"},
		{"busy", &app{statusText: "Status: starting..."}, "BUSY"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := tt.app
			a.busy.Store(tt.name == "busy")
			a.tooltip = appName + " - detailed status"
			text, tooltip := a.statusText, a.tooltip
			s := a.menuSnapshot()
			if s.badge != tt.want || a.statusText != text || a.tooltip != tooltip {
				t.Fatalf("badge=%q, want %q without mutating status/tooltip", s.badge, tt.want)
			}
			// Descriptive/cached status text is for the tooltip, not a second
			// status label in the panel's value-only rendering snapshot.
			a.statusText = "Status: another description"
			if got := a.menuSnapshot(); got != s {
				t.Fatal("panel must not carry a duplicate status description")
			}
		})
	}
}

func TestTrayMenuSelection(t *testing.T) {
	s := (&app{startEnabled: true, configEnabled: true, bootEnabled: true}).menuSnapshot()
	for _, tt := range []struct{ current, direction, want int }{
		{-1, 1, 0}, {0, 1, 2}, {2, -1, 0}, {4, 1, 0}, {0, -1, 4}, {3, 1, 4},
	} {
		if got := menuSelection(s, tt.current, tt.direction); got != tt.want {
			t.Fatalf("selection(%d, %d) = %d, want %d", tt.current, tt.direction, got, tt.want)
		}
	}
	if menuSelection(menuSnapshot{}, -1, 1) != -1 || menuSelection(menuSnapshot{}, 0, -1) != -1 {
		t.Fatal("no enabled rows must yield no selection")
	}
	for i := range s.items {
		s.items[i].enabled = i == 2
	}
	if menuSelection(s, 2, 1) != 2 || menuSelection(s, 2, -1) != 2 {
		t.Fatal("single enabled row must wrap to itself")
	}
}

func TestTrayMenuMatchingPressRelease(t *testing.T) {
	s := (&app{startEnabled: true, configEnabled: true}).menuSnapshot()
	for _, tt := range []struct {
		press, release int
		down, up       uint32
		want           uint32
	}{
		{0, 0, win.WM_LBUTTONDOWN, win.WM_LBUTTONDOWN, menuStart},
		{2, 2, win.WM_RBUTTONDOWN, win.WM_RBUTTONDOWN, menuConfig},
		{0, 2, 1, 1, 0}, {-1, 0, 1, 1, 0}, {0, -1, 1, 1, 0},
		{1, 1, 1, 1, 0}, {0, 0, 0, 0, 0}, {0, 0, win.WM_LBUTTONDOWN, win.WM_RBUTTONDOWN, 0},
		{5, 5, 1, 1, 0},
	} {
		if got := menuCommand(s, tt.press, tt.release, tt.down, tt.up); got != tt.want {
			t.Fatalf("command(%+v) = %d", tt, got)
		}
	}
	s.items[0].enabled = false // Worker refresh between DOWN and UP.
	if menuCommand(s, 0, 0, 1, 1) != 0 {
		t.Fatal("disabled-after-press action must not execute")
	}
}

func TestTrayMenuWindowPolicy(t *testing.T) {
	if menuExStyle&(win.WS_EX_TOOLWINDOW|win.WS_EX_TOPMOST) != win.WS_EX_TOOLWINDOW|win.WS_EX_TOPMOST ||
		menuExStyle&(win.WS_EX_NOACTIVATE|win.WS_EX_APPWINDOW) != 0 {
		t.Fatal("interactive panel must be activating, topmost and absent from taskbar")
	}
	if popupExStyle&win.WS_EX_NOACTIVATE == 0 || popupShowCommand != win.SW_SHOWNOACTIVATE {
		t.Fatal("notification activation policy must not change")
	}
}

func TestTrayMenuTextAlignment(t *testing.T) {
	if menuBadgeFlags&win.DT_CENTER == 0 || menuBadgeFlags&win.DT_VCENTER == 0 || menuBadgeFlags&win.DT_SINGLELINE == 0 {
		t.Fatal("badge must use native horizontal and vertical centering")
	}
	if menuLabelFlags&win.DT_VCENTER == 0 || menuLabelFlags&win.DT_SINGLELINE == 0 || menuLabelFlags&win.DT_CENTER != 0 {
		t.Fatal("row labels must be vertically centered and left aligned")
	}
}

func TestTrayMenuRestoresArrowCursor(t *testing.T) {
	// GetCursor reports the calling UI thread's cursor. GetCursorInfo would
	// instead observe whichever desktop application currently owns the pointer.
	// No HWND is created and no pointer movement or activation is required.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	getCursor := syscall.NewLazyDLL("user32.dll").NewProc("GetCursor")
	arrow := win.LoadCursor(0, win.MAKEINTRESOURCE(win.IDC_ARROW))
	wait := win.LoadCursor(0, win.MAKEINTRESOURCE(win.IDC_WAIT))
	if arrow == 0 || wait == 0 {
		t.Fatal("system cursors unavailable")
	}
	previous := win.SetCursor(arrow)
	defer win.SetCursor(previous)
	previousApp := currentApp
	defer func() { currentApp = previousApp }()
	currentApp = &app{menu: trayMenu{window: win.HWND(42), dismissOnRelease: true}}
	for _, message := range []uint32{win.WM_SETCURSOR, win.WM_MOUSEMOVE} {
		win.SetCursor(wait)
		menuWindowProc(currentApp.menu.window, message, 0, 0)
		got, _, _ := getCursor.Call()
		if win.HCURSOR(got) != arrow {
			t.Fatalf("message %#x left a non-arrow cursor: %d", message, got)
		}
	}
	win.SetCursor(wait)
	if popupWindowProc(0, win.WM_SETCURSOR, 0, 0) != 1 {
		t.Fatal("notification cursor message not handled")
	}
	if got, _, _ := getCursor.Call(); win.HCURSOR(got) != arrow {
		t.Fatal("notification hover did not restore arrow")
	}
}

func TestTrayMenuToggleAlignment(t *testing.T) {
	for _, row := range []win.RECT{
		{Left: 12, Top: 248, Right: 332, Bottom: 288},
		{Left: 12, Top: 247, Right: 331, Bottom: 288},
		{Left: 5, Top: 3, Right: 85, Bottom: 20},
		{Left: -10, Top: -15, Right: 20, Bottom: 15},
		{Right: 1, Bottom: 1}, {},
	} {
		offTrack, off := menuToggleBounds(row, false)
		onTrack, on := menuToggleBounds(row, true)
		if offTrack != onTrack {
			t.Fatal("switch track must not move when toggled")
		}
		for _, r := range []win.RECT{offTrack, off, on} {
			if r.Left < row.Left || r.Right > row.Right || r.Top < row.Top || r.Bottom > row.Bottom || r.Right < r.Left || r.Bottom < r.Top {
				t.Fatalf("switch escapes row %+v: %+v", row, r)
			}
		}
		if off.Right-off.Left != off.Bottom-off.Top || on.Right-on.Left != on.Bottom-on.Top {
			t.Fatal("switch thumb must be a circle, not a font glyph or ellipse")
		}
		if off.Left-offTrack.Left != onTrack.Right-on.Right || off.Top != on.Top || off.Bottom != on.Bottom {
			t.Fatal("off/on thumb positions must have identical insets")
		}
		for _, r := range []win.RECT{offTrack, off, on} {
			if delta := (r.Top + r.Bottom) - (row.Top + row.Bottom); delta < -1 || delta > 1 {
				t.Fatalf("switch is not vertically centered in row: %+v, %+v", row, r)
			}
		}
	}
	row := menuLayout(menuWidth, menuHeight).rows[3]
	track, thumb := menuToggleBounds(row, false)
	if track.Right-track.Left != 36 || track.Bottom-track.Top != 20 || thumb.Right-thumb.Left != 14 || row.Right-track.Right != 14 {
		t.Fatalf("unexpected full-size switch geometry: track=%+v thumb=%+v", track, thumb)
	}
}

// Optional real-renderer GDI preview. No HWND is created even when requested.
func TestTrayMenuPreview(t *testing.T) {
	output := os.Getenv("WSL_TUNNELING_MENU_PREVIEW")
	if output == "" {
		t.Skip("offscreen preview not requested")
	}
	dc := win.CreateCompatibleDC(0)
	if dc == 0 {
		t.Fatal("create preview DC")
	}
	defer win.DeleteDC(dc)
	header := win.BITMAPINFOHEADER{
		BiSize: uint32(unsafe.Sizeof(win.BITMAPINFOHEADER{})), BiWidth: menuWidth, BiHeight: -menuHeight,
		BiPlanes: 1, BiBitCount: 32, BiCompression: win.BI_RGB,
	}
	var bits unsafe.Pointer
	bitmap := win.CreateDIBSection(dc, &header, win.DIB_RGB_COLORS, &bits, 0, 0)
	if bitmap == 0 || bits == nil {
		t.Fatal("create preview bitmap")
	}
	defer win.DeleteObject(win.HGDIOBJ(bitmap))
	old := win.SelectObject(dc, win.HGDIOBJ(bitmap))
	if !validNotificationObject(old) {
		t.Fatal("select preview bitmap")
	}
	defer win.SelectObject(dc, old)
	s := (&app{statusText: "Status: running", running: true, stopEnabled: true,
		configEnabled: true, bootEnabled: true, bootChecked: true}).menuSnapshot()
	drawMenuCard(dc, win.RECT{Right: menuWidth, Bottom: menuHeight}, s, 1)
	syscall.NewLazyDLL("gdi32.dll").NewProc("GdiFlush").Call()
	bgra := unsafe.Slice((*byte)(bits), menuWidth*menuHeight*4)
	preview := image.NewRGBA(image.Rect(0, 0, menuWidth, menuHeight))
	for i := 0; i < len(bgra); i += 4 {
		preview.Pix[i], preview.Pix[i+1], preview.Pix[i+2], preview.Pix[i+3] = bgra[i+2], bgra[i+1], bgra[i], 255
	}
	file, err := os.Create(output)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := png.Encode(file, preview); err != nil {
		t.Fatal(err)
	}
}
