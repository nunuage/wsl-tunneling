//go:build windows

package tray

import (
	"strings"
	"unsafe"

	"github.com/lxn/win"
)

const (
	menuWidth      = 344
	menuHeight     = 332
	menuRows       = 5
	menuLabelFlags = win.DT_SINGLELINE | win.DT_VCENTER | win.DT_END_ELLIPSIS | win.DT_NOPREFIX
	menuBadgeFlags = menuLabelFlags | win.DT_CENTER
)

type menuItem struct {
	id      uint32
	label   string
	enabled bool
	checked bool
}

// Value-only snapshot: painting and input never hold the app mutex or read
// mutable worker fields. No network, service or filesystem calls are made here.
type menuSnapshot struct {
	status, badge string
	isError       bool
	items         [menuRows]menuItem
}

func (a *app) menuSnapshot() menuSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	busy := a.busy.Load()
	s := menuSnapshot{
		status:  notificationText(strings.TrimPrefix(a.statusText, "Status: "), 160),
		badge:   "STOPPED",
		isError: a.statusError != "" || a.bootError != "",
		items: [menuRows]menuItem{
			{menuStart, "Start", a.startEnabled && !busy, false},
			{menuStop, "Stop", a.stopEnabled && !busy, false},
			{menuConfig, "Open config folder", a.configEnabled, false},
			{menuBoot, "Start on boot", a.bootEnabled && !busy, a.bootChecked},
			{menuQuit, "Quit", true, false},
		},
	}
	switch {
	case busy:
		s.badge, s.isError = "BUSY", false
	case s.isError:
		s.badge = "ERROR"
	case a.running:
		s.badge = "RUNNING"
	case a.statusLabel == "":
		s.badge = "CHECKING"
	}
	if a.closing {
		for i := range s.items {
			s.items[i].enabled = false
		}
	}
	return s
}

// Position above/left of a bottom-right tray icon, flip if needed, then clamp
// to the cursor monitor's work area (including negative desktop coordinates).
func menuBounds(work win.RECT, cursor win.POINT) win.RECT {
	w, h := work.Right-work.Left, work.Bottom-work.Top
	if w <= 0 || h <= 0 {
		return win.RECT{}
	}
	w, h = min(int32(menuWidth), w), min(int32(menuHeight), h)
	x, y := cursor.X-w, cursor.Y-h
	if x < work.Left {
		x = cursor.X
	}
	if y < work.Top {
		y = cursor.Y
	}
	x = max(work.Left, min(x, work.Right-w))
	y = max(work.Top, min(y, work.Bottom-h))
	return win.RECT{Left: x, Top: y, Right: x + w, Bottom: y + h}
}

type menuCardLayout struct {
	brand, title, status, badge win.RECT
	rows                        [menuRows]win.RECT
}

func menuLayout(width, height int32) menuCardLayout {
	w, h := max(int32(0), width), max(int32(0), height)
	box := func(l, t, r, b int32) win.RECT {
		l, r = min(max(0, l), w), min(max(0, r), w)
		t, b = min(max(0, t*h/menuHeight), h), min(max(0, b*h/menuHeight), h)
		return win.RECT{Left: l, Top: t, Right: max(l, r), Bottom: max(t, b)}
	}
	l := menuCardLayout{
		brand: box(20, 19, w-112, 35), badge: box(w-100, 16, w-20, 39),
		title: box(20, 46, w-20, 71), status: box(20, 77, w-20, 96),
	}
	y := int32(104)
	for i := range l.rows {
		l.rows[i] = box(min(12, w/2), y, w-min(12, w/2), y+40)
		y += 40
		if i == 1 || i == 3 {
			y += 8
		}
	}
	return l
}

func menuContains(r win.RECT, p win.POINT) bool {
	return p.X >= r.Left && p.X < r.Right && p.Y >= r.Top && p.Y < r.Bottom
}

func menuHit(layout menuCardLayout, s menuSnapshot, pt win.POINT) int {
	for i, r := range layout.rows {
		if s.items[i].enabled && menuContains(r, pt) {
			return i
		}
	}
	return -1
}

func menuSelection(s menuSnapshot, current, direction int) int {
	for n := 0; n < menuRows; n++ {
		current = (current + direction + menuRows) % menuRows
		if s.items[current].enabled {
			return current
		}
	}
	return -1
}

// Button identity and row must match; neither dragging into a row nor a stale
// enabled state can execute a command. Also used by keyboard activation.
func menuCommand(s menuSnapshot, pressed, released int, down, up uint32) uint32 {
	if pressed < 0 || pressed >= menuRows || pressed != released || down == 0 || down != up || !s.items[pressed].enabled {
		return 0
	}
	return s.items[pressed].id
}

