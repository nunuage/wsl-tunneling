//go:build windows

package tray

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/lxn/win"

	"github.com/clooooud/wsl-tunneling/internal/config"
	"github.com/clooooud/wsl-tunneling/internal/network"
	"github.com/clooooud/wsl-tunneling/internal/service"
)

//go:embed assets/logo.png
var trayAssets embed.FS

const (
	appName = "wsl-tunneling"

	trayIconID      = 1
	trayCallbackMsg = win.WM_APP + 1
	refreshMsg      = win.WM_APP + 2
	notificationMsg = win.WM_APP + 3

	menuStatus = 1000
	menuStart  = 1001
	menuStop   = 1002
	menuConfig = 1003
	menuBoot   = 1004
	menuQuit   = 1005
)

type app struct {
	ctx        context.Context
	cfg        config.Config
	configPath string
	cachePath  string
	manager    network.Manager
	window     win.HWND
	hicon      win.HICON

	mu            sync.Mutex
	statusText    string
	statusTooltip string
	tooltip       string
	startEnabled  bool
	stopEnabled   bool
	bootEnabled   bool
	bootChecked   bool
	configEnabled bool
	running       bool
	configValid   bool
	status        network.Status
	statusLabel   string
	statusError   string
	bootError     string
	cacheLoaded   bool
	closing       bool
	notifications []notification
	popup         notificationPopup // Accessed only by the message-loop thread.
	menu          trayMenu          // Accessed only by the message-loop thread.
	busy          atomic.Bool
	refreshing    atomic.Bool
	closeOnce     sync.Once
}

type trayCache struct {
	Version     int                   `json:"version"`
	UpdatedAt   time.Time             `json:"updatedAt"`
	ConfigPath  string                `json:"configPath"`
	ConfigHash  string                `json:"configHash"`
	ConfigValid bool                  `json:"configValid"`
	Status      cachedStatusFacts     `json:"status"`
	StartOnBoot cachedStartOnBootFact `json:"startOnBoot"`
}

type cachedStatusFacts struct {
	Label         string `json:"label"`
	Tooltip       string `json:"tooltip"`
	DistroRunning bool   `json:"distroRunning"`
	ForwarderUp   bool   `json:"forwarderUp"`
	Route         string `json:"route"`
	DNS           string `json:"dns"`
	TunnelRunning bool   `json:"tunnelRunning"`
	Error         string `json:"error,omitempty"`
}

type cachedStartOnBootFact struct {
	Installed bool   `json:"installed"`
	Error     string `json:"error,omitempty"`
}

var (
	currentApp   *app
	shellExecute = syscall.NewLazyDLL("shell32.dll").NewProc("ShellExecuteW")
)

func windowProc(hwnd win.HWND, msg uint32, wparam uintptr, lparam uintptr) uintptr {
	if currentApp != nil {
		return currentApp.handleMessage(hwnd, msg, wparam, lparam)
	}
	return win.DefWindowProc(hwnd, msg, wparam, lparam)
}

func Run(ctx context.Context, cfg config.Config, configPath string) error {
	if err := config.EnsureDirs(cfg); err != nil {
		return err
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	application := &app{
		ctx:           ctx,
		cfg:           cfg,
		configPath:    configPath,
		cachePath:     cachePath(configPath),
		manager:       network.NewManager(cfg),
		statusText:    "Status: checking...",
		tooltip:       appName,
		configEnabled: true,
	}
	application.loadCache()
	currentApp = application
	defer func() { currentApp = nil }()

	if err := application.createWindow(); err != nil {
		return err
	}
	defer application.destroyWindow()

	if err := application.addNotifyIcon(); err != nil {
		return err
	}
	defer application.deleteNotifyIcon()

	application.scheduleRefresh()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	done := make(chan struct{})
	defer close(done)

	go func() {
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				application.postMessage(win.WM_CLOSE)
				return
			case <-ticker.C:
				application.scheduleRefresh()
			}
		}
	}()

	application.messageLoop()
	return nil
}

func (a *app) createWindow() error {
	className := stringPtr(appName + "-tray-window")
	hinstance := win.GetModuleHandle(nil)
	windowClass := win.WNDCLASSEX{
		CbSize:        uint32(unsafe.Sizeof(win.WNDCLASSEX{})),
		LpfnWndProc:   syscallCallback(windowProc),
		HInstance:     hinstance,
		LpszClassName: className,
	}

	if atom := win.RegisterClassEx(&windowClass); atom == 0 {
		return fmt.Errorf("register tray window class: error %d", win.GetLastError())
	}

	hwnd := win.CreateWindowEx(
		0,
		className,
		stringPtr(appName),
		0,
		0,
		0,
		0,
		0,
		win.HWND_MESSAGE,
		0,
		hinstance,
		nil,
	)
	if hwnd == 0 {
		win.UnregisterClass(className)
		return fmt.Errorf("create tray window: error %d", win.GetLastError())
	}

	a.window = hwnd
	return nil
}

