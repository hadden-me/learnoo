package media

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

type worstCase struct {
	name, file string
	data       []byte
}

// phoneJPEG is a w x h baseline 4:2:0 JPEG from Go's encoder (as phones
// write them) with EXIF orientation 6 and an MPF Ultra HDR gain map.
func phoneJPEG(t testing.TB, w, h int) []byte {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := m.PixOffset(x, y)
			m.Pix[i], m.Pix[i+1], m.Pix[i+2], m.Pix[i+3] = uint8(x/16), uint8(y/16), uint8((x+y)/32), 0xFF
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, m, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	prim := insertJPEGSegment(b.Bytes(), 0xE1, orientation(binary.BigEndian, 6))
	return withMPF(prim, binary.BigEndian, [][]byte{gainMap(t)}, nil)
}

func png8(t testing.TB, w, h int) []byte {
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m.SetNRGBA(x, y, color.NRGBA{uint8(x / 16), uint8(y / 16), 0x80, 0xFF})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// worstCases are the measured inputs: the most expensive variants at the
// largest accepted size, and ordinary phone photos and screenshots.
func worstCases(t testing.TB) []worstCase {
	const n = 4000 // 4000 x 4000 = MaxPixels
	rot6 := [][]byte{app1(orientation(binary.BigEndian, 6))}
	spec := func(comps []jpegComp, progressive bool, adobe int, segs [][]byte) []byte {
		return encodeSpec(jpegSpec{w: n, h: n, comps: comps, progressive: progressive, jfif: adobe < 0, adobe: adobe, segments: segs})
	}
	return []worstCase{
		{"JPEG 16 MP progressive CMYK", "prog-cmyk.jpg", spec(cmyk, true, 0, nil)},
		{"JPEG 16 MP progressive YCCK", "prog-ycck.jpg", spec(cmyk, true, 2, nil)},
		{"JPEG 16 MP baseline CMYK", "base-cmyk.jpg", spec(cmyk, false, 0, nil)},
		{"JPEG 16 MP progressive RGB, orientation 6", "prog-rgb-rot6.jpg", spec(rgb444, true, 0, rot6)},
		{"JPEG 16 MP progressive 4:4:4, orientation 6", "prog-444-rot6.jpg", spec(ycc444, true, -1, rot6)},
		{"JPEG 16 MP progressive 4:4:4", "prog-444.jpg", spec(ycc444, true, -1, nil)},
		{"JPEG 16 MP progressive 4:2:0, orientation 6", "prog-420-rot6.jpg", spec(ycc420, true, -1, rot6)},
		{"JPEG 16 MP baseline 4:4:4, orientation 6", "base-444-rot6.jpg", spec(ycc444, false, -1, rot6)},
		{"JPEG 16 MP progressive grayscale, orientation 6", "prog-gray-rot6.jpg", spec(gray, true, -1, rot6)},
		{"Phone JPEG 12 MP baseline 4:2:0, orientation 6, Ultra HDR", "phone-12mp.jpg", phoneJPEG(t, 4000, 3000)},
		{"Phone JPEG 16 MP baseline 4:2:0, orientation 6, Ultra HDR", "phone-16mp.jpg", phoneJPEG(t, n, n)},
		{"PNG 16 MP 8-bit RGBA", "png8.png", png8(t, n, n)},
		{"PNG 16 MP 16-bit RGBA", "png16.png", png16(t, n, n)},
		{"PNG 16 MP 16-bit RGBA, Adam7 interlaced", "png16-adam7.png", adam7PNG16(n, n)},
		{"WebP lossless 16 MP", "lossless.webp", webpLossless(n, n, false)},
		{"WebP lossless 16 MP, 16-colour palette", "lossless-palette.webp", webpLossless(n, n, true)},
	}
}

// TestWriteWorstCaseInputs writes the inputs for the whole-process
// measurement (scripts/upload-memory) to $LEARNOO_WORST_CASE_DIR.
func TestWriteWorstCaseInputs(t *testing.T) {
	dir := os.Getenv("LEARNOO_WORST_CASE_DIR")
	if dir == "" {
		t.Skip("set LEARNOO_WORST_CASE_DIR to write the inputs")
	}
	for _, c := range worstCases(t) {
		if err := os.WriteFile(filepath.Join(dir, c.file), c.data, 0o644); err != nil {
			t.Fatal(err)
		}
		fmt.Println(c.file, len(c.data))
	}
}
