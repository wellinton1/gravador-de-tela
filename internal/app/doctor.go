// Package app is the recorder's user interface and its recording logic.
//
// Everything low level lives in the native library and is reached through
// internal/native, so this package deals in what a user chooses (which display,
// which codec, where to save) rather than in how a frame is captured.
package app

import (
	"fmt"
	"io"
	"strings"
	"time"

	"screenrec/internal/native"
)

// Doctor prints a capabilities report.
//
// It exists because recording on Windows has independent failure points —
// display access, the encoder and the audio endpoint — and the error shown during
// a recording rarely says which one is at fault. Every line here is a fact the
// native library reported rather than an assumption.
func Doctor(w io.Writer) error {
	fmt.Fprintln(w, "Screen recorder diagnostics")
	fmt.Fprintln(w, "===========================")

	if info, err := native.BuildInfo(); err != nil {
		fmt.Fprintf(w, "  ERROR %v\n", err)
		return err
	} else {
		fmt.Fprintf(w, "  native layer: %s\n", info)
	}

	if err := doctorDisplays(w); err != nil {
		return err
	}
	if err := doctorCapture(w); err != nil {
		return err
	}
	if err := doctorEncoders(w); err != nil {
		return err
	}
	return doctorAudio(w)
}

func doctorDisplays(w io.Writer) error {
	fmt.Fprintln(w, "\nDisplays")
	list, err := native.Adapters()
	if err != nil {
		fmt.Fprintf(w, "  ERROR %v\n", err)
		return err
	}
	caps, err := native.QueryCapabilities()
	if err != nil {
		fmt.Fprintf(w, "  ERROR %v\n", err)
		return err
	}

	left, top, right, bottom := list[0].Bounds.Left, list[0].Bounds.Top,
		list[0].Bounds.Right, list[0].Bounds.Bottom
	for _, a := range list {
		if a.Bounds.Left < left {
			left = a.Bounds.Left
		}
		if a.Bounds.Top < top {
			top = a.Bounds.Top
		}
		if a.Bounds.Right > right {
			right = a.Bounds.Right
		}
		if a.Bounds.Bottom > bottom {
			bottom = a.Bounds.Bottom
		}
	}
	fmt.Fprintf(w, "  %d output(s), virtual desktop %dx%d at (%d,%d)\n",
		len(list), right-left, bottom-top, left, top)
	for _, a := range list {
		tag := ""
		switch {
		case a.Primary:
			tag = " [primary]"
		case a.Software:
			tag = " [software adapter]"
		}
		rotation := ""
		if a.Rotation != 0 {
			rotation = fmt.Sprintf(" rotated %d°", a.Rotation*90)
		}
		fmt.Fprintf(w, "  [%d] %s %s %.0f Hz%s%s duplicate=%t\n",
			a.Index, a.Description, a.Bounds, a.RefreshHz, rotation, tag, a.CanDuplicate)
	}

	fmt.Fprintf(w, "  graphics: %s\n", describeHardware(caps))
	return nil
}

// describeHardware answers the question a user actually has: will this machine use
// the video card, and if not, what happens instead.
func describeHardware(caps native.Capabilities) string {
	switch {
	case caps.Renderer == "":
		return "no adapter reported; recording will use the software path"
	case caps.HardwareH264:
		return fmt.Sprintf("%s can encode H.264 on the video card", caps.Renderer)
	case caps.SoftwareH264:
		return fmt.Sprintf("%s encodes H.264 in software (Motion JPEG fallback ready)",
			caps.Renderer)
	case caps.HardwareAdapters == 0:
		return fmt.Sprintf("%s reports no hardware adapter; Motion JPEG in software will be used",
			caps.Renderer)
	default:
		return fmt.Sprintf("%s; video is Motion JPEG in software", caps.Renderer)
	}
}

func doctorCapture(w io.Writer) error {
	fmt.Fprintln(w, "\nScreen capture")
	list, err := native.Adapters()
	if err != nil {
		fmt.Fprintf(w, "  ERROR %v\n", err)
		return err
	}
	target := list[0]
	session, note, err := native.OpenVerified(native.CaptureOptions{
		Adapter:     target.Adapter,
		Output:      target.Output,
		TrackCursor: true,
	})
	if err != nil {
		fmt.Fprintf(w, "  ERROR %v\n", err)
		return err
	}
	defer session.Close()

	fmt.Fprintf(w, "  backend: %s\n", session.Backend())
	if note != "" {
		fmt.Fprintf(w, "  note: %s\n", note)
	}

	frames := 0
	colours := 0
	cursorSeen := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && frames < 30 {
		f, err := session.Grab(500 * time.Millisecond)
		if err != nil {
			if native.Timeout(err) {
				continue
			}
			fmt.Fprintf(w, "  ERROR grabbing frames: %v\n", err)
			return err
		}
		frames++
		colours = native.DistinctColours(f)
		if f.Cursor.Valid {
			cursorSeen = true
		}
	}
	if frames == 0 {
		fmt.Fprintln(w, "  WARNING no frame arrived within 3s (a fully idle desktop can do this)")
		return nil
	}
	width, height := sessionSize(session)
	fmt.Fprintf(w, "  grabbed %d frame(s), %dx%d, %d distinct colours in the last one\n",
		frames, width, height, colours)
	if cursorSeen {
		fmt.Fprintln(w, "  pointer position is reported by the driver")
	} else {
		fmt.Fprintln(w, "  pointer position is not reported by the driver")
	}
	if colours < 2 {
		fmt.Fprintln(w, "  WARNING the frame looks blank; encoding would produce a solid-colour video")
	}
	return nil
}

