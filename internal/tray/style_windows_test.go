//go:build windows

package tray

import (
	"image"
	"image/png"
	"os"
	"syscall"
	"testing"
	"unsafe"

	"github.com/lxn/win"
)

// Pure presentation tests: no HWND, GDI allocation, timer or tray startup.
func TestNotificationPresentation(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   notification
		want string
	}{
		{"start", notification{title: "Tunnel started", message: "start completed"}, "WSL networking is ready."},
		{"stop", notification{title: "Tunnel stopped", message: "stop completed"}, "Previous network settings restored."},
		{"normalized", notification{message: " START completed \r\n"}, "WSL networking is ready."},
		{"error preserved", notification{message: "start completed", isError: true}, "start completed"},
		{"diagnostic preserved", notification{message: "start completed: diagnostic"}, "start completed: diagnostic"},
		{"other operation", notification{message: "restart completed"}, "restart completed"},
		{"unicode", notification{message: "Échec réseau 🚀"}, "Échec réseau 🚀"},
		{"empty", notification{}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			original := tt.in
			got := notificationPresentation(tt.in)
			if got.message != tt.want || got.title != tt.in.title || got.isError != tt.in.isError {
				t.Fatalf("presentation = %+v, want message %q with unchanged metadata", got, tt.want)
			}
			if tt.in != original {
				t.Fatal("presentation mutated the queued payload")
			}
		})
	}
}

func TestNotificationCardLayout(t *testing.T) {
	layout := notificationLayout(340, 112)
	if !layout.showIcon || layout.icon != (win.RECT{Left: 16, Top: 39, Right: 52, Bottom: 75}) {
		t.Fatalf("unexpected icon layout: %+v", layout)
	}
	if layout.brand.Left != 68 || layout.body.Right != 320 || layout.body.Bottom != 100 {
		t.Fatalf("expected breathing room around text: %+v", layout)
	}
	if layout.brand.Bottom > layout.title.Top || layout.title.Bottom > layout.body.Top || layout.icon.Right >= layout.title.Left {
		t.Fatal("brand, title, body and icon must not overlap")
	}
	for _, size := range [][2]int32{{340, 112}, {240, 100}, {168, 48}, {1, 1}, {0, 0}, {-1, -1}} {
		w, h := max(0, size[0]), max(0, size[1])
		got := notificationLayout(size[0], size[1])
		for _, rect := range []win.RECT{got.brand, got.title, got.body, got.icon} {
			if rect.Left < 0 || rect.Top < 0 || rect.Right > w || rect.Bottom > h || rect.Right < rect.Left || rect.Bottom < rect.Top {
				t.Fatalf("layout for %v escapes client: %+v", size, rect)
			}
		}
	}
}

func TestNotificationCardTheme(t *testing.T) {
	success, failure := notificationTheme(false), notificationTheme(true)
	if success.background != win.RGB(32, 37, 45) || failure.background != success.background {
		t.Fatal("expected slate #20252d background for both states")
	}
	if success.accent != win.RGB(92, 214, 158) || failure.accent != win.RGB(255, 139, 124) {
		t.Fatal("expected green success / coral error accents")
	}
	if success.disk == failure.disk || success.disk == success.accent || failure.disk == failure.accent {
		t.Fatal("status disks must be tinted, not solid accent colors")
	}
	if success.border == success.background || success.title == success.body || success.brand == success.title {
		t.Fatal("expected subtle border and distinct text hierarchy")
	}
}

func TestNotificationFontDefinition(t *testing.T) {
	for _, spec := range [][2]int32{{11, win.FW_NORMAL}, {17, win.FW_SEMIBOLD}, {13, win.FW_NORMAL}} {
		font := notificationFont(spec[0], spec[1])
		if syscall.UTF16ToString(font.LfFaceName[:]) != "Segoe UI" || font.LfQuality != win.CLEARTYPE_QUALITY ||
			font.LfHeight != -spec[0] || font.LfWeight != spec[1] || font.LfCharSet != win.DEFAULT_CHARSET {
			t.Fatalf("unexpected font: %+v", font)
		}
	}
	if notificationCornerDiameter != 20 || popupFadeTime != 160 {
		t.Fatal("expected rounded card and unchanged 160ms exit fade")
	}
}

// Optional offscreen preview: exercise the actual GDI renderer without opening
// any window, touching the tunnel or activating the desktop.
func TestNotificationCardPreview(t *testing.T) {
	output := os.Getenv("WSL_TUNNELING_NOTIFICATION_PREVIEW")
	if output == "" {
		t.Skip("offscreen preview not requested")
	}
	dc := win.CreateCompatibleDC(0)
	if dc == 0 {
		t.Fatal("create preview DC")
	}
	defer win.DeleteDC(dc)
	const width, height = 340, 112
	header := win.BITMAPINFOHEADER{
		BiSize: uint32(unsafe.Sizeof(win.BITMAPINFOHEADER{})),
		BiWidth: width, BiHeight: -height, BiPlanes: 1, BiBitCount: 32, BiCompression: win.BI_RGB,
	}
	var bits unsafe.Pointer
	bitmap := win.CreateDIBSection(dc, &header, win.DIB_RGB_COLORS, &bits, 0, 0)
	if bitmap == 0 || bits == nil {
		t.Fatal("create preview bitmap")
	}
	defer win.DeleteObject(win.HGDIOBJ(bitmap))
	previous := win.SelectObject(dc, win.HGDIOBJ(bitmap))
	defer win.SelectObject(dc, previous)
	drawNotificationCard(dc, win.RECT{Right: width, Bottom: height},
		notification{title: "Tunnel started", message: "start completed"})
	// Flush deferred GDI operations before reading DIB memory.
	flush := syscall.NewLazyDLL("gdi32.dll").NewProc("GdiFlush")
	flush.Call()
	bgra := unsafe.Slice((*byte)(bits), width*height*4)
	preview := image.NewRGBA(image.Rect(0, 0, width, height))
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