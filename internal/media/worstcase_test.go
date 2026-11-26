package media

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// Worst-case inputs for the decode memory budget (docs/measurements/
// upload-memory.md), generated here: no downloaded files. Go's encoders
// cannot write progressive, 4:4:4, RGB or CMYK JPEGs, interlaced PNGs or WebP,
// so this file has small writers for those. Decode memory depends on the
// headers (dimensions, components, sampling, progressive, bit depth,
// interlacing), not on the pixel values; the content is only varied enough to
// look like an image and to stay under MaxUploadBytes.

// jpegComp is one frame component: its identifier and sampling factors.
type jpegComp struct{ id, h, v byte }

// jpegSpec describes a generated JPEG.
type jpegSpec struct {
	w, h        int
	comps       []jpegComp
	progressive bool
	jfif        bool
	adobe       int      // Adobe APP14 transform (0 RGB/CMYK, 1 YCbCr, 2 YCCK), or -1 for none
	segments    [][]byte // extra segments (marker byte, then payload) after APP0
}

var (
	ycc420 = []jpegComp{{1, 2, 2}, {2, 1, 1}, {3, 1, 1}}
	ycc444 = []jpegComp{{1, 1, 1}, {2, 1, 1}, {3, 1, 1}}
	rgb444 = []jpegComp{{'R', 1, 1}, {'G', 1, 1}, {'B', 1, 1}}
	cmyk   = []jpegComp{{1, 1, 1}, {2, 1, 1}, {3, 1, 1}, {4, 1, 1}}
	gray   = []jpegComp{{1, 1, 1}}
)

// app1 wraps an APP1 payload (for example orientation()) for jpegSpec.segments.
func app1(payload []byte) []byte { return append([]byte{0xE1}, payload...) }

// coefficient returns block (bx, by) of component c: a DC gradient and one
// pseudo-random low-frequency AC coefficient (zig-zag index and value).
func coefficient(c, bx, by int) (dc int32, k int, ac int32) {
	dc = int32((bx+by+16*c)%64-32) * 16
	x := uint32(bx*73856093) ^ uint32(by*19349663) ^ uint32(c*83492791)
	x ^= x >> 13
	x *= 0x5bd1e995
	x ^= x >> 15
	k = 1 + int(x%5)
	ac = int32(x>>8%31) - 15
	if ac == 0 {
		ac = 1
	}
	return dc, k, ac
}

// jpegBits is an entropy-coded segment writer with 0xFF byte stuffing.
type jpegBits struct {
	out   []byte
	acc   uint32
	nbits uint
}

func (w *jpegBits) put(v uint32, n uint) {
	for i := int(n) - 1; i >= 0; i-- {
		w.acc = w.acc<<1 | (v>>uint(i))&1
		w.nbits++
		if w.nbits == 8 {
			w.out = append(w.out, byte(w.acc))
			if byte(w.acc) == 0xFF {
				w.out = append(w.out, 0)
			}
			w.acc, w.nbits = 0, 0
		}
	}
}

func (w *jpegBits) flush() []byte {
	for w.nbits != 0 {
		w.put(1, 1)
	}
	return w.out
}

// category returns the JPEG magnitude category of v and its extra bits.
func category(v int32) (uint, uint32) {
	a := v
	if a < 0 {
		a = -a
	}
	n := uint(0)
	for a > 0 {
		n++
		a >>= 1
	}
	if v < 0 {
		v += 1<<n - 1
	}
	return n, uint32(v)
}

// Generated Huffman tables: DC categories 0-11 as 4-bit codes, and AC
// symbols EOB, ZRL and run/size (run 0-15, size 1-10) as 8-bit codes.
var acSymbols = func() []byte {
	s := []byte{0x00, 0xF0}
	for run := 0; run < 16; run++ {
		for size := 1; size <= 10; size++ {
			s = append(s, byte(run<<4|size))
		}
	}
	return s
}()

func acCode(sym byte) uint32 { return uint32(bytes.IndexByte(acSymbols, sym)) }

func (w *jpegBits) dc(diff int32) {
	n, extra := category(diff)
	w.put(uint32(n), 4)
	w.put(extra, n)
}

