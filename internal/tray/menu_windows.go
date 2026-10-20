//go:build windows

package tray

import (
	"syscall"
	"time"
	"unsafe"

	"github.com/lxn/win"
)

const (
	menuClassName = appName + "-controls"
	// Unlike notifications this interactive window intentionally takes focus.
	menuExStyle = win.WS_EX_TOOLWINDOW | win.WS_EX_TOPMOST
)

var (
	menuMonitorFromRect = notificationUser.NewProc("MonitorFromRect")
	menuGetCapture      = notificationUser.NewProc("GetCapture")
	menuFillRect        = notificationUser.NewProc("FillRect")
	menuIconRect        = syscall.NewLazyDLL("shell32.dll").NewProc("Shell_NotifyIconGetRect")
)

type trayMenu struct {
	window           win.HWND
	registered       bool
	content          menuSnapshot
	layout           menuCardLayout
	selected         int
	pressed          int
	button           uint32
	dismissOnRelease bool
	suppressTray     time.Time
}

func (a *app) toggleMenu() {
	p := &a.menu
	if p.window != 0 {
		p.destroy()
		return
	}
	// If the shell stole activation/capture before the captured UP, it may
	// still send a tray callback. Do not immediately reopen the dismissed panel.
	if !p.suppressTray.IsZero() {
		until := p.suppressTray
		p.suppressTray = time.Time{}
		if time.Now().Before(until) {
			return
		}
	}
	s := a.menuSnapshot()
	a.mu.Lock()
	closing := a.closing
	a.mu.Unlock()
	if closing {
		return
	}
	var cursor win.POINT
	if !win.GetCursorPos(&cursor) {
		return
	}
	anchor := win.RECT{Left: cursor.X, Top: cursor.Y, Right: cursor.X + 1, Bottom: cursor.Y + 1}
	monitor, _, _ := menuMonitorFromRect.Call(uintptr(unsafe.Pointer(&anchor)), win.MONITOR_DEFAULTTONEAREST)
	info := win.MONITORINFO{CbSize: uint32(unsafe.Sizeof(win.MONITORINFO{}))}
	if monitor == 0 || !win.GetMonitorInfo(win.HMONITOR(monitor), &info) {
		return
	}
	p.show(a.window, menuBounds(info.RcWork, cursor), s)
}

func (a *app) refreshMenu() {
	p := &a.menu
	if p.window == 0 {
		return
	}
	p.content = a.menuSnapshot()
	if p.selected >= 0 && !p.content.items[p.selected].enabled {
		p.selected = menuSelection(p.content, p.selected, 1)
	}
	if p.pressed >= 0 && !p.content.items[p.pressed].enabled {
		p.pressed, p.button = -1, 0
	}
	win.InvalidateRect(p.window, nil, false)
}

func (p *trayMenu) show(owner win.HWND, bounds win.RECT, content menuSnapshot) bool {
	w, h := bounds.Right-bounds.Left, bounds.Bottom-bounds.Top
	if w <= 0 || h <= 0 {
		return false
	}
	if !p.registered {
		class := win.WNDCLASSEX{
			CbSize: uint32(unsafe.Sizeof(win.WNDCLASSEX{})), Style: win.CS_DROPSHADOW,
			LpfnWndProc: syscallCallback(menuWindowProc), HInstance: win.GetModuleHandle(nil),
			HCursor: win.LoadCursor(0, win.MAKEINTRESOURCE(win.IDC_ARROW)), LpszClassName: stringPtr(menuClassName),
		}
		if win.RegisterClassEx(&class) == 0 {
			class.Style = 0 // Optional shadow may be unavailable on older systems.
			if win.RegisterClassEx(&class) == 0 {
				return false
			}
		}
		p.registered = true
	}
	p.content, p.layout = content, menuLayout(w, h)
	p.selected, p.pressed, p.button = menuSelection(content, -1, 1), -1, 0
	hwnd := win.CreateWindowEx(menuExStyle, stringPtr(menuClassName), stringPtr(appName), win.WS_POPUP,
		bounds.Left, bounds.Top, w, h, owner, 0, win.GetModuleHandle(nil), nil)
	if hwnd == 0 {
		p.content = menuSnapshot{}
		return false
	}
	p.window = hwnd
	roundNotificationWindow(hwnd, w, h)
	// Every activation call can synchronously dispatch WM_ACTIVATE/WM_CLOSE.
	// Never use a cached HWND after such a call without checking ownership.
	win.ShowWindow(hwnd, win.SW_SHOW)
	if p.window != hwnd {
		return false
	}
	foreground := win.SetForegroundWindow(hwnd)
	if p.window != hwnd {
		return false
	}
	if !foreground {
		p.destroy()
		return false
	}
	win.SetFocus(hwnd)
	if p.window != hwnd {
		return false
	}
	win.SetCapture(hwnd)
	if p.window != hwnd {
		return false
	}
	capture, _, _ := menuGetCapture.Call()
	if win.HWND(capture) != hwnd {
		p.destroy()
		return false
	}
	win.InvalidateRect(hwnd, nil, false)
	setTrayArrowCursor()
	return true
}

