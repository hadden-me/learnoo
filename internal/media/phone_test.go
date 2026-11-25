package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"strings"
	"sync"
	"testing"
)

// Phone-photo fixtures are synthesized here: JPEGs from Go's encoder plus
// hand-built EXIF, MPF and XMP segments, gain-map JPEGs and minimal ISO BMFF
// videos. No real photos or downloaded files are used.

// encodeJPEG encodes img at quality 100.
func encodeJPEG(t testing.TB, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// gainMap is a small grayscale JPEG standing in for an Ultra HDR gain map or
// MPF preview.
func gainMap(t testing.TB) []byte {
	g := image.NewGray(image.Rect(0, 0, 8, 8))
	for i := range g.Pix {
		g.Pix[i] = uint8(i * 4)
	}
	return encodeJPEG(t, g)
}

// mp4 is a minimal ISO BMFF file: an ftyp box and an mdat box.
func mp4() []byte {
	b := []byte("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00isommp42")
	return append(b, []byte("\x00\x00\x00\x10mdat\xAB\xCD\xEF\x01\x23\x45\x67\x89")...)
}

type mpEntry struct{ size, offset uint32 }

// mpfSegment builds an APP2 MPF payload ("MPF\0" + TIFF) listing entries.
func mpfSegment(order binary.ByteOrder, entries []mpEntry) []byte {
	n := len(entries)
	t := make([]byte, 8+2+3*12+4+16*n)
	if order == binary.BigEndian {
		copy(t, "MM\x00*")
	} else {
		copy(t, "II*\x00")
	}
	order.PutUint32(t[4:], 8)
	order.PutUint16(t[8:], 3)
	entry := func(i int, tag, typ uint16, count uint32, value []byte) {
		e := t[10+12*i:]
		order.PutUint16(e, tag)
		order.PutUint16(e[2:], typ)
		order.PutUint32(e[4:], count)
		copy(e[8:12], value)
	}
	u32 := func(v uint32) []byte { b := make([]byte, 4); order.PutUint32(b, v); return b }
	entriesAt := uint32(8 + 2 + 3*12 + 4)
	entry(0, 0xB000, 7, 4, []byte("0100"))
	entry(1, 0xB001, 4, 1, u32(uint32(n)))
	entry(2, 0xB002, 7, uint32(16*n), u32(entriesAt))
	for i, e := range entries {
		d := t[int(entriesAt)+16*i:]
		if i == 0 {
			order.PutUint32(d, 0x20030000) // primary, baseline
		}
		order.PutUint32(d[4:], e.size)
		order.PutUint32(d[8:], e.offset)
	}
	return append([]byte("MPF\x00"), t...)
}

// mpfBase is where the TIFF header of an MPF segment inserted right after
// SOI starts: SOI, marker, length, "MPF\0".
const mpfBase = 2 + 4 + 4

// withMPF inserts an MPF index describing secondaries appended right after
// the primary's EOI, then appends them. edit may change the entries.
func withMPF(primary []byte, order binary.ByteOrder, secondaries [][]byte, edit func([]mpEntry)) []byte {
	entries := make([]mpEntry, 1+len(secondaries))
	segLen := len(mpfSegment(order, entries))
	eoi := len(primary) + 4 + segLen
	entries[0] = mpEntry{uint32(eoi), 0}
	pos := eoi
	for i, s := range secondaries {
		entries[i+1] = mpEntry{uint32(len(s)), uint32(pos - mpfBase)}
		pos += len(s)
	}
	if edit != nil {
		edit(entries)
	}
	out := insertJPEGSegment(primary, 0xE2, mpfSegment(order, entries))
	for _, s := range secondaries {
		out = append(out, s...)
	}
	return out
}

// xmp builds a standard XMP APP1 payload. attrs go on rdf:Description; body
// inside it.
func xmp(attrs, body string) []byte {
	return []byte("http://ns.adobe.com/xap/1.0/\x00" +
		`<?xpacket begin="` + "\uFEFF" + `" id="W5M0MpCehiHzreSzNTczkc9d"?>` +
		`<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">` +
		`<rdf:Description rdf:about="" xmlns:Camera="http://ns.google.com/photos/1.0/camera/"` +
		` xmlns:GCamera="http://ns.google.com/photos/1.0/camera/"` +
		` xmlns:Container="http://ns.google.com/photos/1.0/container/"` +
		` xmlns:Item="http://ns.google.com/photos/1.0/container/item/"` +
		` xmlns:hdrgm="http://ns.adobe.com/hdr-gain-map/1.0/" ` + attrs + `>` + body +
		`</rdf:Description></rdf:RDF></x:xmpmeta><?xpacket end="w"?>`)
}

// directory builds a Container:Directory; each item is its attribute list.
func directory(items ...string) string {
	var b strings.Builder
	b.WriteString(`<Container:Directory><rdf:Seq>`)
	for _, it := range items {
		b.WriteString(`<rdf:li rdf:parseType="Resource"><Container:Item ` + it + `/></rdf:li>`)
	}
	b.WriteString(`</rdf:Seq></Container:Directory>`)
	return b.String()
}

const primaryItem = `Item:Semantic="Primary" Item:Mime="image/jpeg"`

func gainMapItem(n int) string {
	return fmt.Sprintf(`Item:Semantic="GainMap" Item:Mime="image/jpeg" Item:Length="%d"`, n)
}

func videoItem(n int) string {
	return fmt.Sprintf(`Item:Semantic="MotionPhoto" Item:Mime="video/mp4" Item:Length="%d"`, n)
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// photo is the primary image used by the phone fixtures.
func photo(t testing.TB) []byte { return encodeJPEG(t, testImage(24, 16)) }

func TestProcessPhoneJPEG(t *testing.T) {
	prim := photo(t)
	gm := gainMap(t)
	vid := mp4()
	// A structurally valid JPEG whose scan data cannot be decoded: accepted as
	// a secondary, which proves secondaries are never decoded.
	undecodable := append([]byte{}, gm...)
	sos := bytes.Index(undecodable, []byte{0xFF, 0xDA})
	scanStart := sos + 2 + int(binary.BigEndian.Uint16(undecodable[sos+2:]))
	for i := scanStart; i < len(undecodable)-2; i++ {
		undecodable[i] = 0x00
	}
	if _, err := jpeg.Decode(bytes.NewReader(undecodable)); err == nil {
		t.Fatal("fixture decodes")
	}

	ultraHDR := withMPF(insertJPEGSegment(prim, 0xE1, xmp(`hdrgm:Version="1.0"`, directory(primaryItem, gainMapItem(len(gm))))),
		binary.BigEndian, [][]byte{gm}, nil)
	motionContainer := cat(insertJPEGSegment(prim, 0xE1, xmp(`Camera:MotionPhoto="1" Camera:MotionPhotoVersion="1"`,
		directory(primaryItem, videoItem(len(vid))))), vid)
	microVideo := cat(insertJPEGSegment(prim, 0xE1, xmp(fmt.Sprintf(`GCamera:MicroVideo="1" GCamera:MicroVideoVersion="1" GCamera:MicroVideoOffset="%d"`, len(vid)), "")), vid)
	// Ultra HDR motion photo: gain map (MPF and Container) then the video,
	// also located by MicroVideoOffset.
	both := withMPF(insertJPEGSegment(prim, 0xE1, xmp(
		fmt.Sprintf(`Camera:MotionPhoto="1" hdrgm:Version="1.0" GCamera:MicroVideo="1" GCamera:MicroVideoOffset="%d"`, len(vid)),
		directory(primaryItem, gainMapItem(len(gm)), videoItem(len(vid))))), binary.BigEndian, [][]byte{gm}, nil)
	both = append(both, vid...)
	paddedVideo := cat(insertJPEGSegment(prim, 0xE1, xmp(`Camera:MotionPhoto="1"`,
		directory(primaryItem+` Item:Padding="7"`, videoItem(len(vid))))), make([]byte, 7), vid)

	// A final mdat whose size is 0 ("to the end"), as streaming writers produce.
	openMdat := cat(vid[:24], []byte("\x00\x00\x00\x00mdat\x01\x02\x03"))
	tests := map[string][]byte{
		"motion photo final mdat to the end": cat(insertJPEGSegment(prim, 0xE1, xmp(fmt.Sprintf(`GCamera:MicroVideo="1" GCamera:MicroVideoOffset="%d"`, len(openMdat)), "")), openMdat),
		"mpf preview big-endian":             withMPF(prim, binary.BigEndian, [][]byte{gm}, nil),
		"mpf two images little-endian":       withMPF(prim, binary.LittleEndian, [][]byte{gm, encodeJPEG(t, testImage(4, 4))}, nil),
		"mpf undecodable secondary":          withMPF(prim, binary.BigEndian, [][]byte{undecodable}, nil),
		"mpf then zero padding":              append(withMPF(prim, binary.BigEndian, [][]byte{gm}, nil), make([]byte, 100)...),
		"ultra hdr gain map":                 ultraHDR,
		"motion photo container":             motionContainer,
		"motion photo micro video":           microVideo,
		"ultra hdr motion photo":             both,
		"container padding before video":     paddedVideo,
		"one zero byte":                      append(append([]byte{}, prim...), 0),
		"zero padding at the limit":          append(append([]byte{}, prim...), make([]byte, MaxTrailingZeros)...),
		"xmp without trailer":                insertJPEGSegment(prim, 0xE1, xmp(`Camera:MotionPhoto="1"`, directory(primaryItem, videoItem(9)))),
	}
	want, err := process(prim, "")
	if err != nil {
		t.Fatal(err)
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			a, err := process(in, "image/jpeg")
			if err != nil {
				t.Fatalf("Process: %v", err)
			}
			// Same pixels as the bare primary: nothing from the trailer is kept.
			if !bytes.Equal(a.Data(), want.Data()) || a.Width() != 24 || a.Height() != 16 {
				t.Fatalf("stored %dx%d, %d bytes; want the primary image only", a.Width(), a.Height(), a.ByteSize())
			}
			for _, s := range []string{"MPF", "ftyp", "mdat", "Container", "GCamera", "hdrgm", "xpacket"} {
				if bytes.Contains(a.Data(), []byte(s)) {
					t.Errorf("stored bytes contain %q", s)
				}
			}
			if bytes.Count(a.Data(), []byte{0xFF, 0xD8}) != 1 {
				t.Error("stored bytes contain a second image")
			}
		})
	}
}

