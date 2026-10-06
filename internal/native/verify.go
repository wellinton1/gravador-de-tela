package native

import (
	"fmt"
	"time"
)

// OpenVerified starts a capture session and checks that it actually produces
// pictures.
//
// Duplication is the preferred backend because it is fast and already contains
// the pointer, but a driver can accept it and still hand back empty frames, which
// would otherwise produce a black recording with no error anywhere. When the
// first frames are uniformly blank the session is dropped and the GDI fallback is
// used instead, and the reason is reported rather than swallowed.
func OpenVerified(opts CaptureOptions) (*Session, string, error) {
	session, err := Open(opts)
	if err != nil {
		return nil, "", err
	}
	if session.Backend() == backendGDI || opts.PreferGDI {
		return session, "", nil
	}

	blank, err := session.blankFrames(6, 250*time.Millisecond)
	if err != nil {
		// The session works but said nothing; that is not a reason to change it.
		if Timeout(err) {
			return session, "", nil
		}
		session.Close()
		return nil, "", err
	}
	if !blank {
		return session, "", nil
	}

	session.Close()
	opts.PreferGDI = true
	fallback, err := Open(opts)
	if err != nil {
		return nil, "", fmt.Errorf("duplication produced blank frames and the GDI fallback "+
			"failed as well: %w", err)
	}
	return fallback, "desktop duplication produced blank frames on this machine, " +
		"so the GDI fallback is in use", nil
}

// blankFrames reports whether every frame that arrived was a single flat colour.
//
// A recording of one colour is the failure this is meant to catch, so the test is
// deliberately strict: a real desktop, even a black one with a taskbar, has more
// than one colour in it.
func (s *Session) blankFrames(attempts int, timeout time.Duration) (bool, error) {
	seen := 0
	for i := 0; i < attempts; i++ {
		f, err := s.Grab(timeout)
		if err != nil {
			if Timeout(err) {
				continue
			}
			return false, err
		}
		seen++
		if DistinctColours(f) > 1 {
			return false, nil
		}
	}
	if seen == 0 {
		// No frame arrived at all: a static desktop legitimately produces no
		// updates, so there is nothing to judge. Report "not blank" and let
		// the caller decide; giving up here would refuse to record idle
		// screens, which are the common case, not the failure.
		return false, nil
	}
	return true, nil
}

// DistinctColours samples a frame and counts how many colours appear, which is
// a cheap way to tell a real desktop from a buffer that was never written.
func DistinctColours(f Frame) int {
	const limit = 8
	seen := make(map[uint32]struct{}, limit)
	step := 16
	for y := 0; y < f.Height && len(seen) < limit; y += step {
		for x := 0; x < f.Width && len(seen) < limit; x += step {
			i := f.At(x, y)
			if i+3 >= len(f.Pixels) {
				continue
			}
			key := uint32(f.Pixels[i])<<16 | uint32(f.Pixels[i+1])<<8 | uint32(f.Pixels[i+2])
			seen[key] = struct{}{}
		}
	}
	return len(seen)
}
