// Package native is the Go side of the boundary with recorder_native.dll.
//
// Everything low level lives in that library: adapter discovery, screen capture,
// Media Foundation encoding and WASAPI audio. This package is the only place
// that knows the C ABI, so the rest of the program deals in Go types.
//
// The library is loaded through the plain syscall package rather than cgo, which
// keeps the build free of a C toolchain and means a mismatch in the ABI shows up
// as an ordinary error rather than a build failure. The struct layouts below
// mirror native/include/recorder_native.h exactly and must be kept in step with
// it; the ABI version is checked at load time so a stale DLL is refused instead
// of being misread.
package native

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"screenrec/internal/native/embedded"
)

// abiVersion is the value of RECORDER_NATIVE_ABI in the header.
const abiVersion = 3

// Status values shared with the library.
const (
	StatusOK          = 0
	StatusFailed      = 1
	StatusTimeout     = 2
	StatusUnsupported = 3
	StatusLost        = 4
)

// Backends a capture session can report.
const (
	BackendNone = 0
	BackendDXGI = 1
	BackendGDI  = 2
)

// Capture flags matching RecorderCaptureFlag* in the header.
const (
	CaptureFlagNone      = 0
	CaptureFlagClickRing = 1
	CaptureFlagHalo      = 2
)

// Codecs and containers.
const (
	// H.264/H.265 are reserved for a future Media Foundation path; the native
	// layer refuses them until then, so new code must use CodecMjpeg.
	CodecH264  = 0
	CodecH265  = 1
	CodecMjpeg = 2

	ContainerMP4 = 0
	ContainerAVI = 1
)

const (
	adapterDescCap = 128
	adapterNameCap = 32
	encoderPathCap = 260
)

type rect struct {
	Left, Top, Right, Bottom int32
}

func (r rect) Width() int  { return int(r.Right - r.Left) }
func (r rect) Height() int { return int(r.Bottom - r.Top) }
func (r rect) Empty() bool { return r.Right <= r.Left || r.Bottom <= r.Top }

// String renders the rectangle the way the interface and the log show it.
func (r rect) String() string {
	return fmt.Sprintf("%dx%d at (%d,%d)", r.Width(), r.Height(), r.Left, r.Top)
}

type adapterInfo struct {
	StructSize        uint32
	AdapterIndex      uint32
	OutputIndex       uint32
	Bounds            rect
	Rotation          uint32
	RefreshHz         float32
	AttachedToDesktop uint32
	Primary           uint32
	CanDuplicate      uint32
	IsSoftware        uint32
	VendorID          uint32
	DeviceID          uint32
	Description       [adapterDescCap]byte
	DeviceName        [adapterNameCap]byte
}

type capabilities struct {
	StructSize           uint32
	AdapterCount         uint32
	AdapterCountHardware uint32
	HasDXGIDuplication   uint32
	HardwareH264         uint32
	HardwareH265         uint32
	SoftwareH264         uint32
	SoftwareH265         uint32
	AudioLoopback        uint32
	Renderer             [adapterDescCap]byte
	// ABI 2 additions; everything above keeps its v1 offset.
	Mjpeg uint32
}

type captureConfig struct {
	StructSize        uint32
	AdapterIndex      uint32
	OutputIndex       uint32
	Flags             uint32
	Region            rect
	HasRegion         uint32
	TrackCursor       uint32
	DrawCursor        uint32
	HideCursorOnClick uint32
	PreferGDI         uint32
	Reserved          uint32
}

type cursor struct {
	Valid                 uint32
	Left, Top, Right, Bot int32
}

type frame struct {
	StructSize       uint32
	Width            uint32
	Height           uint32
	Stride           int32
	PresentTicks     uint64
	AccumulatedFrame uint32
	Cursor           cursor
}

type encoderConfig struct {
	StructSize       uint32
	Width            uint32
	Height           uint32
	FramesPerSecond  uint32
	BitsPerSecond    uint32
	Codec            uint32
	Container        uint32
	AllowHardware    uint32
	KeyframeInterval uint32
	OutputPath       [encoderPathCap]byte
	// ABI 2 additions for the MJPEG/AVI muxer.
	HasAudio           uint32
	AudioSampleRate    uint32
	AudioChannels      uint32
	AudioBitsPerSample uint32
	AudioFormatTag     uint32
	JpegQuality        uint32
	// ABI 3 addition for the H.264 path.
	VideoQuality uint32
}

type buffer struct {
	StructSize    uint32
	Kind          uint32
	Data          *byte
	Size          uint32
	KeyFrame      uint32
	Duration100ns uint64
}

type audioConfig struct {
	StructSize uint32
	Reserved   uint32
}

type audioFormat struct {
	StructSize    uint32
	SampleRate    uint32
	Channels      uint32
	BitsPerSample uint32
	FormatTag     uint32
	DeviceID      uint64
}

