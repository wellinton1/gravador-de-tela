// Package rec orchestrates a recording: screen capture plus loopback audio
// muxed into a Motion JPEG AVI file.
//
// Capture produces BGRA frames and the encoder takes BGRA frames, so pixels
// are never converted in between: a grab hands its buffer straight to the
// muxer. Audio blocks land in arrival order next to the video chunks, and the
// idx1 index the muxer writes at the end is what lets a player seek a file
// whose streams were never interleaved on purpose.
package rec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"screenrec/internal/native"
)

// Options selects what is recorded and where it is written.
type Options struct {
	// Adapter and Output choose the display. Zero values mean the first output
	// of the first adapter, which is the leftmost display.
	Adapter uint32
	Output  uint32
	// Region restricts capture to part of the display, in virtual-desktop
	// coordinates. The zero value captures the whole output.
	Region    native.Rect
	HasRegion bool
	// FPS sets the file timeline. 30 is the default; zero means 30.
	FPS int
	// Codec selects the encoder: zero means automatic (H.264 into MP4, with
	// Motion JPEG into AVI as the fallback), otherwise CodecH264 or
	// CodecMjpeg from the native package.
	Codec int
	// Bitrate is the H.264 target in bits per second; zero means 600000.
	// Video (~270 MB/h) plus 96 kbps AAC (~43 MB/h) ceilings at ~313 MB/h,
	// and real desktops land well below it because still frames cost almost
	// nothing. Ignored by MJPEG.
	Bitrate int
	// JpegQuality is 1-100; zero means the native default of 80.
	JpegQuality int
	// VideoQuality is the H.264 quality level 1-100; zero means 70.
	VideoQuality int
	// WithAudio adds the system mix: AAC in MP4, PCM in AVI.
	WithAudio bool
	// OutputPath is the recording file. Empty means a timestamped file in
	// the user's Videos folder, falling back to the working directory; an
	// extensionless name gains the container's extension.
	OutputPath string
	// DrawCursor bakes the pointer into the pixels. Only the GDI fallback can
	// do this; duplication receives it already composited.
	DrawCursor bool
	// ShowClick draws a red ring at the pointer while its button is held.
	ShowClick bool
	// Halo draws a soft highlight around the pointer.
	Halo bool
	// StopAfter ends the recording after the duration, like vokoscreen's
	// timer. Zero means record until Stop.
	StopAfter time.Duration
}

// Stats is a point-in-time snapshot of a running recording.
type Stats struct {
	Recording   bool
	Backend     string
	Codec       string // "H.264" or "Motion JPEG", as the file is written
	Width       int
	Height      int
	VideoFrames int64
	AudioBytes  int64
	Audio       bool
	OutputPath  string
	Elapsed     time.Duration
}

// Recorder owns a capture session, an optional audio capture and the AVI
// muxer. Its methods are safe for concurrent use; the recording itself runs
// on goroutines owned by Start and released by Stop.
type Recorder struct {
	opts    Options
	path    string
	codec   string
	session *native.Session
	encoder *native.Encoder
	audio   *native.Audio

	stop   chan struct{}
	result chan error
	once   sync.Once

	backend  string
	width    int
	height   int
	started  time.Time
	frames   atomic.Int64
	abytes   atomic.Int64
	audioErr atomic.Value // error, set once by the audio loop
}

