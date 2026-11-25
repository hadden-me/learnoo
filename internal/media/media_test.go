package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"strings"
	"testing"
)

// WebP fixtures: a 4x3 gradient PNG generated in Go (see testImage) converted
// with libwebp's cwebp 1.6.0:
//
//	cwebp -quiet -lossless -metadata none in.png -o ll.webp
//	cwebp -quiet -q 80 -metadata none in.png -o lossy.webp
//
// Progressive JPEG fixture: testImage(16, 16) encoded by Go's image/jpeg
// (default quality 75, baseline 4:2:0), then converted losslessly with
// libjpeg-turbo 3.2.0's jpegtran:
//
//	jpegtran -progressive -copy none -outfile prog.jpg base.jpg
const (
	jpegProgressiveHex = "ffd8ffe000104a46494600010100000100010000ffdb004300080606070605080707070909080a0c140d0c0b0b0c1912130f141d1a1f1e1d1a1c1c20242e2720222c231c1c2837292c30313434341f27393d38323c2e333432ffdb0043010909090c0b0c180d0d1832211c213232323232323232323232323232323232323232323232323232323232323232323232323232323232323232323232323232ffc20011080010001003012200021101031101ffc4001500010100000000000000000000000000000001ffc4001501010100000000000000000000000000000305ffda000c030100021003100000018291ff00ffc4001810000301010000000000000000000000000102030013ffda000801010001050249c3919c3217e44befffc4001911000105000000000000000000000000000100111221a1ffda0008010301013f010f017a57ffc4001911000105000000000000000000000000000001112131a1ffda0008010201013f0197bd53ffc4001c1000010403010000000000000000000000000203313221525322ffda0008010100063f024f86e7a146efd04e1c9d124397d127ffc4001c100002020203000000000000000000000000610121415191a1f0ffda0008010100013f2140b8ea5173a968483c525cf0a4ffda000c03010002000300000010fbffc40017110003010000000000000000000000000000018191ffda0008010301013f108f581fffc400161100030000000000000000000000000000000121ffda0008010201013f107607ffc400161001010100000000000000000000000000007110ffda0008010100013f10271153e20502a30fffd9"
	webpLosslessHex    = "5249464638000000574542505650384c2b0000002f038000005f40906d33c422dcdfe30ec333c8f3df830cb05c00820012a51452186e8515468de87f50970300"
	webpLossyHex       = "524946465a00000057454250565038204e0000003002009d012a0400030001402625b002744c807e8001d9a493c400fec2ad3c6da06240fff26b1fa7d2200fd774f0d6ebf36dd31e1ef53cdfb2ffbfff2bb53eb7b3fb6d51fd3ba0c7dfff92ee0000"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testImage(w, h int) *image.NRGBA {
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m.Set(x, y, color.NRGBA{uint8(60 * x), uint8(80 * y), 200, 255})
		}
	}
	return m
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, testImage(w, h)); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, testImage(w, h), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// pngChunk builds a PNG chunk with a valid CRC.
func pngChunk(typ string, data []byte) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, uint32(len(data)))
	b.WriteString(typ)
	b.Write(data)
	_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(typ), data...)))
	return b.Bytes()
}

// pngHeaderOnly is a structurally complete PNG whose IHDR claims w x h but
// whose IDAT holds no pixel data: DecodeConfig succeeds, a full decode fails,
// so ErrDimensions proves rejection before the full decode.
func pngHeaderOnly(w, h uint32) []byte {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 6 // RGBA
	out := append([]byte("\x89PNG\r\n\x1a\n"), pngChunk("IHDR", ihdr)...)
	out = append(out, pngChunk("IDAT", []byte{0})...)
	return append(out, pngChunk("IEND", nil)...)
}

// insertBeforeIEND inserts extra chunks before the final IEND chunk.
func insertBeforeIEND(p []byte, chunks ...[]byte) []byte {
	cut := len(p) - 12
	out := append([]byte{}, p[:cut]...)
	for _, c := range chunks {
		out = append(out, c...)
	}
	return append(out, p[cut:]...)
}

