package native

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestMjpegAviFile encodes synthetic frames plus a PCM block and validates the
// resulting AVI structurally: RIFF/AVI, an hdrl that declares MJPG, movi
// chunks whose video payloads start with a JPEG SOI marker, and an idx1 whose
// entry count matches the chunks written. A file that passes this is what any
// AVI reader opens; the container has no other moving parts.
func TestMjpegAviFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.avi")
	const (
		width  = 320
		height = 240
		frames = 10
	)
	enc, err := OpenEncoder(EncoderConfig{
		Width:       width,
		Height:      height,
		FPS:         30,
		Codec:       CodecMjpeg,
		Container:   ContainerAVI,
		Audio:       &AudioFormat{SampleRate: 8000, Channels: 1, BitsPerSample: 16, FormatTag: 1},
		JpegQuality: 80,
		OutputPath:  path,
	})
	if err != nil {
		t.Fatalf("OpenEncoder: %v", err)
	}
	stride := width * 4
	pixels := make([]byte, stride*height)
	for i := 0; i < frames; i++ {
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				o := y*stride + x*4
				pixels[o] = byte(x + i*8)
				pixels[o+1] = byte(y + i*4)
				pixels[o+2] = byte(x + y)
				pixels[o+3] = 0xFF
			}
		}
		if err := enc.EncodeBGRA(pixels, stride, 0); err != nil {
			enc.Close()
			t.Fatalf("EncodeBGRA frame %d: %v", i, err)
		}
	}
	// One PCM block: 1000 samples of 16-bit mono silence with a click.
	pcm := make([]byte, 2000)
	pcm[0], pcm[1] = 0xFF, 0x7F
	if err := enc.WriteAudio(pcm); err != nil {
		enc.Close()
		t.Fatalf("WriteAudio: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	avi, err := parseAVI(raw)
	if err != nil {
		t.Fatalf("parseAVI: %v", err)
	}
	if avi.videoChunks != frames {
		t.Errorf("movi has %d 00dc chunks, want %d", avi.videoChunks, frames)
	}
	if avi.audioChunks != 1 {
		t.Errorf("movi has %d 01wb chunks, want 1", avi.audioChunks)
	}
	if avi.idxVideo != frames || avi.idxAudio != 1 {
		t.Errorf("idx1 has %d video + %d audio entries, want %d + 1",
			avi.idxVideo, avi.idxAudio, frames)
	}
	if avi.totalFrames != frames {
		t.Errorf("avih dwTotalFrames = %d, want %d", avi.totalFrames, frames)
	}
	if !avi.jpegSOI {
		t.Error("first 00dc payload does not start with a JPEG SOI marker")
	}
	t.Logf("%d bytes, %d video chunks, handler=%q", len(raw), avi.videoChunks, avi.handler)
}

// TestH264Opens documents the current contract: the Media Foundation path
// is live, so H.264 into MP4 must configure. Only H.265 stays refused.
func TestH264Opens(t *testing.T) {
	enc, err := OpenEncoder(EncoderConfig{
		Width: 640, Height: 360, FPS: 30,
		Codec: CodecH264, Container: ContainerMP4,
		OutputPath: filepath.Join(t.TempDir(), "x.mp4"),
	})
	if err != nil {
		t.Fatalf("OpenEncoder H.264: %v", err)
	}
	t.Logf("H.264 open ok, hardware=%t", enc.Hardware())
	// Finalize needs at least one sample to build a moov box, so the open
	// contract is proven with real frames, not an empty shell.
	pixels := make([]byte, 640*4*360)
	for i := range pixels {
		pixels[i] = byte(i)
	}
	for i := 0; i < 3; i++ {
		if err := enc.EncodeBGRA(pixels, 640*4, 0); err != nil {
			enc.Close()
			t.Fatalf("EncodeBGRA: %v", err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestH265Refused documents the remaining reservation: no H.265 encoder
// exists in this build, so it must fail with StatusUnsupported rather than
// a broken file.
func TestH265Refused(t *testing.T) {
	_, err := OpenEncoder(EncoderConfig{
		Width: 640, Height: 360, FPS: 30,
		Codec: CodecH265, Container: ContainerMP4,
		OutputPath: filepath.Join(t.TempDir(), "x.mp4"),
	})
	if err == nil {
		t.Fatal("H.265 open succeeded; no H.265 encoder exists in this build")
	}
	if !Unsupported(err) {
		t.Fatalf("H.265 open failed with %v, want StatusUnsupported", err)
	}
	t.Logf("H.265 refused as designed: %v", err)
}

type aviInfo struct {
	videoChunks int
	audioChunks int
	idxVideo    int
	idxAudio    int
	totalFrames uint32
	jpegSOI     bool
	handler     string
	// strfVideoOk means the BITMAPINFOHEADER declares 1 plane of 24-bit
	// MJPG. A swapped planes/bitcount DWORD is exactly the kind of typo a
	// strict splitter (Media Foundation, WMP) rejects the whole file for.
	strfVideoOk bool
	// idxFirstOk means the first idx1 entry points at its chunk under the
	// muxer's convention: the offset lands on the chunk's size field, four
	// bytes before its data. Data-relative offsets enumerate nothing under
	// Media Foundation, so the convention is checked, not just the count.
	idxFirstOk bool
}

func parseAVI(raw []byte) (aviInfo, error) {
	var out aviInfo
	if len(raw) < 12 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "AVI " {
		return out, errors.New("not a RIFF AVI file")
	}
	if int(binary.LittleEndian.Uint32(raw[4:8]))+8 != len(raw) {
		return out, errors.New("RIFF size does not match the file")
	}
	// The muxer always writes hdrl, then movi, then idx1, so one walk finds the
	// movi data start first and the validation below can resolve idx offsets.
	moviData := -1
	for off := 12; off+8 <= len(raw); {
		size := int(binary.LittleEndian.Uint32(raw[off+4 : off+8]))
		body := off + 8
		if string(raw[off:off+4]) == "LIST" && body+4 <= len(raw) &&
			string(raw[body:body+4]) == "movi" {
			moviData = body + 4
		}
		off = body + size + size&1
	}
	if moviData < 0 {
		return out, errors.New("no movi list")
	}
	// Walk top-level chunks.
	for off := 12; off+8 <= len(raw); {
		tag := string(raw[off : off+4])
		size := int(binary.LittleEndian.Uint32(raw[off+4 : off+8]))
		body := off + 8
		switch tag {
		case "LIST":
			if body+4 > len(raw) {
				return out, errors.New("truncated LIST")
			}
			kind := string(raw[body : body+4])
			switch kind {
			case "hdrl":
				if err := parseHdrl(raw[body+4:body+size], &out); err != nil {
					return out, err
				}
			case "movi":
				end := body + size
				for p := body + 4; p+8 <= end && p+8 <= len(raw); {
					ctag := string(raw[p : p+4])
					csize := int(binary.LittleEndian.Uint32(raw[p+4 : p+8]))
					data := p + 8
					switch ctag {
					case "00dc":
						out.videoChunks++
						if out.videoChunks == 1 && csize >= 2 &&
							raw[data] == 0xFF && raw[data+1] == 0xD8 {
							out.jpegSOI = true
						}
					case "01wb":
						out.audioChunks++
					}
					p = data + csize + csize&1
				}
			}
		case "idx1":
			for p := body; p+16 <= body+size && p+16 <= len(raw); p += 16 {
				switch string(raw[p : p+4]) {
				case "00dc":
					out.idxVideo++
				case "01wb":
					out.idxAudio++
				}
			}
			if size >= 16 {
				off := int(binary.LittleEndian.Uint32(raw[body+8 : body+12]))
				want := int(binary.LittleEndian.Uint32(raw[body+12 : body+16]))
				if abs := moviData + off; abs-4 >= 0 && abs+4+want <= len(raw) {
					out.idxFirstOk = string(raw[abs-4:abs]) == string(raw[body:body+4]) &&
						int(binary.LittleEndian.Uint32(raw[abs:abs+4])) == want
				}
			}
		}
		off = body + size + size&1
	}
	if out.videoChunks == 0 {
		return out, errors.New("no 00dc chunks in movi")
	}
	if !out.idxFirstOk {
		return out, errors.New("first idx1 entry does not point at its chunk")
	}
	return out, nil
}

func parseHdrl(body []byte, out *aviInfo) error {
	for off := 0; off+8 <= len(body); {
		tag := string(body[off : off+4])
		size := int(binary.LittleEndian.Uint32(body[off+4 : off+8]))
		data := off + 8
		switch tag {
		case "avih":
			if size >= 20 {
				out.totalFrames = binary.LittleEndian.Uint32(body[data+16 : data+20])
			}
		case "LIST":
			if data+4 <= len(body) && string(body[data:data+4]) == "strl" {
				end := data + size
				var chain []string
				for p := data + 4; p+8 <= end && p+8 <= len(body); {
					st := string(body[p : p+4])
					ss := int(binary.LittleEndian.Uint32(body[p+4 : p+8]))
					sd := p + 8
					chain = append(chain, st)
					if st == "strh" && ss >= 12 {
						if string(body[sd:sd+4]) == "vids" {
							out.handler = string(body[sd+4 : sd+8])
						}
					}
					if st == "strf" && ss >= 16 {
						// biSize(0) biWidth(4) biHeight(8) biPlanes+biBitCount(12).
						if binary.LittleEndian.Uint32(body[sd:sd+4]) == 40 &&
							binary.LittleEndian.Uint32(body[sd+12:sd+16]) == (24<<16)|1 &&
							string(body[sd+16:sd+20]) == "MJPG" {
							out.strfVideoOk = true
						}
					}
					p = sd + ss + ss&1
				}
				// A strl chains exactly strh then strf. A wrong size field
				// desynchronises the walk, so the chain comes out garbled and
				// this catches it where field checks alone cannot.
				if len(chain) != 2 || chain[0] != "strh" || chain[1] != "strf" {
					return errors.New("a strl does not chain strh then strf")
				}
			}
		}
		off = data + size + size&1
	}
	if out.handler != "MJPG" {
		return errors.New("video stream handler is not MJPG")
	}
	if !out.strfVideoOk {
		return errors.New("video BITMAPINFOHEADER is not 1-plane 24-bit MJPG")
	}
	return nil
}
