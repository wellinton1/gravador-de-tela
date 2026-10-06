//go:build windows

// Package ui is the recorder's control window: a plain Win32 dialog built
// with raw syscalls, no cgo and no toolkit.
//
// Everything crosses into user32 through the lazy procs below, and everything
// the recording goroutines report crosses back onto the UI thread through a
// drain message: workers never touch a window handle, they queue closures and
// post WM_APP_DRAIN, and the window procedure runs them. Passing Go pointers
// through message parameters would pin the garbage collector to the message
// queue, so the queue owns the closures instead.
package ui

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"screenrec/internal/native"
	"screenrec/internal/rec"
)

// Deps are the pieces of the program the window drives. Doctor lives in app,
// and passing it in keeps this package from importing app back.
type Deps struct {
	Doctor func(w io.Writer) error
}

const (
	wsOverlapped    = 0x00000000
	wsCaption       = 0x00C00000
	wsSysMenu       = 0x00080000
	wsMinimizeBox   = 0x00020000
	wsChild         = 0x40000000
	wsVisible       = 0x10000000
	wsBorder        = 0x00800000
	wsVScroll       = 0x00200000
	wsTabStop       = 0x00010000
	wsGroup         = 0x00020000
	wsDisabled      = 0x08000000
	esAutohscroll   = 0x0080
	bsDefPushbtn    = 0x0001
	bsAutoCheckbox  = 0x0003
	cbsDropdownlist = 0x0003
	cbsHasStrings   = 0x0200
	lbsNointegral   = 0x0100
	lbsHasStrings   = 0x0040

	swShow = 5

	wmCreate     = 0x0001
	wmDestroy    = 0x0002
	wmClose      = 0x0010
	wmCommand    = 0x0111
	wmTimer      = 0x0113
	wmSetFont    = 0x0030
	wmGetText    = 0x000D
	wmGetTextLen = 0x000E

	bmGetCheck   = 0x00F0
	bmSetCheck   = 0x00F1
	bstChecked   = 1
	bstUnchecked = 0

	cbGetCount = 0x0146

	cbAddString   = 0x0143
	cbGetCurSel   = 0x0147
	cbSetCurSel   = 0x014E
	cbGetItemData = 0x0150
	cbSetItemData = 0x0151
	cbReset       = 0x014B

	lbAddString   = 0x0180
	lbGetCount    = 0x018B
	lbDelete      = 0x0182
	lbSetTopIndex = 0x019C

	bnClicked = 0

	colorBtnFace   = 15
	idcArrow       = 32512
	defaultGUIFont = 17

	wmApp      = 0x8000
	wmDrain    = 0x8001
	timerID    = 1
	beatID     = 2
	maxLogLine = 400

	idStart      = 101
	idStop       = 102
	idDiag       = 103
	idCombo      = 104
	idCheck      = 105
	idDir        = 106
	idStatus     = 107
	idLog        = 108
	idFPS        = 109
	idQuality    = 110
	idCountdown  = 111
	idLimit      = 112
	idClick      = 113
	idHalo       = 114
	idBrowse     = 115
	idRegionBtn  = 116
	idRegionClr  = 117
	idRegionLbl  = 118
	idSnap       = 119
	idOpen       = 120

	wmSize        = 0x0005
	sizeMinimized = 1
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")

	pRegisterClassExW = user32.NewProc("RegisterClassExW")
	pCreateWindowExW  = user32.NewProc("CreateWindowExW")
	pDefWindowProcW   = user32.NewProc("DefWindowProcW")
	pGetMessageW      = user32.NewProc("GetMessageW")
	pTranslateMessage = user32.NewProc("TranslateMessage")
	pDispatchMessageW = user32.NewProc("DispatchMessageW")
	pPostQuitMessage  = user32.NewProc("PostQuitMessage")
	pShowWindow       = user32.NewProc("ShowWindow")
	pUpdateWindow     = user32.NewProc("UpdateWindow")
	pSetWindowTextW   = user32.NewProc("SetWindowTextW")
	pSendMessageW     = user32.NewProc("SendMessageW")
	pPostMessageW     = user32.NewProc("PostMessageW")
	pEnableWindow     = user32.NewProc("EnableWindow")
	pDestroyWindow    = user32.NewProc("DestroyWindow")
	pLoadCursorW      = user32.NewProc("LoadCursorW")
	pSetTimer         = user32.NewProc("SetTimer")
	pKillTimer        = user32.NewProc("KillTimer")
	pGetStockObject   = gdi32.NewProc("GetStockObject")
	pGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
)

