//go:build windows

package ui

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

// Systray: the window hides to the notification area instead of closing, so
// a recording survives the X button the way vokoscreen's does. The tray menu
// offers show, start/stop, snapshot, open-last and quit; a double-click
// restores the window.
//
// Hotkeys: F9 toggles recording, F10 takes a snapshot, anywhere in the
// system. They are registered on the control window and unregistered when it
// goes away; if the system refuses one (already taken), the buttons stay and
// a log line says so.
//
// Folder picker and "open file" also live here: they are small Shell calls
// the buttons share.

const (
	wmTray       = 0x8002
	wmHotkey     = 0x0312
	wmSetIcon    = 0x0080
	iconSmall    = 0
	iconBig      = 1
	imageIcon    = 1
	lrLoadFromFile = 0x10
	hotStartStop = 1
	hotSnapshot  = 2
	vkF9         = 0x78
	vkF10        = 0x79
	trayShow     = 201
	trayToggle   = 202
	traySnapshot = 203
	trayOpenLast = 204
	trayQuit     = 205

	nimAdd    = 0x0
	nimModify = 0x1
	nimDelete = 0x2

	nifMessage = 0x1
	nifIcon    = 0x2
	nifTip     = 0x4

	mfString    = 0x0
	mfSeparator = 0x800
	mfGrayed    = 0x1

	tpmLeftAlign   = 0x0
	tpmReturnCmd   = 0x100
	tpmRightButton = 0x2

	idiApplication = 32512

	bifReturnOnlyFSDirs = 0x1
	bifNewDialogStyle   = 0x40

	swShowNormal = 1
)

var (
	pshell32                = syscall.NewLazyDLL("shell32.dll")
	pShellNotifyIconW       = pshell32.NewProc("Shell_NotifyIconW")
	pShellExecuteW          = pshell32.NewProc("ShellExecuteW")
	pSHBrowseForFolderW     = pshell32.NewProc("SHBrowseForFolderW")
	pSHGetPathFromIDListW   = pshell32.NewProc("SHGetPathFromIDListW")
	pLoadIconW              = user32.NewProc("LoadIconW")
	pLoadImageW             = user32.NewProc("LoadImageW")
	pRegisterHotKey         = user32.NewProc("RegisterHotKey")
	pUnregisterHotKey       = user32.NewProc("UnregisterHotKey")
	pCreatePopupMenu        = user32.NewProc("CreatePopupMenu")
	pAppendMenuW            = user32.NewProc("AppendMenuW")
	pTrackPopupMenu         = user32.NewProc("TrackPopupMenu")
	pDestroyMenu            = user32.NewProc("DestroyMenu")
	pGetCursorPos           = user32.NewProc("GetCursorPos")
	pSetForegroundWindow    = user32.NewProc("SetForegroundWindow")
	pCoTaskMemFree          = syscall.NewLazyDLL("ole32.dll").NewProc("CoTaskMemFree")
)

type notifyIconData struct {
	size            uint32
	hwnd            syscall.Handle
	id              uint32
	flags           uint32
	callbackMessage uint32
	icon            syscall.Handle
	tip             [128]uint16
	state           uint32
	stateMask       uint32
	info            [256]uint16
	timeoutVersion  uint32
	infoTitle       [64]uint16
	infoFlags       uint32
	guidItem        [16]byte
	hBalloonIcon    syscall.Handle
}

type browseInfo struct {
	hwndOwner   syscall.Handle
	pidlRoot    uintptr
	displayName [260]uint16
	title       *uint16
	flags       uint32
	callback    uintptr
	lParam      uintptr
	image       int32
}

// assetsDir finds the deployed assets folder: next to the exe, in the
// working directory, or one up (running bin\recorder.exe from a checkout).
func assetsDir() string {
	exe, err := os.Executable()
	if err == nil {
		if dir := filepath.Join(filepath.Dir(exe), "assets"); dirExists(dir) {
			return dir
		}
		if dir := filepath.Join(filepath.Dir(exe), "..", "assets"); dirExists(dir) {
			return dir
		}
	}
	if dir := filepath.Join(".", "assets"); dirExists(dir) {
		return dir
	}
	return ""
}