// sessionSize reports the frame size, which the session only knows after a grab.
func sessionSize(s *native.Session) (width, height int) {
	return s.Size()
}

func doctorEncoders(w io.Writer) error {
	fmt.Fprintln(w, "\nEncoders")
	caps, err := native.QueryCapabilities()
	if err != nil {
		fmt.Fprintf(w, "  ERROR %v\n", err)
		return err
	}
	fmt.Fprintf(w, "  H.264: hardware=%t software=%t\n", caps.HardwareH264, caps.SoftwareH264)
	fmt.Fprintf(w, "  H.265: reserved for a later build\n")
	fmt.Fprintf(w, "  Motion JPEG into AVI: available=%t\n", caps.Mjpeg)
	if !caps.HardwareH264 && !caps.SoftwareH264 && !caps.Mjpeg {
		fmt.Fprintln(w, "  ERROR no video encoder was found on this machine")
		return fmt.Errorf("no video encoder is available")
	}

	// A real encode is what proves the chain works, so short files are
	// written and measured rather than merely configuring the encoder. Each
	// probe is one second, so its byte count is bytes per second and ×3600 is
	// the MB/h figure a long recording converges to.
	dir, err := tempDir()
	if err != nil {
		fmt.Fprintf(w, "  ERROR %v\n", err)
		return err
	}
	if caps.HardwareH264 || caps.SoftwareH264 {
		for _, probe := range []struct {
			name      string
			file      string
			withAudio bool
		}{
			{"H.264 video", "probe-h264.mp4", false},
			{"H.264 + AAC", "probe-h264-av.mp4", true},
		} {
			path := dir + string(osSeparator) + probe.file
			frames, size, hardware, err := probeH264(path, probe.withAudio, 600000, 70)
			switch {
			case err != nil:
				fmt.Fprintf(w, "  %-12s unavailable: %v\n", probe.name, err)
			default:
				fmt.Fprintf(w, "  %-12s wrote %d frames, %d bytes (~%.0f MB/h), hardware=%t\n",
					probe.name, frames, size, float64(size)*3600/1e6, hardware)
			}
		}
	} else {
		fmt.Fprintln(w, "  H.264 unavailable; recordings fall back to Motion JPEG")
	}
	for _, probe := range []struct {
		name      string
		file      string
		withAudio bool
	}{
		{"MJPEG video", "encoder-probe.avi", false},
		{"MJPEG + PCM", "encoder-probe-av.avi", true},
	} {
		path := dir + string(osSeparator) + probe.file
		frames, size, err := probeEncoder(path, probe.withAudio)
		switch {
		case err != nil:
			fmt.Fprintf(w, "  %-12s unavailable: %v\n", probe.name, err)
		default:
			fmt.Fprintf(w, "  %-12s wrote %d frames, %d bytes (~%.0f MB/h)\n",
				probe.name, frames, size, float64(size)*3600/1e6)
		}
	}
	return nil
}

func doctorAudio(w io.Writer) error {
	fmt.Fprintln(w, "\nAudio")
	devices, err := native.AudioDevices()
	if err != nil {
		fmt.Fprintf(w, "  ERROR %v\n", err)
		return err
	}
	if len(devices) == 0 {
		fmt.Fprintln(w, "  no output device found; recordings will have no sound")
		return nil
	}
	for _, name := range devices {
		fmt.Fprintf(w, "  %s\n", name)
	}
	capture, err := native.OpenAudio()
	if err != nil {
		fmt.Fprintf(w, "  loopback capture unavailable: %v\n", err)
		return nil
	}
	defer capture.Close()
	format, err := capture.Format()
	if err != nil {
		fmt.Fprintf(w, "  ERROR %v\n", err)
		return err
	}
	fmt.Fprintf(w, "  loopback capture OK: %d Hz, %d channel(s), %d-bit\n",
		format.SampleRate, format.Channels, format.BitsPerSample)
	return nil
}

// DescribeEncoders renders the encoder summary for the interface.
func DescribeEncoders(caps native.Capabilities) string {
	parts := make([]string, 0, 2)
	switch {
	case caps.HardwareH264:
		parts = append(parts, "H.264 hardware")
	case caps.SoftwareH264:
		parts = append(parts, "H.264 software")
	}
	if caps.Mjpeg {
		parts = append(parts, "Motion JPEG")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}