// jpegWithDims patches the SOF0 frame header of a baseline JPEG.
func jpegWithDims(t *testing.T, w, h uint16) []byte {
	t.Helper()
	b := jpegBytes(t, 8, 8)
	i := bytes.Index(b, []byte{0xFF, 0xC0})
	if i < 0 {
		t.Fatal("no SOF0 marker")
	}
	binary.BigEndian.PutUint16(b[i+5:], h)
	binary.BigEndian.PutUint16(b[i+7:], w)
	return b
}

func process(data []byte, declared string) (*Asset, error) {
	return Process(context.Background(), bytes.NewReader(data), declared)
}

// insertJPEGSegment inserts a marker segment right after SOI.
func insertJPEGSegment(jpg []byte, marker byte, payload []byte) []byte {
	seg := []byte{0xFF, marker, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(payload)+2))
	seg = append(seg, payload...)
	return append(append(append([]byte{}, jpg[:2]...), seg...), jpg[2:]...)
}

// patchSOF sets the dimensions in the first SOF marker of the given type.
func patchSOF(t *testing.T, b []byte, sof byte, w, h uint16) []byte {
	t.Helper()
	b = append([]byte{}, b...)
	i := bytes.Index(b, []byte{0xFF, sof})
	if i < 0 {
		t.Fatalf("no SOF 0x%02X marker", sof)
	}
	binary.BigEndian.PutUint16(b[i+5:], h)
	binary.BigEndian.PutUint16(b[i+7:], w)
	return b
}

// riff wraps WebP chunks in a RIFF container with a correct length.
func riff(chunks ...[]byte) []byte {
	body := []byte("WEBP")
	for _, c := range chunks {
		body = append(body, c...)
	}
	out := []byte("RIFF\x00\x00\x00\x00")
	binary.LittleEndian.PutUint32(out[4:], uint32(len(body)))
	return append(out, body...)
}

func webpChunk(id string, data []byte) []byte {
	c := []byte(id + "\x00\x00\x00\x00")
	binary.LittleEndian.PutUint32(c[4:], uint32(len(data)))
	c = append(c, data...)
	if len(data)%2 == 1 {
		c = append(c, 0)
	}
	return c
}

// vp8xChunk declares a w x h canvas with the given feature flags.
func vp8xChunk(flags byte, w, h uint32) []byte {
	d := make([]byte, 10)
	d[0] = flags
	w--
	h--
	d[4], d[5], d[6] = byte(w), byte(w>>8), byte(w>>16)
	d[7], d[8], d[9] = byte(h), byte(h>>8), byte(h>>16)
	return webpChunk("VP8X", d)
}

// losslessVP8L returns the VP8L chunk of the lossless fixture (4x3).
func losslessVP8L(t *testing.T) []byte {
	return mustHex(t, webpLosslessHex)[12:]
}

// webpVP8LWithDims patches the VP8L bitstream header of the lossless fixture.
func webpVP8LWithDims(t *testing.T, w, h uint32) []byte {
	b := mustHex(t, webpLosslessHex)
	if b[20] != 0x2f {
		t.Fatal("fixture is not VP8L")
	}
	bits := binary.LittleEndian.Uint32(b[21:])
	bits = bits&^(1<<28-1) | (w - 1) | (h-1)<<14
	binary.LittleEndian.PutUint32(b[21:], bits)
	return b
}