func dirExists(dir string) bool {
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

// loadIconFile loads one size from an .ico on disk, or zero when assets are
// absent — every caller falls back to the stock icon, so a missing folder
// never breaks the window.
func loadIconFile(name string, size int) syscall.Handle {
	dir := assetsDir()
	if dir == "" {
		return 0
	}
	path, _ := syscall.UTF16PtrFromString(filepath.Join(dir, name))
	h, _, _ := pLoadImageW.Call(0, uintptr(unsafe.Pointer(path)),
		uintptr(imageIcon), uintptr(size), uintptr(size), uintptr(lrLoadFromFile))
	runtime.KeepAlive(path)
	return syscall.Handle(h)
}

// setWindowIcons gives the control window (and its taskbar button) the app
// art instead of the generic executable glyph.
func setWindowIcons(hwnd syscall.Handle) {
	if big := loadIconFile("icon-app.ico", 32); big != 0 {
		sendMsg(hwnd, wmSetIcon, iconBig, uintptr(big))
	}
	if small := loadIconFile("icon-app.ico", 16); small != 0 {
		sendMsg(hwnd, wmSetIcon, iconSmall, uintptr(small))
	}
}

// traySetIcon swaps the notification icon, e.g. to the red dot while
// recording. A missing file keeps the current icon instead of blanking it.
func traySetIcon(hwnd syscall.Handle, name string) {
	icon := loadIconFile(name, 16)
	if icon == 0 {
		return
	}
	var data notifyIconData
	data.size = uint32(unsafe.Sizeof(data))
	data.hwnd = hwnd
	data.id = 1
	data.flags = nifIcon
	data.icon = icon
	pShellNotifyIconW.Call(uintptr(nimModify), uintptr(unsafe.Pointer(&data)))
	runtime.KeepAlive(data)
}

// trayAdd puts the icon in the notification area; the window must exist.
func trayAdd(hwnd syscall.Handle) {
	icon, _, _ := pLoadIconW.Call(0, uintptr(idiApplication))
	if custom := loadIconFile("icon-tray.ico", 16); custom != 0 {
		icon = uintptr(custom)
	}
	tip, _ := syscall.UTF16FromString("Gravador de Tela")
	var data notifyIconData
	data.size = uint32(unsafe.Sizeof(data))
	data.hwnd = hwnd
	data.id = 1
	data.flags = nifMessage | nifIcon | nifTip
	data.callbackMessage = wmTray
	data.icon = syscall.Handle(icon)
	copy(data.tip[:], tip)
	pShellNotifyIconW.Call(uintptr(nimAdd), uintptr(unsafe.Pointer(&data)))
	runtime.KeepAlive(tip)
	runtime.KeepAlive(data)
}

// trayRemove takes the icon back out; call before the window dies.
func trayRemove(hwnd syscall.Handle) {
	var data notifyIconData
	data.size = uint32(unsafe.Sizeof(data))
	data.hwnd = hwnd
	data.id = 1
	pShellNotifyIconW.Call(uintptr(nimDelete), uintptr(unsafe.Pointer(&data)))
}

func appendMenuItem(menu uintptr, id uintptr, text string, enabled bool) {
	t, _ := syscall.UTF16PtrFromString(text)
	flags := uintptr(mfString)
	if !enabled {
		flags |= mfGrayed
	}
	pAppendMenuW.Call(menu, flags, id, uintptr(unsafe.Pointer(t)))
	runtime.KeepAlive(t)
}

func trayMenu(hwnd syscall.Handle, recording bool) {
	menu, _, _ := pCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer pDestroyMenu.Call(menu)
	appendMenuItem(menu, trayShow, "Abrir janela", true)
	toggle := "Iniciar gravação"
	if recording {
		toggle = "Parar gravação"
	}
	appendMenuItem(menu, trayToggle, toggle, true)
	appendMenuItem(menu, traySnapshot, "Tirar foto", true)
	appendMenuItem(menu, trayOpenLast, "Abrir última gravação", lastRecordingPath() != "")
	pAppendMenuW.Call(menu, mfSeparator, 0, 0)
	appendMenuItem(menu, trayQuit, "Sair", true)
	var pt point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	runtime.KeepAlive(pt)
	pSetForegroundWindow.Call(uintptr(hwnd))
	cmd, _, _ := pTrackPopupMenu.Call(menu, tpmLeftAlign|tpmRightButton|tpmReturnCmd,
		uintptr(pt.x), uintptr(pt.y), 0, uintptr(hwnd), 0)
	switch uint32(cmd) {
	case trayShow:
		showMainWindow()
	case trayToggle:
		queue(func() {
			if st.rec.Load() != nil {
				onStop()
			} else {
				onStart()
			}
		})
	case traySnapshot:
		queue(func() { onSnapshot() })
	case trayOpenLast:
		openLastRecording()
	case trayQuit:
		quitApp()
	}
}

// showMainWindow restores the control window from the tray.
func showMainWindow() {
	pShowWindow.Call(uintptr(st.hwnd), swShowNormal)
	pUpdateWindow.Call(uintptr(st.hwnd))
}

// openLastRecording plays the newest file of this session, or opens Videos
// when nothing was recorded yet. The system player does the playing; this
// is the pragmatic stand-in for a built-in player.
func openLastRecording() {
	path := lastRecordingPath()
	if path == "" {
		path = videosDir()
		if _, err := os.Stat(path); err != nil {
			path = "."
		}
	}
	verb, _ := syscall.UTF16PtrFromString("open")
	target, _ := syscall.UTF16PtrFromString(path)
	pShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(target)), 0, 0, swShowNormal)
	runtime.KeepAlive(verb)
	runtime.KeepAlive(target)
}

