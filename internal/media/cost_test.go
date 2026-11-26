package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image/color"
	"image/jpeg"
	"testing"
)

// Expensive variants are refused from their headers with typed errors, and
// ordinary phone photos, screenshots and WebP images at the largest accepted
// size still pass.
func TestProcessDecodeBudget(t *testing.T) {
	const n = 4000
	rot6 := [][]byte{app1(orientation(binary.BigEndian, 6))}
	spec := func(w, h int, comps []jpegComp, progressive bool, adobe int, segs [][]byte) []byte {
		return encodeSpec(jpegSpec{w: w, h: h, comps: comps, progressive: progressive, jfif: adobe < 0, adobe: adobe, segments: segs})
	}
	for _, c := range []struct {
		name string
		data []byte
		want error
	}{
		{"progressive CMYK", spec(64, 64, cmyk, true, 0, nil), ErrUnsupportedCMYK},
		{"baseline CMYK", spec(64, 64, cmyk, false, 0, nil), ErrUnsupportedCMYK},
		{"YCCK", spec(64, 64, cmyk, false, 2, nil), ErrUnsupportedCMYK},
		{"16 MP progressive 4:4:4", spec(n, n, ycc444, true, -1, nil), ErrTooComplex},
		{"16 MP progressive RGB", spec(n, n, rgb444, true, 0, nil), ErrTooComplex},
		{"12 MP progressive 4:4:4, orientation 6", spec(n, 3000, ycc444, true, -1, rot6), ErrTooComplex},
		{"16 MP interlaced 16-bit PNG", adam7PNG16(n, n), ErrTooComplex},
		{"12 MP progressive 4:4:4", spec(n, 3000, ycc444, true, -1, nil), nil},
		{"9 MP progressive 4:4:4, orientation 6", spec(3000, 3000, ycc444, true, -1, rot6), nil},
		{"16 MP progressive 4:2:0, orientation 6", spec(n, n, ycc420, true, -1, rot6), nil},
		{"16 MP baseline RGB, orientation 6", spec(n, n, rgb444, false, 0, rot6), nil},
		{"12 MP phone photo, orientation 6, Ultra HDR", phoneJPEG(t, n, 3000), nil},
		{"16 MP phone photo, orientation 6, Ultra HDR", phoneJPEG(t, n, n), nil},
		{"16 MP 16-bit PNG", png16(t, n, n), nil},
		{"6 MP WebP lossless with palette", webpLossless(2400, 2400, true), nil},
		{"16 MP WebP lossless", webpLossless(n, n, false), ErrTooComplex},
		{"16 MP WebP lossless, 2600 Huffman groups", webpManyGroups(4096, 3906, false), ErrTooComplex},
		{"6 MP WebP lossless, 2600 Huffman groups", webpManyGroups(2400, 2400, true), nil},
		{"4 MP WebP with lossless alpha, 2600 Huffman groups", webpAlphaManyGroups(2048, 2048), nil},
		{"16 MP WebP with lossless alpha", webpAlphaManyGroups(n, n), ErrTooComplex},
		{"16 MP lossy WebP", webpLossy(n, n), nil},
	} {
		_, err := Process(context.Background(), bytes.NewReader(c.data), "")
		if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
		if errors.Is(c.want, ErrUnsupportedCMYK) && !errors.Is(err, ErrUnsupportedType) {
			t.Errorf("%s: %v does not match ErrUnsupportedType", c.name, err)
		}
	}
}