func TestProcessRejectsPhoneJPEGVariants(t *testing.T) {
	prim := photo(t)
	gm := gainMap(t)
	vid := mp4()
	png := pngBytes(t, 2, 2)
	mpf := func(edit func([]mpEntry), secondaries ...[]byte) []byte {
		return withMPF(prim, binary.BigEndian, secondaries, edit)
	}
	withXMP := func(attrs, body string, trailer ...[]byte) []byte {
		return cat(append([][]byte{insertJPEGSegment(prim, 0xE1, xmp(attrs, body))}, trailer...)...)
	}
	microVideo := func(offset int, trailer ...[]byte) []byte {
		return withXMP(fmt.Sprintf(`GCamera:MicroVideo="1" GCamera:MicroVideoOffset="%d"`, offset), "", trailer...)
	}
	motion := func(items ...string) string { return directory(append([]string{primaryItem}, items...)...) }
	noFtyp := bytes.Replace(vid, []byte("ftyp"), []byte("free"), 1)
	scriptInVideo := cat(vid, []byte(script))
	sosOnly := cat([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x04, 'h', 'i'}, []byte{0xFF, 0xD9})
	nonZeroPad := make([]byte, 64)
	nonZeroPad[40] = 1
	badVersion := mpf(nil, gm)
	badVersion[bytes.Index(badVersion, []byte("0100"))+3] = '1'
	tooMany := make([][]byte, maxTrailerParts)
	for i := range tooMany {
		tooMany[i] = gm
	}
	twoMPF := insertJPEGSegment(mpf(nil, gm), 0xE2, mpfSegment(binary.BigEndian, []mpEntry{{0, 0}}))
	twoXMP := insertJPEGSegment(withXMP(`Camera:MotionPhoto="1"`, motion(videoItem(len(vid))), vid), 0xE1, xmp("", ""))

	ftypSizeZero := append([]byte{}, vid...)
	copy(ftypSizeZero, []byte{0, 0, 0, 0})
	freeSizeZero := cat(vid[:24], []byte("\x00\x00\x00\x00free\x01\x02\x03"))
	tests := map[string][]byte{
		// Nothing may follow a video.
		"container video then padding":  withXMP(`Camera:MotionPhoto="1"`, motion(videoItem(len(vid))+` Item:Padding="4"`), vid, make([]byte, 4)),
		"container video then gain map": withXMP(`Camera:MotionPhoto="1"`, motion(videoItem(len(vid)), gainMapItem(len(gm))), vid, gm),
		"micro video ftyp size zero":    microVideo(len(ftypSizeZero), ftypSizeZero),
		"micro video free size zero":    microVideo(len(freeSizeZero), freeSizeZero),
		// MPF
		"mpf size one short":           mpf(func(e []mpEntry) { e[1].size-- }, gm),
		"mpf size one long":            cat(mpf(func(e []mpEntry) { e[1].size++ }, gm), []byte{0}),
		"mpf offset one early":         mpf(func(e []mpEntry) { e[1].offset-- }, gm),
		"mpf offset one late":          cat(mpf(func(e []mpEntry) { e[1].offset++ }, gm), []byte{0}),
		"mpf entry inside primary":     mpf(func(e []mpEntry) { e[1].offset = 2 }, gm),
		"mpf entry beyond file":        mpf(func(e []mpEntry) { e[1].size += 1000 }, gm),
		"mpf zero size entry":          mpf(func(e []mpEntry) { e[1].size = 0 }, gm),
		"mpf overlapping entries":      mpf(func(e []mpEntry) { e[2] = e[1] }, gm, gm),
		"mpf entries overlap partly":   mpf(func(e []mpEntry) { e[2].offset -= 2 }, gm, gm),
		"mpf primary with offset":      mpf(func(e []mpEntry) { e[0].offset = 4 }, gm),
		"mpf secondary not a jpeg":     mpf(nil, png),
		"mpf secondary without scan":   mpf(nil, sosOnly),
		"mpf too many images":          mpf(nil, tooMany...),
		"mpf bad version":              badVersion,
		"mpf twice":                    twoMPF,
		"mpf then png":                 cat(mpf(nil, gm), png),
		"mpf then script":              cat(mpf(nil, gm), []byte(script)),
		"mpf then second eoi":          cat(mpf(nil, gm), []byte{0xFF, 0xD9}),
		"trailer without any index":    cat(prim, gm),
		"non-zero padding":             cat(prim, nonZeroPad),
		"padding over the limit":       cat(prim, make([]byte, MaxTrailingZeros+1)),
		"zero padding after the video": cat(withXMP(`Camera:MotionPhoto="1"`, motion(videoItem(len(vid))), vid), []byte{0}),
		// Container directory
		"container length one short":     withXMP(`Camera:MotionPhoto="1"`, motion(videoItem(len(vid)-1)), vid),
		"container length one long":      withXMP(`Camera:MotionPhoto="1"`, motion(videoItem(len(vid)+1)), vid, []byte{0}),
		"container extra byte":           withXMP(`Camera:MotionPhoto="1"`, motion(videoItem(len(vid))), vid, []byte{1}),
		"container video without ftyp":   withXMP(`Camera:MotionPhoto="1"`, motion(videoItem(len(noFtyp))), noFtyp),
		"container video hides script":   withXMP(`Camera:MotionPhoto="1"`, motion(videoItem(len(scriptInVideo))), scriptInVideo),
		"container gain map one short":   withXMP("", motion(gainMapItem(len(gm)-1)), gm),
		"container without primary":      withXMP("", directory(gainMapItem(len(gm))), gm),
		"container two primaries":        withXMP("", motion(primaryItem, gainMapItem(len(gm))), gm),
		"container png item":             withXMP("", motion(fmt.Sprintf(`Item:Semantic="Depth" Item:Mime="image/png" Item:Length="%d"`, len(png))), png),
		"container unknown video item":   withXMP("", motion(fmt.Sprintf(`Item:Semantic="Other" Item:Mime="video/mp4" Item:Length="%d"`, len(vid))), vid),
		"container missing length":       withXMP("", motion(`Item:Semantic="MotionPhoto" Item:Mime="video/mp4"`), vid),
		"container negative length":      withXMP("", motion(`Item:Semantic="MotionPhoto" Item:Mime="video/mp4" Item:Length="-5"`), vid),
		"container non-zero padding":     withXMP("", directory(primaryItem+` Item:Padding="4"`, videoItem(len(vid))), []byte{0, 0, 1, 0}, vid),
		"two container directories":      withXMP("", motion(videoItem(len(vid)))+motion(), vid),
		"container item outside":         withXMP("", `<Container:Item `+videoItem(len(vid))+`/>`, vid),
		"container video then zip":       withXMP("", motion(videoItem(len(vid))), vid, []byte("PK\x03\x04zip")),
		"container declares video, zip":  withXMP("", motion(videoItem(9)), []byte("PK\x03\x04zip")),
		"two xmp packets":                twoXMP,
		"malformed xmp":                  cat(insertJPEGSegment(prim, 0xE1, []byte("http://ns.adobe.com/xap/1.0/\x00<x:xmpmeta")), vid),
		"gain map and container overlap": withMPF(insertJPEGSegment(prim, 0xE1, xmp("", motion(videoItem(len(gm))))), binary.BigEndian, [][]byte{gm}, nil),
		// MicroVideoOffset
		"micro video one short":        microVideo(len(vid)-1, vid),
		"micro video one long":         microVideo(len(vid)+1, vid),
		"micro video extra byte":       microVideo(len(vid), vid, []byte{1}),
		"micro video without ftyp":     microVideo(len(noFtyp), noFtyp),
		"micro video into primary":     microVideo(len(vid)+100, vid),
		"micro video then garbage":     microVideo(len(vid), []byte(script), vid),
		"micro video flag off":         withXMP(fmt.Sprintf(`GCamera:MicroVideo="0" GCamera:MicroVideoOffset="%d"`, len(vid)), "", vid),
		"micro video and container":    withXMP(fmt.Sprintf(`GCamera:MicroVideo="1" GCamera:MicroVideoOffset="%d"`, len(vid)), motion(videoItem(len(vid)-8)), vid[:8], vid),
		"micro video offset not a num": withXMP(`GCamera:MicroVideo="1" GCamera:MicroVideoOffset="0x20"`, "", vid),
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			a, err := process(in, "")
			if !errors.Is(err, ErrTrailingData) {
				t.Fatalf("got asset=%v err=%v, want ErrTrailingData", a != nil, err)
			}
		})
	}
}

