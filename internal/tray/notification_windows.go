//go:build windows

package tray

import (
	"fmt"
	"strings"
	"time"
	"unsafe"

	"github.com/lxn/win"
)

const (
	popupClassName   = appName + "-notification"
	popupExStyle     = win.WS_EX_NOACTIVATE | win.WS_EX_TOOLWINDOW | win.WS_EX_TOPMOST
	popupStyle       = win.WS_POPUP
	popupShowCommand = win.SW_SHOWNOACTIVATE
	popupHideFlags   = win.AW_HIDE | win.AW_BLEND // Never AW_ACTIVATE.
	popupDuration    = 3000
	popupFadeTime    = 160
	// MA_NOACTIVATE accepts the close-button click without taking focus.
	popupMouseActivate = 3
)

type notification struct {
	title   string
	message string
	isError bool
	elapsed time.Duration
	timed   bool
}

type notificationPopup struct {
	window     win.HWND
	timer      uintptr
	registered bool
	content    notification
	close      notificationCloseState
}

func operationNotification(name, completed string, elapsed time.Duration, err error) notification {
	n := notification{
		title: "Tunnel " + completed, message: name + " completed",
		elapsed: max(0, elapsed), timed: true,
	}
	if err != nil {
		n.title, n.message, n.isError = name+" failed", err.Error(), true
	}
	return n
}

func notificationDuration(elapsed time.Duration) string {
	elapsed = max(0, elapsed).Round(time.Millisecond)
	if elapsed < time.Second {
		return fmt.Sprintf("%d ms", elapsed/time.Millisecond)
	}
	return fmt.Sprintf("%.2f s", elapsed.Seconds())
}

// No mouse capture or activation is needed. Leaving the button cancels a press.
type notificationCloseState struct {
	hovered bool
	pressed bool
}

func (s *notificationCloseState) update(message uint32, hit bool) (redraw, dismiss bool) {
	before := *s
	switch message {
	case win.WM_MOUSEMOVE:
		s.hovered = hit
		if !hit {
			s.pressed = false
		}
	case win.WM_LBUTTONDOWN:
		s.hovered, s.pressed = hit, hit
	case win.WM_LBUTTONUP:
		dismiss = s.pressed && hit
		s.hovered, s.pressed = hit, false
	case win.WM_MOUSELEAVE, win.WM_CANCELMODE, win.WM_CAPTURECHANGED:
		*s = notificationCloseState{}
	}
	return before != *s, dismiss
}

func notificationText(text string, limit int) string {
	// Embedded NULs would otherwise truncate Win32 text (or panic in StringToUTF16).
	runes := []rune(strings.TrimSpace(strings.ReplaceAll(text, "\x00", "\ufffd")))
	if len(runes) > limit {
		runes = append(runes[:limit-1], '…')
	}
	return string(runes)
}

func (a *app) showNotification(title, message string, isError bool) {
	a.queueNotification(notification{title: title, message: message, isError: isError})
}

func (a *app) queueNotification(n notification) {
	n.title = notificationText(n.title, 96)
	n.message = notificationText(n.message, 512)
	a.enqueueNotification(n, func() bool {
		return a.window != 0 && win.PostMessage(a.window, notificationMsg, 0, 0) != 0
	})
}

// Only Go values cross the worker/UI boundary, never pointers in PostMessage.
// wake runs under mu so shutdown cannot destroy the target HWND during a post.
func (a *app) enqueueNotification(n notification, wake func() bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closing {
		return false
	}
	a.notifications = append(a.notifications, n)
	if !wake() {
		a.notifications[len(a.notifications)-1] = notification{}
		a.notifications = a.notifications[:len(a.notifications)-1]
		return false
	}
	return true
}

func (a *app) nextNotification() (notification, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closing || len(a.notifications) == 0 {
		return notification{}, false
	}
	n := a.notifications[0]
	a.notifications[0] = notification{}
	a.notifications = a.notifications[1:]
	if len(a.notifications) == 0 {
		a.notifications = nil
	}
	return n, true
}

func (a *app) stopNotifications() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closing = true
	a.notifications = nil
}

