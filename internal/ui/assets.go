//go:build windows

package ui

import (
	"encoding/binary"
	"runtime"
	"syscall"
	"unsafe"

	"screenrec/internal/ui/embedded"
)

// Embedded art: icons and background decoded from the bytes baked into the
// executable (see the embedded package) straight into GDI handles, with no
// temporary files. Icons go through CreateIconFromResourceEx, which accepts
// the PNG-compressed entries make-icons.ps1 writes; the background goes
// through CreateDIBSection plus a pixel copy. Every loader reports zero on
// any malformed input, and every caller falls back to the on-disk assets
// folder and then to stock handles, so bad art never breaks the window.

var (
	pCreateIconFromResourceEx = user32.NewProc("CreateIconFromResourceEx")
	pCreateDIBSection         = gdi32.NewProc("CreateDIBSection")
)

const (
	iconVersion3  = 0x30000
	dibRGBColors  = 0
	biRGB         = 0
	bmpHeaderSize = 40
)

// embeddedIcon maps an asset file name to its baked-in bytes.
func embeddedIcon(name string) []byte {
	switch name {
	case "icon-app.ico":
		return embedded.AppIcon
	case "icon-tray.ico":
		return embedded.TrayIcon
	case "icon-rec.ico":
		return embedded.RecIcon
	}
	return nil
}

// iconFromMemory decodes one size out of ICO bytes. It picks the exact
// entry when present, otherwise the smallest entry at or above the request,
// otherwise the largest — Windows scales from there.
func iconFromMemory(data []byte, size int) syscall.Handle {
	if len(data) < 6 || binary.LittleEndian.Uint16(data[0:2]) != 0 ||
		binary.LittleEndian.Uint16(data[2:4]) != 1 {
		return 0
	}
	count := int(binary.LittleEndian.Uint16(data[4:6]))
	if count <= 0 || len(data) < 6+16*count {
		return 0
	}
	best, bestW := -1, 0
	for i := 0; i < count; i++ {
		e := data[6+16*i : 6+16*i+16]
		w := int(e[0])
		if w == 0 {
			w = 256
		}
		if w == size {
			best = i
			break
		}
		if w > size && (best < 0 || w < bestW) {
			best, bestW = i, w
		}
	}
	if best < 0 {
		// Nothing at or above the request: take the largest entry.
		for i := 0; i < count; i++ {
			w := int(data[6+16*i])
			if w == 0 {
				w = 256
			}
			if best < 0 || w > bestW {
				best, bestW = i, w
			}
		}
	}
	e := data[6+16*best : 6+16*best+16]
	length := int(binary.LittleEndian.Uint32(e[8:12]))
	offset := int(binary.LittleEndian.Uint32(e[12:16]))
	if length <= 0 || offset < 0 || offset+length > len(data) {
		return 0
	}
	img := data[offset : offset+length]
	h, _, _ := pCreateIconFromResourceEx.Call(
		uintptr(unsafe.Pointer(&img[0])), uintptr(length),
		1, uintptr(iconVersion3), uintptr(size), uintptr(size), 0)
	runtime.KeepAlive(img)
	return syscall.Handle(h)
}

// bitmapFromMemory decodes an uncompressed 24/32-bit BMP into a DIB section
// the window blits like the file-loaded bitmap it replaces.
func bitmapFromMemory(data []byte) syscall.Handle {
	if len(data) < 54 || data[0] != 'B' || data[1] != 'M' {
		return 0
	}
	offBits := int(binary.LittleEndian.Uint32(data[10:14]))
	if binary.LittleEndian.Uint32(data[14:18]) != bmpHeaderSize {
		return 0
	}
	width := int(int32(binary.LittleEndian.Uint32(data[18:22])))
	height := int(int32(binary.LittleEndian.Uint32(data[22:26])))
	planes := binary.LittleEndian.Uint16(data[26:28])
	bpp := binary.LittleEndian.Uint16(data[28:30])
	comp := binary.LittleEndian.Uint32(data[30:34])
	if width <= 0 || height == 0 || planes != 1 ||
		(bpp != 24 && bpp != 32) || comp != biRGB {
		return 0
	}
	if height < 0 {
		height = -height
	}
	stride := ((width*int(bpp) + 31) / 32) * 4
	need := stride * height
	if offBits < 0 || offBits+need > len(data) {
		return 0
	}
	var info [bmpHeaderSize]byte
	copy(info[:], data[14:14+bmpHeaderSize])
	var bits uintptr
	h, _, _ := pCreateDIBSection.Call(0, uintptr(unsafe.Pointer(&info[0])),
		uintptr(dibRGBColors), uintptr(unsafe.Pointer(&bits)), 0, 0)
	runtime.KeepAlive(info)
	if h == 0 || bits == 0 {
		return 0
	}
	dst := unsafe.Slice((*byte)(unsafe.Pointer(bits)), need)
	copy(dst, data[offBits:offBits+need])
	runtime.KeepAlive(data)
	return syscall.Handle(h)
}