func (a *app) destroyWindow() {
	a.stopNotifications()
	a.menu.cleanup()
	a.popup.cleanup()
	if a.window != 0 {
		win.DestroyWindow(a.window)
		a.mu.Lock()
		a.window = 0
		a.mu.Unlock()
	}
	win.UnregisterClass(stringPtr(appName + "-tray-window"))
}

func (a *app) addNotifyIcon() error {
	a.hicon = loadTrayIcon()
	if a.hicon == 0 {
		a.hicon = win.LoadIcon(0, win.MAKEINTRESOURCE(win.IDI_APPLICATION))
	}

	data := a.notifyIconData(win.NIF_MESSAGE | win.NIF_ICON | win.NIF_TIP)
	if !win.Shell_NotifyIcon(win.NIM_ADD, &data) {
		return fmt.Errorf("add tray icon: error %d", win.GetLastError())
	}
	return nil
}

func (a *app) deleteNotifyIcon() {
	data := a.notifyIconData(0)
	win.Shell_NotifyIcon(win.NIM_DELETE, &data)
	if a.hicon != 0 {
		win.DestroyIcon(a.hicon)
		a.hicon = 0
	}
}

func (a *app) updateNotifyIcon() {
	data := a.notifyIconData(win.NIF_TIP)
	win.Shell_NotifyIcon(win.NIM_MODIFY, &data)
}

func (a *app) notifyIconData(flags uint32) win.NOTIFYICONDATA {
	a.mu.Lock()
	tooltip := a.tooltip
	a.mu.Unlock()

	data := win.NOTIFYICONDATA{
		CbSize:           uint32(unsafe.Sizeof(win.NOTIFYICONDATA{})),
		HWnd:             a.window,
		UID:              trayIconID,
		UFlags:           flags,
		UCallbackMessage: trayCallbackMsg,
		HIcon:            a.hicon,
	}
	copyString(data.SzTip[:], tooltip)
	return data
}

func (a *app) messageLoop() {
	var msg win.MSG
	for win.GetMessage(&msg, 0, 0, 0) > 0 {
		win.TranslateMessage(&msg)
		win.DispatchMessage(&msg)
	}
}

func (a *app) handleMessage(hwnd win.HWND, msg uint32, wparam uintptr, lparam uintptr) uintptr {
	switch msg {
	case trayCallbackMsg:
		if lparam == win.WM_LBUTTONUP || lparam == win.WM_RBUTTONUP || lparam == win.WM_CONTEXTMENU {
			a.showMenu()
			return 0
		}
	case win.WM_COMMAND:
		a.handleCommand(uint32(wparam) & 0xffff)
		return 0
	case refreshMsg:
		a.updateNotifyIcon()
		a.refreshMenu()
		return 0
	case notificationMsg:
		a.showNextNotification()
		return 0
	case win.WM_CLOSE:
		a.close()
		return 0
	case win.WM_DESTROY:
		win.PostQuitMessage(0)
		return 0
	}
	return win.DefWindowProc(hwnd, msg, wparam, lparam)
}

func (a *app) showMenu() {
	a.toggleMenu()
}

func (a *app) handleCommand(id uint32) {
	switch id {
	case menuStart:
		a.runOperation("start", "started", func(ctx context.Context) error { return a.currentManager().Start(ctx) })
	case menuStop:
		a.runOperation("stop", "stopped", func(ctx context.Context) error { return a.currentManager().Stop(ctx) })
	case menuConfig:
		a.openConfigFolder()
	case menuBoot:
		a.toggleStartOnBoot()
	case menuQuit:
		a.close()
	}
}

func (a *app) close() {
	a.closeOnce.Do(func() {
		a.stopNotifications()
		a.menu.destroy()
		a.popup.destroy()
		// Deferred cleanup removes the icon before destroying its owner HWND.
		win.PostQuitMessage(0)
	})
}

// Serialize worker posts with shutdown: no worker can post to a destroyed HWND.
func (a *app) postMessage(msg uint32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.closing && a.window != 0 {
		win.PostMessage(a.window, msg, 0, 0)
	}
}