// All methods below run exclusively on the locked message-loop thread.
func (a *app) showNextNotification() {
	if a.popup.window != 0 {
		return
	}
	for {
		n, ok := a.nextNotification()
		if !ok {
			return
		}
		if a.popup.show(n) {
			return
		}
		// A failed Win32 call must not leave a stuck popup or block the queue.
	}
}

func (a *app) dismissNotification(fade bool) {
	p := &a.popup
	if p.window == 0 {
		return
	}
	win.KillTimer(p.window, p.timer)
	if fade {
		// Brief, synchronous hide on this thread; no activation flag.
		win.AnimateWindow(p.window, popupFadeTime, popupHideFlags)
	}
	p.destroy()
	a.showNextNotification()
}

// Anchor to the primary monitor's work area, not its full screen rectangle:
// taskbars on any edge and negative desktop coordinates are respected.
func notificationBounds(work win.RECT) win.RECT {
	w, h := work.Right-work.Left, work.Bottom-work.Top
	if w <= 0 || h <= 0 {
		return win.RECT{}
	}
	mx, my := min(int32(16), (w-1)/2), min(int32(16), (h-1)/2)
	width, height := min(int32(340), w-2*mx), min(int32(112), h-2*my)
	right, bottom := work.Right-mx, work.Bottom-my
	return win.RECT{Left: right - width, Top: bottom - height, Right: right, Bottom: bottom}
}

func (p *notificationPopup) show(n notification) bool {
	info := win.MONITORINFO{CbSize: uint32(unsafe.Sizeof(win.MONITORINFO{}))}
	monitor := win.MonitorFromWindow(0, win.MONITOR_DEFAULTTOPRIMARY)
	if !win.GetMonitorInfo(monitor, &info) {
		return false
	}
	bounds := notificationBounds(info.RcWork)
	if bounds.Right <= bounds.Left || bounds.Bottom <= bounds.Top {
		return false
	}
	if !p.registered {
		class := win.WNDCLASSEX{
			CbSize:        uint32(unsafe.Sizeof(win.WNDCLASSEX{})),
			Style:         win.CS_DROPSHADOW,
			LpfnWndProc:   syscallCallback(popupWindowProc),
			HInstance:     win.GetModuleHandle(nil),
			HCursor:       win.LoadCursor(0, win.MAKEINTRESOURCE(win.IDC_ARROW)),
			LpszClassName: stringPtr(popupClassName),
		}
		if win.RegisterClassEx(&class) == 0 {
			// Older systems may reject the optional class shadow.
			class.Style = 0
			if win.RegisterClassEx(&class) == 0 {
				return false
			}
		}
		p.registered = true
	}
	p.content = n
	p.close = notificationCloseState{}
	p.window = win.CreateWindowEx(popupExStyle, stringPtr(popupClassName), nil,
		popupStyle, bounds.Left, bounds.Top, bounds.Right-bounds.Left, bounds.Bottom-bounds.Top,
		0, 0, win.GetModuleHandle(nil), nil)
	if p.window == 0 {
		p.content = notification{}
		return false
	}
	roundNotificationWindow(p.window, bounds.Right-bounds.Left, bounds.Bottom-bounds.Top)
	if !win.SetWindowPos(p.window, win.HWND_TOPMOST, bounds.Left, bounds.Top,
		bounds.Right-bounds.Left, bounds.Bottom-bounds.Top, win.SWP_NOACTIVATE) {
		p.destroy()
		return false
	}
	// A new ID for each popup also rejects stale WM_TIMER messages.
	p.timer++
	if p.timer == 0 {
		p.timer++
	}
	if win.SetTimer(p.window, p.timer, popupDuration, 0) == 0 {
		p.destroy()
		return false
	}
	win.ShowWindow(p.window, popupShowCommand)
	return true
}

func setTrayArrowCursor() {
	// Shared system cursor: no allocation or DestroyCursor is required.
	win.SetCursor(win.LoadCursor(0, win.MAKEINTRESOURCE(win.IDC_ARROW)))
}