type point struct{ x, y int32 }

type winMsg struct {
	hwnd    syscall.Handle
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

type wndClassEx struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   syscall.Handle
	icon       syscall.Handle
	cursor     syscall.Handle
	background syscall.Handle
	menuName   *uint16
	className  *uint16
	iconSm     syscall.Handle
}

type state struct {
	hwnd      syscall.Handle
	combo     syscall.Handle
	check     syscall.Handle
	dir       syscall.Handle
	start     syscall.Handle
	stop      syscall.Handle
	diag      syscall.Handle
	status    syscall.Handle
	log       syscall.Handle
	fps       syscall.Handle
	quality   syscall.Handle
	countdown syscall.Handle
	limit     syscall.Handle
	click     syscall.Handle
	halo      syscall.Handle
	browse    syscall.Handle
	regionBtn syscall.Handle
	regionClr syscall.Handle
	regionLbl syscall.Handle
	snap      syscall.Handle
	open      syscall.Handle

	mu      sync.Mutex
	actions []func()
	pending int // log lines currently in the box

	// rec and starting cross threads (UI owns them, the watchdog reads
	// them), so they are atomic; everything else below is UI-thread-only.
	rec       atomic.Pointer[rec.Recorder]
	starting  atomic.Bool
	quitAfter bool
	doctor    func(w io.Writer) error

	region    native.Rect
	hasRegion bool
	selecting bool
	lastPath  string
	countGen  atomic.Int64
	settings  Settings
	// bg is the dimmed art bitmap (0 = plain gray). UI thread only.
	bg syscall.Handle
}

var st state
var wndProcAddr uintptr

// lastUiTick is the last time the window procedure ran anything, in Unix
// nanos. A watchdog goroutine compares it against the clock while a
// recording is active: the UI thread has no blocking calls by construction,
// so 15 silent seconds mean it is stuck, and the dump it writes names the
// exact call instead of leaving another "it just froze" mystery.
var lastUiTick atomic.Int64
var watchdogDumped atomic.Bool

func touchUiTick() { lastUiTick.Store(time.Now().UnixNano()) }

func watchdog() {
	for {
		time.Sleep(2 * time.Second)
		silent := time.Since(time.Unix(0, lastUiTick.Load()))
		// A 1 s heartbeat timer keeps a healthy window procedure busy, so
		// ten silent seconds mean the thread is stuck, idle or recording.
		if silent < 10*time.Second {
			watchdogDumped.Store(false)
			continue
		}
		if !watchdogDumped.CompareAndSwap(false, true) {
			continue
		}
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		path := filepath.Join(os.TempDir(), "gravador-ui-hang.txt")
		detail := fmt.Sprintf("UI thread silent for %v; goroutine dump follows:\n%s",
			silent, buf[:n])
		os.WriteFile(path, []byte(detail), 0o644)
		fmt.Fprintf(os.Stderr, "ui: window thread hung (%v); stacks in %s\n", silent, path)
	}
}

func sendMsg(hwnd syscall.Handle, msg uint32, w, l uintptr) uintptr {
	r, _, _ := pSendMessageW.Call(uintptr(hwnd), uintptr(msg), w, l)
	return r
}

func utf16(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

// A pointer that crosses into a syscall as a uintptr is invisible to the
// garbage collector from then on: without KeepAlive the buffer can be freed
// mid-call and user32 ends up reading whatever reused heap follows it. That
// flaky corruption hung this window inside SetWindowTextW, so every crossing
// below pins its buffer through the call.
func setText(hwnd syscall.Handle, s string) {
	p, _ := syscall.UTF16PtrFromString(s)
	pSetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(p)))
	runtime.KeepAlive(p)
}

func getText(hwnd syscall.Handle) string {
	n := sendMsg(hwnd, wmGetTextLen, 0, 0)
	buf := make([]uint16, n+2)
	sendMsg(hwnd, wmGetText, uintptr(n+1), uintptr(unsafe.Pointer(&buf[0])))
	return syscall.UTF16ToString(buf)
}