func (a *app) runOperation(name string, completed string, operation func(context.Context) error) {
	if !a.busy.CompareAndSwap(false, true) {
		return
	}
	a.setBusy(name)
	go func() {
		err := operation(a.ctx)
		a.busy.Store(false)
		if err != nil {
			a.showError(fmt.Sprintf("%s failed", name), err)
		} else {
			a.showInfo("Tunnel "+completed, fmt.Sprintf("%s completed", name))
		}
		a.scheduleRefresh()
	}()
}

func (a *app) currentManager() network.Manager {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.manager
}

func (a *app) setBusy(name string) {
	a.setBusyText(fmt.Sprintf("Status: %sing...", name))
}

func (a *app) setBusyText(text string) {
	a.mu.Lock()
	a.statusText = text
	a.statusTooltip = ""
	a.tooltip = appName + " - " + strings.TrimPrefix(text, "Status: ")
	a.applyActionStateLocked(false)
	a.mu.Unlock()
	a.updateNotifyIcon()
	a.refreshMenu()
}

func (a *app) scheduleRefresh() {
	if a.busy.Load() || !a.refreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer a.refreshing.Store(false)
		a.refreshAll()
		a.saveCache()
		a.postMessage(refreshMsg)
	}()
}

func (a *app) refreshAll() {
	if a.busy.Load() {
		return
	}
	if !a.reloadConfig() {
		return
	}
	a.refreshStatus()
	a.refreshStartOnBoot()
}

func (a *app) reloadConfig() bool {
	cfg, err := config.Load(a.configPath)
	if err != nil {
		a.mu.Lock()
		a.running = false
		a.configValid = false
		a.status = network.Status{}
		a.statusLabel = "config error"
		a.statusError = err.Error()
		a.statusText = "Status: config error"
		a.statusTooltip = err.Error()
		a.applyActionStateLocked(false)
		a.bootChecked = false
		a.tooltip = appName + " - config error"
		a.mu.Unlock()
		return false
	}

	a.mu.Lock()
	a.cfg = cfg
	a.manager = network.NewManager(cfg)
	a.mu.Unlock()
	return true
}

func (a *app) loadCache() {
	content, err := os.ReadFile(a.cachePath)
	if err != nil {
		return
	}
	var cache trayCache
	if err := json.Unmarshal(content, &cache); err != nil || cache.Version != 2 || cache.ConfigPath != a.configPath || cache.ConfigHash != configHash(a.configPath) {
		return
	}

	a.mu.Lock()
	a.running = cache.Status.TunnelRunning
	a.configValid = cache.ConfigValid
	a.status = network.Status{
		DistroRunning: cache.Status.DistroRunning,
		ForwarderUp:   cache.Status.ForwarderUp,
		Route:         cache.Status.Route,
		DNS:           cache.Status.DNS,
	}
	a.statusLabel = cache.Status.Label
	a.statusError = cache.Status.Error
	a.bootError = cache.StartOnBoot.Error
	a.bootChecked = cache.StartOnBoot.Installed
	a.statusText = cachedStatusText(statusText(cache.Status.Label))
	a.statusTooltip = cache.Status.Tooltip
	if cache.Status.Error != "" {
		a.statusTooltip = cache.Status.Error
	}
	a.tooltip = appName + " - " + cachedLabel(cache.Status.Label)
	a.applyActionStateLocked(cache.ConfigValid)
	a.cacheLoaded = true
	a.mu.Unlock()
}

func (a *app) saveCache() {
	a.mu.Lock()
	cache := trayCache{
		Version:     2,
		UpdatedAt:   time.Now(),
		ConfigPath:  a.configPath,
		ConfigHash:  configHash(a.configPath),
		ConfigValid: a.configValid,
		Status: cachedStatusFacts{
			Label:         a.statusLabel,
			Tooltip:       a.statusTooltip,
			DistroRunning: a.status.DistroRunning,
			ForwarderUp:   a.status.ForwarderUp,
			Route:         a.status.Route,
			DNS:           a.status.DNS,
			TunnelRunning: a.running,
			Error:         a.statusError,
		},
		StartOnBoot: cachedStartOnBootFact{Installed: a.bootChecked, Error: a.bootError},
	}
	a.mu.Unlock()

	content, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return
	}
	content = append(content, '\n')
	if err := os.MkdirAll(filepath.Dir(a.cachePath), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(a.cachePath, content, 0o644)
}

func cachedStatusText(statusText string) string {
	if statusText == "" || strings.Contains(statusText, "cached") {
		return statusText
	}
	return statusText + " (cached)"
}

func cachedLabel(label string) string {
	if label == "" {
		return "checking"
	}
	return label + " (cached)"
}

func statusText(label string) string {
	if label == "" {
		return "Status: checking..."
	}
	return "Status: " + label
}