// api holds the resolved entry points of the loaded library.
type api struct {
	dll *syscall.DLL

	abiVersion        *syscall.Proc
	buildInfo         *syscall.Proc
	enumerateAdapters *syscall.Proc
	queryCapabilities *syscall.Proc
	lastError         *syscall.Proc
	captureOpen       *syscall.Proc
	captureGrab       *syscall.Proc
	captureCursor     *syscall.Proc
	capturePixels     *syscall.Proc
	captureBackend    *syscall.Proc
	captureClose      *syscall.Proc
	encoderOpen       *syscall.Proc
	encoderEncode     *syscall.Proc
	encoderAudioWrite *syscall.Proc
	encoderRead       *syscall.Proc
	encoderFlush      *syscall.Proc
	encoderFinish     *syscall.Proc
	encoderIsHardware *syscall.Proc
	encoderClose      *syscall.Proc
	audioOpen         *syscall.Proc
	audioRead         *syscall.Proc
	audioFormat       *syscall.Proc
	audioCount        *syscall.Proc
	audioClose        *syscall.Proc
}

var (
	loaded  *api
	loadErr error
)

// dllName is the file the build script produces.
const dllName = "recorder_native.dll"

// embeddedOnce/embeddedPath memoise the first-run extraction below: it runs
// at most once per process, before the library is located.
var (
	embeddedOnce sync.Once
	embeddedPath string
)

// ensureEmbedded extracts the DLL baked into the executable (see the
// embedded package) so the program runs as a single file. It prefers the
// executable's own folder and falls back to the temp directory when that
// folder is not writable (e.g. Program Files for a non-admin user).
// Files are only written when missing or different — compared by hash — so
// a library locked by another running instance is never touched while it is
// already current; an update that cannot replace a locked file falls through
// to a versioned fallback name instead of failing.
func ensureEmbedded() {
	embeddedOnce.Do(func() {
		data := embedded.DLL
		if len(data) == 0 {
			return
		}
		sum := sha256.Sum256(data)
		same := func(path string) bool {
			cur, err := os.ReadFile(path)
			if err != nil {
				return false
			}
			return sha256.Sum256(cur) == sum
		}
		// tryWrite stores data at path, writing aside and renaming so a
		// concurrent reader never sees a half-written library. It reports
		// whether path is now usable: already current, or freshly written.
		tryWrite := func(path string) bool {
			if same(path) {
				return true
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return false
			}
			tmp, err := os.CreateTemp(filepath.Dir(path), "rec-*.tmp")
			if err != nil {
				return false
			}
			tmpName := tmp.Name()
			_, werr := tmp.Write(data)
			cerr := tmp.Close()
			if werr != nil || cerr != nil {
				os.Remove(tmpName)
				return false
			}
			if err := os.Rename(tmpName, path); err != nil {
				os.Remove(tmpName)
				return false
			}
			return true
		}
		if exe, err := os.Executable(); err == nil {
			path := filepath.Join(filepath.Dir(exe), dllName)
			if tryWrite(path) {
				embeddedPath = path
				return
			}
		}
		// Versioned fallback: concurrent versions never fight over one
		// name, and stale ones are just temp files the OS cleans up.
		fallback := filepath.Join(os.TempDir(),
			fmt.Sprintf("recorder_native-%x.dll", sum[:4]))
		if tryWrite(fallback) {
			embeddedPath = fallback
		}
	})
}

// locateLibrary finds the native library next to the program, or in the bin
// directory of the source tree, so that `go run` and `go test` work from any
// working directory without a build step of their own.
func locateLibrary() (string, error) {
	// The first-run extraction above always wins when it produced a file:
	// beside the exe in the common case, in temp otherwise.
	ensureEmbedded()
	if embeddedPath != "" {
		if _, err := os.Stat(embeddedPath); err == nil {
			return embeddedPath, nil
		}
	}
	var tried []string
	try := func(path string) bool {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		tried = append(tried, path)
		return false
	}

	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, candidate := range []string{
			filepath.Join(dir, dllName),
			filepath.Join(dir, "bin", dllName),
			filepath.Join(dir, "..", "bin", dllName),
		} {
			if try(candidate) {
				return candidate, nil
			}
		}
	}
	if wd, err := os.Getwd(); err == nil {
		dir := wd
		for i := 0; i < 5; i++ {
			for _, candidate := range []string{
				filepath.Join(dir, dllName),
				filepath.Join(dir, "bin", dllName),
			} {
				if try(candidate) {
					return candidate, nil
				}
			}
			dir = filepath.Dir(dir)
		}
	}
	return "", fmt.Errorf("%s was not found (looked in %v); run build.ps1 to compile it",
		dllName, tried)
}

