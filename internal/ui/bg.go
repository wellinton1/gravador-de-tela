//go:build windows

package ui

import (
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

// Window background art: 2.png baked dim into assets/bg.bmp at build time.
// The main window blits it on erase and paints its labels transparent over
// it, so the art shows through around the native controls. Everything is
// stock GDI (no GDI+, no toolkit): one StretchBlt per repaint, one bitmap
// held for the life of the window.

const (
	wmEraseBkgnd    = 0x0014
	wmCtlColorStatic = 0x0038

	imageBitmap      = 0
	lrCreateDIBSection = 0x2000

	srccopy     = 0xCC0020
	transparent = 1
	nullBrush   = 5

	lightText = 0xE8E8E8 // warm light gray, readable on the dimmed art
)

var (
	pGetClientRect = user32.NewProc("GetClientRect")
	pCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	pSelectObject       = gdi32.NewProc("SelectObject")
	pStretchBlt         = gdi32.NewProc("StretchBlt")
	pDeleteDC           = gdi32.NewProc("DeleteDC")
	pGetObjectW         = gdi32.NewProc("GetObjectW")
	pSetBkMode          = gdi32.NewProc("SetBkMode")
	pSetTextColor       = gdi32.NewProc("SetTextColor")
)

type rect32 struct{ left, top, right, bottom int32 }

type bitmapInfo struct {
	bmType       int32
	bmWidth      int32
	bmHeight     int32
	bmWidthBytes int32
	bmPlanes     uint16
	bmBitsPixel  uint16
	bmBits       uintptr
}

// loadBackground reads assets/bg.bmp once; zero means plain gray.
func loadBackground() syscall.Handle {
	dir := assetsDir()
	if dir == "" {
		return 0
	}
	path, _ := syscall.UTF16PtrFromString(filepath.Join(dir, "bg.bmp"))
	h, _, _ := pLoadImageW.Call(0, uintptr(unsafe.Pointer(path)),
		uintptr(imageBitmap), 0, 0, uintptr(lrLoadFromFile|lrCreateDIBSection))
	runtime.KeepAlive(path)
	return syscall.Handle(h)
}

// paintBackground stretches the art over the client area. Called from
// WM_ERASEBKGND, whose wParam already is the target DC.
func paintBackground(hwnd syscall.Handle, hdc uintptr) uintptr {
	var rc rect32
	pGetClientRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&rc)))
	runtime.KeepAlive(rc)
	cw := rc.right - rc.left
	ch := rc.bottom - rc.top
	if cw <= 0 || ch <= 0 || st.bg == 0 {
		return 0
	}
	var bi bitmapInfo
	got, _, _ := pGetObjectW.Call(uintptr(st.bg), uintptr(unsafe.Sizeof(bi)),
		uintptr(unsafe.Pointer(&bi)))
	runtime.KeepAlive(bi)
	if got == 0 {
		return 0
	}
	if bi.bmWidth <= 0 || bi.bmHeight <= 0 {
		return 0
	}
	mem, _, _ := pCreateCompatibleDC.Call(hdc)
	if mem == 0 {
		return 0
	}
	defer pDeleteDC.Call(mem)
	old, _, _ := pSelectObject.Call(mem, uintptr(st.bg))
	if old == 0 {
		return 0
	}
	defer pSelectObject.Call(mem, old)
	r, _, _ := pStretchBlt.Call(hdc, 0, 0, uintptr(cw), uintptr(ch),
		mem, 0, 0, uintptr(bi.bmWidth), uintptr(bi.bmHeight), srccopy)
	runtime.KeepAlive(bi)
	return r
}

// staticColors makes a label float over the art: transparent background,
// light text, no brush (the art behind stays untouched).
func staticColors(hdc uintptr) uintptr {
	pSetBkMode.Call(hdc, transparent)
	pSetTextColor.Call(hdc, lightText)
	brush, _, _ := pGetStockObject.Call(nullBrush)
	return brush
}