// NIF GUID is zero because the tray uses UID, not NIF_GUID. This mirrors the
// native NOTIFYICONIDENTIFIER layout on both 32-bit and 64-bit Windows.
type menuIconIdentifier struct {
	size uint32
	hwnd win.HWND
	id   uint32
	guid [16]byte
}

func (a *app) cursorOnTray() bool {
	var pt win.POINT
	if !win.GetCursorPos(&pt) || menuIconRect.Find() != nil {
		return false
	}
	id := menuIconIdentifier{hwnd: a.window, id: trayIconID}
	id.size = uint32(unsafe.Sizeof(id))
	var rect win.RECT
	hr, _, _ := menuIconRect.Call(uintptr(unsafe.Pointer(&id)), uintptr(unsafe.Pointer(&rect)))
	return int32(hr) >= 0 && menuContains(rect, pt)
}

func (a *app) suppressMenuTrayClick() {
	if a.cursorOnTray() {
		a.menu.suppressTray = time.Now().Add(750 * time.Millisecond)
	}
}

func (a *app) dismissMenuForFocusLoss() {
	if win.GetKeyState(win.VK_LBUTTON) < 0 || win.GetKeyState(win.VK_RBUTTON) < 0 {
		a.suppressMenuTrayClick()
	}
	a.menu.destroy()
}

func menuPoint(lparam uintptr) win.POINT {
	// Mouse coordinates are SIGNED, especially with capture outside the client.
	return win.POINT{X: int32(int16(lparam & 0xffff)), Y: int32(int16((lparam >> 16) & 0xffff))}
}

func menuWindowProc(hwnd win.HWND, msg uint32, wparam, lparam uintptr) uintptr {
	a := currentApp
	if a == nil || a.menu.window != hwnd || hwnd == 0 {
		return win.DefWindowProc(hwnd, msg, wparam, lparam)
	}
	p := &a.menu
	switch msg {
	case win.WM_SETCURSOR:
		setTrayArrowCursor()
		return 1
	case win.WM_ERASEBKGND:
		return 1
	case win.WM_PAINT:
		p.paint(hwnd)
		return 0
	case win.WM_PRINTCLIENT:
		p.draw(win.HDC(wparam))
		return 0
	case win.WM_MOUSEMOVE:
		// With mouse capture Windows need not send WM_SETCURSOR. Explicitly
		// replace any cursor inherited from the shell, including APPSTARTING.
		setTrayArrowCursor()
		if p.dismissOnRelease {
			return 0
		}
		selected := menuHit(p.layout, p.content, menuPoint(lparam))
		if selected != p.selected {
			p.selected = selected
			win.InvalidateRect(hwnd, nil, false)
		}
		return 0
	case win.WM_LBUTTONDOWN, win.WM_RBUTTONDOWN, win.WM_MBUTTONDOWN, win.WM_XBUTTONDOWN:
		pt := menuPoint(lparam)
		var client win.RECT
		win.GetClientRect(hwnd, &client)
		if !menuContains(client, pt) {
			if a.cursorOnTray() {
				// Keep capture through UP on the icon itself, consuming the
				// complete toggle click even if the user holds the button.
				p.dismissOnRelease = true
				p.selected, p.pressed, p.button = -1, -1, 0
				win.InvalidateRect(hwnd, nil, false)
				return 0
			}
			p.destroy()
			return 0
		}
		if msg != win.WM_LBUTTONDOWN && msg != win.WM_RBUTTONDOWN {
			p.pressed, p.button = -1, 0
			return 0
		}
		a.refreshMenu()
		p.pressed = menuHit(p.layout, p.content, pt)
		p.selected, p.button = p.pressed, msg
		win.InvalidateRect(hwnd, nil, false)
		return 0
	case win.WM_LBUTTONUP, win.WM_RBUTTONUP:
		if p.dismissOnRelease {
			p.destroy()
			return 0
		}
		a.refreshMenu() // Revalidate against a fresh, read-only worker snapshot.
		button := uint32(win.WM_LBUTTONDOWN)
		if msg == win.WM_RBUTTONUP {
			button = win.WM_RBUTTONDOWN
		}
		id := menuCommand(p.content, p.pressed, menuHit(p.layout, p.content, menuPoint(lparam)), p.button, button)
		p.pressed, p.button = -1, 0
		a.executeMenuCommand(id)
		return 0
	case win.WM_MBUTTONUP, win.WM_XBUTTONUP:
		if p.dismissOnRelease {
			p.destroy()
		}
		return 0
	case win.WM_KEYDOWN:
		if p.dismissOnRelease {
			if wparam == win.VK_ESCAPE {
				p.destroy()
			}
			return 0
		}
		a.refreshMenu()
		switch wparam {
		case win.VK_ESCAPE:
			p.destroy()
		case win.VK_UP:
			start := p.selected
			if start < 0 {
				start = 0
			}
			p.selected = menuSelection(p.content, start, -1)
		case win.VK_DOWN, win.VK_TAB:
			direction := 1
			if wparam == win.VK_TAB && win.GetKeyState(win.VK_SHIFT) < 0 {
				direction = -1
			}
			start := p.selected
			if start < 0 && direction < 0 {
				start = 0
			}
			p.selected = menuSelection(p.content, start, direction)
		case win.VK_HOME:
			p.selected = menuSelection(p.content, -1, 1)
		case win.VK_END:
			p.selected = menuSelection(p.content, 0, -1)
		case win.VK_RETURN, win.VK_SPACE:
			id := menuCommand(p.content, p.selected, p.selected, 1, 1)
			a.executeMenuCommand(id)
		}
		if p.window == hwnd {
			win.InvalidateRect(hwnd, nil, false)
		}
		return 0
	case win.WM_ACTIVATE:
		if wparam&0xffff == win.WA_INACTIVE {
			a.dismissMenuForFocusLoss()
		}
		return 0
	case win.WM_KILLFOCUS, win.WM_CANCELMODE:
		a.dismissMenuForFocusLoss()
		return 0
	case win.WM_CLOSE:
		p.destroy()
		return 0
	case win.WM_CAPTURECHANGED:
		if win.HWND(lparam) != hwnd {
			a.dismissMenuForFocusLoss()
		}
		return 0
	case win.WM_NCDESTROY:
		// Also tolerate destruction initiated by Windows/the owner, not just us.
		p.window = 0
		p.reset()
	}
	return win.DefWindowProc(hwnd, msg, wparam, lparam)
}

