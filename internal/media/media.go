// Package media validates uploaded lesson images and stores them as
// immutable, content-addressed files.
//
// Process accepts PNG, JPEG and WebP only. The type is detected from magic
// bytes, never from a filename or client Content-Type. Data after the image's
// own end marker (polyglot payloads) is rejected. Size and dimension limits are
// checked before the full decode, and the decoded pixels are re-encoded so
// that stored bytes carry no metadata (EXIF, GPS, XMP, text chunks, ICC
// profiles). WebP is decoded with golang.org/x/image/webp and stored as
// lossless PNG because the standard library has no WebP encoder. See
// docs/DECISIONS.md ("Media upload limits").
package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"mime"
	"strings"

	"golang.org/x/image/webp"
)

// Limits. Changing them is a recorded decision, not a tuning knob.
const (
	// MaxUploadBytes is the largest accepted upload (5 MiB). Larger input is
	// rejected, never truncated.
	MaxUploadBytes = 5 << 20
	// MaxSide is the largest accepted width or height in pixels.
	MaxSide = 4096
	// MaxPixels is the largest accepted width*height (16 megapixels). Within
	// these limits one Process call can still allocate about 250 MB in total
	// (measured: ~246 MB for a 132 KB 16-bit 4000x4000 PNG, ~230 MB for a
	// progressive 4:4:4 JPEG), hence MaxConcurrentDecodes.
	MaxPixels = 16_000_000
	// MaxConcurrentDecodes caps how many Process calls decode and re-encode at
	// once in this process, bounding decode memory to about 500 MB.
	MaxConcurrentDecodes = 2
	// JPEGQuality is the fixed quality used when re-encoding JPEG uploads.
	JPEGQuality = 90
)

// Content types. Uploads may be any of the three; stored assets are only
// ContentTypePNG or ContentTypeJPEG (WebP is stored as PNG).
const (
	ContentTypePNG  = "image/png"
	ContentTypeJPEG = "image/jpeg"
	ContentTypeWebP = "image/webp"
)

// Rejection reasons. Process wraps one of these, so callers map them with
// errors.Is (for example to HTTP 413 or 415 in the later API slice).
var (
	ErrEmpty           = errors.New("media: empty upload")
	ErrTooLarge        = errors.New("media: upload exceeds size limit")
	ErrUnsupportedType = errors.New("media: unsupported image type")
	ErrTypeMismatch    = errors.New("media: declared content type does not match image content")
	ErrDimensions      = errors.New("media: image dimensions outside limits")
	ErrCorrupt         = errors.New("media: image is corrupt or truncated")
	ErrTrailingData    = errors.New("media: data after the end of the image")
)

// decodeSlots is the process-wide semaphore behind MaxConcurrentDecodes.
var decodeSlots = make(chan struct{}, MaxConcurrentDecodes)

// testHookWaitingForSlot, when set, runs when Process has to wait for a slot.
var testHookWaitingForSlot func()

// Asset is a validated, canonical image ready to be stored. Its fields are
// unexported: only Process creates usable Assets, and Store.Put rejects any
// other value.
type Asset struct {
	processed   bool
	data        []byte
	sha256      string
	contentType string
	sourceType  string
	width       int
	height      int
}

// Data returns a copy of the canonical re-encoded bytes. SHA256, ByteSize and
// the storage key all describe these bytes, not the original upload.
func (a *Asset) Data() []byte { return bytes.Clone(a.data) }

// SHA256 is the lowercase hex SHA-256 of the stored bytes.
func (a *Asset) SHA256() string { return a.sha256 }

// ContentType is the stored type: ContentTypePNG or ContentTypeJPEG.
func (a *Asset) ContentType() string { return a.contentType }

// SourceType is the detected upload type (PNG, JPEG or WebP).
func (a *Asset) SourceType() string { return a.sourceType }

// ByteSize is the length of the stored bytes.
func (a *Asset) ByteSize() int64 { return int64(len(a.data)) }

// Width and Height are the image dimensions in pixels.
func (a *Asset) Width() int  { return a.width }
func (a *Asset) Height() int { return a.height }

// Key returns the storage key for the asset: "<sha256>.<ext>".
func (a *Asset) Key() string {
	return keyFor(a.sha256, a.contentType)
}

type format struct {
	contentType  string
	checkEnd     func([]byte) error
	decodeConfig func(io.Reader) (image.Config, error)
	decode       func(io.Reader) (image.Image, error)
}

var (
	formatPNG  = format{ContentTypePNG, checkPNGEnd, png.DecodeConfig, png.Decode}
	formatJPEG = format{ContentTypeJPEG, checkJPEGEnd, jpeg.DecodeConfig, jpeg.Decode}
	formatWebP = format{ContentTypeWebP, checkWebPStructure, webp.DecodeConfig, webp.Decode}
)

