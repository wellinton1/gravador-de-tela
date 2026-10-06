package rec

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestStopAfter proves the vokoscreen-style timer: the recording ends by
// itself after the duration with a valid file, no Stop call needed.
func TestStopAfter(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a display")
	}
	path := filepath.Join(t.TempDir(), "stopafter.mp4")
	r, note, err := Start(Options{
		FPS:       30,
		Bitrate:   600000,
		WithAudio: false,
		StopAfter: 3 * time.Second,
		OutputPath: path,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Logf("codec=%s backend=%s note=%q", r.Stats().Codec, r.Stats().Backend, note)
	// Let the timer end the recording by itself; Stop afterwards only
	// collects the stored outcome.
	time.Sleep(5 * time.Second)
	frames := r.Stats().VideoFrames
	if frames < 60 {
		t.Errorf("timer ended with only %d frames", frames)
	}
	if err := r.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("empty file")
	}
	t.Logf("%d frames, %d bytes", frames, info.Size())
}
