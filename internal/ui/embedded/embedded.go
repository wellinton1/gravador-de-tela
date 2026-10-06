// Package embedded carries the program art inside the executable so the
// tray/window icons and the background need no sidecar files.
//
// The bytes come from copies of assets/icon-*.ico and assets/bg.bmp placed
// next to this file by build-exe.ps1 before the Go build runs: go:embed
// cannot reach outside this directory, so that copy step is mandatory (a
// missing file fails the build with "cannot find file"). At startup the ui
// package decodes these bytes straight into GDI handles — icons through
// CreateIconFromResourceEx, the background through CreateDIBSection — so no
// temporary files are ever written. The on-disk assets folder remains as a
// fallback (handy when iterating on the art without rebuilding).
package embedded

import _ "embed"

var (
	//go:embed icon-app.ico
	AppIcon []byte
	//go:embed icon-tray.ico
	TrayIcon []byte
	//go:embed icon-rec.ico
	RecIcon []byte
	//go:embed bg.bmp
	Background []byte
)