func TestProcessAccepts(t *testing.T) {
	tests := []struct {
		name, declared, source, stored string
		data                           []byte
		w, h                           int
	}{
		{"png", "image/png", ContentTypePNG, ContentTypePNG, pngBytes(t, 4, 3), 4, 3},
		{"png no declared type", "", ContentTypePNG, ContentTypePNG, pngBytes(t, 4, 3), 4, 3},
		{"png octet-stream", "application/octet-stream", ContentTypePNG, ContentTypePNG, pngBytes(t, 4, 3), 4, 3},
		{"jpeg", "image/jpeg", ContentTypeJPEG, ContentTypeJPEG, jpegBytes(t, 40, 30), 40, 30},
		{"jpeg declared with params", "Image/JPEG; charset=binary", ContentTypeJPEG, ContentTypeJPEG, jpegBytes(t, 2, 2), 2, 2},
		{"jpeg progressive", "image/jpeg", ContentTypeJPEG, ContentTypeJPEG, mustHex(t, jpegProgressiveHex), 16, 16},
		{"webp lossless", "image/webp", ContentTypeWebP, ContentTypePNG, mustHex(t, webpLosslessHex), 4, 3},
		{"webp lossy", "image/webp", ContentTypeWebP, ContentTypePNG, mustHex(t, webpLossyHex), 4, 3},
		{"webp vp8x still", "", ContentTypeWebP, ContentTypePNG, riff(vp8xChunk(0, 4, 3), losslessVP8L(t)), 4, 3},
		{"max side", "", ContentTypePNG, ContentTypePNG, pngBytes(t, MaxSide, 1), MaxSide, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, err := process(tc.data, tc.declared)
			if err != nil {
				t.Fatalf("Process: %v", err)
			}
			if a.ContentType() != tc.stored || a.SourceType() != tc.source || a.Width() != tc.w || a.Height() != tc.h {
				t.Fatalf("got %s from %s %dx%d", a.ContentType(), a.SourceType(), a.Width(), a.Height())
			}
			if a.ByteSize() != int64(len(a.Data())) || len(a.SHA256()) != 64 || !ValidKey(a.Key()) {
				t.Fatalf("inconsistent asset: size %d key %q", a.ByteSize(), a.Key())
			}
			// Stored bytes are a valid image of the stored type.
			f, _ := sniff(a.Data())
			if f.contentType != tc.stored {
				t.Fatalf("stored bytes sniff as %q", f.contentType)
			}
			if _, err := f.decode(bytes.NewReader(a.Data())); err != nil {
				t.Fatalf("stored bytes do not decode: %v", err)
			}
		})
	}
}

func TestProcessDeterministic(t *testing.T) {
	for name, in := range map[string][]byte{
		"png":              pngBytes(t, 5, 5),
		"jpeg":             jpegBytes(t, 24, 24),
		"jpeg progressive": mustHex(t, jpegProgressiveHex),
		"webp lossless":    mustHex(t, webpLosslessHex),
		"webp lossy":       mustHex(t, webpLossyHex),
	} {
		a, err := process(in, "")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, err := process(in, "")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if a.SHA256() != b.SHA256() || !bytes.Equal(a.Data(), b.Data()) {
			t.Errorf("%s: same input produced different canonical bytes", name)
		}
	}
}

