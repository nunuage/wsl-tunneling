//go:build windows

package tray

import (
	"strings"
	"syscall"

	"github.com/lxn/win"
)

const notificationCornerDiameter = 20

// These APIs are not exported by the pinned lxn/win version.
var (
	notificationGDI          = syscall.NewLazyDLL("gdi32.dll")
	notificationUser         = syscall.NewLazyDLL("user32.dll")
	notificationCreateBrush  = notificationGDI.NewProc("CreateSolidBrush")
	notificationCreatePen    = notificationGDI.NewProc("CreatePen")
	notificationPatBlt       = notificationGDI.NewProc("PatBlt")
	notificationCreateRegion = notificationGDI.NewProc("CreateRoundRectRgn")
	notificationSetRegion    = notificationUser.NewProc("SetWindowRgn")
)

type notificationPalette struct {
	background, border, brand, title, body, accent, disk win.COLORREF
}

func notificationTheme(isError bool) notificationPalette {
	theme := notificationPalette{
		background: win.RGB(32, 37, 45), // #20252d
		border:     win.RGB(58, 66, 79),
		brand:      win.RGB(150, 162, 180),
		title:      win.RGB(241, 245, 249),
		body:       win.RGB(190, 201, 216),
		accent:     win.RGB(92, 214, 158),
		disk:       win.RGB(37, 65, 57),
	}
	if isError {
		theme.accent = win.RGB(255, 139, 124)
		theme.disk = win.RGB(70, 47, 48)
	}
	return theme
}

// Presentation only: preserve worker payloads and all diagnostic/error text.
func notificationPresentation(n notification) notification {
	if !n.isError {
		switch strings.ToLower(strings.TrimSpace(n.message)) {
		case "start completed":
			n.message = "WSL networking is ready."
		case "stop completed":
			n.message = "Previous network settings restored."
		}
	}
	return n
}

type notificationCardLayout struct {
	brand, title, body, icon, duration, close win.RECT
	showIcon                                  bool
}

func notificationLayout(width, height int32) notificationCardLayout {
	width, height = max(0, width), max(0, height)
	left, right := min(int32(16), width/2), max(int32(0), width-20)
	showIcon := width >= 240 && height >= 100
	if showIcon {
		left = 68
	}
	right = max(left, right)
	box := func(top, bottom int32) win.RECT {
		return win.RECT{Left: left, Top: min(top, height), Right: right, Bottom: min(bottom, height)}
	}
	layout := notificationCardLayout{
		brand: box(14, 29), title: box(32, 55), body: box(60, 100), showIcon: showIcon,
	}
	if showIcon {
		layout.icon = win.RECT{Left: 16, Top: 39, Right: 52, Bottom: 75}
	}
	size := min(int32(24), width, height)
	// Equal top/right insets keep the button balanced in the rounded corner.
	mx, my := min(int32(8), (width-size)/2), min(int32(8), (height-size)/2)
	layout.close = win.RECT{Left: width - mx - size, Top: my, Right: width - mx, Bottom: my + size}
	layout.brand.Right = max(left, min(right, layout.close.Left-8))
	if layout.brand.Right-left >= 180 {
		layout.duration = layout.brand
		layout.duration.Left = layout.duration.Right - 80
		layout.brand.Right = layout.duration.Left - 8
	}
	return layout
}

func notificationCloseHit(layout notificationCardLayout, point win.POINT) bool {
	r := layout.close
	return point.X >= r.Left && point.X < r.Right && point.Y >= r.Top && point.Y < r.Bottom
}

func notificationFont(height, weight int32) win.LOGFONT {
	font := win.LOGFONT{
		LfHeight: -height, LfWeight: weight,
		LfCharSet: win.DEFAULT_CHARSET, LfQuality: win.CLEARTYPE_QUALITY,
	}
	copy(font.LfFaceName[:], syscall.StringToUTF16("Segoe UI"))
	return font
}

func roundNotificationWindow(hwnd win.HWND, width, height int32) {
	if notificationCreateRegion.Find() != nil || notificationSetRegion.Find() != nil {
		return // Rectangular fallback; the card remains fully painted.
	}
	region, _, _ := notificationCreateRegion.Call(0, 0, uintptr(width+1), uintptr(height+1),
		notificationCornerDiameter, notificationCornerDiameter)
	if region == 0 {
		return
	}
	result, _, _ := notificationSetRegion.Call(uintptr(hwnd), region, 0)
	if result == 0 {
		win.DeleteObject(win.HGDIOBJ(region))
	}
	// On success the system owns the region, including its eventual deletion.
}

func validNotificationObject(object win.HGDIOBJ) bool {
	return object != 0 && uintptr(object) != ^uintptr(0)
}

// Restore the previous object before deleting ours, including on paint failures.
func withNotificationObject(dc win.HDC, object win.HGDIOBJ, draw func()) {
	if !validNotificationObject(object) {
		return
	}
	defer win.DeleteObject(object)
	old := win.SelectObject(dc, object)
	if !validNotificationObject(old) {
		return
	}
	defer win.SelectObject(dc, old)
	draw()
}

func notificationBrush(color win.COLORREF) win.HGDIOBJ {
	if notificationCreateBrush.Find() != nil {
		return 0
	}
	brush, _, _ := notificationCreateBrush.Call(uintptr(color))
	return win.HGDIOBJ(brush)
}

func notificationPen(color win.COLORREF, width int32) win.HGDIOBJ {
	if notificationCreatePen.Find() != nil {
		return 0
	}
	pen, _, _ := notificationCreatePen.Call(win.PS_SOLID, uintptr(width), uintptr(color))
	return win.HGDIOBJ(pen)
}