// Start opens capture, audio and the muxer, and begins recording.
//
// The note is non-empty when capture fell back to GDI; it is worth showing,
// not failing on, so it travels next to the error rather than inside it.
func Start(opts Options) (*Recorder, string, error) {
	fps := opts.FPS
	if fps <= 0 {
		fps = 30
	}
	bitrate := opts.Bitrate
	if bitrate <= 0 {
		bitrate = 600000
	}
	// H.264 first, MJPEG when it is forced or H.264 is refused. The path
	// follows the container because a mismatched extension only confuses
	// players and indexers.
	type attempt struct {
		codec     int
		container int
		label     string
	}
	attempts := []attempt{{native.CodecH264, native.ContainerMP4, "H.264"}}
	switch {
	case opts.Codec == native.CodecMjpeg:
		attempts = []attempt{{native.CodecMjpeg, native.ContainerAVI, "Motion JPEG"}}
	case opts.Codec == native.CodecH264:
		// As listed: no fallback, so a missing encoder surfaces as an error.
	default:
		attempts = append(attempts,
			attempt{native.CodecMjpeg, native.ContainerAVI, "Motion JPEG"})
	}
	outputPath := func(container int) (string, error) {
		path := opts.OutputPath
		if path == "" {
			dir := videosDir()
			if err := os.MkdirAll(dir, 0o755); err != nil {
				dir = "."
			}
			return native.DefaultOutputPath(dir, container), nil
		}
		// The extension always follows the container: a previous attempt (or
		// the caller) may have left an .avi on an H.264 path, and the sink
		// writer picks its muxer by extension, so a stale suffix records
		// into the wrong container or refuses outright.
		if ext := filepath.Ext(path); ext != native.OutputExtension(container) {
			path = path[:len(path)-len(ext)] + native.OutputExtension(container)
		}
		if dir := filepath.Dir(path); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", err
			}
		}
		return path, nil
	}

	session, note, err := native.OpenVerified(native.CaptureOptions{
		Adapter:     opts.Adapter,
		Output:      opts.Output,
		Region:      opts.Region,
		HasRegion:   opts.HasRegion,
		TrackCursor: true,
		DrawCursor:  opts.DrawCursor,
		ShowClick:   opts.ShowClick,
		Halo:        opts.Halo,
	})
	if err != nil {
		return nil, "", fmt.Errorf("capture: %w", err)
	}

	// The encoder must match the frames it will be fed, and the session only
	// knows its size after a grab, so the first frame is awaited here rather
	// than in the loop.
	var first native.Frame
	deadline := time.Now().Add(2 * time.Second)
	gotFrame := false
	for time.Now().Before(deadline) {
		f, err := session.Grab(500 * time.Millisecond)
		if err != nil {
			if native.Timeout(err) {
				continue
			}
			session.Close()
			return nil, "", fmt.Errorf("capture: %w", err)
		}
		first, gotFrame = f, true
		break
	}
	if !gotFrame {
		// The compositor is silent: a static desktop, an occluded session.
		// GDI reads the current screen unconditionally, so fall back to it
		// rather than refusing to record an idle screen.
		session.Close()
		gdi, _, err := native.OpenVerified(native.CaptureOptions{
			Adapter:     opts.Adapter,
			Output:      opts.Output,
			Region:      opts.Region,
			HasRegion:   opts.HasRegion,
			TrackCursor: true,
			DrawCursor:  opts.DrawCursor,
			ShowClick:   opts.ShowClick,
			Halo:        opts.Halo,
			PreferGDI:   true,
		})
		if err != nil {
			return nil, "", fmt.Errorf("capture produced no frame: %w", err)
		}
		session = gdi
		if note != "" {
			note += "; "
		}
		note += "desktop duplication is silent on this machine, so the GDI fallback is in use"
		f, err := session.Grab(2 * time.Second)
		if err != nil {
			session.Close()
			return nil, "", fmt.Errorf("capture produced no frame: %w", err)
		}
		first = f
	}

	var capture *native.Audio
	var audioFormat *native.AudioFormat
	if opts.WithAudio {
		capture, err = native.OpenAudio()
		if err != nil {
			session.Close()
			return nil, "", fmt.Errorf("audio: %w", err)
		}
		format := capture.CachedFormat()
		audioFormat = &format
	}

	// H.264 first, MJPEG when it is forced or H.264 is refused. Trying the
	// preferred encoder and degrading beats failing on machines whose
	// graphics stack cannot do H.264.
	var encoder *native.Encoder
	var path, codecLabel string
	var lastErr error
	for _, a := range attempts {
		candidate, err := outputPath(a.container)
		if err != nil {
			lastErr = err
			continue
		}
		encoder, err = native.OpenEncoder(native.EncoderConfig{
			Width:        first.Width,
			Height:       first.Height,
			FPS:          fps,
			Bitrate:      bitrate,
			Codec:        a.codec,
			Container:    a.container,
			Audio:        audioFormat,
			JpegQuality:  opts.JpegQuality,
			VideoQuality: opts.VideoQuality,
			OutputPath:   candidate,
		})
		if err != nil {
			lastErr = err
			// In automatic mode every parameter is generated here and sane,
			// so any H.264 failure is environmental and worth degrading
			// over; an explicitly forced codec still fails loud.
			if len(attempts) > 1 {
				continue
			}
			break
		}
		path, codecLabel = candidate, a.label
		lastErr = nil
		break
	}
	if lastErr != nil {
		if capture != nil {
			capture.Close()
		}
		session.Close()
		return nil, "", fmt.Errorf("encoder: %w", lastErr)
	}
	if codecLabel != attempts[0].label {
		if note != "" {
			note += "; "
		}
		note += "H.264 is unavailable, so this recording is Motion JPEG"
	}
	// Refuse early when the disk cannot hold a recording instead of dying
	// mid-file with a truncated container.
	if free, err := freeBytes(filepath.Dir(path)); err == nil && free < minStartBytes {
		encoder.Close()
		if capture != nil {
			capture.Close()
		}
		session.Close()
		return nil, "", fmt.Errorf("pouco espaço em disco (%d MB livres)", free>>20)
	}

	r := &Recorder{
		opts:    opts,
		path:    path,
		codec:   codecLabel,
		session: session,
		encoder: encoder,
		audio:   capture,
		stop:    make(chan struct{}),
		result:  make(chan error, 1),
		backend: session.Backend(),
		width:   first.Width,
		height:  first.Height,
		started: time.Now(),
	}
	go r.videoLoop(first, fps)
	if capture != nil {
		go r.audioLoop()
	}
	return r, note, nil
}