// queue runs f on the UI thread. Workers call this instead of touching a
// handle, which keeps every window call on the thread that owns the windows.
func queue(f func()) {
	st.mu.Lock()
	st.actions = append(st.actions, f)
	st.mu.Unlock()
	pPostMessageW.Call(uintptr(st.hwnd), uintptr(wmDrain), 0, 0)
}

func logLine(s string) {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		p, _ := syscall.UTF16PtrFromString(line)
		sendMsg(st.log, lbAddString, 0, uintptr(unsafe.Pointer(p)))
		runtime.KeepAlive(p)
		st.mu.Lock()
		st.pending++
		if st.pending > maxLogLine {
			for i := 0; i < 100; i++ {
				sendMsg(st.log, lbDelete, 0, 0)
			}
			st.pending -= 100
		}
		st.mu.Unlock()
	}
	count := sendMsg(st.log, lbGetCount, 0, 0)
	if count > 0 {
		sendMsg(st.log, lbSetTopIndex, count-1, 0)
	}
}

func setStatus(s string) { setText(st.status, s) }

func fmtDuration(d time.Duration) string {
	s := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}

func fmtBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

func mkChild(class, text string, style uint32, x, y, w, h int32, id uintptr,
	parent syscall.Handle, font uintptr) syscall.Handle {
	cls, _ := syscall.UTF16PtrFromString(class)
	txt, _ := syscall.UTF16PtrFromString(text)
	ret, _, _ := pCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(cls)),
		uintptr(unsafe.Pointer(txt)),
		uintptr(style),
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		uintptr(parent), id, 0, 0)
	hwnd := syscall.Handle(ret)
	if font != 0 {
		sendMsg(hwnd, wmSetFont, font, 1)
	}
	runtime.KeepAlive(cls)
	runtime.KeepAlive(txt)
	return hwnd
}

const childBase = wsChild | wsVisible

const comboStyle = childBase | wsVScroll | wsTabStop | cbsDropdownlist | cbsHasStrings

// comboFill lists labels with a numeric payload each, selecting sel.
func comboFill(hwnd syscall.Handle, labels []string, values []int, sel int) {
	for i, label := range labels {
		lp, _ := syscall.UTF16PtrFromString(label)
		at, _, _ := pSendMessageW.Call(uintptr(hwnd), uintptr(cbAddString), 0,
			uintptr(unsafe.Pointer(lp)))
		runtime.KeepAlive(lp)
		pSendMessageW.Call(uintptr(hwnd), uintptr(cbSetItemData), at, uintptr(values[i]))
	}
	if sel < 0 || sel >= len(labels) {
		sel = 0
	}
	sendMsg(hwnd, cbSetCurSel, uintptr(sel), 0)
}

// comboValue returns the payload of the current selection.
func comboValue(hwnd syscall.Handle) int {
	sel := sendMsg(hwnd, cbGetCurSel, 0, 0)
	if sel == 0xFFFFFFFF {
		return 0
	}
	return int(sendMsg(hwnd, cbGetItemData, sel, 0))
}

// comboSelect clamps the selection into range.
func comboSelect(hwnd syscall.Handle, sel, count int) {
	if sel < 0 || sel >= count {
		sel = 0
	}
	sendMsg(hwnd, cbSetCurSel, uintptr(sel), 0)
}

func checkSet(hwnd syscall.Handle, on bool) {
	if on {
		sendMsg(hwnd, bmSetCheck, bstChecked, 0)
	} else {
		sendMsg(hwnd, bmSetCheck, bstUnchecked, 0)
	}
}

