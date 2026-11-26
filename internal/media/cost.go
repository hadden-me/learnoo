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

// webpCost bounds the decode allocations of a w x h WebP: at most 8.5 bytes
// per pixel (lossy with lossless alpha: the 4:2:0 frame padded to 16 px, the
// alpha plane decoded as a full lossless image, a colour-indexing transform
// buffer and the extracted alpha), rounded up to 9; lossless images need at
// most 6.
func webpCost(w, h int) int64 {
	return 9 * int64(w) * int64(h)
}