func TestProcessRejects(t *testing.T) {
	var gifBuf bytes.Buffer
	if err := gif.Encode(&gifBuf, testImage(2, 2), nil); err != nil {
		t.Fatal(err)
	}
	truncatedPNG := pngBytes(t, 64, 64)
	truncatedPNG = truncatedPNG[:len(truncatedPNG)/2]
	corruptJPEG := jpegBytes(t, 32, 32)
	for i := len(corruptJPEG) / 2; i < len(corruptJPEG)-2; i++ {
		corruptJPEG[i] = 0xFF
	}
	webpLossless := mustHex(t, webpLosslessHex)
	tooBig := append(pngBytes(t, 1, 1), make([]byte, MaxUploadBytes)...)
	progressive := mustHex(t, jpegProgressiveHex)
	// SOF9 (arithmetic coding) is valid JPEG that Go does not support.
	arithmetic := patchSOF(t, jpegBytes(t, 8, 8), 0xC0, 8, 8)
	arithmetic[bytes.Index(arithmetic, []byte{0xFF, 0xC0})+1] = 0xC9
	vp8xHuge := riff(vp8xChunk(0, 5000, 10), losslessVP8L(t))
	vp8xMegapixels := riff(vp8xChunk(0, 4000, 4001), losslessVP8L(t))
	animated := riff(vp8xChunk(0x02, 4, 3), webpChunk("ANIM", make([]byte, 6)))
	animChunkNoFlag := riff(vp8xChunk(0, 4, 3), webpChunk("ANIM", make([]byte, 6)), losslessVP8L(t))
	riffTooLong := append([]byte{}, webpLossless...)
	binary.LittleEndian.PutUint32(riffTooLong[4:], uint32(len(riffTooLong)))

	tests := []struct {
		name, declared string
		data           []byte
		want           error
	}{
		{"empty", "", nil, ErrEmpty},
		{"svg", "", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), ErrUnsupportedType},
		{"svg with xml prolog", "image/svg+xml", []byte(`<?xml version="1.0"?><svg/>`), ErrUnsupportedType},
		{"gif", "image/gif", gifBuf.Bytes(), ErrUnsupportedType},
		{"bmp", "", append([]byte("BM"), make([]byte, 64)...), ErrUnsupportedType},
		{"html", "text/html", []byte("<!doctype html><html><script>alert(1)</script></html>"), ErrUnsupportedType},
		{"text", "", []byte("just some text"), ErrUnsupportedType},
		{"jpeg arithmetic coding", "", arithmetic, ErrUnsupportedType},
		{"webp animated flag", "", animated, ErrUnsupportedType},
		{"webp ANIM chunk without flag", "", animChunkNoFlag, ErrUnsupportedType},
		{"webp unknown first chunk", "", riff(webpChunk("EXIF", []byte("xx")), losslessVP8L(t)), ErrUnsupportedType},
		{"png declared as jpeg", "image/jpeg", pngBytes(t, 2, 2), ErrTypeMismatch},
		{"png declared as svg", "image/svg+xml", pngBytes(t, 2, 2), ErrTypeMismatch},
		{"webp declared as png", "image/png", webpLossless, ErrTypeMismatch},
		{"jpeg declared garbage", "not a type", jpegBytes(t, 2, 2), ErrTypeMismatch},
		{"over 5 MiB", "image/png", tooBig, ErrTooLarge},
		{"png too wide (header only)", "", pngHeaderOnly(MaxSide+1, 1), ErrDimensions},
		{"png too tall (header only)", "", pngHeaderOnly(1, MaxSide+1), ErrDimensions},
		{"png too many pixels (header only)", "", pngHeaderOnly(MaxSide, MaxSide), ErrDimensions},
		{"png huge (header only)", "", pngHeaderOnly(100_000, 100_000), ErrDimensions},
		{"jpeg too wide (header only)", "", jpegWithDims(t, MaxSide+1, 8), ErrDimensions},
		{"jpeg too many pixels (header only)", "", jpegWithDims(t, 4000, 4001), ErrDimensions},
		{"progressive jpeg too wide (header only)", "", patchSOF(t, progressive, 0xC2, MaxSide+1, 16), ErrDimensions},
		{"progressive jpeg too many pixels (header only)", "", patchSOF(t, progressive, 0xC2, 4000, 4001), ErrDimensions},
		{"webp vp8l too wide (header only)", "", webpVP8LWithDims(t, MaxSide+1, 3), ErrDimensions},
		{"webp vp8l too many pixels (header only)", "", webpVP8LWithDims(t, 4000, 4001), ErrDimensions},
		{"webp vp8x canvas too wide (header only)", "", vp8xHuge, ErrDimensions},
		{"webp vp8x canvas too many pixels (header only)", "", vp8xMegapixels, ErrDimensions},
		{"jpeg zero height", "", jpegWithDims(t, 8, 0), ErrDimensions},
		{"png zero width", "", pngHeaderOnly(0, 8), ErrCorrupt},
		{"truncated png", "", truncatedPNG, ErrCorrupt},
		{"corrupt jpeg", "", corruptJPEG, ErrCorrupt},
		{"truncated jpeg", "", progressive[:len(progressive)-40], ErrCorrupt},
		{"truncated webp", "", webpLossless[:30], ErrCorrupt},
		{"webp RIFF length beyond file", "", riffTooLong, ErrCorrupt},
		{"webp vp8x and vp8l disagree", "", riff(vp8xChunk(0, 8, 3), losslessVP8L(t)), ErrCorrupt},
		{"signature only png", "", []byte("\x89PNG\r\n\x1a\n"), ErrCorrupt},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, err := process(tc.data, tc.declared)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got asset=%v err=%v, want %v", a != nil, err, tc.want)
			}
		})
	}
}

