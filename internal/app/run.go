package app

import (
	"screenrec/internal/ui"
)

// Run starts the recorder's user interface.
//
// The graphical control window is plain Win32 built with raw syscalls, so
// this build needs no C toolchain and no toolkit: ui owns the window, rec
// owns the recording, and this wrapper keeps the command's entry point small.
func Run() error {
	return ui.Run(ui.Deps{Doctor: Doctor})
}