// TestProcessRejectsRandomTrailers appends random bytes (never all zero) to
// accepted phone JPEGs; every result must be rejected.
func TestProcessRejectsRandomTrailers(t *testing.T) {
	prim := photo(t)
	gm := gainMap(t)
	vid := mp4()
	bases := map[string][]byte{
		"plain":       prim,
		"mpf":         withMPF(prim, binary.BigEndian, [][]byte{gm}, nil),
		"motion":      cat(insertJPEGSegment(prim, 0xE1, xmp(`Camera:MotionPhoto="1"`, directory(primaryItem, videoItem(len(vid))))), vid),
		"micro video": cat(insertJPEGSegment(prim, 0xE1, xmp(fmt.Sprintf(`GCamera:MicroVideo="1" GCamera:MicroVideoOffset="%d"`, len(vid)), "")), vid),
	}
	rng := rand.New(rand.NewSource(42))
	for name, base := range bases {
		if _, err := process(base, ""); err != nil {
			t.Fatalf("%s base rejected: %v", name, err)
		}
		for i := 0; i < 300; i++ {
			junk := make([]byte, 1+rng.Intn(600))
			rng.Read(junk)
			if i%3 == 0 { // mostly zeros, one stray byte
				clear(junk)
			}
			junk[rng.Intn(len(junk))] |= 1 + byte(rng.Intn(255))
			if i%5 == 0 { // a JPEG-looking start
				copy(junk, []byte{0xFF, 0xD8, 0xFF})
			}
			if _, err := checkJPEGTrailerOf(cat(base, junk)); !errors.Is(err, ErrTrailingData) {
				t.Fatalf("%s + %x: got %v", name, junk, err)
			}
		}
	}
}

