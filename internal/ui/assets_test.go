//go:build windows

package ui

import (
	"syscall"
	"testing"
	"unsafe"

	"screenrec/internal/ui/embedded"
)

var (
	pGetIconInfo = user32.NewProc("GetIconInfo")
	pDestroyIcon = user32.NewProc("DestroyIcon")
	pDeleteObj   = gdi32.NewProc("DeleteObject")
)

// iconInfo mirrors Win32 ICONINFO on 64-bit: the two handles need
// 8-alignment, hence the explicit padding after the hotspot.
type iconInfo struct {
	isIcon int32
	xHot   uint32
	yHot   uint32
	_pad   uint32
	mask   syscall.Handle
	color  syscall.Handle
}

// TestEmbeddedIcons decodes every baked-in icon at the sizes the program
// uses (16 px tray, 16/32 px window) and checks the handles are real icons
// of the requested size. Needs a desktop (like the other tests here).
func TestEmbeddedIcons(t *testing.T) {
	cases := map[string][]byte{
		"icon-app.ico":  embedded.AppIcon,
		"icon-tray.ico": embedded.TrayIcon,
		"icon-rec.ico":  embedded.RecIcon,
	}
	for _, size := range []int{16, 32} {
		for name, data := range cases {
			if len(data) == 0 {
				t.Errorf("%s: no embedded bytes (run build-exe.ps1 first)", name)
				continue
			}
			h := iconFromMemory(data, size)
			if h == 0 {
				t.Errorf("%s @ %d: decode failed", name, size)
				continue
			}
			var info iconInfo
			ok, _, _ := pGetIconInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&info)))
			pDestroyIcon.Call(uintptr(h))
			if ok == 0 || info.color == 0 {
				t.Errorf("%s @ %d: not a valid icon", name, size)
				continue
			}
			var bm bitmapInfo
			got, _, _ := pGetObjectW.Call(uintptr(info.color), uintptr(unsafe.Sizeof(bm)),
				uintptr(unsafe.Pointer(&bm)))
			pDeleteObj.Call(uintptr(info.color))
			pDeleteObj.Call(uintptr(info.mask))
			if got == 0 {
				t.Errorf("%s @ %d: color bitmap unreadable", name, size)
				continue
			}
			if int(bm.bmWidth) != size || int(bm.bmHeight) != size {
				t.Errorf("%s @ %d: decoded %dx%d", name, size, bm.bmWidth, bm.bmHeight)
			}
		}
	}
	if h := iconFromMemory([]byte("nope"), 16); h != 0 {
		pDestroyIcon.Call(uintptr(h))
		t.Error("garbage input decoded into an icon")
	}
}

// TestEmbeddedBackground decodes the baked-in bg.bmp and checks the bitmap
// dimensions match the art make-icons.ps1 generates (480x540).
func TestEmbeddedBackground(t *testing.T) {
	if len(embedded.Background) == 0 {
		t.Fatal("no embedded background (run build-exe.ps1 first)")
	}
	h := bitmapFromMemory(embedded.Background)
	if h == 0 {
		t.Fatal("background decode failed")
	}
	defer pDeleteObj.Call(uintptr(h))
	var bm bitmapInfo
	got, _, _ := pGetObjectW.Call(uintptr(h), uintptr(unsafe.Sizeof(bm)),
		uintptr(unsafe.Pointer(&bm)))
	if got == 0 {
		t.Fatal("background bitmap unreadable")
	}
	if int(bm.bmWidth) != 480 || int(bm.bmHeight) != 540 {
		t.Errorf("background is %dx%d, want 480x540", bm.bmWidth, bm.bmHeight)
	}
	if h := bitmapFromMemory([]byte("BM-nope")); h != 0 {
		pDeleteObj.Call(uintptr(h))
		t.Error("garbage input decoded into a bitmap")
	}
}