func buildControls(hwnd syscall.Handle) {
	font, _, _ := pGetStockObject.Call(defaultGUIFont)

	mkChild("STATIC", "Monitor:", 0x50000000, 12, 14, 60, 20, 0, hwnd, font)
	st.combo = mkChild("COMBOBOX", "", comboStyle, 80, 10, 380, 200, idCombo, hwnd, font)

	st.check = mkChild("BUTTON", "Capturar áudio do sistema",
		childBase|wsTabStop|bsAutoCheckbox, 12, 40, 240, 22, idCheck, hwnd, font)
	mkChild("STATIC", "FPS:", 0x50000000, 258, 42, 30, 20, 0, hwnd, font)
	st.fps = mkChild("COMBOBOX", "", comboStyle, 290, 38, 60, 110, idFPS, hwnd, font)
	comboFill(st.fps, []string{"15", "30", "60"}, []int{15, 30, 60}, 1)
	mkChild("STATIC", "Qualidade:", 0x50000000, 356, 42, 60, 20, 0, hwnd, font)
	st.quality = mkChild("COMBOBOX", "", comboStyle, 418, 38, 54, 110, idQuality, hwnd, font)
	comboFill(st.quality, []string{"Baixa", "Média", "Alta"}, []int{0, 1, 2}, 1)

	mkChild("STATIC", "Contagem:", 0x50000000, 12, 66, 70, 20, 0, hwnd, font)
	st.countdown = mkChild("COMBOBOX", "", comboStyle, 86, 62, 96, 110, idCountdown, hwnd, font)
	comboFill(st.countdown, []string{"Desligada", "3 s", "5 s", "10 s"}, []int{0, 3, 5, 10}, 0)
	mkChild("STATIC", "Limite:", 0x50000000, 188, 66, 44, 20, 0, hwnd, font)
	st.limit = mkChild("COMBOBOX", "", comboStyle, 236, 62, 104, 130, idLimit, hwnd, font)
	comboFill(st.limit, []string{"Desligado", "5 min", "15 min", "30 min", "1 h", "2 h"},
		[]int{0, 5, 15, 30, 60, 120}, 0)
	st.click = mkChild("BUTTON", "Clique",
		childBase|wsTabStop|bsAutoCheckbox, 348, 62, 58, 22, idClick, hwnd, font)
	st.halo = mkChild("BUTTON", "Halo",
		childBase|wsTabStop|bsAutoCheckbox, 412, 62, 60, 22, idHalo, hwnd, font)

	mkChild("STATIC", "Pasta de destino:", 0x50000000, 12, 90, 448, 18, 0, hwnd, font)
	st.dir = mkChild("EDIT", defaultDir(),
		childBase|wsBorder|wsTabStop|esAutohscroll, 12, 108, 360, 24, idDir, hwnd, font)
	st.browse = mkChild("BUTTON", "Procurar",
		childBase|wsTabStop, 376, 106, 84, 26, idBrowse, hwnd, font)

	mkChild("STATIC", "Área:", 0x50000000, 12, 140, 40, 20, 0, hwnd, font)
	st.regionLbl = mkChild("STATIC", "tela cheia", 0x50000000, 56, 140, 196, 20, idRegionLbl, hwnd, font)
	st.regionBtn = mkChild("BUTTON", "Selecionar área",
		childBase|wsTabStop, 256, 136, 110, 26, idRegionBtn, hwnd, font)
	st.regionClr = mkChild("BUTTON", "Tela cheia",
		childBase|wsTabStop, 370, 136, 90, 26, idRegionClr, hwnd, font)

	st.start = mkChild("BUTTON", "Iniciar gravação",
		childBase|wsTabStop|bsDefPushbtn, 12, 170, 100, 32, idStart, hwnd, font)
	st.stop = mkChild("BUTTON", "Parar",
		childBase|wsTabStop|wsDisabled, 118, 170, 80, 32, idStop, hwnd, font)
	st.snap = mkChild("BUTTON", "Foto",
		childBase|wsTabStop, 204, 170, 80, 32, idSnap, hwnd, font)
	st.open = mkChild("BUTTON", "Abrir",
		childBase|wsTabStop, 290, 170, 80, 32, idOpen, hwnd, font)
	st.diag = mkChild("BUTTON", "Diagnóstico",
		childBase|wsTabStop, 376, 170, 84, 32, idDiag, hwnd, font)

	st.status = mkChild("STATIC", "Pronto.", 0x50000000, 12, 208, 448, 20, idStatus, hwnd, font)
	st.log = mkChild("LISTBOX", "",
		childBase|wsBorder|wsVScroll|wsTabStop|lbsNointegral|lbsHasStrings,
		12, 232, 448, 252, idLog, hwnd, font)

	fillDisplays()
	applySettings(LoadSettings())
	st.bg = loadBackground()
}