// The PNG decoder rejects a zero dimension itself (ErrCorrupt above); the JPEG
// header path reaches checkDimensions (ErrDimensions above).
func TestCheckDimensions(t *testing.T) {
	for _, d := range [][2]int{{0, 1}, {1, 0}, {-1, 5}, {MaxSide + 1, 1}, {MaxSide, MaxSide}} {
		if err := checkDimensions(d[0], d[1]); !errors.Is(err, ErrDimensions) {
			t.Errorf("%v: got %v", d, err)
		}
	}
	for _, d := range [][2]int{{1, 1}, {MaxSide, 1}, {4000, 4000}} {
		if err := checkDimensions(d[0], d[1]); err != nil {
			t.Errorf("%v: got %v", d, err)
		}
	}
}

// countingReader yields an endless stream of zero bytes and counts reads.
type countingReader struct{ n int64 }

func (r *countingReader) Read(p []byte) (int, error) {
	clear(p)
	r.n += int64(len(p))
	return len(p), nil
}

func TestProcessReadsAtMostLimit(t *testing.T) {
	r := &countingReader{}
	_, err := Process(context.Background(), io.MultiReader(bytes.NewReader(pngBytes(t, 1, 1)), r), "")
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v", err)
	}
	if r.n > MaxUploadBytes+1 {
		t.Fatalf("read %d bytes from an unbounded stream", r.n)
	}
}

func TestProcessExactlyAtLimit(t *testing.T) {
	// A private ancillary chunk pads a valid PNG to exactly the limit.
	in := pngBytes(t, 3, 3)
	pad := MaxUploadBytes - len(in) - 12
	in = insertBeforeIEND(in, pngChunk("prVt", bytes.Repeat([]byte("x"), pad)))
	if len(in) != MaxUploadBytes {
		t.Fatalf("fixture is %d bytes", len(in))
	}
	a, err := process(in, "image/png")
	if err != nil {
		t.Fatalf("upload of exactly %d bytes rejected: %v", MaxUploadBytes, err)
	}
	if a.ByteSize() > 1024 {
		t.Fatalf("padding kept: %d bytes stored", a.ByteSize())
	}
}

const script = "<script>alert(document.cookie)</script>"

