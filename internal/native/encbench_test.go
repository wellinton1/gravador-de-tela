package native

import (
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkEncode1080(b *testing.B) {
	enc, err := OpenEncoder(EncoderConfig{Width: 1920, Height: 1080, FPS: 30,
		Codec: CodecMjpeg, Container: ContainerAVI,
		OutputPath: filepath.Join(b.TempDir(), "bench.avi")})
	if err != nil {
		b.Fatalf("OpenEncoder: %v", err)
	}
	pixels := make([]byte, 1920*4*1080)
	for i := range pixels {
		pixels[i] = byte(i)
	}
	b.ResetTimer()
	start := time.Now()
	const n = 20
	for i := 0; i < n; i++ {
		if err := enc.EncodeBGRA(pixels, 1920*4, 0); err != nil {
			b.Fatalf("EncodeBGRA: %v", err)
		}
	}
	elapsed := time.Since(start)
	b.Logf("%d frames 1080p em %v (%.1f ms/frame)", n, elapsed, float64(elapsed.Milliseconds())/n)
	enc.Close()
}