// sniff identifies an accepted format from magic bytes only.
func sniff(b []byte) (format, bool) {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return formatPNG, true
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		return formatJPEG, true
	case len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return formatWebP, true
	}
	return format{}, false
}

// Process reads at most MaxUploadBytes from r, validates the image and returns
// its canonical form. declaredType is the client's Content-Type. An empty value
// or application/octet-stream counts as undeclared; any other value must name
// the detected type.
//
// Decoding waits for one of MaxConcurrentDecodes slots; if ctx ends first,
// Process returns ctx.Err(). Reading the upload happens before that wait, so
// callers must still bound request bodies and concurrent uploads.
func Process(ctx context.Context, r io.Reader, declaredType string) (*Asset, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxUploadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("media: read upload: %w", err)
	}
	if len(raw) > MaxUploadBytes {
		return nil, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, MaxUploadBytes)
	}
	if len(raw) == 0 {
		return nil, ErrEmpty
	}

	f, ok := sniff(raw)
	if !ok {
		return nil, fmt.Errorf("%w: only PNG, JPEG and WebP are accepted", ErrUnsupportedType)
	}
	if declaredType != "" {
		mt, _, err := mime.ParseMediaType(declaredType)
		if err != nil || (!strings.EqualFold(mt, f.contentType) && !strings.EqualFold(mt, "application/octet-stream")) {
			return nil, fmt.Errorf("%w: declared %q, detected %s", ErrTypeMismatch, declaredType, f.contentType)
		}
	}
	if err := f.checkEnd(raw); err != nil {
		return nil, err
	}

	// Header-only check before allocating pixels (decompression-bomb defence).
	cfg, err := f.decodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, decodeError(err)
	}
	if err := checkDimensions(cfg.Width, cfg.Height); err != nil {
		return nil, err
	}

	if err := acquireSlot(ctx); err != nil {
		return nil, err
	}
	defer func() { <-decodeSlots }()

	img, err := f.decode(bytes.NewReader(raw))
	if err != nil {
		return nil, decodeError(err)
	}
	b := img.Bounds()
	if b.Dx() != cfg.Width || b.Dy() != cfg.Height {
		return nil, fmt.Errorf("%w: header and decoded dimensions differ", ErrCorrupt)
	}

	var out bytes.Buffer
	stored := ContentTypePNG
	switch f.contentType {
	case ContentTypeJPEG:
		stored = ContentTypeJPEG
		err = jpeg.Encode(&out, img, &jpeg.Options{Quality: JPEGQuality})
	default: // PNG, and WebP stored as lossless PNG.
		err = (&png.Encoder{CompressionLevel: png.DefaultCompression}).Encode(&out, img)
	}
	if err != nil {
		return nil, fmt.Errorf("media: re-encode: %w", err)
	}

	sum := sha256.Sum256(out.Bytes())
	return &Asset{
		processed:   true,
		data:        out.Bytes(),
		sha256:      hex.EncodeToString(sum[:]),
		contentType: stored,
		sourceType:  f.contentType,
		width:       b.Dx(),
		height:      b.Dy(),
	}, nil
}