// applySettings pushes stored choices into the controls.
func applySettings(s Settings) {
	st.settings = s
	if s.Dir != "" {
		setText(st.dir, s.Dir)
	}
	checkSet(st.check, s.WithAudio)
	checkSet(st.click, s.ShowClick)
	checkSet(st.halo, s.Halo)
	comboSelect(st.fps, s.FPS, 3)
	comboSelect(st.quality, s.Quality, 3)
	comboSelect(st.countdown, s.Countdown, 4)
	comboSelect(st.limit, s.LimitMin, 6)
	count := int(sendMsg(st.combo, cbGetCount, 0, 0))
	comboSelect(st.combo, s.Display, count)
	updateRegionLabel()
}

// saveSettingsFromUI gathers the controls into the stored file.
func saveSettingsFromUI() {
	s := Settings{
		Dir:       strings.TrimSpace(getText(st.dir)),
		WithAudio: sendMsg(st.check, bmGetCheck, 0, 0) == bstChecked,
		ShowClick: sendMsg(st.click, bmGetCheck, 0, 0) == bstChecked,
		Halo:      sendMsg(st.halo, bmGetCheck, 0, 0) == bstChecked,
		FPS:       int(sendMsg(st.fps, cbGetCurSel, 0, 0)),
		Quality:   int(sendMsg(st.quality, cbGetCurSel, 0, 0)),
		Countdown: int(sendMsg(st.countdown, cbGetCurSel, 0, 0)),
		LimitMin:  int(sendMsg(st.limit, cbGetCurSel, 0, 0)),
		Display:   int(sendMsg(st.combo, cbGetCurSel, 0, 0)),
	}
	if s.FPS == int(0xFFFFFFFF) {
		s.FPS = 1
	}
	st.settings = s
	s.Save()
}

// updateRegionLabel renders the area line: full screen or WxH at (x,y).
func updateRegionLabel() {
	if !st.hasRegion {
		setText(st.regionLbl, "tela cheia")
		return
	}
	r := st.region
	setText(st.regionLbl, fmt.Sprintf("%dx%d em (%d,%d)", r.Width(), r.Height(), r.Left, r.Top))
}

// fillDisplays lists every output the native layer reports. The combo's item
// data packs adapter and output indices, so the selection maps back to a
// capture target without reparsing the label.
func fillDisplays() {
	list, err := native.Adapters()
	if err != nil {
		queue(func() { logLine(fmt.Sprintf("monitores: %v", err)) })
		return
	}
	for _, a := range list {
		label := fmt.Sprintf("[%d] %s %s", a.Index, a.Description, a.Bounds)
		if a.Primary {
			label += " (principal)"
		}
		lp, _ := syscall.UTF16PtrFromString(label)
		i := sendMsg(st.combo, cbAddString, 0, uintptr(unsafe.Pointer(lp)))
		runtime.KeepAlive(lp)
		packed := uintptr(a.Adapter<<16 | a.Output)
		sendMsg(st.combo, cbSetItemData, i, packed)
		if a.Primary {
			sendMsg(st.combo, 0x014E, i, 0) // CB_SETCURSEL
		}
	}
	if sendMsg(st.combo, cbGetCurSel, 0, 0) == 0xFFFFFFFF {
		sendMsg(st.combo, 0x014E, 0, 0)
	}
}

func defaultDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, "Videos")
	}
	return "."
}

func selectedDisplay() (adapter, output uint32) {
	sel := sendMsg(st.combo, cbGetCurSel, 0, 0)
	if sel == 0xFFFFFFFF {
		return 0, 0
	}
	packed := uint32(sendMsg(st.combo, cbGetItemData, sel, 0))
	return packed >> 16, packed & 0xFFFF
}

var qualityBitrate = []int{400000, 650000, 1200000}
var qualityLevel = []int{60, 70, 85}
var countdownSecs = []int{0, 3, 5, 10}
var limitMinutes = []int{0, 5, 15, 30, 60, 120}
var fpsValues = []int{15, 30, 60}

func comboIndexToValue(hwnd syscall.Handle, table []int) int {
	sel := int(sendMsg(hwnd, cbGetCurSel, 0, 0))
	if sel < 0 || sel >= len(table) {
		return table[0]
	}
	return table[sel]
}