func (a *app) applyActionStateLocked(configValid bool) {
	busy := a.busy.Load()
	a.startEnabled = configValid && !busy && !a.running
	a.stopEnabled = configValid && !busy && a.running
	a.bootEnabled = configValid && !busy
	a.configEnabled = true
}

func configHash(configPath string) string {
	if configPath == "" {
		configPath = config.DefaultPath()
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(content)
	return fmt.Sprintf("%x", sum[:])
}

func cachePath(configPath string) string {
	if configPath == "" {
		configPath = config.DefaultPath()
	}
	return filepath.Join(filepath.Dir(configPath), "tray-cache.json")
}

func loadTrayIcon() win.HICON {
	file, err := trayAssets.Open("assets/logo.png")
	if err != nil {
		return 0
	}
	defer file.Close()

	source, err := png.Decode(file)
	if err != nil {
		return 0
	}
	size := win.GetSystemMetrics(win.SM_CXSMICON)
	if height := win.GetSystemMetrics(win.SM_CYSMICON); height > 0 && height < size {
		size = height
	}
	if size <= 0 {
		size = 16
	}

	iconImage := scaleImage(source, int(size), int(size))
	colorBitmap, maskBitmap := iconBitmaps(iconImage)
	if colorBitmap == 0 || maskBitmap == 0 {
		if colorBitmap != 0 {
			win.DeleteObject(win.HGDIOBJ(colorBitmap))
		}
		if maskBitmap != 0 {
			win.DeleteObject(win.HGDIOBJ(maskBitmap))
		}
		return 0
	}
	defer win.DeleteObject(win.HGDIOBJ(colorBitmap))
	defer win.DeleteObject(win.HGDIOBJ(maskBitmap))

	return win.CreateIconIndirect(&win.ICONINFO{
		FIcon:    win.TRUE,
		HbmColor: colorBitmap,
		HbmMask:  maskBitmap,
	})
}

func iconBitmaps(img *image.RGBA) (win.HBITMAP, win.HBITMAP) {
	width := img.Bounds().Dx()
	height := img.Bounds().Dy()
	var colorBits unsafe.Pointer
	colorHeader := win.BITMAPINFOHEADER{
		BiSize:        uint32(unsafe.Sizeof(win.BITMAPINFOHEADER{})),
		BiWidth:       int32(width),
		BiHeight:      -int32(height),
		BiPlanes:      1,
		BiBitCount:    32,
		BiCompression: win.BI_RGB,
	}
	colorBitmap := win.CreateDIBSection(0, &colorHeader, win.DIB_RGB_COLORS, &colorBits, 0, 0)
	if colorBitmap == 0 || colorBits == nil {
		return 0, 0
	}
	colorBytes := unsafe.Slice((*byte)(colorBits), width*height*4)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			pixelOffset := img.PixOffset(x, y)
			bitmapOffset := (y*width + x) * 4
			colorBytes[bitmapOffset+0] = img.Pix[pixelOffset+2]
			colorBytes[bitmapOffset+1] = img.Pix[pixelOffset+1]
			colorBytes[bitmapOffset+2] = img.Pix[pixelOffset+0]
			colorBytes[bitmapOffset+3] = img.Pix[pixelOffset+3]
		}
	}

	var maskBits unsafe.Pointer
	maskHeader := win.BITMAPINFOHEADER{
		BiSize:        uint32(unsafe.Sizeof(win.BITMAPINFOHEADER{})),
		BiWidth:       int32(width),
		BiHeight:      int32(height),
		BiPlanes:      1,
		BiBitCount:    1,
		BiCompression: win.BI_RGB,
	}
	maskBitmap := win.CreateDIBSection(0, &maskHeader, win.DIB_RGB_COLORS, &maskBits, 0, 0)
	if maskBitmap == 0 || maskBits == nil {
		win.DeleteObject(win.HGDIOBJ(colorBitmap))
		return 0, 0
	}
	return colorBitmap, maskBitmap
}

func scaleImage(source image.Image, width int, height int) *image.RGBA {
	destination := image.NewRGBA(image.Rect(0, 0, width, height))
	bounds := source.Bounds()
	for y := 0; y < height; y++ {
		sourceY := bounds.Min.Y + y*bounds.Dy()/height
		for x := 0; x < width; x++ {
			sourceX := bounds.Min.X + x*bounds.Dx()/width
			destination.Set(x, y, source.At(sourceX, sourceY))
		}
	}
	return destination
}

