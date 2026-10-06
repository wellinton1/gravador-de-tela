// Command recorder is a Windows screen recorder.
//
// Run with no arguments to open the control window; "doctor" prints a
// diagnostics report describing the capture and encoding capabilities of the
// machine, which is the first thing to check when a recording fails.
package main

import (
	"fmt"
	"os"

	"screenrec/internal/app"
)

func main() {
	if len(os.Args) > 1 {
		// GUI-subsystem binary: borrow the caller's console for CLI output.
		// Run console commands with `start /wait recorder.exe doctor` so the
		// prompt waits for the output instead of returning at once.
		attachParentConsole()
		switch os.Args[1] {
		case "doctor":
			if err := app.Doctor(os.Stdout); err != nil {
				fmt.Fprintln(os.Stderr, "doctor:", err)
				os.Exit(1)
			}
			return
		case "record":
			if err := app.Record(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "record:", err)
				os.Exit(1)
			}
			return
		case "snapshot":
			if err := app.Snapshot(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "snapshot:", err)
				os.Exit(1)
			}
			return
		case "-h", "--help", "help":
			usage()
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
			usage()
			os.Exit(2)
		}
	}

	if err := app.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "recorder:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`recorder - Windows screen recorder

Usage:
  recorder                 open the control window
  recorder doctor          report capture, encoder and audio capabilities
  recorder record [flags]  record headless (flags: --out, --seconds, --fps,
                           --display, --quality, --bitrate, --mjpeg, --no-audio)
  recorder snapshot [flags] save one PNG (flags: --out, --display)
  recorder help            show this message`)
}