func onStart() {
	if st.rec.Load() != nil {
		return
	}
	adapter, output := selectedDisplay()
	withAudio := sendMsg(st.check, bmGetCheck, 0, 0) == bstChecked
	dir := strings.TrimSpace(getText(st.dir))
	if dir == "" {
		dir = defaultDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logLine(fmt.Sprintf("pasta inválida: %v", err))
		return
	}
	limitMin := comboIndexToValue(st.limit, limitMinutes)
	var stopAfter time.Duration
	if limitMin > 0 {
		stopAfter = time.Duration(limitMin) * time.Minute
	}
	opts := rec.Options{
		Adapter:      adapter,
		Output:       output,
		FPS:          comboIndexToValue(st.fps, fpsValues),
		Bitrate:      comboIndexToValue(st.quality, qualityBitrate),
		VideoQuality: comboIndexToValue(st.quality, qualityLevel),
		WithAudio:    withAudio,
		ShowClick: sendMsg(st.click, bmGetCheck, 0, 0) == bstChecked,
		Halo:      sendMsg(st.halo, bmGetCheck, 0, 0) == bstChecked,
		StopAfter: stopAfter,
		// The edit holds a folder; the file inside it is timestamped.
		OutputPath: timestampedIn(dir),
	}
	if st.hasRegion {
		opts.Region = st.region
		opts.HasRegion = true
	}
	saveSettingsFromUI()

	pEnableWindow.Call(uintptr(st.start), 0)
	pEnableWindow.Call(uintptr(st.stop), 1)
	pEnableWindow.Call(uintptr(st.combo), 0)
	st.starting.Store(true)
	// Countdown (vokoscreen style): announce each second on the status line,
	// then hand over to the same starter. Parar cancels via the generation.
	gen := st.countGen.Add(1)
	secs := comboIndexToValue(st.countdown, countdownSecs)
	go func() {
		for s := secs; s > 0; s-- {
			if st.countGen.Load() != gen {
				return
			}
			n := s
			queue(func() { setStatus(fmt.Sprintf("Gravando em %d…", n)) })
			time.Sleep(time.Second)
		}
		if st.countGen.Load() != gen {
			return
		}
		queue(func() { setStatus("Iniciando captura…") })
		logLine("Iniciando captura…")
		r, note, err := rec.Start(opts)
		queue(func() {
			st.starting.Store(false)
			if err != nil {
				logLine(fmt.Sprintf("falhou: %v", err))
				pEnableWindow.Call(uintptr(st.start), 1)
				pEnableWindow.Call(uintptr(st.stop), 0)
				pEnableWindow.Call(uintptr(st.combo), 1)
				if st.quitAfter {
					pDestroyWindow.Call(uintptr(st.hwnd))
				}
				return
			}
			st.rec.Store(r)
			traySetIcon(st.hwnd, "icon-rec.ico")
			s := r.Stats()
			logLine(fmt.Sprintf("gravando em %s (%dx%d, %s)", s.OutputPath, s.Width, s.Height, s.Backend))
			if note != "" {
				logLine("nota: " + note)
			}
			setStatus(fmt.Sprintf("Gravando em %s (%s)…", s.OutputPath, s.Codec))
			// The window may have been asked to close while capture was
			// opening; stopping right away finalises the file instead of
			// leaking a recording nobody owns.
			if st.quitAfter {
				onStop()
				return
			}
		})
	}()
}

func timestampedIn(dir string) string {
	return native.DefaultOutputPath(dir, native.ContainerAVI)
}

func onStop() {
	// A pending countdown belongs to a previous generation from here on.
	st.countGen.Add(1)
	r := st.rec.Load()
	if r == nil {
		return
	}
	pEnableWindow.Call(uintptr(st.stop), 0)
	go func() {
		err := r.Stop()
		s := r.Stats()
		queue(func() {
			st.rec.Store(nil)
			st.lastPath = s.OutputPath
			traySetIcon(st.hwnd, "icon-tray.ico")
			pEnableWindow.Call(uintptr(st.start), 1)
			pEnableWindow.Call(uintptr(st.combo), 1)
			if err != nil {
				logLine(fmt.Sprintf("parou com erro: %v", err))
			} else if info, serr := os.Stat(s.OutputPath); serr == nil {
				logLine(fmt.Sprintf("salvo: %s (%s, %d frames)",
					s.OutputPath, fmtBytes(info.Size()), s.VideoFrames))
			} else {
				logLine(fmt.Sprintf("salvo: %s (%d frames)", s.OutputPath, s.VideoFrames))
			}
			setStatus("Pronto.")
			if st.quitAfter {
				pDestroyWindow.Call(uintptr(st.hwnd))
			}
		})
	}()
}

