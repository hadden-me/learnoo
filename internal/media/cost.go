package media

import (
	"encoding/binary"
	"fmt"
)

// Decode memory budget (docs/measurements/upload-memory.md). Before the full
// decode, Process estimates from the headers how many bytes the decoder and
// the orientation step will allocate for the large buffers (pixels, the
// progressive JPEG coefficients, colour conversion, the rotated copy) and
// rejects the image with ErrTooComplex above MaxDecodeBytes. The estimate is
// the sum of these allocations, not their peak, so it stays an upper bound
// even when the garbage collector has not yet freed the earlier buffers.
// Small per-row buffers and short-lived garbage are not counted.

// jpegFrame is what the estimate needs from a JPEG's primary image, read with
// the same rules as Go's decoder (image/jpeg).
type jpegFrame struct {
	progressive bool
	comps       []jpegComponent
	// jfif is set by the last APP0 segment of at least 5 bytes; adobe and
	// transform by APP14 "Adobe" segments (the last one wins).
	jfif, adobe bool
	transform   byte
}

type jpegComponent struct{ id, h, v byte }

// parseSOF records the first frame header (Go's decoder refuses a second).
func (f *jpegFrame) parseSOF(marker byte, payload []byte) {
	if f.comps != nil || len(payload) < 6 {
		return
	}
	n := int(payload[5])
	if n == 0 || len(payload) < 6+3*n {
		return
	}
	f.progressive = marker == 0xC2
	for i := 0; i < n; i++ {
		c := payload[6+3*i:]
		f.comps = append(f.comps, jpegComponent{c[0], c[1] >> 4, c[1] & 0x0F})
	}
}

// isRGB mirrors image/jpeg: three components stored as RGB rather than
// YCbCr, which the decoder converts into an additional RGBA image.
func (f *jpegFrame) isRGB() bool {
	if len(f.comps) != 3 || f.jfif {
		return false
	}
	if f.adobe && f.transform == 0 {
		return true
	}
	return f.comps[0].id == 'R' && f.comps[1].id == 'G' && f.comps[2].id == 'B'
}

func clampSampling(v byte) int64 {
	return int64(min(max(v, 1), 4))
}

// jpegCost estimates the decode allocations of a w x h JPEG with EXIF
// orientation o. Four-component (CMYK or YCCK) JPEGs are rejected here:
// phones and screenshots never produce them, Go converts their colours
// without the colour profile, and they are the most expensive to decode.
func jpegCost(f *jpegFrame, w, h, o int) (int64, error) {
	if f == nil || f.comps == nil {
		return 0, fmt.Errorf("%w: jpeg frame header missing", ErrCorrupt)
	}
	n := len(f.comps)
	if n == 4 {
		return 0, ErrUnsupportedCMYK
	}
	if n != 1 && n != 3 {
		return 0, nil // the decoder rejects other component counts
	}
	h0, v0 := clampSampling(f.comps[0].h), clampSampling(f.comps[0].v)
	if n == 1 { // non-interleaved by definition: 1x1
		h0, v0 = 1, 1
	}
	mxx := (int64(w) + 8*h0 - 1) / (8 * h0)
	myy := (int64(h) + 8*v0 - 1) / (8 * v0)
	// Samples of all components, padded to whole MCUs: the decoded image.
	var samples int64
	for _, c := range f.comps {
		hi, vi := clampSampling(c.h), clampSampling(c.v)
		if n == 1 {
			hi, vi = 1, 1
		}
		samples += 64 * mxx * myy * hi * vi
	}
	px := int64(w) * int64(h)
	cost := samples
	if f.progressive {
		cost += 4 * samples // one int32 coefficient per sample, kept for all scans
	}
	if f.isRGB() {
		cost += 4 * px // converted to RGBA
	}
	if o >= 2 && o <= 8 {
		if n == 1 {
			cost += px // rotated grayscale copy
		} else {
			cost += 4 * px // rotated RGBA copy
		}
	}
	return cost, nil
}

// pngCost estimates the decode allocations of a w x h PNG from its IHDR: Go
// decodes palette images to one byte per pixel, other images of up to 8 bits
// per sample to at most 4 bytes (grayscale or RGB with a transparent colour
// become NRGBA), and 16-bit images to at most 8. An Adam7-interlaced image is
// decoded pass by pass into separate images merged into the result, which
// together hold as many pixels again.
func pngCost(b []byte, w, h int) (int64, error) {
	// Signature (8), IHDR length and type (8), then the 13-byte IHDR.
	if len(b) < 8+8+13 || string(b[12:16]) != "IHDR" {
		return 0, fmt.Errorf("%w: png IHDR missing", ErrCorrupt)
	}
	ihdr := b[16:29]
	if binary.BigEndian.Uint32(ihdr) != uint32(w) || binary.BigEndian.Uint32(ihdr[4:]) != uint32(h) {
		return 0, fmt.Errorf("%w: png header and IHDR differ", ErrCorrupt)
	}
	depth, colorType, interlace := ihdr[8], ihdr[9], ihdr[12]
	bpp := int64(4)
	switch {
	case colorType == 3:
		bpp = 1
	case depth == 16:
		bpp = 8
	}
	cost := bpp * int64(w) * int64(h)
	if interlace != 0 {
		cost *= 2
	}
	return cost, nil
}

