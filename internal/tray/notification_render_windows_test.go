//go:build windows

package tray

import (
	"image"
	"runtime"
	"testing"
	"unsafe"

	"github.com/lxn/win"
)

// Exercise actual GDI pixels without creating an HWND or touching desktop input.
func renderNotificationTestImage(t *testing.T, width, height int32, draw func(win.HDC)) *image.RGBA {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	dc := win.CreateCompatibleDC(0)
	if dc == 0 {
		t.Fatal("create render DC")
	}
	defer win.DeleteDC(dc)
	header := win.BITMAPINFOHEADER{
		BiSize: uint32(unsafe.Sizeof(win.BITMAPINFOHEADER{})), BiWidth: width, BiHeight: -height,
		BiPlanes: 1, BiBitCount: 32, BiCompression: win.BI_RGB,
	}
	var bits unsafe.Pointer
	bitmap := win.CreateDIBSection(dc, &header, win.DIB_RGB_COLORS, &bits, 0, 0)
	if bitmap == 0 || bits == nil {
		t.Fatal("create render bitmap")
	}
	defer win.DeleteObject(win.HGDIOBJ(bitmap))
	previous := win.SelectObject(dc, win.HGDIOBJ(bitmap))
	if !validNotificationObject(previous) {
		t.Fatal("select render bitmap")
	}
	defer win.SelectObject(dc, previous)
	bgra := unsafe.Slice((*byte)(bits), int(width)*int(height)*4)
	clear(bgra)
	draw(dc)
	notificationGDI.NewProc("GdiFlush").Call()
	result := image.NewRGBA(image.Rect(0, 0, int(width), int(height)))
	for i := 0; i < len(bgra); i += 4 {
		result.Pix[i], result.Pix[i+1], result.Pix[i+2], result.Pix[i+3] = bgra[i+2], bgra[i+1], bgra[i], 255
	}
	return result
}

func TestNotificationCloseRasterSymmetry(t *testing.T) {
	for _, button := range []win.RECT{
		{Left: 12, Top: 12, Right: 36, Bottom: 36},
		{Left: 11, Top: 11, Right: 36, Bottom: 36},
		{Left: 12, Top: 11, Right: 36, Bottom: 36},
	} {
		var normalMask []bool
		for _, state := range []notificationCloseState{{}, {hovered: true}, {hovered: true, pressed: true}} {
			theme := notificationTheme(false)
			ink := theme.brand
			if state.hovered {
				ink = theme.title
			}
			pixels := renderNotificationTestImage(t, 48, 48, func(dc win.HDC) {
				drawNotificationClose(dc, button, theme, state)
			})
			mask := make([]bool, 48*48)
			bounds := image.Rectangle{}
			painted := image.Rectangle{}
			for y := 0; y < 48; y++ {
				for x := 0; x < 48; x++ {
					pixel := pixels.RGBAAt(x, y)
					if pixel.R != 0 || pixel.G != 0 || pixel.B != 0 {
						painted = painted.Union(image.Rect(x, y, x+1, y+1))
					}
					if pixel.R == byte(ink) && pixel.G == byte(ink>>8) && pixel.B == byte(ink>>16) {
						mask[y*48+x] = true
						bounds = bounds.Union(image.Rect(x, y, x+1, y+1))
					}
				}
			}
			left := int(button.Left + (button.Right-button.Left-10)/2)
			top := int(button.Top + (button.Bottom-button.Top-10)/2)
			want := image.Rect(left, top, left+10, top+10)
			if bounds != want {
				t.Fatalf("button=%+v state=%+v: cross pixel bounds=%v, want centered %v", button, state, bounds, want)
			}
			if state.hovered && painted != image.Rect(int(button.Left), int(button.Top), int(button.Right), int(button.Bottom)) {
				t.Fatalf("hover background must share button bounds, got %v for %+v", painted, button)
			}
			for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
				for x := bounds.Min.X; x < bounds.Max.X; x++ {
					mirrorX, mirrorY := bounds.Min.X+bounds.Max.X-1-x, bounds.Min.Y+bounds.Max.Y-1-y
					if mask[y*48+x] != mask[y*48+mirrorX] || mask[y*48+x] != mask[mirrorY*48+x] {
						t.Fatalf("asymmetric cross pixel (%d,%d), button=%+v state=%+v", x, y, button, state)
					}
				}
			}
			if normalMask == nil {
				normalMask = mask
			} else {
				for index := range mask {
					if mask[index] != normalMask[index] {
						t.Fatal("hover/press must not move or reshape the cross")
					}
				}
			}
		}
	}
}