// checkJPEGTrailerOf runs the structural check alone (fast enough for many
// random inputs); TestProcessPhoneJPEG covers the full Process path.
func checkJPEGTrailerOf(b []byte) (inspection, error) { return inspectJPEG(b) }

// exifSegment builds an APP1 Exif payload whose IFD0 holds the given
// orientation entries (tag, type, count, value).
func exifSegment(order binary.ByteOrder, entries ...[4]uint32) []byte {
	t := make([]byte, 8+2+12*len(entries)+4)
	if order == binary.BigEndian {
		copy(t, "MM\x00*")
	} else {
		copy(t, "II*\x00")
	}
	order.PutUint32(t[4:], 8)
	order.PutUint16(t[8:], uint16(len(entries)))
	for i, e := range entries {
		d := t[10+12*i:]
		order.PutUint16(d, uint16(e[0]))
		order.PutUint16(d[2:], uint16(e[1]))
		order.PutUint32(d[4:], e[2])
		if e[1] == 3 {
			order.PutUint16(d[8:], uint16(e[3]))
		} else {
			order.PutUint32(d[8:], e[3])
		}
	}
	return append([]byte("Exif\x00\x00"), t...)
}

func orientation(order binary.ByteOrder, o uint32) []byte {
	return exifSegment(order, [4]uint32{0x010F, 2, 4, 0}, [4]uint32{0x0112, 3, 1, o})
}

