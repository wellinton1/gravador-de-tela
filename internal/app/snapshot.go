package app

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"screenrec/internal/native"
)

// Snapshot grabs one frame and saves it as PNG, like vokoscreen's snapshot.
func Snapshot(args []string) error {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	out := fs.String("out", "", "output .png file (default: timestamped in Pictures)")
	display := fs.Int("display", 0, "display index from `recorder doctor`")
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

	path := *out
	if path == "" {
		dir := picturesDir()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			dir = "."
		}
		path = filepath.Join(dir, fmt.Sprintf("foto-%s.png", time.Now().Format("20060102-150405")))
	}

	return SaveSnapshot(target.Adapter, target.Output, path)
}

// SaveSnapshot stores one frame as PNG through the shared native worker.
func SaveSnapshot(adapter, output uint32, path string) error {
	saved, err := native.SaveSnapshot(adapter, output, path)
	if err != nil {
		return err
	}
	fmt.Printf("saved %s\n", saved)
	return nil
}

func picturesDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, "Pictures")
	}
	return "."
}
