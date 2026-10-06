//go:build windows

package main

import (
	"os"
	"syscall"
)

var (
	modkernel32       = syscall.NewLazyDLL("kernel32.dll")
	procAttachConsole = modkernel32.NewProc("AttachConsole")
	procGetStdHandle  = modkernel32.NewProc("GetStdHandle")
)

// attachParentConsole gives CLI invocations (doctor, record, help) a console
// to print to even though this binary is built GUI-subsystem, so a
// double-click opens only the control window with no black box behind it.
// Without the attach, stdout would vanish.
func attachParentConsole() {
	const attachParent = ^uint32(0) // ATTACH_PARENT_PROCESS
	ok, _, _ := procAttachConsole.Call(uintptr(attachParent))
	if ok == 0 {
		return
	}
	// Go bound the standard files at startup, before the attach; rebind
	// them to the console handles so prints land where the user looks.
	rebind(syscall.STD_OUTPUT_HANDLE, &os.Stdout, "stdout")
	rebind(syscall.STD_ERROR_HANDLE, &os.Stderr, "stderr")
	rebind(syscall.STD_INPUT_HANDLE, &os.Stdin, "stdin")
}

func rebind(std int32, dst **os.File, name string) {
	h, _, _ := procGetStdHandle.Call(uintptr(uint32(std)))
	if h == 0 || h == ^uintptr(0) {
		return
	}
	*dst = os.NewFile(h, "/dev/"+name)
}
