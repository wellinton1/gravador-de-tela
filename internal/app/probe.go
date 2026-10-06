package app

import (
	"fmt"
	"math"
	"os"
	"path/filepath"

	"screenrec/internal/native"
)

const osSeparator = os.PathSeparator

func tempDir() (string, error) {
	return os.MkdirTemp("", "recorder-doctor-")
}

// probeEncoder records a short synthetic clip and reports the file size.
//
// Configuring an encoder successfully proves very little: the failure that
// matters is an encoder that accepts a frame and produces nothing. A moving
// gradient is fed in so the encoder has real work and the file has content.
func probeEncoder(path string, withAudio bool) (frames int, size int64, err error) {
	const (
		width  = 640
		height = 360
		fps    = 30
		count  = 30
	)
	var audio *native.AudioFormat
	if withAudio {
		audio = &native.AudioFormat{SampleRate: 44100, Channels: 2, BitsPerSample: 16, FormatTag: 1}
	}
	encoder, err := native.OpenEncoder(native.EncoderConfig{
		Width:       width,
		Height:      height,
		FPS:         fps,
		Codec:       native.CodecMjpeg,
		Container:   native.ContainerAVI,
		Audio:       audio,
		JpegQuality: 80,
		OutputPath:  path,
	})
	if err != nil {
		return 0, 0, err
	}

	stride := width * 4
	pixels := make([]byte, stride*height)
	for i := 0; i < count; i++ {
		paintBGRA(pixels, stride, width, height, i)
		if err := encoder.EncodeBGRA(pixels, stride, 0); err != nil {
			encoder.Close()
			return 0, 0, err
		}
	}
	if withAudio {
		// A 440 Hz tone, one second of 16-bit stereo: exercises the 01wb path
		// with bytes a player must decode as sound, not silence.
		tone := make([]byte, 44100*2*2)
		for s := range 44100 {
			v := int16(12000 * math.Sin(2*math.Pi*440*float64(s)/44100))
			tone[s*4] = byte(v)
			tone[s*4+1] = byte(v >> 8)
			tone[s*4+2] = byte(v)
			tone[s*4+3] = byte(v >> 8)
		}
		for off := 0; off < len(tone); off += 8192 {
			end := min(off+8192, len(tone))
			if err := encoder.WriteAudio(tone[off:end]); err != nil {
				encoder.Close()
				return 0, 0, err
			}
		}
	}
	if err := encoder.Close(); err != nil {
		return 0, 0, err
	}

	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	if info.Size() == 0 {
		return 0, 0, fmt.Errorf("the encoder produced an empty file")
	}
	return count, info.Size(), nil
}

func containerFor(path string) int {
	if filepath.Ext(path) == ".avi" {
		return native.ContainerAVI
	}
	return native.ContainerMP4
}

// probeH264 records a short synthetic H.264 clip and reports frames, bytes
// and whether the GPU did the work. The clip is one second at 30 fps, so its
// byte count reads directly as bytes per second — multiply by 3600 for the
// honest MB/h figure the user asked about.
func probeH264(path string, withAudio bool, bitrate, quality int) (frames int, size int64, hardware bool, err error) {
	const (
		width  = 640
		height = 360
		fps    = 30
		count  = 30
	)
	var audio *native.AudioFormat
	if withAudio {
		audio = &native.AudioFormat{SampleRate: 44100, Channels: 2, BitsPerSample: 16, FormatTag: 1}
	}
	encoder, err := native.OpenEncoder(native.EncoderConfig{
		Width:        width,
		Height:       height,
		FPS:          fps,
		Bitrate:      bitrate,
		Codec:        native.CodecH264,
		Container:    native.ContainerMP4,
		Audio:        audio,
		VideoQuality: quality,
		OutputPath:   path,
	})
	if err != nil {
		return 0, 0, false, err
	}
	hardware = encoder.Hardware()

	stride := width * 4
	pixels := make([]byte, stride*height)
	// H.264 needs even dimensions for NV12; 640x360 already is.
	for i := 0; i < count; i++ {
		paintBGRA(pixels, stride, width, height, i)
		if err := encoder.EncodeBGRA(pixels, stride, 0); err != nil {
			encoder.Close()
			return 0, 0, hardware, err
		}
	}
	if withAudio {
		// One second of 16-bit stereo tone, in ~46 ms blocks like loopback.
		const total = 44100
		block := make([]byte, 4096)
		for s := 0; s < total; {
			n := min(len(block)/4, total-s)
			for i := 0; i < n; i++ {
				v := int16(12000 * math.Sin(2*math.Pi*440*float64(s+i)/44100))
				block[i*4] = byte(v)
				block[i*4+1] = byte(v >> 8)
				block[i*4+2] = byte(v)
				block[i*4+3] = byte(v >> 8)
			}
			if err := encoder.WriteAudio(block[:n*4]); err != nil {
				encoder.Close()
				return 0, 0, hardware, err
			}
			s += n
		}
	}
	if err := encoder.Close(); err != nil {
		return 0, 0, hardware, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, hardware, err
	}
	if info.Size() == 0 {
		return 0, 0, hardware, fmt.Errorf("the encoder produced an empty file")
	}
	return count, info.Size(), hardware, nil
}

// paintBGRA fills a frame with a moving colour gradient, which gives the
// encoder real inter-frame motion instead of a still image.
func paintBGRA(pixels []byte, stride, width, height, phase int) {
	for y := 0; y < height; y++ {
		base := y * stride
		for x := 0; x < width; x++ {
			i := base + x*4
			pixels[i] = byte((x + phase*8) & 0xFF)       // B
			pixels[i+1] = byte((y*2 + phase*4) & 0xFF)   // G
			pixels[i+2] = byte((x + y + phase*2) & 0xFF) // R
			pixels[i+3] = 0xFF                           // A
		}
	}
}
