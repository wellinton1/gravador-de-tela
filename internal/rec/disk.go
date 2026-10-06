package rec

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

var procGetDiskFreeSpaceExW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

const (
	// minStartBytes refuses to start below half a gigabyte free: an hour at
	// the default ceiling needs ~300 MB plus container slack.
	minStartBytes = 500 << 20
	// minKeepBytes stops a running recording before the disk itself fills,
	// which would corrupt the file being written.
	minKeepBytes = 300 << 20
)

// freeBytes reports the bytes available to the caller on path's volume.
func freeBytes(path string) (uint64, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	r, _, _ := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&free)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)))
	_, _ = total, totalFree
	runtime.KeepAlive(p)
	if r == 0 {
		return 0, fmt.Errorf("não foi possível ler o espaço em %s", path)
	}
	return free, nil
}