// onSnapshot saves one PNG of the selected display, like vokoscreen's
// snapshot button, without disturbing any recording.
func onSnapshot() {
	adapter, output := selectedDisplay()
	go func() {
		path, err := snapshotSave(adapter, output)
		queue(func() {
			if err != nil {
				logLine(fmt.Sprintf("foto falhou: %v", err))
				return
			}
			st.lastPath = path
			logLine(fmt.Sprintf("foto salva: %s", path))
		})
	}()
}

// snapshotSave stores a timestamped PNG in Pictures through the shared
// native worker.
func snapshotSave(adapter, output uint32) (string, error) {
	dir := picturesDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		dir = "."
	}
	path := filepath.Join(dir, fmt.Sprintf("foto-%s.png", time.Now().Format("20060102-150405")))
	return native.SaveSnapshot(adapter, output, path)
}

func picturesDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, "Pictures")
	}
	return "."
}

// onRegionSelect opens the drag-to-select overlay; recording must be idle.
func onRegionSelect() {
	if st.rec.Load() != nil || st.selecting {
		if st.rec.Load() != nil {
			logLine("pare a gravação para trocar a área")
		}
		return
	}
	st.selecting = true
	logLine("arraste para selecionar a área (Esc cancela)…")
	showRegionOverlay()
}

// onRegionDone lands the overlay result back on the control window.
func onRegionDone(region native.Rect, ok bool) {
	st.selecting = false
	if !ok {
		logLine("área cancelada")
		return
	}
	st.region = region
	st.hasRegion = true
	updateRegionLabel()
	saveSettingsFromUI()
	logLine(fmt.Sprintf("área: %dx%d em (%d,%d)", region.Width(), region.Height(), region.Left, region.Top))
}

// onRegionClear goes back to the full screen.
func onRegionClear() {
	st.hasRegion = false
	updateRegionLabel()
	saveSettingsFromUI()
	logLine("área limpa: tela cheia")
}

// onBrowse picks the destination folder with the system dialog.
func onBrowse() {
	current := strings.TrimSpace(getText(st.dir))
	if current == "" {
		current = defaultDir()
	}
	if dir, ok := browseFolder(st.hwnd, current); ok {
		setText(st.dir, dir)
		saveSettingsFromUI()
	}
}

// onOpen plays the last recording (or opens Videos) in the system player.
func onOpen() {
	openLastRecording()
}

func onDiag() {
	pEnableWindow.Call(uintptr(st.diag), 0)
	go func() {
		var buf bytes.Buffer
		err := st.doctor(&buf)
		lines := buf.String()
		queue(func() {
			for _, line := range strings.Split(lines, "\n") {
				if strings.TrimSpace(line) != "" {
					logLine(line)
				}
			}
			if err != nil {
				logLine(fmt.Sprintf("diagnóstico: %v", err))
			}
			pEnableWindow.Call(uintptr(st.diag), 1)
		})
	}()
}