// acBand codes the coefficient band 1-63 of one block with a single nonzero
// coefficient at zig-zag index k.
func (w *jpegBits) acBand(k int, ac int32) {
	run := k - 1
	for run >= 16 {
		w.put(acCode(0xF0), 8)
		run -= 16
	}
	n, extra := category(ac)
	w.put(acCode(byte(run<<4)|byte(n)), 8)
	w.put(extra, n)
	if k < 63 {
		w.put(acCode(0x00), 8) // EOB (in progressive scans: EOB run of 1)
	}
}

func segment(marker byte, payload []byte) []byte {
	s := []byte{0xFF, marker, 0, 0}
	binary.BigEndian.PutUint16(s[2:], uint16(len(payload)+2))
	return append(s, payload...)
}

// encodeSpec writes a baseline (SOF0) or progressive (SOF2, spectral
// selection only) JPEG, in the block order Go's decoder reads.
func encodeSpec(s jpegSpec) []byte {
	out := []byte{0xFF, 0xD8}
	if s.jfif {
		out = append(out, segment(0xE0, []byte("JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00"))...)
	}
	if s.adobe >= 0 {
		out = append(out, segment(0xEE, []byte{'A', 'd', 'o', 'b', 'e', 0, 100, 0, 0, 0, 0, byte(s.adobe)})...)
	}
	for _, seg := range s.segments {
		out = append(out, segment(seg[0], seg[1:])...)
	}
	q := make([]byte, 65)
	for i := 1; i < 65; i++ {
		q[i] = 1
	}
	out = append(out, segment(0xDB, q)...)
	sof := []byte{8, byte(s.h >> 8), byte(s.h), byte(s.w >> 8), byte(s.w), byte(len(s.comps))}
	for _, c := range s.comps {
		sof = append(sof, c.id, c.h<<4|c.v, 0)
	}
	marker := byte(0xC0)
	if s.progressive {
		marker = 0xC2
	}
	out = append(out, segment(marker, sof)...)
	dht := []byte{0x00, 0, 0, 0, 12, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	for i := 0; i < 12; i++ {
		dht = append(dht, byte(i))
	}
	dht = append(dht, 0x10, 0, 0, 0, 0, 0, 0, 0, byte(len(acSymbols)), 0, 0, 0, 0, 0, 0, 0, 0)
	dht = append(dht, acSymbols...)
	out = append(out, segment(0xC4, dht)...)

	h0, v0 := int(s.comps[0].h), int(s.comps[0].v)
	if len(s.comps) == 1 {
		h0, v0 = 1, 1
	}
	mxx, myy := (s.w+8*h0-1)/(8*h0), (s.h+8*v0-1)/(8*v0)
	sampling := func(i int) (int, int) {
		if len(s.comps) == 1 {
			return 1, 1
		}
		return int(s.comps[i].h), int(s.comps[i].v)
	}
	// scan writes one scan over comps (indices into s.comps).
	scan := func(comps []int, ss, se byte, block func(w *jpegBits, c, bx, by int, pred *[4]int32)) {
		sos := []byte{byte(len(comps))}
		for _, c := range comps {
			sos = append(sos, s.comps[c].id, 0x00)
		}
		sos = append(sos, ss, se, 0)
		out = append(out, segment(0xDA, sos)...)
		var w jpegBits
		var pred [4]int32
		if len(comps) > 1 { // interleaved: MCU order
			for my := 0; my < myy; my++ {
				for mx := 0; mx < mxx; mx++ {
					for _, c := range comps {
						hi, vi := sampling(c)
						for j := 0; j < hi*vi; j++ {
							block(&w, c, hi*mx+j%hi, vi*my+j/hi, &pred)
						}
					}
				}
			}
		} else { // one component: raster order, as Go's decoder reads it
			c := comps[0]
			hi, vi := sampling(c)
			for n := 0; n < mxx*myy*hi*vi; n++ {
				bx, by := n%(mxx*hi), n/(mxx*hi)
				if bx*8 >= s.w || by*8 >= s.h {
					continue
				}
				block(&w, c, bx, by, &pred)
			}
		}
		out = append(out, w.flush()...)
	}
	dcBlock := func(w *jpegBits, c, bx, by int, pred *[4]int32) {
		dc, _, _ := coefficient(c, bx, by)
		w.dc(dc - pred[c])
		pred[c] = dc
	}
	acBlock := func(w *jpegBits, c, bx, by int, _ *[4]int32) {
		_, k, ac := coefficient(c, bx, by)
		w.acBand(k, ac)
	}
	all := make([]int, len(s.comps))
	for i := range all {
		all[i] = i
	}
	if !s.progressive {
		scan(all, 0, 63, func(w *jpegBits, c, bx, by int, pred *[4]int32) {
			dcBlock(w, c, bx, by, pred)
			acBlock(w, c, bx, by, pred)
		})
	} else {
		scan(all, 0, 0, dcBlock)
		for c := range s.comps {
			scan([]int{c}, 1, 63, acBlock)
		}
	}
	return append(out, 0xFF, 0xD9)
}

// pngChunkTo appends one PNG chunk with its CRC.
func pngChunkTo(out []byte, typ string, data []byte) []byte {
	return append(out, pngChunk(typ, data)...)
}

// adam7PNG16 writes w x h 16-bit RGBA, Adam7-interlaced (Go's encoder cannot
// interlace), with filter type 0 on every row.
func adam7PNG16(w, h int) []byte {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr, uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(h))
	ihdr[8], ihdr[9], ihdr[12] = 16, 6, 1
	passes := [7][4]int{{0, 0, 8, 8}, {4, 0, 8, 8}, {0, 4, 4, 8}, {2, 0, 4, 4}, {0, 2, 2, 4}, {1, 0, 2, 2}, {0, 1, 1, 2}}
	var raw bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&raw, zlib.BestCompression)
	for _, p := range passes {
		for y := p[1]; y < h; y += p[3] {
			row := []byte{0}
			for x := p[0]; x < w; x += p[2] {
				r, g, b := uint16(x/512*8192), uint16(y/512*8192), uint16(0x4000)
				row = append(row, byte(r>>8), byte(r), byte(g>>8), byte(g), byte(b>>8), byte(b), 0xFF, 0xFF)
			}
			if len(row) > 1 {
				zw.Write(row)
			}
		}
	}
	zw.Close()
	out := []byte("\x89PNG\r\n\x1a\n")
	out = pngChunkTo(out, "IHDR", ihdr)
	out = pngChunkTo(out, "IDAT", raw.Bytes())
	return pngChunkTo(out, "IEND", nil)
}