// WebP (golang.org/x/image v0.45.0). Line numbers below refer to that
// version; a dependency update must re-check them.
//
// A lossless (VP8L) stream decodes its pixels into one 4-byte-per-pixel
// buffer (vp8l/decode.go:463). The colour-indexing transform packs pixels
// before decoding (w is replaced by nTiles(w, bits), :122) and then expands
// them into a new full-size buffer (transform.go:247): at most 2 + 4 bytes
// per pixel plus row padding. Predictor and cross-colour transforms keep a
// sub-image of 4 bytes per tile of at least 4x4 pixels (:101), and the meta
// prefix image is another such sub-image (:337). The colour table is at most
// 256 pixels (:123).
//
// The Huffman groups of the top-level image do not depend on the image size:
// up to 2600 groups (:358-362), each built only if a tile references it
// (:390-406), with five trees whose node arrays hold 2n-1 nodes of 8 bytes
// for n symbols (huffman.go:161) and a 128-entry lookup table (huffman.go:60).
// The alphabets are 280 + 2^ccBits green symbols with ccBits at most 11
// (:442), 256 for red, blue and alpha and 40 for distances (:421). The
// code-length buffers of each tree (:277, huffman.go:138) are released before
// the next tree is read, so only one tree's worth is counted. Sub-images
// (transforms, the meta image) are not top-level and have one group each.
const (
	vp8lMaxGroups = 2600
	// One group: the hGroup itself (5 trees of a slice header and a 128 x
	// uint32 table, 2680 bytes), plus node arrays rounded up to Go's
	// allocation sizes: green (2*2328-1)*8 = 37240 -> 40960 (whole pages),
	// red, blue, alpha 511*8 = 4088 -> 4096 each, distance 79*8 = 632 -> 640.
	vp8lGroupBytes = 2680 + 40960 + 3*4096 + 640
	// One tree's code lengths and codes (2 x 2328 x 4 bytes, rounded), the
	// code-length tree, the colour cache (2048 x 4 bytes), the group index
	// mapping (2 bytes per group) and the bit reader's buffer.
	vp8lFixedBytes = 2*20480 + 4096 + 8192 + 2*vp8lMaxGroups + 4096
	// The Huffman groups of the sub-images (at most three transforms and the
	// meta image), each released or kept small.
	vp8lSubImageGroups = 4
)

// vp8lCost bounds the allocations of decoding one w x h VP8L stream.
func vp8lCost(w, h int) int64 {
	px := int64(w) * int64(h)
	tiles := int64((w+3)/4) * int64((h+3)/4)
	pixels := 6*px + 4*int64(h) // decoded (packed) pixels and the colour-indexing expansion
	subImages := 3*4*tiles + 4*256
	groups := int64(vp8lMaxGroups+vp8lSubImageGroups)*vp8lGroupBytes + vp8lFixedBytes
	return pixels + subImages + groups
}

// webpCost bounds the decode allocations of a w x h WebP b (structure
// already checked by checkWebPStructure):
//   - lossless (VP8L, also inside VP8X): vp8lCost;
//   - lossy (VP8): the 4:2:0 frame padded to whole 16x16 macroblocks
//     (vp8/decode.go:205), per-macroblock filter parameters (:207) and copies
//     of the partitions, at most the file size (:286, :309, :326);
//   - an ALPH chunk adds the w x h alpha plane (webp/decode.go:206, :238) and,
//     when it is losslessly compressed, a VP8L decode of the same size (:222).
//
// The input bytes themselves are counted once as a margin.
func webpCost(b []byte, w, h int) int64 {
	lossless, alpha, compressedAlpha := false, false, false
	for off := 12; off+8 <= len(b); {
		id := string(b[off : off+4])
		n := int(binary.LittleEndian.Uint32(b[off+4:]))
		payload := off + 8
		switch id {
		case "VP8L":
			lossless = true
		case "ALPH":
			alpha = true
			compressedAlpha = compressedAlpha || n < 1 || payload >= len(b) || b[payload]&0x03 != 0
		}
		if n < 0 || payload+n > len(b) {
			break
		}
		off = payload + n + n&1
	}
	px := int64(w) * int64(h)
	cost := int64(len(b))
	if lossless {
		cost += vp8lCost(w, h)
	} else {
		mbw, mbh := int64((w+15)/16), int64((h+15)/16)
		cost += 16*16*mbw*mbh*3/2 + 16*mbw*mbh
	}
	if alpha {
		cost += px
		if compressedAlpha {
			cost += vp8lCost(w, h)
		}
	}
	return cost
}
