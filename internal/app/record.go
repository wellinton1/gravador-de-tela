package app

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"screenrec/internal/native"
	"screenrec/internal/rec"
)

// Record runs a headless recording: the same pipeline the window drives,
// without the window. It ends after --seconds or on Ctrl+C, whichever comes
// first, and reports the file it wrote.
func Record(args []string) error {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	out := fs.String("out", "", "output .avi file (default: timestamped in Videos)")
	seconds := fs.Int("seconds", 0, "stop after N seconds (default: run until Ctrl+C)")
	fps := fs.Int("fps", 30, "frames per second")
	display := fs.Int("display", 0, "display index from `recorder doctor`")
	noAudio := fs.Bool("no-audio", false, "record without sound")
	quality := fs.Int("quality", 80, "JPEG quality 1-100 (MJPEG only)")
	bitrate := fs.Int("bitrate", 600000, "H.264 target bits per second")
	vquality := fs.Int("vquality", 70, "H.264 quality level 1-100")
	mjpeg := fs.Bool("mjpeg", false, "force Motion JPEG into AVI instead of H.264")
	if err := fs.Parse(args); err != nil {
		return err
	}

	adapters, err := native.Adapters()
	if err != nil {
		return fmt.Errorf("no display: %w", err)
	}
	if *display < 0 || *display >= len(adapters) {
		return fmt.Errorf("display %d does not exist (have %d)", *display, len(adapters))
	}
	target := adapters[*display]

	codec := 0
	if *mjpeg {
		codec = native.CodecMjpeg
	}
	r, note, err := rec.Start(rec.Options{
		Adapter:      target.Adapter,
		Output:       target.Output,
		FPS:          *fps,
		Codec:        codec,
		Bitrate:      *bitrate,
		JpegQuality:  *quality,
		VideoQuality: *vquality,
		WithAudio:    !*noAudio,
		OutputPath:   *out,
	})
	if err != nil {
		return err
	}
	s := r.Stats()
	fmt.Printf("recording %dx%d (%s, %s) to %s\n", s.Width, s.Height, s.Backend, s.Codec, s.OutputPath)
	if note != "" {
		fmt.Printf("note: %s\n", note)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	defer signal.Stop(stop)

	if *seconds > 0 {
		select {
		case <-stop:
		case <-time.After(time.Duration(*seconds) * time.Second):
		}
	} else {
		fmt.Println("press Ctrl+C to stop")
		<-stop
	}

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	done := make(chan error, 1)
	go func() { done <- r.Stop() }()
	last := int64(-1)
	for {
		select {
		case err := <-done:
			final := r.Stats()
			if err != nil {
				return fmt.Errorf("record: %w", err)
			}
			info, serr := os.Stat(final.OutputPath)
			size := int64(0)
			if serr == nil {
				size = info.Size()
			}
			fmt.Printf("\nsaved %s (%d frames, %d bytes)\n", final.OutputPath, final.VideoFrames, size)
			return nil
		case <-tick.C:
			now := r.Stats().VideoFrames
			if now != last {
				fmt.Printf("\r%d frames…", now)
				last = now
			}
		}
	}
}

func videosDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home + string(osSeparator) + "Videos"
	}
	return "."
}