// lastRecordingPath is the file the current or previous recording wrote,
// for the tray and the Abrir button.
func lastRecordingPath() string {
	if r := st.rec.Load(); r != nil {
		return r.OutputPath()
	}
	return st.lastPath
}

// browseFolder opens the system folder picker and returns the choice.
func browseFolder(owner syscall.Handle, current string) (string, bool) {
	title, _ := syscall.UTF16PtrFromString("Escolha a pasta de destino")
	var info browseInfo
	info.hwndOwner = owner
	info.title = title
	info.flags = bifReturnOnlyFSDirs | bifNewDialogStyle
	pidl, _, _ := pSHBrowseForFolderW.Call(uintptr(unsafe.Pointer(&info)))
	runtime.KeepAlive(title)
	runtime.KeepAlive(info)
	if pidl == 0 {
		return "", false
	}
	defer pCoTaskMemFree.Call(pidl)
	var path [260]uint16
	ok, _, _ := pSHGetPathFromIDListW.Call(pidl, uintptr(unsafe.Pointer(&path[0])))
	runtime.KeepAlive(path)
	if ok == 0 {
		return "", false
	}
	return syscall.UTF16ToString(path[:]), true
}

// registerHotkeys binds F9 (record toggle) and F10 (snapshot) globally.
func registerHotkeys(hwnd syscall.Handle) {
	if r, _, _ := pRegisterHotKey.Call(uintptr(hwnd), hotStartStop, 0, vkF9); r == 0 {
		queue(func() { logLine("atalho F9 indisponível (já em uso)") })
	} else {
		queue(func() { logLine("atalhos: F9 inicia/para, F10 tira foto") })
	}
	if r, _, _ := pRegisterHotKey.Call(uintptr(hwnd), hotSnapshot, 0, vkF10); r == 0 {
		queue(func() { logLine("atalho F10 indisponível (já em uso)") })
	}
}

func unregisterHotkeys(hwnd syscall.Handle) {
	pUnregisterHotKey.Call(uintptr(hwnd), hotStartStop)
	pUnregisterHotKey.Call(uintptr(hwnd), hotSnapshot)
}

// quitApp stops any recording first (finalising the file), then leaves.
// It is the only path that destroys the window; the X button hides to tray.
func quitApp() {
	if st.rec.Load() != nil || st.starting.Load() {
		st.quitAfter = true
		if st.rec.Load() != nil {
			onStop()
			return
		}
		return
	}
	trayRemove(st.hwnd)
	unregisterHotkeys(st.hwnd)
	saveSettingsFromUI()
	pDestroyWindow.Call(uintptr(st.hwnd))
}

// videosDir mirrors rec's default so Abrir lands where files do.
func videosDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, "Videos")
	}
	return "."
}