func menuFill(dc win.HDC, r win.RECT, color win.COLORREF) {
	if r.Right <= r.Left || r.Bottom <= r.Top {
		return
	}
	brush := notificationBrush(color)
	if !validNotificationObject(brush) {
		return
	}
	defer win.DeleteObject(brush)
	menuFillRect.Call(uintptr(dc), uintptr(unsafe.Pointer(&r)), uintptr(brush))
}

func menuPill(dc win.HDC, r win.RECT, color win.COLORREF) {
	if r.Right <= r.Left || r.Bottom <= r.Top {
		return
	}
	withNotificationObject(dc, notificationBrush(color), func() {
		old := win.SelectObject(dc, win.GetStockObject(win.NULL_PEN))
		defer win.SelectObject(dc, old)
		win.RoundRect(dc, r.Left, r.Top, r.Right, r.Bottom, r.Bottom-r.Top, r.Bottom-r.Top)
	})
}

// All switch geometry is derived from the row, never from font glyph metrics.
// Equal inset on every side keeps the circular thumb centred in either state.
func menuToggleBounds(row win.RECT, checked bool) (track, thumb win.RECT) {
	w, h := max(int32(0), row.Right-row.Left), max(int32(0), row.Bottom-row.Top)
	padding := min(int32(14), w/2)
	width, height := min(int32(36), w-2*padding), min(int32(20), h)
	right := row.Right - padding
	top := row.Top + (h-height)/2
	track = win.RECT{Left: right - width, Top: top, Right: right, Bottom: top + height}
	inset := min(int32(3), min(width, height)/2)
	diameter := min(width, height) - 2*inset
	x := track.Left + inset
	if checked {
		x = track.Right - inset - diameter
	}
	y := track.Top + (height-diameter)/2
	thumb = win.RECT{Left: x, Top: y, Right: x + diameter, Bottom: y + diameter}
	return track, thumb
}

func drawMenuToggle(dc win.HDC, row win.RECT, checked, enabled bool, theme notificationPalette) {
	track, thumb := menuToggleBounds(row, checked)
	trackColor, thumbColor := theme.border, theme.body
	if checked {
		trackColor, thumbColor = theme.disk, theme.accent
	}
	if !enabled {
		trackColor, thumbColor = theme.border, theme.brand
	}
	menuPill(dc, track, trackColor)
	withNotificationObject(dc, notificationBrush(thumbColor), func() {
		old := win.SelectObject(dc, win.GetStockObject(win.NULL_PEN))
		defer win.SelectObject(dc, old)
		win.Ellipse(dc, thumb.Left, thumb.Top, thumb.Right, thumb.Bottom)
	})
}

func drawMenuCard(dc win.HDC, rect win.RECT, s menuSnapshot, selected int) {
	saved := win.SaveDC(dc)
	if saved == 0 {
		return
	}
	defer win.RestoreDC(dc, saved)
	theme := notificationTheme(s.isError)
	l := menuLayout(rect.Right, rect.Bottom)
	menuFill(dc, rect, theme.background)
	withNotificationObject(dc, notificationBrush(theme.background), func() {
		withNotificationObject(dc, notificationPen(theme.border, 1), func() {
			win.RoundRect(dc, 0, 0, rect.Right, rect.Bottom, notificationCornerDiameter, notificationCornerDiameter)
		})
	})
	win.SetBkMode(dc, win.TRANSPARENT)
	drawNotificationText(dc, appName, l.brand, theme.brand, 11, win.FW_NORMAL, false)
	drawNotificationText(dc, "Tunnel controls", l.title, theme.title, 19, win.FW_SEMIBOLD, false)
	drawNotificationText(dc, s.status, l.status, theme.body, 13, win.FW_NORMAL, false)
	badgeColor, badgeText := theme.disk, theme.accent
	if s.badge != "RUNNING" && s.badge != "ERROR" {
		badgeColor, badgeText = theme.border, theme.body
	}
	menuPill(dc, l.badge, badgeColor)
	drawNotificationTextAligned(dc, s.badge, l.badge, badgeText, 10, win.FW_SEMIBOLD, menuBadgeFlags)
	for i, row := range l.rows {
		item := s.items[i]
		if i == selected && item.enabled {
			menuFill(dc, row, theme.border)
			marker := row
			marker.Right = min(row.Right, row.Left+3)
			menuFill(dc, marker, theme.accent)
		}
		color := theme.title
		if !item.enabled {
			color = theme.brand
		}
		text := row
		text.Left = min(row.Right, row.Left+14)
		if item.id == menuBoot {
			track, _ := menuToggleBounds(row, item.checked)
			text.Right = max(text.Left, track.Left-12)
			drawMenuToggle(dc, row, item.checked, item.enabled, theme)
		}
		drawNotificationTextAligned(dc, item.label, text, color, 14, win.FW_NORMAL, menuLabelFlags)
		if i == 1 || i == 3 {
			line := row
			line.Top = min(rect.Bottom, row.Bottom+4)
			line.Bottom = min(rect.Bottom, line.Top+1)
			menuFill(dc, line, theme.border)
		}
	}
}