func (a *app) refreshStatus() {
	if err := a.cfg.Validate(); err != nil {
		a.mu.Lock()
		a.running = false
		a.configValid = false
		a.status = network.Status{}
		a.statusLabel = "config required"
		a.statusError = err.Error()
		a.statusText = "Status: config required"
		a.statusTooltip = err.Error()
		a.applyActionStateLocked(false)
		a.tooltip = appName + " - config required"
		a.mu.Unlock()
		return
	}

	status, err := a.manager.Status(a.ctx)
	if err != nil {
		a.mu.Lock()
		a.running = false
		a.configValid = true
		a.status = status
		a.statusLabel = "error"
		a.statusError = err.Error()
		a.statusText = "Status: error"
		a.statusTooltip = err.Error()
		a.applyActionStateLocked(true)
		a.tooltip = appName + " - status error"
		a.mu.Unlock()
		return
	}

	label := "stopped"
	running := status.DistroRunning && status.ForwarderUp && strings.Contains(status.Route, a.cfg.GatewayIP)
	if running {
		label = "running"
	} else if status.DistroRunning {
		label = "WSL running"
	}

	a.mu.Lock()
	a.running = running
	a.configValid = true
	a.status = status
	a.statusLabel = label
	a.statusError = ""
	a.cacheLoaded = false
	a.statusText = "Status: " + label
	a.statusTooltip = statusTooltip(a.cfg, status)
	a.applyActionStateLocked(true)
	a.tooltip = appName + " - " + label
	a.mu.Unlock()
}

func (a *app) refreshStartOnBoot() {
	installed, err := service.IsInstalled(a.ctx, a.cfg)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.bootError = err.Error()
		a.applyActionStateLocked(a.configValid)
		a.bootChecked = false
		return
	}
	a.bootError = ""
	a.applyActionStateLocked(a.configValid)
	a.bootChecked = installed
}

func (a *app) toggleStartOnBoot() {
	if !a.busy.CompareAndSwap(false, true) {
		return
	}
	a.setBusyText("Status: updating start on boot...")
	go func() {
		installed, err := service.IsInstalled(a.ctx, a.cfg)
		if err == nil {
			if installed {
				err = service.Uninstall(a.ctx, a.cfg)
			} else {
				err = service.Install(a.ctx, a.cfg, a.configPath)
			}
		}
		a.busy.Store(false)
		if err != nil {
			a.showError("Start on boot failed", err)
		}
		a.scheduleRefresh()
	}()
}

func (a *app) openConfigFolder() {
	if _, err := os.Stat(a.configPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			a.showError("Open config failed", err)
			return
		}
		if err := config.SaveExample(a.configPath); err != nil {
			a.showError("Create config failed", err)
			return
		}
	}
	dir := filepath.Dir(a.configPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		a.showError("Open config failed", err)
		return
	}
	if err := openFolder(a.window, dir); err != nil {
		a.showError("Open config failed", err)
	}
}

func openFolder(owner win.HWND, dir string) error {
	operation := unsafeStringToUTF16("open")
	target := unsafeStringToUTF16(dir)
	result, _, callErr := shellExecute.Call(
		uintptr(owner),
		uintptr(unsafe.Pointer(&operation[0])),
		uintptr(unsafe.Pointer(&target[0])),
		0,
		0,
		1,
	)
	runtime.KeepAlive(operation)
	runtime.KeepAlive(target)
	if result > 32 {
		return nil
	}
	if callErr != syscall.Errno(0) {
		return callErr
	}
	return fmt.Errorf("shell open failed with code %d", result)
}

func (a *app) showError(title string, err error) {
	a.showNotification(title, err.Error(), true)
}

func (a *app) showInfo(title string, message string) {
	a.showNotification(title, message, false)
}

func copyString(dst []uint16, value string) {
	encoded := unsafeStringToUTF16(value)
	if len(encoded) > len(dst) {
		encoded = encoded[:len(dst)]
		encoded[len(encoded)-1] = 0
	}
	copy(dst, encoded)
}

func stringPtr(value string) *uint16 {
	encoded := unsafeStringToUTF16(value)
	return &encoded[0]
}

func unsafeStringToUTF16(value string) []uint16 {
	return syscall.StringToUTF16(value)
}

func syscallCallback(fn any) uintptr {
	return syscall.NewCallback(fn)
}

func statusTooltip(cfg config.Config, status network.Status) string {
	if !status.DistroRunning {
		return fmt.Sprintf("Distro: %s\nStopped", cfg.Distro)
	}
	return fmt.Sprintf(
		"Distro: %s\nForwarder: %t\nRoute: %s\nDNS: %s",
		cfg.Distro,
		status.ForwarderUp,
		strings.TrimSpace(status.Route),
		strings.TrimSpace(status.DNS),
	)
}