// blocks is an asymmetric 3x2 grid of 16 px colour blocks (48x32), so every
// one of the 8 orientations gives a different picture.
var blockColors = [2][3]color.RGBA{
	{{255, 0, 0, 255}, {0, 200, 0, 255}, {0, 0, 255, 255}},
	{{255, 255, 255, 255}, {0, 0, 0, 255}, {255, 255, 0, 255}},
}

func blocks() *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, 48, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 48; x++ {
			m.SetRGBA(x, y, blockColors[y/16][x/16])
		}
	}
	return m
}

// Reference transforms, written independently of orient.
func flipH(m *image.RGBA) *image.RGBA {
	b := m.Bounds()
	out := image.NewRGBA(b)
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			out.SetRGBA(b.Dx()-1-x, y, m.RGBAAt(x, y))
		}
	}
	return out
}

func flipV(m *image.RGBA) *image.RGBA {
	b := m.Bounds()
	out := image.NewRGBA(b)
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			out.SetRGBA(x, b.Dy()-1-y, m.RGBAAt(x, y))
		}
	}
	return out
}

func transpose(m *image.RGBA) *image.RGBA {
	b := m.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dy(), b.Dx()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			out.SetRGBA(y, x, m.RGBAAt(x, y))
		}
	}
	return out
}

