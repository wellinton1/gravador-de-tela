package native

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"time"
	"unsafe"
)

// Adapter is one display output, described well enough for a user to recognise
// it and for the recorder to capture it.
type Adapter struct {
	Index        int
	Adapter      uint32
	Output       uint32
	Bounds       Rect
	Rotation     int
	RefreshHz    float64
	Primary      bool
	Attached     bool
	CanDuplicate bool
	Software     bool
	Description  string
	DeviceName   string
}

// Rect is a rectangle in virtual-desktop coordinates.
type Rect struct{ Left, Top, Right, Bottom int32 }

func (r Rect) Width() int  { return int(r.Right - r.Left) }
func (r Rect) Height() int { return int(r.Bottom - r.Top) }
func (r Rect) Empty() bool { return r.Right <= r.Left || r.Bottom <= r.Top }

// String renders the rectangle the way the interface and the log show it.
func (r Rect) String() string {
	return fmt.Sprintf("%dx%d at (%d,%d)", r.Width(), r.Height(), r.Left, r.Top)
}

// Cursor is the pointer rectangle in captured-frame coordinates.
type Cursor struct {
	Valid                 bool
	Left, Top, Right, Bot int
}

// Capabilities is the answer to "is this machine worth using a GPU for", reported
// rather than assumed so a software fallback can be explained instead of being
// silent.
type Capabilities struct {
	Adapters         int
	HardwareAdapters int
	HasDuplication   bool
	HardwareH264     bool
	HardwareH265     bool
	SoftwareH264     bool
	SoftwareH265     bool
	AudioLoopback    bool
	Renderer         string
	// Mjpeg reports the WIC JPEG encoder the AVI path needs. It is present on
	// every supported Windows, so false here means something is deeply wrong.
	Mjpeg bool
}

// HasHardwareEncoder reports whether the graphics adapter can encode video,
// which is what decides between the fast path and a software one.
func (c Capabilities) HasHardwareEncoder() bool { return c.HardwareH264 || c.HardwareH265 }