func drawNotificationCard(dc win.HDC, rect win.RECT, content notification, close notificationCloseState) {
	// Save/restore also protects the animation DC's text settings/current point.
	saved := win.SaveDC(dc)
	if saved == 0 {
		return
	}
	defer win.RestoreDC(dc, saved)
	theme := notificationTheme(content.isError)
	layout := notificationLayout(rect.Right, rect.Bottom)
	content = notificationPresentation(content)
	withNotificationObject(dc, notificationBrush(theme.background), func() {
		old := win.SelectObject(dc, win.GetStockObject(win.NULL_PEN))
		defer win.SelectObject(dc, old)
		win.Rectangle_(dc, 0, 0, rect.Right+1, rect.Bottom+1)
		withNotificationObject(dc, notificationPen(theme.border, 1), func() {
			win.RoundRect(dc, 0, 0, rect.Right, rect.Bottom,
				notificationCornerDiameter, notificationCornerDiameter)
		})
	})
	if layout.showIcon {
		drawNotificationIcon(dc, layout.icon, theme, content.isError)
	}
	win.SetBkMode(dc, win.TRANSPARENT)
	drawNotificationText(dc, "wsl-tunneling", layout.brand, theme.brand, 11, win.FW_NORMAL, false)
	if content.timed {
		drawNotificationTextAligned(dc, notificationDuration(content.elapsed), layout.duration, theme.body, 11, win.FW_NORMAL,
			win.DT_RIGHT|win.DT_SINGLELINE|win.DT_END_ELLIPSIS|win.DT_NOPREFIX)
	}
	drawNotificationText(dc, content.title, layout.title, theme.title, 17, win.FW_SEMIBOLD, false)
	drawNotificationText(dc, content.message, layout.body, theme.body, 13, win.FW_NORMAL, true)
	drawNotificationClose(dc, layout.close, theme, close)
}

func drawNotificationClose(dc win.HDC, rect win.RECT, theme notificationPalette, state notificationCloseState) {
	if rect.Right-rect.Left < 12 || rect.Bottom-rect.Top < 12 {
		return
	}
	color := theme.brand
	if state.hovered {
		color = theme.title
		background := theme.border
		if state.pressed {
			background = theme.disk
		}
		withNotificationObject(dc, notificationBrush(background), func() {
			old := win.SelectObject(dc, win.GetStockObject(win.NULL_PEN))
			defer win.SelectObject(dc, old)
			// With NULL_PEN, GDI omits the last row/column of this fill.
			win.RoundRect(dc, rect.Left, rect.Top, rect.Right+1, rect.Bottom+1, 8, 8)
		})
	}
	// Pixel-aligned, mirrored spans avoid GDI LineTo's excluded endpoints and
	// asymmetric thick-pen caps. Both diagonals occupy the same centered 10x10 box.
	const size int32 = 10
	left, top := rect.Left+(rect.Right-rect.Left-size)/2, rect.Top+(rect.Bottom-rect.Top-size)/2
	withNotificationObject(dc, notificationBrush(color), func() {
		for row := int32(0); row < size; row++ {
			start, end := max(int32(0), row-1), min(size, row+2)
			notificationPatBlt.Call(uintptr(dc), uintptr(left+start), uintptr(top+row), uintptr(end-start), 1, win.PATCOPY)
			notificationPatBlt.Call(uintptr(dc), uintptr(left+size-end), uintptr(top+row), uintptr(end-start), 1, win.PATCOPY)
		}
	})
}

func drawNotificationText(dc win.HDC, text string, rect win.RECT, color win.COLORREF, height, weight int32, wrap bool) {
	flags := uint32(win.DT_END_ELLIPSIS | win.DT_NOPREFIX | win.DT_SINGLELINE)
	if wrap {
		flags = win.DT_END_ELLIPSIS | win.DT_NOPREFIX | win.DT_WORDBREAK | win.DT_EDITCONTROL
	}
	drawNotificationTextAligned(dc, text, rect, color, height, weight, flags)
}

func drawNotificationTextAligned(dc win.HDC, text string, rect win.RECT, color win.COLORREF, height, weight int32, flags uint32) {
	if rect.Right <= rect.Left || rect.Bottom <= rect.Top {
		return
	}
	font := notificationFont(height, weight)
	withNotificationObject(dc, win.HGDIOBJ(win.CreateFontIndirect(&font)), func() {
		win.SetTextColor(dc, color)
		win.DrawTextEx(dc, stringPtr(text), -1, &rect, flags, nil)
	})
}

func drawNotificationIcon(dc win.HDC, rect win.RECT, theme notificationPalette, isError bool) {
	withNotificationObject(dc, notificationBrush(theme.disk), func() {
		old := win.SelectObject(dc, win.GetStockObject(win.NULL_PEN))
		defer win.SelectObject(dc, old)
		win.Ellipse(dc, rect.Left, rect.Top, rect.Right, rect.Bottom)
	})
	withNotificationObject(dc, notificationPen(theme.accent, 2), func() {
		x, y := rect.Left, rect.Top
		if isError {
			win.MoveToEx(dc, int(x+18), int(y+10), nil)
			win.LineTo(dc, x+18, y+21)
			win.MoveToEx(dc, int(x+18), int(y+24), nil)
			win.LineTo(dc, x+18, y+26)
		} else {
			win.MoveToEx(dc, int(x+10), int(y+18), nil)
			win.LineTo(dc, x+16, y+24)
			win.LineTo(dc, x+26, y+13)
		}
	})
}