// upright is what a viewer shows for stored pixels m with orientation o.
func upright(m *image.RGBA, o int) *image.RGBA {
	switch o {
	case 2:
		return flipH(m)
	case 3:
		return flipH(flipV(m))
	case 4:
		return flipV(m)
	case 5:
		return transpose(m)
	case 6:
		return flipH(transpose(m))
	case 7:
		return flipH(flipV(transpose(m)))
	case 8:
		return flipV(transpose(m))
	}
	return m
}

func near(a, b color.Color) bool {
	r1, g1, b1, _ := a.RGBA()
	r2, g2, b2, _ := b.RGBA()
	d := func(x, y uint32) bool { return x>>8 > y>>8+40 || y>>8 > x>>8+40 }
	return !d(r1, r2) && !d(g1, g2) && !d(b1, b2)
}

// checkUpright compares the stored image's block centres with want.
func checkUpright(t *testing.T, a *Asset, want *image.RGBA) {
	t.Helper()
	got, err := jpeg.Decode(bytes.NewReader(a.Data()))
	if err != nil {
		t.Fatal(err)
	}
	if got.Bounds().Size() != want.Bounds().Size() || a.Width() != want.Bounds().Dx() || a.Height() != want.Bounds().Dy() {
		t.Fatalf("stored %v (asset %dx%d), want %v", got.Bounds().Size(), a.Width(), a.Height(), want.Bounds().Size())
	}
	for y := 8; y < want.Bounds().Dy(); y += 16 {
		for x := 8; x < want.Bounds().Dx(); x += 16 {
			if !near(got.At(x, y), want.At(x, y)) {
				t.Fatalf("pixel (%d,%d) = %v, want %v", x, y, got.At(x, y), want.At(x, y))
			}
		}
	}
}

func TestProcessAppliesEXIFOrientation(t *testing.T) {
	stored := blocks()
	base := encodeJPEG(t, stored)
	for o := 1; o <= 8; o++ {
		for _, order := range []binary.ByteOrder{binary.BigEndian, binary.LittleEndian} {
			t.Run(fmt.Sprintf("%d %v", o, order), func(t *testing.T) {
				in := insertJPEGSegment(base, 0xE1, orientation(order, uint32(o)))
				a, err := process(in, "")
				if err != nil {
					t.Fatal(err)
				}
				checkUpright(t, a, upright(stored, o))
				if bytes.Contains(a.Data(), []byte("Exif")) {
					t.Fatal("stored bytes keep EXIF")
				}
			})
		}
	}
	// With a phone trailer too.
	gm := gainMap(t)
	in := withMPF(insertJPEGSegment(base, 0xE1, orientation(binary.BigEndian, 6)), binary.BigEndian, [][]byte{gm}, nil)
	a, err := process(in, "")
	if err != nil {
		t.Fatal(err)
	}
	checkUpright(t, a, upright(stored, 6))
}

func TestProcessOrientsGrayscaleAndProgressive(t *testing.T) {
	g := image.NewGray(image.Rect(0, 0, 32, 16))
	for x := 0; x < 16; x++ {
		for y := 0; y < 16; y++ {
			g.SetGray(x, y, color.Gray{255})
		}
	}
	a, err := process(insertJPEGSegment(encodeJPEG(t, g), 0xE1, orientation(binary.BigEndian, 6)), "")
	if err != nil {
		t.Fatal(err)
	}
	// Rotated clockwise: the white left half becomes the top half.
	got, _ := jpeg.Decode(bytes.NewReader(a.Data()))
	if a.Width() != 16 || a.Height() != 32 || !near(got.At(8, 8), color.White) || !near(got.At(8, 24), color.Black) {
		t.Fatalf("got %dx%d, top %v, bottom %v", a.Width(), a.Height(), got.At(8, 8), got.At(8, 24))
	}
	p, err := process(insertJPEGSegment(mustHex(t, jpegProgressiveHex), 0xE1, orientation(binary.LittleEndian, 8)), "")
	if err != nil || p.Width() != 16 || p.Height() != 16 {
		t.Fatalf("progressive: %v", err)
	}
}

