package native

import (
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkEncodeH264(b *testing.B) {
	enc, err := OpenEncoder(EncoderConfig{Width: 1920, Height: 1080, FPS: 30,
		Bitrate: 600000, Codec: CodecH264, Container: ContainerMP4,
		OutputPath: filepath.Join(b.TempDir(), "bench.mp4")})
	if err != nil {
		b.Fatalf("OpenEncoder: %v", err)
	}
	b.Logf("hardware=%t", enc.Hardware())
	pixels := make([]byte, 1920*4*1080)
	for i := range pixels {
		pixels[i] = byte(i * 31)
	}
	b.ResetTimer()
	start := time.Now()
	const n = 60
	for i := 0; i < n; i++ {
		if err := enc.EncodeBGRA(pixels, 1920*4, 0); err != nil {
			b.Fatalf("EncodeBGRA: %v", err)
		}
	}
	elapsed := time.Since(start)
	b.Logf("%d frames 1080p em %v (%.1f ms/frame)", n, elapsed, float64(elapsed.Milliseconds())/n)
	if err := enc.Close(); err != nil {
		b.Fatalf("Close: %v", err)
	}
}