// The callback signature is all-uintptr as syscall.NewCallback requires;
// narrowing any parameter (e.g. uint32 msg) reads a partial register slot
// and corrupts message dispatch on some calls.
func wndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	touchUiTick()
	switch uint32(msg) {
	case wmCreate:
		st.hwnd = syscall.Handle(hwnd)
		buildControls(syscall.Handle(hwnd))
		return 0
	case wmDrain:
		st.mu.Lock()
		actions := st.actions
		st.actions = nil
		st.mu.Unlock()
		for _, f := range actions {
			f()
		}
		return 0
	case wmCommand:
		id := uint32(wParam & 0xFFFF)
		code := uint32((wParam >> 16) & 0xFFFF)
		if code == bnClicked {
			switch id {
			case idStart:
				onStart()
			case idStop:
				onStop()
			case idDiag:
				onDiag()
			case idBrowse:
				onBrowse()
			case idRegionBtn:
				onRegionSelect()
			case idRegionClr:
				onRegionClear()
			case idSnap:
				onSnapshot()
			case idOpen:
				onOpen()
			}
		}
		return 0
	case wmTray:
		// lParam carries the mouse message over the icon.
		switch uint32(lParam) {
		case 0x205: // WM_RBUTTONUP: menu.
			trayMenu(syscall.Handle(hwnd), st.rec.Load() != nil)
		case 0x203: // WM_LBUTTONDBLCLK: restore.
			showMainWindow()
		}
		return 0
	case wmHotkey:
		switch uint32(wParam) {
		case hotStartStop:
			if st.rec.Load() != nil {
				onStop()
			} else if !st.starting.Load() {
				onStart()
			}
		case hotSnapshot:
			onSnapshot()
		}
		return 0
	case wmSize:
		// Minimising hides to the tray instead of the taskbar; the window
		// comes back from the icon or the F9 toggle. A recording survives.
		if wParam == sizeMinimized {
			pShowWindow.Call(hwnd, swHide)
			return 0
		}
	case wmTimer:
		// Only the heartbeat lands here now. The per-tick status readout
		// used to live here and is gone on purpose: four SetWindowTextW
		// calls a second turned a flaky native hang into a certain one by
		// rolling the dice hundreds of times per recording. Progress is
		// reported on state changes instead (start/stop/failure lines in
		// the log carry frame counts and sizes); a frozen-looking label
		// never again costs the whole window.
		return 0
	case wmEraseBkgnd:
		// wParam is the target DC. A handled erase returns nonzero and
		// keeps the class brush out of it.
		if st.bg != 0 {
			if paintBackground(syscall.Handle(hwnd), wParam) != 0 {
				return 1
			}
		}
	case wmCtlColorStatic:
		// Labels float over the art when there is art; otherwise the
		// default gray handling runs untouched.
		if st.bg != 0 {
			return staticColors(wParam)
		}
	case wmClose:
		// The X button hides to the tray; only Sair in the tray menu really
		// quits, and it finalises any recording first.
		pShowWindow.Call(hwnd, swHide)
		return 0
	case wmDestroy:
		pKillTimer.Call(hwnd, beatID)
		trayRemove(syscall.Handle(hwnd))
		unregisterHotkeys(syscall.Handle(hwnd))
		saveSettingsFromUI()
		pPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

// Run opens the control window and pumps messages until it closes.
func Run(deps Deps) error {
	// Windows, timers, sent messages and the tray icon all belong to the
	// thread that created them: if this goroutine ever migrates, its
	// GetMessage starts pumping a foreign empty queue while the real one —
	// with the windows, the timers and every click — starves unwatched.
	// That migration is exactly the "records fine, window freezes after a
	// while" hang, so the UI thread is pinned for the life of the process.
	runtime.LockOSThread()
	st.doctor = deps.Doctor
	wndProcAddr = syscall.NewCallback(wndProc)

	instance, _, _ := pGetModuleHandleW.Call(0)
	cursor, _, _ := pLoadCursorW.Call(0, uintptr(idcArrow))
	className := utf16("ScreenRecMain")
	title := utf16("Gravador de Tela")

	var cls wndClassEx
	cls.size = uint32(unsafe.Sizeof(cls))
	cls.wndProc = wndProcAddr
	cls.instance = syscall.Handle(instance)
	cls.cursor = syscall.Handle(cursor)
	cls.background = syscall.Handle(colorBtnFace + 1)
	cls.className = className
	if r, _, _ := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&cls))); r == 0 {
		return fmt.Errorf("ui: RegisterClassExW failed")
	}
	runtime.KeepAlive(cls)

	// 484x570 fits the controls above with room for the log; the frame is
	// fixed (no thick frame, no maximize box) so the layout never reflows.
	hwnd, _, _ := pCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		uintptr(wsOverlapped|wsCaption|wsSysMenu|wsMinimizeBox|wsVisible),
		0x80000000, 0x80000000, 484, 570,
		0, 0, instance, 0)
	runtime.KeepAlive(className)
	runtime.KeepAlive(title)
	if hwnd == 0 {
		return fmt.Errorf("ui: CreateWindowExW failed")
	}
	trayAdd(syscall.Handle(hwnd))
	setWindowIcons(syscall.Handle(hwnd))
	registerHotkeys(syscall.Handle(hwnd))
	pShowWindow.Call(hwnd, swShow)
	pUpdateWindow.Call(hwnd)

	touchUiTick()
	go watchdog()
	// The heartbeat exists so the watchdog can tell a stuck thread from an
	// idle one: a pumping window sees this every second, hang or no hang.
	pSetTimer.Call(uintptr(hwnd), beatID, 1000, 0)

	var m winMsg
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	return nil
}