// videoLoop paces grabs at the configured rate. A timeout is not a failure —
// a static desktop produces no updates — so the last frame is repeated to
// keep the video timeline aligned with the wall clock the audio follows.
func (r *Recorder) videoLoop(first native.Frame, fps int) {
	interval := time.Second / time.Duration(fps)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var stopTimer <-chan time.Time
	if r.opts.StopAfter > 0 {
		timer := time.NewTimer(r.opts.StopAfter)
		defer timer.Stop()
		stopTimer = timer.C
	}

	last := first
	frames := int64(0)
	encode := func(f native.Frame) error {
		duration := time.Duration(frames) * interval
		if err := r.encoder.EncodeBGRA(f.Pixels, f.Stride, duration); err != nil {
			return err
		}
		frames++
		r.frames.Store(frames)
		// Disk full mid-recording corrupts the container, so free space is
		// rechecked about every ten seconds and the recording ends cleanly
		// while the file is still valid.
		if frames%300 == 0 {
			if free, err := freeBytes(filepath.Dir(r.path)); err == nil && free < minKeepBytes {
				return fmt.Errorf("disco cheio (%d MB livres); gravação finalizada", free>>20)
			}
		}
		return nil
	}

	if err := encode(last); err != nil {
		r.teardown(err)
		return
	}
	for {
		select {
		case <-r.stop:
			r.teardown(nil)
			return
		case <-stopTimer:
			r.teardown(nil)
			return
		case <-ticker.C:
			// The timeout is deliberately short: AcquireNextFrame waits the
			// whole timeout when the desktop is static, and waiting would
			// steal the encode budget and collapse the frame rate. A fresh
			// frame is taken when one is ready; otherwise the last frame is
			// repeated below, which is what keeps the timeline at pace.
			f, err := r.session.Grab(5 * time.Millisecond)
			if err != nil {
				if native.Timeout(err) {
					if err := encode(last); err != nil {
						r.teardown(err)
						return
					}
					continue
				}
				r.teardown(fmt.Errorf("capture: %w", err))
				return
			}
			last = f
			if err := encode(f); err != nil {
				r.teardown(err)
				return
			}
		}
	}
}