func TestProcessIgnoresMalformedEXIF(t *testing.T) {
	base := encodeJPEG(t, blocks())
	be := binary.BigEndian
	truncated := orientation(be, 6)
	truncated = truncated[:len(truncated)-10]
	hugeCount := orientation(be, 6)
	binary.BigEndian.PutUint16(hugeCount[6+8:], 0xFFFF)
	badOffset := orientation(be, 6)
	binary.BigEndian.PutUint32(badOffset[6+4:], 0xFFFFFFF0)
	badMagic := orientation(be, 6)
	badMagic[6+2] = 'X'
	for name, seg := range map[string][][]byte{
		"value 0":          {orientation(be, 0)},
		"value 9":          {orientation(be, 9)},
		"value 0xFFFF":     {orientation(be, 0xFFFF)},
		"long type":        {exifSegment(be, [4]uint32{0x0112, 4, 1, 6})},
		"count 2":          {exifSegment(be, [4]uint32{0x0112, 3, 2, 6})},
		"duplicate tag":    {exifSegment(be, [4]uint32{0x0112, 3, 1, 6}, [4]uint32{0x0112, 3, 1, 8})},
		"two exif":         {orientation(be, 6), orientation(be, 8)},
		"truncated ifd":    {truncated},
		"huge entry count": {hugeCount},
		"ifd offset out":   {badOffset},
		"bad tiff magic":   {badMagic},
		"header only":      {[]byte("Exif\x00\x00MM")},
	} {
		t.Run(name, func(t *testing.T) {
			in := base
			for _, s := range seg {
				in = insertJPEGSegment(in, 0xE1, s)
			}
			a, err := process(in, "")
			if err != nil {
				t.Fatal(err)
			}
			checkUpright(t, a, blocks())
		})
	}
}

func TestProcessDimensionsAfterRotation(t *testing.T) {
	a, err := process(insertJPEGSegment(jpegBytes(t, MaxSide, 2), 0xE1, orientation(binary.BigEndian, 6)), "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Width() != 2 || a.Height() != MaxSide {
		t.Fatalf("got %dx%d", a.Width(), a.Height())
	}
	for _, o := range []uint32{5, 6, 7, 8} {
		for _, d := range [][2]uint16{{MaxSide + 1, 8}, {8, MaxSide + 1}, {4000, 4001}} {
			in := insertJPEGSegment(jpegWithDims(t, d[0], d[1]), 0xE1, orientation(binary.BigEndian, o))
			if _, err := process(in, ""); !errors.Is(err, ErrDimensions) {
				t.Errorf("orientation %d %v: got %v", o, d, err)
			}
		}
	}
	if w, h := orientedSize(3, 5, 6); w != 5 || h != 3 {
		t.Fatalf("orientedSize: %dx%d", w, h)
	}
}

// heif builds the start of an ISO BMFF file with the given brands.
func heif(major string, compatible ...string) []byte {
	b := []byte("\x00\x00\x00\x00ftyp" + major + "\x00\x00\x00\x00" + strings.Join(compatible, ""))
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	return append(b, []byte("\x00\x00\x00\x0cmeta\x00\x00\x00\x00")...)
}

func TestProcessRejectsHEIC(t *testing.T) {
	for name, c := range map[string]struct {
		data     []byte
		declared string
		heic     bool
	}{
		"heic":               {heif("heic", "mif1", "heic"), "", true},
		"heic declared":      {heif("heic", "mif1", "heic"), "image/heic", true},
		"heic declared jpeg": {heif("heic", "mif1", "heic"), "image/jpeg", true},
		"heif mif1":          {heif("mif1", "mif1", "heic"), "", true},
		"heix":               {heif("heix"), "", true},
		"avif":               {heif("avif", "mif1", "avif"), "", false},
		"mp4":                {mp4(), "", false},
		"ftyp box truncated": {[]byte("\x00\x00\x00\x18ftyphe"), "", false},
		"huge ftyp size":     {append([]byte("\xFF\xFF\xFF\xFFftypheic\x00\x00\x00\x00"), make([]byte, 8)...), "", true},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := process(c.data, c.declared)
			if !errors.Is(err, ErrUnsupportedType) || errors.Is(err, ErrUnsupportedHEIC) != c.heic {
				t.Fatalf("got %v, heic %v", err, c.heic)
			}
		})
	}
}