func acquireSlot(ctx context.Context) error {
	select {
	case decodeSlots <- struct{}{}:
		return nil
	default:
	}
	if testHookWaitingForSlot != nil {
		testHookWaitingForSlot()
	}
	select {
	case decodeSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// decodeError classifies decoder errors: unsupported variants (for example
// arithmetic-coded JPEG) are ErrUnsupportedType, everything else ErrCorrupt.
func decodeError(err error) error {
	var je jpeg.UnsupportedError
	var pe png.UnsupportedError
	if errors.As(err, &je) || errors.As(err, &pe) {
		return fmt.Errorf("%w: %v", ErrUnsupportedType, err)
	}
	return fmt.Errorf("%w: %v", ErrCorrupt, err)
}

func checkDimensions(w, h int) error {
	if w <= 0 || h <= 0 {
		return fmt.Errorf("%w: %dx%d has a zero dimension", ErrDimensions, w, h)
	}
	if w > MaxSide || h > MaxSide {
		return fmt.Errorf("%w: %dx%d exceeds %d px per side", ErrDimensions, w, h, MaxSide)
	}
	if int64(w)*int64(h) > MaxPixels {
		return fmt.Errorf("%w: %dx%d exceeds %d pixels", ErrDimensions, w, h, MaxPixels)
	}
	return nil
}

// checkPNGEnd walks the chunk list and requires the file to end exactly after
// the IEND chunk.
func checkPNGEnd(b []byte) error {
	off := uint64(8)
	n := uint64(len(b))
	for {
		if off+12 > n {
			return fmt.Errorf("%w: png chunk list ends without IEND", ErrCorrupt)
		}
		length := uint64(binary.BigEndian.Uint32(b[off:]))
		typ := string(b[off+4 : off+8])
		end := off + 12 + length
		if end > n {
			return fmt.Errorf("%w: png chunk %q is truncated", ErrCorrupt, typ)
		}
		if typ == "IEND" {
			if end != n {
				return fmt.Errorf("%w: %d bytes after png IEND", ErrTrailingData, n-end)
			}
			return nil
		}
		off = end
	}
}

// checkJPEGEnd walks the marker segments and entropy-coded data and requires
// the file to end exactly after the EOI marker. Segment payloads (including
// EXIF thumbnails that contain their own SOI/EOI) are skipped by length.
func checkJPEGEnd(b []byte) error {
	n := len(b)
	pos := 2 // after SOI
	for {
		if pos >= n || b[pos] != 0xFF {
			return fmt.Errorf("%w: jpeg marker expected", ErrCorrupt)
		}
		for pos < n && b[pos] == 0xFF { // fill bytes
			pos++
		}
		if pos >= n {
			return fmt.Errorf("%w: jpeg ends without EOI", ErrCorrupt)
		}
		m := b[pos]
		pos++
		switch {
		case m == 0xD9: // EOI
			if pos != n {
				return fmt.Errorf("%w: %d bytes after jpeg EOI", ErrTrailingData, n-pos)
			}
			return nil
		case m == 0x01 || (m >= 0xD0 && m <= 0xD7): // no length
			continue
		case m == 0x00 || m == 0xD8:
			return fmt.Errorf("%w: unexpected jpeg marker 0x%02X", ErrCorrupt, m)
		}
		if pos+2 > n {
			return fmt.Errorf("%w: jpeg segment truncated", ErrCorrupt)
		}
		length := int(binary.BigEndian.Uint16(b[pos:]))
		if length < 2 || pos+length > n {
			return fmt.Errorf("%w: jpeg segment length invalid", ErrCorrupt)
		}
		pos += length
		if m != 0xDA { // not SOS
			continue
		}
		// Entropy-coded data runs until a marker other than a stuffed 0xFF00
		// or a restart marker.
		for {
			if pos+1 >= n {
				return fmt.Errorf("%w: jpeg ends without EOI", ErrCorrupt)
			}
			if b[pos] == 0xFF {
				next := b[pos+1]
				if next == 0x00 || (next >= 0xD0 && next <= 0xD7) {
					pos += 2
					continue
				}
				if next != 0xFF {
					break
				}
			}
			pos++
		}
	}
}

// checkWebPStructure requires the RIFF length to match the file exactly, the
// chunk list to fill it, and a still image (VP8, VP8L, or VP8X without the
// animation flag and without ANIM/ANMF chunks).
func checkWebPStructure(b []byte) error {
	size := uint64(binary.LittleEndian.Uint32(b[4:8]))
	n := uint64(len(b))
	if 8+size > n {
		return fmt.Errorf("%w: webp RIFF length exceeds file", ErrCorrupt)
	}
	if 8+size < n {
		return fmt.Errorf("%w: %d bytes after webp RIFF data", ErrTrailingData, n-8-size)
	}
	off := uint64(12)
	first := true
	for off < n {
		if off+8 > n {
			return fmt.Errorf("%w: webp chunk header truncated", ErrCorrupt)
		}
		id := string(b[off : off+4])
		length := uint64(binary.LittleEndian.Uint32(b[off+4:]))
		payload := off + 8
		if payload+length > n {
			return fmt.Errorf("%w: webp chunk %q truncated", ErrCorrupt, id)
		}
		if first {
			switch id {
			case "VP8 ", "VP8L":
			case "VP8X":
				if length < 1 {
					return fmt.Errorf("%w: webp VP8X chunk empty", ErrCorrupt)
				}
				if b[payload]&0x02 != 0 {
					return fmt.Errorf("%w: animated webp", ErrUnsupportedType)
				}
			default:
				return fmt.Errorf("%w: webp chunk %q", ErrUnsupportedType, id)
			}
			first = false
		}
		if id == "ANIM" || id == "ANMF" {
			return fmt.Errorf("%w: animated webp", ErrUnsupportedType)
		}
		off = payload + length + length&1
	}
	if first || off != n {
		return fmt.Errorf("%w: webp chunk list malformed", ErrCorrupt)
	}
	return nil
}