// png16 is a w x h 16-bit RGBA gradient from Go's encoder (not interlaced).
func png16(t testing.TB, w, h int) []byte {
	m := image.NewNRGBA64(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m.SetNRGBA64(x, y, color.NRGBA64{uint16(x * 16), uint16(y * 16), uint16((x + y) * 8), 0xFFFF})
		}
	}
	var b bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// vp8lBits is the LSB-first bit writer of the WebP lossless format.
type vp8lBits struct {
	out   []byte
	acc   uint64
	nbits uint
}

func (b *vp8lBits) put(v uint32, n uint) {
	b.acc |= uint64(v) << b.nbits
	b.nbits += n
	for b.nbits >= 8 {
		b.out = append(b.out, byte(b.acc))
		b.acc >>= 8
		b.nbits -= 8
	}
}

// simpleCodes writes the five prefix codes of one group as one-symbol
// "simple" codes (zero bits per pixel).
func (b *vp8lBits) simpleCodes(green, red, blue, alpha byte) {
	for _, s := range []byte{green, red, blue, alpha, 0} {
		b.put(1, 1) // simple code
		b.put(0, 1) // one symbol
		b.put(1, 1) // 8-bit symbol
		b.put(uint32(s), 8)
	}
}

// webpLossless is a w x h VP8L image of a single colour. With palette it
// uses a 16-colour colour-indexing transform (pixels packed two per byte),
// the transform for which Go's decoder allocates a second full-size buffer.
func webpLossless(w, h int, palette bool) []byte {
	var b vp8lBits
	b.put(0x2f, 8)
	b.put(uint32(w-1), 14)
	b.put(uint32(h-1), 14)
	b.put(0, 1) // alpha hint
	b.put(0, 3) // version
	if palette {
		b.put(1, 1)  // transform present
		b.put(3, 2)  // colour indexing
		b.put(15, 8) // 16 colours: bits = 1
		b.put(0, 1)  // colour table: no colour cache
		b.simpleCodes(0x20, 0x40, 0x60, 0xFF)
	}
	b.put(0, 1) // no more transforms
	b.put(0, 1) // no colour cache
	b.put(0, 1) // no meta prefix codes
	b.simpleCodes(0x03, 0, 0, 0xFF)
	b.put(0, 7)
	return riff(webpChunk("VP8L", b.out))
}