// Adapters lists every display output the machine exposes, left to right.
func Adapters() ([]Adapter, error) {
	a, err := load()
	if err != nil {
		return nil, err
	}
	// The count is the return value of the same call that fills the array, so
	// the array is sized exactly rather than guessed at.
	count := adaptersCount(a)
	if count == 0 {
		return nil, errors.New("no display outputs were found")
	}
	buf := make([]adapterInfo, count)
	status, _, _ := a.enumerateAdapters.Call(
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if status == 0 {
		return nil, lastError(a)
	}

	out := make([]Adapter, 0, len(buf))
	for _, info := range buf {
		out = append(out, Adapter{
			Index:   len(out),
			Adapter: info.AdapterIndex,
			Output:  info.OutputIndex,
			Bounds: Rect{info.Bounds.Left, info.Bounds.Top,
				info.Bounds.Right, info.Bounds.Bottom},
			Rotation:     int(info.Rotation),
			RefreshHz:    float64(info.RefreshHz),
			Primary:      info.Primary != 0,
			Attached:     info.AttachedToDesktop != 0,
			CanDuplicate: info.CanDuplicate != 0,
			Software:     info.IsSoftware != 0,
			Description:  cStringField(info.Description[:]),
			DeviceName:   cStringField(info.DeviceName[:]),
		})
	}
	return out, nil
}

// adaptersCount asks the library how many outputs there are by letting it write
// nothing; the return value is the true count either way.
func adaptersCount(a *api) uint32 {
	count, _, _ := a.enumerateAdapters.Call(0, 0)
	return uint32(count)
}

// QueryCapabilities reports what the machine can do.
func QueryCapabilities() (Capabilities, error) {
	a, err := load()
	if err != nil {
		return Capabilities{}, err
	}
	var out capabilities
	out.StructSize = uint32(unsafe.Sizeof(out))
	status, _, _ := a.queryCapabilities.Call(uintptr(unsafe.Pointer(&out)))
	if err := check(a, int32(status)); err != nil {
		return Capabilities{}, err
	}
	return Capabilities{
		Adapters:         int(out.AdapterCount),
		HardwareAdapters: int(out.AdapterCountHardware),
		HasDuplication:   out.HasDXGIDuplication != 0,
		HardwareH264:     out.HardwareH264 != 0,
		HardwareH265:     out.HardwareH265 != 0,
		SoftwareH264:     out.SoftwareH264 != 0,
		SoftwareH265:     out.SoftwareH265 != 0,
		AudioLoopback:    out.AudioLoopback != 0,
		Renderer:         cStringField(out.Renderer[:]),
		Mjpeg:            out.Mjpeg != 0,
	}, nil
}

// Frame is one captured image in BGRA8. Pixels points into the native buffer and
// stays valid until the next Grab on the same session.
type Frame struct {
	Width, Height int
	Stride        int
	PresentTicks  uint64
	Accumulated   int
	Cursor        Cursor
	Pixels        []byte
}

// At is the offset of a pixel in the frame's buffer.
func (f Frame) At(x, y int) int { return y*f.Stride + x*4 }

// CaptureOptions configures a capture session.
type CaptureOptions struct {
	// Adapter selects the output to record. Zero is the leftmost display.
	Adapter uint32
	// Output selects which output of that adapter.
	Output uint32
	// Region restricts capture to part of the display, in virtual-desktop
	// coordinates. The zero value captures the whole output.
	Region    Rect
	HasRegion bool
	// TrackCursor reports the pointer position with each frame.
	TrackCursor bool
	// DrawCursor draws the pointer into the pixels. Only the GDI backend can do
	// this; duplication receives it already composited.
	DrawCursor bool
	// HideCursorOnClick leaves the pointer out while the mouse button is held.
	HideCursorOnClick bool
	// PreferGDI skips duplication and goes straight to the fallback.
	PreferGDI bool
	// ShowClick draws a red ring where the pointer is while its button is
	// held, baked into the recorded pixels.
	ShowClick bool
	// Halo draws a soft highlight around the pointer, baked into the pixels.
	Halo bool
}

// Session is a running capture. Its methods are not safe for concurrent use,
// which matches the native session's single-threaded contract.
type Session struct {
	api     *api
	handle  uintptr
	pin     *pinnedThread
	backend int

	mu     sync.Mutex
	frame  frame
	closed bool
}

// Open starts capturing.
func Open(opts CaptureOptions) (*Session, error) {
	a, err := load()
	if err != nil {
		return nil, err
	}
	var config captureConfig
	config.StructSize = uint32(unsafe.Sizeof(config))
	config.AdapterIndex = opts.Adapter
	config.OutputIndex = opts.Output
	if opts.ShowClick {
		config.Flags |= CaptureFlagClickRing
	}
	if opts.Halo {
		config.Flags |= CaptureFlagHalo
	}
	config.Region = rect{opts.Region.Left, opts.Region.Top, opts.Region.Right, opts.Region.Bottom}
	if opts.HasRegion {
		config.HasRegion = 1
	}
	if opts.TrackCursor {
		config.TrackCursor = 1
	}
	if opts.DrawCursor {
		config.DrawCursor = 1
	}
	if opts.HideCursorOnClick {
		config.HideCursorOnClick = 1
	}
	if opts.PreferGDI {
		config.PreferGDI = 1
	}

	var handle uintptr
	// The session belongs to one OS thread, so the goroutine is pinned before it
	// is created and stays pinned until it is closed.
	pinned := pin()
	status, _, _ := a.captureOpen.Call(uintptr(unsafe.Pointer(&config)), uintptr(unsafe.Pointer(&handle)))
	runtime.KeepAlive(config)
	if status != StatusOK || handle == 0 {
		pinned.release()
		return nil, check(a, int32(status))
	}
	s := &Session{api: a, handle: handle, pin: pinned}
	backend, _, _ := a.captureBackend.Call(handle)
	s.backend = int(backend)
	return s, nil
}

// backendGDI is how Backend spells the fallback, used where the decision is made
// rather than reported.
const backendGDI = "GDI BitBlt"

// Backend reports which capture path is in use, so a reduced fidelity can be
// shown rather than suffered silently.
func (s *Session) Backend() string {
	switch s.backend {
	case BackendDXGI:
		return "DXGI desktop duplication"
	case BackendGDI:
		return "GDI BitBlt"
	default:
		return "none"
	}
}

// Grab waits up to timeout for the next frame.
//
// A timeout is not an error: a static desktop produces no updates, so the caller
// simply asks again. Check it with Timeout.
func (s *Session) Grab(timeout time.Duration) (Frame, error) {
	if s == nil || s.handle == 0 {
		return Frame{}, errors.New("capture: the session is closed")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	ms := uint32(timeout / time.Millisecond)
	if ms == 0 {
		ms = 1
	}
	s.frame = frame{}
	status, _, _ := s.api.captureGrab.Call(s.handle, uintptr(ms),
		uintptr(unsafe.Pointer(&s.frame)))
	if int32(status) != StatusOK {
		return Frame{}, check(s.api, int32(status))
	}

	size := int(s.frame.Stride) * int(s.frame.Height)
	pixels, _, _ := s.api.capturePixels.Call(s.handle)
	if pixels == 0 || size <= 0 {
		return Frame{}, errors.New("capture: the native library returned no pixels")
	}
	return Frame{
		Width:        int(s.frame.Width),
		Height:       int(s.frame.Height),
		Stride:       int(s.frame.Stride),
		PresentTicks: s.frame.PresentTicks,
		Accumulated:  int(s.frame.AccumulatedFrame),
		Cursor: Cursor{
			Valid: s.frame.Cursor.Valid != 0,
			Left:  int(s.frame.Cursor.Left), Top: int(s.frame.Cursor.Top),
			Right: int(s.frame.Cursor.Right), Bot: int(s.frame.Cursor.Bot),
		},
		Pixels: unsafe.Slice((*byte)(unsafe.Pointer(pixels)), size),
	}, nil
}

// Size reports the captured frame size without grabbing a frame.
func (s *Session) Size() (width, height int) {
	if s == nil || s.handle == 0 {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The frame description is only written by a grab, so one is requested with
	// a zero timeout: a desktop that has just changed still answers.
	var described frame
	status, _, _ := s.api.captureGrab.Call(s.handle, 0, uintptr(unsafe.Pointer(&described)))
	if status == StatusOK {
		s.frame = described
	}
	return int(s.frame.Width), int(s.frame.Height)
}

// Close releases the session and unpins the goroutine.
func (s *Session) Close() {
	if s == nil || s.handle == 0 {
		return
	}
	s.api.captureClose.Call(s.handle)
	s.handle = 0
	s.closed = true
	if s.pin != nil {
		s.pin.release()
		s.pin = nil
	}
	runtime.KeepAlive(s)
}

// EncoderConfig configures the MJPEG/AVI recording.
type EncoderConfig struct {
	Width, Height int
	FPS           int
	// Bitrate is kept for the future H.264 path and ignored by MJPEG.
	Bitrate int
	// Codec must be CodecMjpeg and Container must be ContainerAVI; anything
	// else is refused by the native layer with StatusUnsupported.
	Codec      int
	Container  int
	OutputPath string
	// Audio, when non-nil, adds a PCM 01wb stream in the loopback format.
	// The bytes handed to WriteAudio must match this layout exactly.
	Audio *AudioFormat
	// JpegQuality is 1-100; zero means the native default of 80.
	JpegQuality int
	// VideoQuality is the H.264 quality level 1-100; zero means 70. Higher
	// keeps text crisp on motion with quality VBR.
	VideoQuality int
}

// Encoder turns BGRA frames into a Motion JPEG AVI file. Chunks stream to
// disk as they arrive, so there is nothing to read back: Encode and
// WriteAudio append, Close patches the headers and the index.
type Encoder struct {
	api    *api
	handle uintptr
	closed bool
}

// OpenEncoder starts a recording. Width and Height must match the frames that
// will be submitted; FPS sets the AVI timeline one frame lands on.
func OpenEncoder(cfg EncoderConfig) (*Encoder, error) {
	a, err := load()
	if err != nil {
		return nil, err
	}
	var config encoderConfig
	config.StructSize = uint32(unsafe.Sizeof(config))
	config.Width = uint32(cfg.Width)
	config.Height = uint32(cfg.Height)
	config.FramesPerSecond = uint32(cfg.FPS)
	config.BitsPerSecond = uint32(cfg.Bitrate)
	config.Codec = uint32(cfg.Codec)
	config.Container = uint32(cfg.Container)
	config.JpegQuality = uint32(cfg.JpegQuality)
	config.VideoQuality = uint32(cfg.VideoQuality)
	if cfg.Audio != nil {
		config.HasAudio = 1
		config.AudioSampleRate = uint32(cfg.Audio.SampleRate)
		config.AudioChannels = uint32(cfg.Audio.Channels)
		config.AudioBitsPerSample = uint32(cfg.Audio.BitsPerSample)
		config.AudioFormatTag = uint32(cfg.Audio.FormatTag)
	}
	if err := writeCString(config.OutputPath[:], cfg.OutputPath); err != nil {
		return nil, err
	}

	var handle uintptr
	status, _, _ := a.encoderOpen.Call(uintptr(unsafe.Pointer(&config)),
		uintptr(unsafe.Pointer(&handle)))
	runtime.KeepAlive(config)
	if status != StatusOK || handle == 0 {
		return nil, check(a, int32(status))
	}
	return &Encoder{api: a, handle: handle}, nil
}

// Hardware reports whether the graphics adapter is doing the encoding. MJPEG
// goes through WIC in software (always false); H.264 answers from the
// encoder the sink writer actually engaged.
func (e *Encoder) Hardware() bool {
	if e == nil || e.handle == 0 {
		return false
	}
	value, _, _ := e.api.encoderIsHardware.Call(e.handle)
	return value != 0
}

// Encode appends one top-down BGRA frame. Pixels is read synchronously, so the
// caller may reuse the buffer as soon as Encode returns. Duration anchors the
// frame on the timeline and is currently informational: one call always lands
// exactly one frame at the configured rate.
func (e *Encoder) EncodeBGRA(pixels []byte, stride int, duration time.Duration) error {
	if e == nil || e.handle == 0 {
		return errors.New("encode: the encoder is closed")
	}
	if len(pixels) == 0 {
		return errors.New("encode: no pixels were supplied")
	}
	if stride <= 0 {
		return errors.New("encode: stride must be positive")
	}
	d := uint64(duration / (100 * time.Nanosecond))
	status, _, _ := e.api.encoderEncode.Call(e.handle,
		uintptr(unsafe.Pointer(&pixels[0])), uintptr(stride),
		uintptr(d))
	runtime.KeepAlive(pixels)
	return check(e.api, int32(status))
}

// WriteAudio appends one block of PCM bytes in the layout declared in
// EncoderConfig. Short blocks are fine; odd lengths are padded by the muxer.
func (e *Encoder) WriteAudio(data []byte) error {
	if e == nil || e.handle == 0 {
		return errors.New("audio_write: the encoder is closed")
	}
	if len(data) == 0 {
		return nil
	}
	status, _, _ := e.api.encoderAudioWrite.Call(e.handle,
		uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)))
	runtime.KeepAlive(data)
	return check(e.api, int32(status))
}

// Close patches the AVI headers and the index, then releases the encoder.
func (e *Encoder) Close() error {
	if e == nil || e.handle == 0 {
		return nil
	}
	defer func() {
		e.api.encoderClose.Call(e.handle)
		e.handle = 0
	}()
	status, _, _ := e.api.encoderFinish.Call(e.handle, 0)
	return check(e.api, int32(status))
}

// AudioFormat describes the captured audio stream.
type AudioFormat struct {
	SampleRate    int
	Channels      int
	BitsPerSample int
	FormatTag     int
}

// Audio is a running loopback capture.
type Audio struct {
	api        *api
	handle     uintptr
	buf        []byte
	blockAlign int
	format     AudioFormat
}

// OpenAudio starts loopback capture of the default output device.
func OpenAudio() (*Audio, error) {
	a, err := load()
	if err != nil {
		return nil, err
	}
	var config audioConfig
	config.StructSize = uint32(unsafe.Sizeof(config))
	var handle uintptr
	status, _, _ := a.audioOpen.Call(uintptr(unsafe.Pointer(&config)),
		uintptr(unsafe.Pointer(&handle)))
	runtime.KeepAlive(config)
	if status != StatusOK || handle == 0 {
		return nil, check(a, int32(status))
	}
	out := &Audio{api: a, handle: handle, buf: make([]byte, 48000*4*4)}
	format, err := out.Format()
	if err != nil {
		out.Close()
		return nil, err
	}
	out.format = format
	out.blockAlign = format.Channels * format.BitsPerSample / 8
	if out.blockAlign <= 0 {
		out.Close()
		return nil, errors.New("audio: the device reported an unusable format")
	}
	return out, nil
}

// Format reports the stream layout.
func (a *Audio) Format() (AudioFormat, error) {
	var out audioFormat
	out.StructSize = uint32(unsafe.Sizeof(out))
	status, _, _ := a.api.audioFormat.Call(a.handle, uintptr(unsafe.Pointer(&out)))
	if err := check(a.api, int32(status)); err != nil {
		return AudioFormat{}, err
	}
	return AudioFormat{
		SampleRate:    int(out.SampleRate),
		Channels:      int(out.Channels),
		BitsPerSample: int(out.BitsPerSample),
		FormatTag:     int(out.FormatTag),
	}, nil
}

// CachedFormat is the stream layout fixed at open time, without a syscall.
func (a *Audio) CachedFormat() AudioFormat { return a.format }

// Read returns the next block of PCM in the cached format, or a timeout error
// (check with Timeout) when nothing has been played since the last call.
func (a *Audio) Read() ([]byte, error) {
	var frames int32
	status, _, _ := a.api.audioRead.Call(a.handle,
		uintptr(unsafe.Pointer(&a.buf[0])), uintptr(len(a.buf)),
		uintptr(unsafe.Pointer(&frames)))
	if int32(status) != StatusOK {
		return nil, check(a.api, int32(status))
	}
	if frames <= 0 {
		return nil, nil
	}
	return a.buf[:int(frames)*a.blockAlign], nil
}

// Close releases the audio capture.
func (a *Audio) Close() {
	if a == nil || a.handle == 0 {
		return
	}
	a.api.audioClose.Call(a.handle)
	a.handle = 0
}

// AudioDevices lists the active output devices, for the diagnostics report.
func AudioDevices() ([]string, error) {
	a, err := load()
	if err != nil {
		return nil, err
	}
	const capacity = 16
	storage := make([][128]byte, capacity)
	pointers := make([]uintptr, capacity)
	for i := range storage {
		pointers[i] = uintptr(unsafe.Pointer(&storage[i][0]))
	}
	count, _, _ := a.audioCount.Call(uintptr(unsafe.Pointer(&pointers[0])),
		uintptr(capacity))
	if count == 0 {
		return nil, nil
	}
	names := make([]string, 0, count)
	for i := 0; i < int(count) && i < capacity; i++ {
		names = append(names, cStringField(storage[i][:]))
	}
	return names, nil
}

// OutputExtension is the file extension a container is written with.
func OutputExtension(container int) string {
	if container == ContainerAVI {
		return ".avi"
	}
	return ".mp4"
}

// DefaultOutputPath builds a timestamped file name for a recording.
func DefaultOutputPath(dir string, container int) string {
	name := fmt.Sprintf("gravacao-%s%s", time.Now().Format("20060102-150405"),
		OutputExtension(container))
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, name)
}

func writeCString(dst []byte, value string) error {
	if len(value)+1 > len(dst) {
		return fmt.Errorf("native: %q is too long for the ABI field", value)
	}
	copy(dst, value)
	dst[len(value)] = 0
	return nil
}
