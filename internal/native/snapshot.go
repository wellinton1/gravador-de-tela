package native

import (
	"fmt"
	"image"
	"image/png"
	"os"
	"time"
)

// SaveSnapshot grabs one verified frame and stores it as PNG. It is the
// shared worker behind the command line and the window button: no recording
// involved, one frame in, one file out.
func SaveSnapshot(adapter, output uint32, path string) (string, error) {
	session, err := Open(CaptureOptions{Adapter: adapter, Output: output})
	if err != nil {
		return "", err
	}
	defer session.Close()

	var frame Frame
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := session.Grab(500 * time.Millisecond)
		if err != nil {
			if Timeout(err) && time.Now().Before(deadline) {
				continue
			}
			return "", fmt.Errorf("capture produced no frame: %w", err)
		}
		frame = f
		break
	}

	// BGRA pixels become NRGBA: same bytes per pixel, red and blue swapped.
	img := image.NewNRGBA(image.Rect(0, 0, frame.Width, frame.Height))
	for y := 0; y < frame.Height; y++ {
		src := frame.Pixels[y*frame.Stride : y*frame.Stride+frame.Width*4]
		dst := img.Pix[y*img.Stride : y*img.Stride+frame.Width*4]
		for x := 0; x < frame.Width; x++ {
			dst[x*4] = src[x*4+2]
			dst[x*4+1] = src[x*4+1]
			dst[x*4+2] = src[x*4]
			dst[x*4+3] = src[x*4+3]
		}
	}

	file, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if err := png.Encode(file, img); err != nil {
		return "", err
	}
	return path, nil
}