func (a *app) executeMenuCommand(id uint32) {
	if id == 0 {
		return
	}
	// Detach, release capture and destroy first. Actions (including Quit and
	// ShellExecute) run later in the owner message loop, outside this callback.
	a.menu.destroy()
	win.PostMessage(a.window, win.WM_COMMAND, uintptr(id), 0)
}

func (p *trayMenu) paint(hwnd win.HWND) {
	var ps win.PAINTSTRUCT
	dc := win.BeginPaint(hwnd, &ps)
	if dc == 0 {
		return
	}
	defer win.EndPaint(hwnd, &ps)
	p.draw(dc)
}

func (p *trayMenu) draw(dc win.HDC) {
	var rect win.RECT
	if dc == 0 || !win.GetClientRect(p.window, &rect) || rect.Right <= 0 || rect.Bottom <= 0 {
		return
	}
	buffer := win.CreateCompatibleDC(dc)
	if buffer != 0 {
		defer win.DeleteDC(buffer)
		bitmap := win.CreateCompatibleBitmap(dc, rect.Right, rect.Bottom)
		if bitmap != 0 {
			defer win.DeleteObject(win.HGDIOBJ(bitmap))
			old := win.SelectObject(buffer, win.HGDIOBJ(bitmap))
			if validNotificationObject(old) {
				drawMenuCard(buffer, rect, p.content, p.selected)
				copied := win.BitBlt(dc, 0, 0, rect.Right, rect.Bottom, buffer, 0, 0, win.SRCCOPY)
				win.SelectObject(buffer, old)
				if copied {
					return
				}
			}
		}
	}
	drawMenuCard(dc, rect, p.content, p.selected)
}

func (p *trayMenu) reset() {
	p.content, p.layout = menuSnapshot{}, menuCardLayout{}
	p.selected, p.pressed, p.button = -1, -1, 0
	p.dismissOnRelease = false
}

func (p *trayMenu) destroy() {
	hwnd := p.window
	if hwnd == 0 {
		return
	}
	// ReleaseCapture synchronously sends WM_CAPTURECHANGED. Detach BEFORE that
	// call, and release BEFORE DestroyWindow, so neither callback can recurse.
	p.window = 0
	p.reset()
	capture, _, _ := menuGetCapture.Call()
	if win.HWND(capture) == hwnd {
		win.ReleaseCapture()
	}
	win.DestroyWindow(hwnd)
}

func (p *trayMenu) cleanup() {
	p.destroy()
	if p.registered {
		win.UnregisterClass(stringPtr(menuClassName))
		p.registered = false
	}
}
