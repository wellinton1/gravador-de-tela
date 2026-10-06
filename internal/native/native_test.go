package native

import "testing"

// TestLoadAndDescribe is the first thing that has to work: the library is found,
// its ABI matches, and it reports the machine it is running on.
func TestLoadAndDescribe(t *testing.T) {
	info, err := BuildInfo()
	if err != nil {
		t.Fatalf("BuildInfo: %v", err)
	}
	t.Logf("native library: %s", info)

	caps, err := QueryCapabilities()
	if err != nil {
		t.Fatalf("QueryCapabilities: %v", err)
	}
	t.Logf("adapters=%d hardware=%d duplication=%t renderer=%q", caps.Adapters,
		caps.HardwareAdapters, caps.HasDuplication, caps.Renderer)
	t.Logf("H.264 hardware=%t software=%t | H.265 hardware=%t software=%t",
		caps.HardwareH264, caps.SoftwareH264, caps.HardwareH265, caps.SoftwareH265)
	t.Logf("audio loopback=%t", caps.AudioLoopback)
}

func TestAdapters(t *testing.T) {
	list, err := Adapters()
	if err != nil {
		t.Fatalf("Adapters: %v", err)
	}
	for _, a := range list {
		t.Logf("[%d] %s %s rotation=%d %.0f Hz primary=%t duplicate=%t software=%t",
			a.Index, a.DeviceName, a.Bounds, a.Rotation, a.RefreshHz, a.Primary,
			a.CanDuplicate, a.Software)
	}
	if len(list) == 0 {
		t.Fatal("a machine with a display must report at least one adapter")
	}
}

// TestCaptureFrames checks both backends, because the fallback is the one that
// has to work when duplication is refused.
func TestCaptureFrames(t *testing.T) {
	list, err := Adapters()
	if err != nil {
		t.Skipf("no display: %v", err)
	}
	first := list[0]

	for _, preferGDI := range []bool{false, true} {
		name := "dxgi"
		if preferGDI {
			name = "gdi"
		}
		t.Run(name, func(t *testing.T) {
			session, note, err := OpenVerified(CaptureOptions{
				Adapter:     first.Adapter,
				Output:      first.Output,
				TrackCursor: true,
				PreferGDI:   preferGDI,
			})
			if err != nil {
				t.Fatalf("OpenVerified: %v", err)
			}
			defer session.Close()
			t.Logf("backend: %s", session.Backend())
			if note != "" {
				t.Logf("note: %s", note)
			}

			frames := 0
			for attempt := 0; attempt < 40 && frames < 3; attempt++ {
				f, err := session.Grab(200_000_000)
				if err != nil {
					if Timeout(err) {
						continue
					}
					t.Fatalf("Grab: %v", err)
				}
				if frames == 0 {
					t.Logf("frame %dx%d stride=%d cursor=%+v", f.Width, f.Height,
						f.Stride, f.Cursor)
				}
				if colours := DistinctColours(f); colours < 2 {
					t.Errorf("frame looks blank: %d distinct colours", colours)
				}
				frames++
			}
			if frames == 0 {
				t.Error("no frame arrived; a live desktop always produces some")
			}
		})
	}
}