// Adversarial lossless WebP: the most Huffman groups Go's decoder accepts
// (2600, each referenced by a tile of the meta image so that all are built),
// each with five full trees and an 11-bit colour cache in the green alphabet,
// as small as possible in the file. Pixels are one literal and LZ77 copies.

// code writes a Huffman code MSB first, as the decoder reads it.
func (b *vp8lBits) code(c uint32, n uint) {
	for i := int(n) - 1; i >= 0; i-- {
		b.put(c>>uint(i)&1, 1)
	}
}

// prefixCode is a canonical prefix code: lengths and codes per symbol.
type prefixCode struct{ len, code []uint32 }

// canonical assigns canonical codes to lengths (0 = unused symbol).
func canonical(lengths []uint32) prefixCode {
	codes := make([]uint32, len(lengths))
	next := uint32(0)
	for l := uint32(1); l <= 15; l++ {
		for s, sl := range lengths {
			if sl == l {
				codes[s] = next
				next++
			}
		}
		next <<= 1
	}
	return prefixCode{lengths, codes}
}

func (b *vp8lBits) sym(c prefixCode, s uint32) { b.code(c.code[s], uint(c.len[s])) }

// fullTree writes a complete normal prefix code over all n symbols (Go's
// decoder allocates 2n-1 nodes for it): the first 2^L-n symbols get length
// L-1, the rest length L, with L = ceil(log2 n). The code lengths are coded
// with a code-length code of the two lengths and 16 (repeat the previous
// length 3-6 times). It returns the code for writing symbols.
func (b *vp8lBits) fullTree(n int) prefixCode {
	L := uint32(1)
	for 1<<L < n {
		L++
	}
	short := 1<<L - n
	lengths := make([]uint32, n)
	for i := range lengths {
		lengths[i] = L
		if i < short {
			lengths[i] = L - 1
		}
	}
	clLengths := make([]uint32, 19)
	clLengths[16] = 1
	if short > 0 {
		clLengths[L-1], clLengths[L] = 2, 2
	} else {
		clLengths[L] = 1
	}
	cl := canonical(clLengths)
	order := []int{17, 18, 0, 1, 2, 3, 4, 5, 16, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	last := 0
	for i, sym := range order {
		if clLengths[sym] != 0 {
			last = i
		}
	}
	b.put(0, 1) // normal code
	b.put(uint32(last+1-4), 4)
	for _, sym := range order[:last+1] {
		b.put(clLengths[sym], 3)
	}
	b.put(0, 1) // lengths for the whole alphabet
	run := func(l uint32, count int) {
		if count == 0 {
			return
		}
		b.sym(cl, l)
		r := count - 1
		for r >= 3 {
			k := min(r, 6)
			if rem := r - k; (rem == 1 || rem == 2) && k-(3-rem) >= 3 {
				k -= 3 - rem
			}
			b.sym(cl, 16)
			b.put(uint32(k-3), 2)
			r -= k
		}
		for ; r > 0; r-- {
			b.sym(cl, l)
		}
	}
	run(L-1, short)
	run(L, n-short)
	return canonical(lengths)
}

// oneSymbol writes a simple prefix code with a single symbol (zero bits).
func (b *vp8lBits) oneSymbol(s uint32) {
	b.put(1, 1)
	b.put(0, 1)
	b.put(1, 1)
	b.put(s, 8)
}

// lz77 returns the prefix symbol and extra bits of an LZ77 length or distance.
func lz77(v uint32) (sym uint32, extra uint32, n uint) {
	if v <= 4 {
		return v - 1, 0, 0
	}
	for s := uint32(4); ; s++ {
		eb := (s - 2) >> 1
		off := (2 + s&1) << eb
		if v-1 >= off && v-1 < off+1<<eb {
			return s, v - 1 - off, uint(eb)
		}
	}
}

// manyGroupsVP8L returns the VP8L bitstream after the 5-byte header (as an
// ALPH chunk carries it) for a w x h image; palette adds a 16-colour colour
// indexing transform.
func manyGroupsVP8L(w, h int, palette bool) []byte {
	var b vp8lBits
	if palette {
		b.put(1, 1)
		b.put(3, 2)
		b.put(15, 8)
		b.put(0, 1)
		b.simpleCodes(0x20, 0x40, 0x60, 0xFF)
		w = (w + 1) / 2
	}
	b.put(0, 1) // no more transforms
	b.put(1, 1) // colour cache
	b.put(11, 4)
	// The largest tiles (smallest meta image) that still give every group a
	// tile; the decoder reads as many groups as the largest index plus one.
	tiles := func(bits int) (int, int) { return (w + 1<<bits - 1) >> bits, (h + 1<<bits - 1) >> bits }
	hBits := 2
	for hBits < 9 {
		tw, th := tiles(hBits + 1)
		if tw*th < 2600 {
			break
		}
		hBits++
	}
	tw, th := tiles(hBits)
	groups := min(2600, tw*th)
	b.put(1, 1) // meta prefix codes
	b.put(uint32(hBits-2), 3)
	// Meta image: tile i uses group i % groups; green and red carry the index.
	b.put(0, 1) // no colour cache
	mg := b.fullTree(280)
	mr := b.fullTree(256)
	b.oneSymbol(0)
	b.oneSymbol(0)
	b.oneSymbol(0)
	for i := 0; i < tw*th; i++ {
		g := uint32(i % groups)
		b.sym(mg, g&0xFF)
		b.sym(mr, g>>8)
	}
	var green, red, blue, alpha, dist prefixCode
	for g := 0; g < groups; g++ {
		green = b.fullTree(280 + 2048)
		red = b.fullTree(256)
		blue = b.fullTree(256)
		alpha = b.fullTree(256)
		dist = b.fullTree(40)
	}
	// One literal pixel, then copies of the previous pixel.
	b.sym(green, 3)
	b.sym(red, 0)
	b.sym(blue, 0)
	b.sym(alpha, 255)
	for left := uint32(w*h - 1); left > 0; {
		n := min(left, 4096)
		s, e, eb := lz77(n)
		b.sym(green, 256+s)
		b.put(e, eb)
		ds, de, deb := lz77(121) // distance code 121: the previous pixel
		b.sym(dist, ds)
		b.put(de, deb)
		left -= n
	}
	b.put(0, 7)
	return b.out
}

// webpManyGroups is a lossless WebP with manyGroupsVP8L.
func webpManyGroups(w, h int, palette bool) []byte {
	var hdr vp8lBits
	hdr.put(0x2f, 8)
	hdr.put(uint32(w-1), 14)
	hdr.put(uint32(h-1), 14)
	hdr.put(0, 1)
	hdr.put(0, 3)
	return riff(webpChunk("VP8L", append(hdr.out, manyGroupsVP8L(w, h, palette)...)))
}

// vp8Frame is a lossy key frame of w x h with zero-filled partitions, which
// Go's decoder reads as a flat image.
func vp8Frame(w, h int) []byte {
	const firstPart = 1 << 16
	frame := make([]byte, 10+firstPart+1<<16)
	tag := uint32(1<<4 | firstPart<<5) // key frame, version 0, shown
	frame[0], frame[1], frame[2] = byte(tag), byte(tag>>8), byte(tag>>16)
	copy(frame[3:], []byte{0x9d, 0x01, 0x2a})
	binary.LittleEndian.PutUint16(frame[6:], uint16(w))
	binary.LittleEndian.PutUint16(frame[8:], uint16(h))
	return frame
}

// webpLossy is a flat w x h lossy WebP.
func webpLossy(w, h int) []byte { return riff(webpChunk("VP8 ", vp8Frame(w, h))) }

// webpAlphaManyGroups is a lossy WebP whose alpha channel is the
// adversarial lossless stream.
func webpAlphaManyGroups(w, h int) []byte {
	alph := append([]byte{1}, manyGroupsVP8L(w, h, false)...) // lossless compression, no filter
	return riff(vp8xChunk(0x10, uint32(w), uint32(h)), webpChunk("ALPH", alph), webpChunk("VP8 ", vp8Frame(w, h)))
}