// audioLoop forwards loopback blocks to the muxer. Losing the device ends
// audio but not the recording: a silent tail is better than no file.
//
// When nothing is playing, loopback delivers nothing (Timeout), so without
// intervention the audio stream's timestamps freeze while the video clock
// keeps advancing. The MP4 sink writer then throttles the ahead stream:
// video WriteSample calls start blocking for up to a second each, the frame
// rate collapses to a few fps, and the file comes out fast-forwarded and
// far shorter than the wall clock. To keep the streams together, silence is
// written for every playback gap, so a quiet desktop records true silence
// instead of a stalled timeline. (The AVI path benefits the same way: its
// audio stream then spans the whole recording instead of ending early.)
func (r *Recorder) audioLoop() {
	format := r.audio.CachedFormat()
	blockAlign := format.Channels * format.BitsPerSample / 8
	// lastWrite is the wall-clock instant the muxer's audio clock covers.
	// Real blocks cover up to now; silence top-ups advance it in chunks
	// while nothing plays.
	lastWrite := time.Now()
	const silenceAfter = 100 * time.Millisecond
	const maxSilence = time.Second
	// topUp writes silence for the unwritten span since lastWrite, in one
	// capped chunk. Zeros are silence in both layouts the MP4 path accepts
	// (16-bit PCM and 32-bit float) and in the AVI path's PCM. It reports
	// false when the write itself fails, with the reason stored.
	topUp := func(now time.Time) bool {
		if blockAlign <= 0 || format.SampleRate <= 0 {
			return true
		}
		gap := now.Sub(lastWrite)
		if gap < silenceAfter {
			return true
		}
		if gap > maxSilence {
			gap = maxSilence
		}
		frames := int64(format.SampleRate) * gap.Nanoseconds() / int64(time.Second)
		if frames <= 0 {
			return true
		}
		if err := r.encoder.WriteAudio(make([]byte, frames*int64(blockAlign))); err != nil {
			r.audioErr.CompareAndSwap(nil, fmt.Errorf("audio: %w", err))
			return false
		}
		r.abytes.Add(frames * int64(blockAlign))
		lastWrite = lastWrite.Add(time.Duration(frames) * time.Second / time.Duration(format.SampleRate))
		return true
	}
	for {
		select {
		case <-r.stop:
			return
		default:
		}
		block, err := r.audio.Read()
		if err != nil {
			if native.Timeout(err) {
				// Nothing has been played: polling without pause would spin
				// a core at 100% for the whole recording, starving the very
				// interface the user watches it through. The silence top-up
				// keeps the audio clock tracking the wall clock instead.
				if !topUp(time.Now()) {
					return
				}
				select {
				case <-r.stop:
					return
				case <-time.After(10 * time.Millisecond):
				}
				continue
			}
			var status *native.StatusError
			if errors.As(err, &status) && status.Status == native.StatusLost {
				r.audioErr.CompareAndSwap(nil, errors.New("audio: the output device disappeared; video continued without sound"))
			} else {
				r.audioErr.CompareAndSwap(nil, fmt.Errorf("audio: %w", err))
			}
			return
		}
		if len(block) == 0 {
			if !topUp(time.Now()) {
				return
			}
			select {
			case <-r.stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			continue
		}
		if err := r.encoder.WriteAudio(block); err != nil {
			r.audioErr.CompareAndSwap(nil, fmt.Errorf("audio: %w", err))
			return
		}
		r.abytes.Add(int64(len(block)))
		lastWrite = time.Now()
	}
}

// teardown closes the muxer and every session exactly once, however the
// recording ended: a loop error, Stop, or both racing. The first error wins:
// the loop's, then a stored audio failure, then the muxer's own close.
func (r *Recorder) teardown(loopErr error) {
	r.once.Do(func() {
		close(r.stop)
		encErr := r.encoder.Close()
		r.session.Close()
		if r.audio != nil {
			r.audio.Close()
		}
		err := loopErr
		if err == nil {
			if v, ok := r.audioErr.Load().(error); ok {
				err = v
			}
		}
		if err == nil {
			err = encErr
		}
		r.result <- err
	})
}

// Stop ends the recording, finalises the AVI file and reports how it went.
// It blocks until the muxer has patched the headers, and it is idempotent:
// concurrent or repeated calls all report the same outcome.
func (r *Recorder) Stop() error {
	r.teardown(nil)
	return <-r.result
}

// Stats snapshots the recording for the interface.
func (r *Recorder) Stats() Stats {
	return Stats{
		Recording:   true,
		Backend:     r.backend,
		Codec:       r.codec,
		Width:       r.width,
		Height:      r.height,
		VideoFrames: r.frames.Load(),
		AudioBytes:  r.abytes.Load(),
		Audio:       r.audio != nil,
		OutputPath:  r.path,
		Elapsed:     time.Since(r.started),
	}
}

// OutputPath is the file being written.
func (r *Recorder) OutputPath() string { return r.path }

func videosDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, "Videos")
	}
	return "."
}
