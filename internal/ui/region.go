//go:build windows

package ui

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"screenrec/internal/native"
)

// Region selector: a fullscreen click-drag overlay, like vokoscreen's area
// picker. The overlay window belongs to the UI thread (it is created there
// and its messages arrive through the main loop); dragging only flips state
// the main thread owns, and finishing reports back through the drain queue,
// so there is no blocking call and no cross-thread struct sharing.
//
// Coordinates are virtual-desktop pixels, which is exactly what capture
// expects, and the size is forced even because H.264 needs even dimensions.

const (
	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79

	wsPopup      = 0x80000000
	wsExTopmost  = 0x00000008
	wsExToolWin  = 0x00000080
	wsExLayered  = 0x00080000
	wsExNoActive = 0x08000000
	lwaAlpha     = 0x2
	gwlExStyle   = -20

	wmLButtonDown = 0x0201
	wmLButtonUp   = 0x0202
	wmMouseMove   = 0x0200
	wmRButtonDown = 0x0204
	wmKeyDown     = 0x0100
	vkEscape      = 0x1B

	swHide = 0
)

var (
	pGetSystemMetrics         = user32.NewProc("GetSystemMetrics")
	pGetDC                    = user32.NewProc("GetDC")
	pReleaseDC                = user32.NewProc("ReleaseDC")
	pDrawFocusRect            = user32.NewProc("DrawFocusRect")
	pSetCapture               = user32.NewProc("SetCapture")
	pReleaseCapture           = user32.NewProc("ReleaseCapture")
	pSetWindowLongW           = user32.NewProc("SetWindowLongW")
	pSetLayeredWindowAttributes = user32.NewProc("SetLayeredWindowAttributes")
)

type rect16 struct{ left, top, right, bottom int32 }

var regionProcAddr uintptr

// regionSel tracks an in-progress selection; UI thread only.
type regionSel struct {
	hwnd     syscall.Handle
	dragging bool
	x0, y0   int32
	x1, y1   int32
	drawn    bool
}

var regionActive regionSel

func regionWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	x := int32(int16(lParam & 0xFFFF))
	y := int32(int16((lParam >> 16) & 0xFFFF))
	switch uint32(msg) {
	case wmLButtonDown:
		regionActive.x0, regionActive.y0 = x, y
		regionActive.x1, regionActive.y1 = x, y
		regionActive.dragging = true
		regionActive.drawn = false
		pSetCapture.Call(uintptr(hwnd))
		return 0
	case wmMouseMove:
		if regionActive.dragging {
			regionDraw(false)
			regionActive.x1, regionActive.y1 = x, y
			regionDraw(true)
		}
		return 0
	case wmLButtonUp:
		if regionActive.dragging {
			regionDraw(false)
			regionActive.dragging = false
			regionActive.x1, regionActive.y1 = x, y
			finishRegion(true)
		}
		return 0
	case wmRButtonDown:
		if regionActive.dragging {
			regionDraw(false)
			regionActive.dragging = false
		}
		finishRegion(false)
		return 0
	case wmKeyDown:
		if wParam == vkEscape {
			if regionActive.dragging {
				regionDraw(false)
				regionActive.dragging = false
			}
			finishRegion(false)
			return 0
		}
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

// regionDraw xors the rubber band on or off; drawing twice erases, which is
// the whole trick, so every path that draws must undraw before finishing.
func regionDraw(show bool) {
	if !show && !regionActive.drawn {
		return
	}
	x0, x1 := regionActive.x0, regionActive.x1
	if x1 < x0 {
		x0, x1 = x1, x0
	}
	y0, y1 := regionActive.y0, regionActive.y1
	if y1 < y0 {
		y0, y1 = y1, y0
	}
	hdc, _, _ := pGetDC.Call(uintptr(regionActive.hwnd))
	if hdc == 0 {
		return
	}
	rc := rect16{left: x0, top: y0, right: x1, bottom: y1}
	pDrawFocusRect.Call(hdc, uintptr(unsafe.Pointer(&rc)))
	pReleaseDC.Call(uintptr(regionActive.hwnd), hdc)
	runtime.KeepAlive(rc)
	regionActive.drawn = show
}

func finishRegion(ok bool) {
	var result native.Rect
	if ok {
		x0, x1 := regionActive.x0, regionActive.x1
		if x1 < x0 {
			x0, x1 = x1, x0
		}
		y0, y1 := regionActive.y0, regionActive.y1
		if y1 < y0 {
			y0, y1 = y1, y0
		}
		// Even origin and size: odd dimensions break NV12 outright.
		x0 &^= 1
		y0 &^= 1
		x1 &^= 1
		y1 &^= 1
		if x1-x0 >= 64 && y1-y0 >= 64 {
			result = native.Rect{Left: x0, Top: y0, Right: x1, Bottom: y1}
		} else {
			ok = false
		}
	}
	hwnd := regionActive.hwnd
	regionActive = regionSel{}
	pReleaseCapture.Call()
	pShowWindow.Call(uintptr(hwnd), swHide)
	pDestroyWindow.Call(uintptr(hwnd))
	queue(func() { onRegionDone(result, ok) })
}

// showRegionOverlay creates the fullscreen veil. UI thread only: the window
// must belong to the thread that pumps messages.
func showRegionOverlay() {
	vx, _, _ := pGetSystemMetrics.Call(smXVirtualScreen)
	vy, _, _ := pGetSystemMetrics.Call(smYVirtualScreen)
	vw, _, _ := pGetSystemMetrics.Call(smCXVirtualScreen)
	vh, _, _ := pGetSystemMetrics.Call(smCYVirtualScreen)

	if regionProcAddr == 0 {
		regionProcAddr = syscall.NewCallback(regionWndProc)
		instance, _, _ := pGetModuleHandleW.Call(0)
		cursor, _, _ := pLoadCursorW.Call(0, uintptr(idcArrow))
		className, _ := syscall.UTF16PtrFromString("ScreenRecRegion")
		var cls wndClassEx
		cls.size = uint32(unsafe.Sizeof(cls))
		cls.wndProc = regionProcAddr
		cls.instance = syscall.Handle(instance)
		cls.cursor = syscall.Handle(cursor)
		cls.background = 0
		cls.className = className
		pRegisterClassExW.Call(uintptr(unsafe.Pointer(&cls)))
		runtime.KeepAlive(cls)
		runtime.KeepAlive(className)
	}

	instance, _, _ := pGetModuleHandleW.Call(0)
	className, _ := syscall.UTF16PtrFromString("ScreenRecRegion")
	title, _ := syscall.UTF16PtrFromString("Arraste para selecionar (Esc cancela)")
	hwnd, _, _ := pCreateWindowExW.Call(
		uintptr(wsExTopmost|wsExToolWin|wsExNoActive),
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		uintptr(wsPopup|wsVisible),
		vx, vy, vw, vh, 0, 0, instance, 0)
	runtime.KeepAlive(className)
	runtime.KeepAlive(title)
	if hwnd == 0 {
		fmt.Fprintln(os.Stderr, "region: CreateWindowExW failed")
		queue(func() { logLine("não foi possível abrir o seletor de área") })
		return
	}
	// A dim veil: translucent black over everything makes the selection
	// readable without hiding what is picked.
	exStyle := int32(gwlExStyle)
	pSetWindowLongW.Call(hwnd, uintptr(exStyle),
		uintptr(wsExTopmost|wsExToolWin|wsExNoActive|wsExLayered))
	pSetLayeredWindowAttributes.Call(hwnd, 0, 90, lwaAlpha)
	pShowWindow.Call(hwnd, swShow)
	pUpdateWindow.Call(hwnd)
	regionActive.hwnd = syscall.Handle(hwnd)
}