func TestJPEGCost(t *testing.T) {
	frame := func(progressive bool, comps ...jpegComponent) *jpegFrame {
		return &jpegFrame{progressive: progressive, comps: comps, jfif: true}
	}
	y420 := []jpegComponent{{1, 2, 2}, {2, 1, 1}, {3, 1, 1}}
	y444 := []jpegComponent{{1, 1, 1}, {2, 1, 1}, {3, 1, 1}}
	for _, c := range []struct {
		name string
		f    *jpegFrame
		w, h int
		o    int
		want int64
	}{
		{"baseline 4:2:0", frame(false, y420...), 4000, 4000, 1, 4000 * 4000 * 3 / 2},
		{"baseline 4:2:0 rotated", frame(false, y420...), 4000, 4000, 6, 4000*4000*3/2 + 4*4000*4000},
		{"progressive 4:2:0", frame(true, y420...), 4000, 4000, 1, 5 * 4000 * 4000 * 3 / 2},
		{"progressive 4:4:4", frame(true, y444...), 4000, 4000, 1, 15 * 4000 * 4000},
		{"padded to MCUs", frame(false, y420...), 17, 9, 1, 32*16 + 2*16*8},
		{"grayscale ignores sampling", frame(true, jpegComponent{1, 2, 2}), 16, 8, 8, 5*16*8 + 16*8},
		{"RGB", &jpegFrame{comps: []jpegComponent{{'R', 1, 1}, {'G', 1, 1}, {'B', 1, 1}}}, 8, 8, 1, 3*64 + 4*64},
	} {
		got, err := jpegCost(c.f, c.w, c.h, c.o)
		if err != nil || got != c.want {
			t.Errorf("%s: got %d %v, want %d", c.name, got, err, c.want)
		}
	}
	if _, err := jpegCost(frame(false, jpegComponent{1, 1, 1}, jpegComponent{2, 1, 1}, jpegComponent{3, 1, 1}, jpegComponent{4, 1, 1}), 8, 8, 1); !errors.Is(err, ErrUnsupportedCMYK) {
		t.Errorf("four components: %v", err)
	}
}

// The RGB rule follows image/jpeg, which reports such images with
// color.RGBAModel.
func TestJPEGFrameRGBMatchesDecoder(t *testing.T) {
	for _, c := range []struct {
		name  string
		comps []jpegComp
		jfif  bool
		adobe int
	}{
		{"JFIF YCbCr", ycc444, true, -1},
		{"plain YCbCr", ycc444, false, -1},
		{"Adobe transform 0", ycc444, false, 0},
		{"Adobe transform 1", ycc444, false, 1},
		{"JFIF wins over Adobe", ycc444, true, 0},
		{"RGB identifiers", rgb444, false, -1},
		{"RGB identifiers with JFIF", rgb444, true, -1},
	} {
		b := encodeSpec(jpegSpec{w: 16, h: 16, comps: c.comps, jfif: c.jfif, adobe: c.adobe})
		ins, err := inspectJPEG(b)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := ins.jpeg.isRGB(), cfg.ColorModel == color.RGBAModel; got != want {
			t.Errorf("%s: isRGB %v, decoder %v", c.name, got, want)
		}
	}
}

func TestPNGCost(t *testing.T) {
	header := func(depth, colorType, interlace byte) []byte {
		b := pngHeaderOnly(100, 10)
		b[24], b[25], b[28] = depth, colorType, interlace
		return b
	}
	for _, c := range []struct {
		depth, colorType, interlace byte
		want                        int64
	}{
		{8, 6, 0, 4000}, {8, 2, 0, 4000}, {8, 0, 0, 4000}, {1, 0, 0, 4000},
		{16, 6, 0, 8000}, {16, 0, 0, 8000}, {8, 3, 0, 1000}, {16, 6, 1, 16000}, {8, 3, 1, 2000},
	} {
		if got, err := pngCost(header(c.depth, c.colorType, c.interlace), 100, 10); err != nil || got != c.want {
			t.Errorf("depth %d type %d interlace %d: %d %v, want %d", c.depth, c.colorType, c.interlace, got, err, c.want)
		}
	}
}

func TestProcessRejectsOversizedEncoding(t *testing.T) {
	defer func(old int) { encodedLimit = old }(encodedLimit)
	in := pngBytes(t, 64, 64)
	encodedLimit = 100
	if _, err := process(in, ""); !errors.Is(err, ErrTooComplex) {
		t.Fatalf("got %v", err)
	}
	encodedLimit = MaxEncodedBytes
	if _, err := process(in, ""); err != nil {
		t.Fatal(err)
	}
}
