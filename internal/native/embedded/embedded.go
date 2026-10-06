// Package embedded carries the native library inside the executable so the
// program can ship as a single file.
//
// The bytes come from a copy of bin/recorder_native.dll placed next to this
// file by build-exe.ps1 before the Go build runs: go:embed cannot reach
// outside this directory, so that copy step is mandatory (a missing file
// fails the build with "cannot find file"). At startup the native package
// extracts these bytes next to the executable — or to a fallback directory
// when that folder is not writable — and loads them from there, because
// Windows only loads DLLs from files on disk, never from memory.
package embedded

import _ "embed"

// DLL is the full contents of recorder_native.dll, baked into the exe
// (about 100 KB). Refreshed by build-exe.ps1 on every executable build,
// so it always matches the sources the exe was built from: exe and DLL
// must always be rebuilt together, embedded or not.
var (
	//go:embed recorder_native.dll
	DLL []byte
)