func load() (*api, error) {
	if loaded != nil || loadErr != nil {
		return loaded, loadErr
	}
	path, err := locateLibrary()
	if err != nil {
		loadErr = err
		return nil, loadErr
	}
	dll, err := syscall.LoadDLL(path)
	if err != nil {
		loadErr = fmt.Errorf("loading %s: %w", path, err)
		return nil, loadErr
	}

	a := &api{dll: dll}
	var bindErr error
	a.abiVersion = bind(dll, "recorder_abi_version", &bindErr)
	a.buildInfo = bind(dll, "recorder_build_info", &bindErr)
	a.enumerateAdapters = bind(dll, "recorder_enumerate_adapters", &bindErr)
	a.queryCapabilities = bind(dll, "recorder_query_capabilities", &bindErr)
	a.lastError = bind(dll, "recorder_last_error", &bindErr)
	a.captureOpen = bind(dll, "recorder_capture_open", &bindErr)
	a.captureGrab = bind(dll, "recorder_capture_grab", &bindErr)
	a.captureCursor = bind(dll, "recorder_capture_cursor", &bindErr)
	a.capturePixels = bind(dll, "recorder_capture_pixels", &bindErr)
	a.captureBackend = bind(dll, "recorder_capture_backend", &bindErr)
	a.captureClose = bind(dll, "recorder_capture_close", &bindErr)
	a.encoderOpen = bind(dll, "recorder_encoder_open", &bindErr)
	a.encoderEncode = bind(dll, "recorder_encoder_encode", &bindErr)
	a.encoderAudioWrite = bind(dll, "recorder_encoder_audio_write", &bindErr)
	a.encoderRead = bind(dll, "recorder_encoder_read", &bindErr)
	a.encoderFlush = bind(dll, "recorder_encoder_flush", &bindErr)
	a.encoderFinish = bind(dll, "recorder_encoder_finish", &bindErr)
	a.encoderIsHardware = bind(dll, "recorder_encoder_is_hardware", &bindErr)
	a.encoderClose = bind(dll, "recorder_encoder_close", &bindErr)
	a.audioOpen = bind(dll, "recorder_audio_open", &bindErr)
	a.audioRead = bind(dll, "recorder_audio_read", &bindErr)
	a.audioFormat = bind(dll, "recorder_audio_format", &bindErr)
	a.audioCount = bind(dll, "recorder_audio_count", &bindErr)
	a.audioClose = bind(dll, "recorder_audio_close", &bindErr)
	if bindErr != nil {
		loadErr = fmt.Errorf("%s is not the expected library: %w", path, bindErr)
		return nil, loadErr
	}
	loaded, loadErr = a, nil
	return a, nil
}

func bind(dll *syscall.DLL, name string, firstErr *error) *syscall.Proc {
	proc, err := dll.FindProc(name)
	if err != nil && *firstErr == nil {
		*firstErr = fmt.Errorf("missing export %s: %w", name, err)
	}
	return proc
}

// BuildInfo describes the native library, for the diagnostics report. It also
// checks the ABI version, so a stale DLL is refused rather than misread.
func BuildInfo() (string, error) {
	a, err := load()
	if err != nil {
		return "", err
	}
	raw, _, _ := a.abiVersion.Call()
	version := uint32(raw)
	if version != abiVersion {
		return "", fmt.Errorf("recorder_native.dll reports ABI %d but this program speaks %d; "+
			"rebuild it with build.ps1", version, abiVersion)
	}
	ptr, _, _ := a.buildInfo.Call()
	if ptr == 0 {
		return "", errors.New("the native library returned no build information")
	}
	return cString(unsafe.Slice((*byte)(unsafe.Pointer(ptr)), 512)), nil
}

func lastError(a *api) error {
	buf := make([]byte, 512)
	n, _, _ := a.lastError.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return errors.New("the native library reported no detail")
	}
	return errors.New(cString(buf))
}

// StatusError carries the status code the library returned alongside its message,
// because the caller has to react differently to "no encoder here" and "the
// device disappeared".
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string { return e.Message }

// Timeout reports whether err means "no new frame yet", which is not a failure:
// a static desktop legitimately produces no updates.
func Timeout(err error) bool {
	var status *StatusError
	return errors.As(err, &status) && status.Status == StatusTimeout
}

// Unsupported reports whether err means the machine cannot do what was asked, in
// which case the caller is expected to fall back rather than give up.
func Unsupported(err error) bool {
	var status *StatusError
	return errors.As(err, &status) && status.Status == StatusUnsupported
}

func check(a *api, status int32) error {
	if status == StatusOK {
		return nil
	}
	err := lastError(a)
	var wrapped *StatusError
	if errors.As(err, &wrapped) {
		return err
	}
	return &StatusError{Status: int(status), Message: err.Error()}
}

func cString(buf []byte) string {
	for i, b := range buf {
		if b == 0 {
			return string(buf[:i])
		}
	}
	return string(buf)
}

func cStringField(field []byte) string {
	return cString(field)
}

// pinnedThread keeps the goroutine on one OS thread for as long as it is held.
// Desktop duplication and GDI device contexts both belong to the thread that
// created them, and a goroutine is free to migrate, so the recorder pins itself
// rather than trusting the scheduler.
type pinnedThread struct{ held bool }

func pin() *pinnedThread {
	runtime.LockOSThread()
	return &pinnedThread{held: true}
}

func (p *pinnedThread) release() {
	if p != nil && p.held {
		p.held = false
		runtime.UnlockOSThread()
	}
}