func TestProcessRejectsTrailingData(t *testing.T) {
	zipData := []byte("PK\x03\x04payload.html<html>PK\x05\x06")
	jpg := jpegBytes(t, 16, 16)
	progressive := mustHex(t, jpegProgressiveHex)
	webpLossless := mustHex(t, webpLosslessHex)
	// A trailer inside the RIFF length is a malformed chunk list, not trailing data.
	webpInnerJunk := append(append([]byte{}, webpLossless...), script...)
	binary.LittleEndian.PutUint32(webpInnerJunk[4:], uint32(len(webpInnerJunk)-8))

	tests := []struct {
		name string
		data []byte
		want error
	}{
		{"png script", append(pngBytes(t, 4, 4), script...), ErrTrailingData},
		{"png zip", append(pngBytes(t, 4, 4), zipData...), ErrTrailingData},
		{"png one byte", append(pngBytes(t, 4, 4), 0), ErrTrailingData},
		{"jpeg script", append(append([]byte{}, jpg...), script...), ErrTrailingData},
		{"jpeg zip", append(append([]byte{}, jpg...), zipData...), ErrTrailingData},
		{"jpeg second EOI", append(append([]byte{}, jpg...), 0xFF, 0xD9), ErrTrailingData},
		{"jpeg one byte", append(append([]byte{}, jpg...), 0), ErrTrailingData},
		{"progressive jpeg script", append(append([]byte{}, progressive...), script...), ErrTrailingData},
		{"webp script", append(append([]byte{}, webpLossless...), script...), ErrTrailingData},
		{"webp zip", append(append([]byte{}, webpLossless...), zipData...), ErrTrailingData},
		{"webp junk inside RIFF", webpInnerJunk, ErrCorrupt},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := process(tc.data, ""); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestProcessStripsMetadata(t *testing.T) {
	pngWithChunks := insertBeforeIEND(pngBytes(t, 4, 4),
		pngChunk("tEXt", []byte("Comment\x00"+script)),
		pngChunk("eXIf", []byte("MM\x00*GPSSECRET")),
		pngChunk("iCCP", []byte("ICCSECRET\x00\x00garbage")),
	)
	jpg := jpegBytes(t, 16, 16)
	// The EXIF payload embeds a thumbnail-like SOI...EOI that must not end the scan.
	exif := []byte("Exif\x00\x00MM\x00*GPSSECRET\xFF\xD8\xFF\xD9" + script)
	jpegWithExif := insertJPEGSegment(jpg, 0xE1, exif)
	xmp := []byte("http://ns.adobe.com/xap/1.0/\x00<x:xmpmeta>XMPSECRET" + script + "</x:xmpmeta>")
	jpegWithXMP := insertJPEGSegment(jpg, 0xE1, xmp)
	icc := []byte("ICC_PROFILE\x00\x01\x01ICCSECRET")
	jpegWithICC := insertJPEGSegment(jpg, 0xE2, icc)
	progressiveWithExif := insertJPEGSegment(mustHex(t, jpegProgressiveHex), 0xE1, exif)
	webpWithMeta := riff(vp8xChunk(0x08|0x04, 4, 3), losslessVP8L(t),
		webpChunk("EXIF", []byte("MM\x00*GPSSECRET")), webpChunk("XMP ", []byte("XMPSECRET"+script)))

	tests := []struct {
		name   string
		data   []byte
		banned []string
	}{
		{"png text exif iccp chunks", pngWithChunks, []string{script, "tEXt", "eXIf", "iCCP", "GPSSECRET", "ICCSECRET"}},
		{"jpeg exif app1", jpegWithExif, []string{"GPSSECRET", "Exif", script, "\xFF\xE1"}},
		{"jpeg xmp app1", jpegWithXMP, []string{"XMPSECRET", "ns.adobe.com", script, "\xFF\xE1"}},
		{"jpeg icc app2", jpegWithICC, []string{"ICCSECRET", "ICC_PROFILE", "\xFF\xE2"}},
		{"progressive jpeg exif app1", progressiveWithExif, []string{"GPSSECRET", "Exif", script}},
		{"webp exif xmp chunks", webpWithMeta, []string{"GPSSECRET", "XMPSECRET", script, "RIFF", "WEBP"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !bytes.Contains(tc.data, []byte(tc.banned[0])) {
				t.Fatalf("fixture lacks %q", tc.banned[0])
			}
			a, err := process(tc.data, "")
			if err != nil {
				t.Fatalf("Process: %v", err)
			}
			for _, s := range tc.banned {
				if bytes.Contains(a.Data(), []byte(s)) {
					t.Errorf("stored bytes contain %q", s)
				}
			}
			if strings.Contains(strings.ToLower(string(a.Data())), "<script") {
				t.Error("stored bytes contain a script tag")
			}
		})
	}
}

func TestProcessConcurrencyCap(t *testing.T) {
	// Occupy every slot, as MaxConcurrentDecodes in-flight decodes would.
	for range MaxConcurrentDecodes {
		decodeSlots <- struct{}{}
	}
	waiting := make(chan struct{}, 2)
	testHookWaitingForSlot = func() { waiting <- struct{}{} }
	defer func() { testHookWaitingForSlot = nil }()

	in := pngBytes(t, 4, 4)
	type result struct {
		a   *Asset
		err error
	}
	blocked := make(chan result, 1)
	go func() {
		a, err := process(in, "")
		blocked <- result{a, err}
	}()
	<-waiting
	select {
	case r := <-blocked:
		t.Fatalf("Process ran while all slots were taken: %v", r.err)
	default:
	}

	// A waiting call honours cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan error, 1)
	go func() {
		_, err := Process(ctx, bytes.NewReader(in), "")
		cancelled <- err
	}()
	<-waiting
	cancel()
	if err := <-cancelled; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait returned %v", err)
	}

	// Freeing one slot lets the blocked call finish, and it releases its slot.
	<-decodeSlots
	if r := <-blocked; r.err != nil || r.a == nil {
		t.Fatalf("blocked Process: %v", r.err)
	}
	for range MaxConcurrentDecodes - 1 {
		<-decodeSlots
	}
	if n := len(decodeSlots); n != 0 {
		t.Fatalf("%d slots leaked", n)
	}
}