// TestProcessPhoneJPEGConcurrent runs phone fixtures through Process from
// several goroutines (meaningful under -race).
func TestProcessPhoneJPEGConcurrent(t *testing.T) {
	prim := photo(t)
	gm := gainMap(t)
	vid := mp4()
	inputs := [][]byte{
		withMPF(insertJPEGSegment(prim, 0xE1, orientation(binary.BigEndian, 6)), binary.BigEndian, [][]byte{gm}, nil),
		cat(insertJPEGSegment(prim, 0xE1, xmp(`Camera:MotionPhoto="1"`, directory(primaryItem, videoItem(len(vid))))), vid),
		insertJPEGSegment(encodeJPEG(t, blocks()), 0xE1, orientation(binary.LittleEndian, 3)),
	}
	want := make([][]byte, len(inputs))
	for i, in := range inputs {
		a, err := process(in, "")
		if err != nil {
			t.Fatal(err)
		}
		want[i] = a.Data()
	}
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for g := 0; g < 24; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			i := g % len(inputs)
			a, err := Process(context.Background(), bytes.NewReader(inputs[i]), "image/jpeg")
			if err == nil && !bytes.Equal(a.Data(), want[i]) {
				err = fmt.Errorf("input %d: different output", i)
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := len(decodeSlots); n != 0 {
		t.Fatalf("%d slots leaked", n)
	}
}

// FuzzJPEGTrailer appends fuzzed bytes after a valid Ultra HDR-style JPEG.
// The only acceptable suffix is zero padding; nothing may panic.
func FuzzJPEGTrailer(f *testing.F) {
	base := withMPF(photo(f), binary.BigEndian, [][]byte{gainMap(f)}, nil)
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0})
	f.Add([]byte(script))
	f.Add(mp4())
	f.Add(gainMap(f))
	f.Fuzz(func(t *testing.T, suffix []byte) {
		in := cat(base, suffix)
		_, err := inspectJPEG(in)
		if err == nil && (!allZero(suffix) || len(suffix) > MaxTrailingZeros) {
			t.Fatalf("accepted suffix %x", suffix)
		}
		if err != nil && !errors.Is(err, ErrTrailingData) {
			t.Fatalf("unexpected error class: %v", err)
		}
	})
}

// FuzzInspectJPEG feeds arbitrary bytes to the structural JPEG check; it must
// never panic, and an accepted image must end within the input.
func FuzzInspectJPEG(f *testing.F) {
	prim := photo(f)
	vid := mp4()
	f.Add(withMPF(prim, binary.LittleEndian, [][]byte{gainMap(f)}, nil))
	f.Add(cat(insertJPEGSegment(prim, 0xE1, xmp(fmt.Sprintf(`GCamera:MicroVideo="1" GCamera:MicroVideoOffset="%d"`, len(vid)), "")), vid))
	f.Add(insertJPEGSegment(prim, 0xE1, orientation(binary.BigEndian, 6)))
	f.Fuzz(func(t *testing.T, b []byte) {
		ins, err := inspectJPEG(b)
		if err == nil && (ins.end < 4 || ins.end > len(b) || ins.orientation < 1 || ins.orientation > 8) {
			t.Fatalf("inconsistent inspection %+v for %d bytes", ins, len(b))
		}
	})
}

// The CMYK fast path (Adobe CMYK JPEGs; Go cannot encode them, so orient is
// tested directly) matches the reference transforms and allocates only the
// destination.
func TestOrientCMYK(t *testing.T) {
	src := image.NewCMYK(image.Rect(0, 0, 5, 3))
	rng := rand.New(rand.NewSource(7))
	rng.Read(src.Pix)
	ref := image.NewRGBA(src.Bounds())
	for y := 0; y < 3; y++ {
		for x := 0; x < 5; x++ {
			c := src.CMYKAt(x, y)
			r, g, b := color.CMYKToRGB(c.C, c.M, c.Y, c.K)
			ref.SetRGBA(x, y, color.RGBA{r, g, b, 255})
		}
	}
	for o := 1; o <= 8; o++ {
		got := orient(src, o)
		if o == 1 {
			if got != image.Image(src) {
				t.Fatal("orientation 1 copied the image")
			}
			continue
		}
		want := upright(ref, o)
		rgba, ok := got.(*image.RGBA)
		if !ok || !bytes.Equal(rgba.Pix, want.Pix) || rgba.Bounds() != want.Bounds() {
			t.Fatalf("orientation %d: got %T %v", o, got, got.Bounds())
		}
	}
	if n := testing.AllocsPerRun(5, func() { orient(src, 6) }); n > 2 {
		t.Fatalf("orient allocates %v times", n)
	}
}