func popupWindowProc(hwnd win.HWND, msg uint32, wparam, lparam uintptr) uintptr {
	if msg == win.WM_SETCURSOR {
		setTrayArrowCursor()
		return 1
	}
	if msg == win.WM_MOUSEACTIVATE {
		// Deliver client clicks (for the cross) without activating the popup.
		return popupMouseActivate
	}
	if currentApp != nil {
		p := &currentApp.popup
		if hwnd == p.window {
			switch msg {
			case win.WM_ERASEBKGND:
				// The buffered renderer covers the entire client area.
				return 1
			case win.WM_PAINT:
				p.paint()
				return 0
			case win.WM_PRINTCLIENT:
				// AnimateWindow asks the window to render into its animation DC.
				p.draw(win.HDC(wparam))
				return 0
			case win.WM_MOUSEMOVE, win.WM_LBUTTONDOWN, win.WM_LBUTTONUP,
				win.WM_MOUSELEAVE, win.WM_CANCELMODE, win.WM_CAPTURECHANGED:
				if msg == win.WM_MOUSEMOVE {
					setTrayArrowCursor()
					track := win.TRACKMOUSEEVENT{
						CbSize:  uint32(unsafe.Sizeof(win.TRACKMOUSEEVENT{})),
						DwFlags: win.TME_LEAVE, HwndTrack: hwnd,
					}
					win.TrackMouseEvent(&track)
				}
				var rect win.RECT
				hit := false
				if win.GetClientRect(hwnd, &rect) {
					point := win.POINT{X: int32(int16(lparam & 0xffff)), Y: int32(int16((lparam >> 16) & 0xffff))}
					hit = notificationCloseHit(notificationLayout(rect.Right, rect.Bottom), point)
				}
				redraw, dismiss := p.close.update(msg, hit)
				if dismiss {
					currentApp.dismissNotification(false)
				} else if redraw {
					win.InvalidateRect(hwnd, nil, false)
				}
				return 0
			case win.WM_TIMER:
				if wparam == p.timer {
					currentApp.dismissNotification(true)
				}
				return 0
			case win.WM_CLOSE:
				currentApp.dismissNotification(false)
				return 0
			}
		}
	}
	// In particular, popup WM_DESTROY must never post WM_QUIT.
	return win.DefWindowProc(hwnd, msg, wparam, lparam)
}

func (p *notificationPopup) paint() {
	var ps win.PAINTSTRUCT
	dc := win.BeginPaint(p.window, &ps)
	if dc == 0 {
		return
	}
	defer win.EndPaint(p.window, &ps)
	p.draw(dc)
}

func (p *notificationPopup) draw(dc win.HDC) {
	if dc == 0 {
		return
	}
	var rect win.RECT
	if !win.GetClientRect(p.window, &rect) || rect.Right <= 0 || rect.Bottom <= 0 {
		return
	}
	// Also used for WM_PRINTCLIENT during AnimateWindow. Fall back to direct
	// painting if a compatible DC/bitmap cannot be allocated.
	buffer := win.CreateCompatibleDC(dc)
	if buffer != 0 {
		defer win.DeleteDC(buffer)
		bitmap := win.CreateCompatibleBitmap(dc, rect.Right, rect.Bottom)
		if bitmap != 0 {
			defer win.DeleteObject(win.HGDIOBJ(bitmap))
			old := win.SelectObject(buffer, win.HGDIOBJ(bitmap))
			if validNotificationObject(old) {
				drawNotificationCard(buffer, rect, p.content, p.close)
				copied := win.BitBlt(dc, 0, 0, rect.Right, rect.Bottom, buffer, 0, 0, win.SRCCOPY)
				win.SelectObject(buffer, old)
				if copied {
					return
				}
			}
		}
	}
	drawNotificationCard(dc, rect, p.content, p.close)
}

func (p *notificationPopup) destroy() {
	if p.window != 0 {
		hwnd := p.window
		p.window = 0
		win.KillTimer(hwnd, p.timer)
		win.DestroyWindow(hwnd)
	}
	p.content = notification{}
	p.close = notificationCloseState{}
}

func (p *notificationPopup) cleanup() {
	p.destroy()
	if p.registered {
		win.UnregisterClass(stringPtr(popupClassName))
		p.registered = false
	}
}