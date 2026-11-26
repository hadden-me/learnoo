// Package media validates uploaded lesson images and stores them as
// immutable, content-addressed files.
//
// Process accepts PNG, JPEG and WebP only (HEIC/HEIF is recognized and
// rejected with ErrUnsupportedHEIC, CMYK JPEGs with ErrUnsupportedCMYK). The
// type is detected from magic bytes, never from a filename or client
// Content-Type. Data after the image's own end marker (polyglot payloads) is
// rejected, except the structurally validated phone-photo structures after a
// JPEG's EOI described in jpeg.go, which are dropped. Size and dimension
// limits and the decode memory estimate (cost.go) are checked before the full
// decode, JPEG EXIF orientation is applied, and the decoded pixels are
// re-encoded so that stored bytes carry no metadata (EXIF, GPS, XMP, text
// chunks, ICC profiles). WebP is decoded with golang.org/x/image/webp and
// stored as lossless PNG because the standard library has no WebP encoder.
// See docs/DECISIONS.md ("Media upload limits").
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
	// MaxPixels is the largest accepted width*height (16 megapixels).
	MaxPixels = 16_000_000
	// MaxDecodeBytes bounds the memory one Process call may allocate for
	// decoding and orienting, as estimated from the headers before the full
	// decode (cost.go). Ordinary phone photos need under 90 MiB; images above
	// the bound (for example large progressive JPEGs without chroma
	// subsampling) are rejected with ErrTooComplex. See
	// docs/measurements/upload-memory.md.
	MaxDecodeBytes = 192 << 20
	// MaxEncodedBytes bounds the re-encoded (stored) image. Larger results,
	// for example a big detailed photo uploaded as WebP and stored as
	// lossless PNG, are rejected with ErrTooComplex.
	MaxEncodedBytes = 16 << 20
	// MaxConcurrentDecodes caps how many Process calls decode and re-encode at
	// once in this process. One decode keeps the process within its memory
	// budget (docs/measurements/upload-memory.md); with the example limit of
	// one CPU, a second concurrent decode would not be faster anyway.
	MaxConcurrentDecodes = 1
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
	// ErrUnsupportedHEIC is a HEIC/HEIF photo. It also matches
	// ErrUnsupportedType; callers that can, explain how to export JPEG.
	ErrUnsupportedHEIC = fmt.Errorf("%w: HEIC/HEIF is not supported", ErrUnsupportedType)
	// ErrUnsupportedCMYK is a four-component (CMYK or YCCK) JPEG, as print
	// workflows produce. It also matches ErrUnsupportedType.
	ErrUnsupportedCMYK = fmt.Errorf("%w: CMYK JPEG is not supported", ErrUnsupportedType)
	// ErrTooComplex is an image within the size limits whose decoding would
	// need more than MaxDecodeBytes, or whose re-encoded form would exceed
	// MaxEncodedBytes.
	ErrTooComplex = errors.New("media: image needs too much memory to process")
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
	inspect      func([]byte) (inspection, error)
	decodeConfig func(io.Reader) (image.Config, error)
	decode       func(io.Reader) (image.Image, error)
}

var (
	formatPNG  = format{ContentTypePNG, whole(checkPNGEnd), png.DecodeConfig, png.Decode}
	formatJPEG = format{ContentTypeJPEG, inspectJPEG, jpeg.DecodeConfig, jpeg.Decode}
	formatWebP = format{ContentTypeWebP, whole(checkWebPStructure), webp.DecodeConfig, webp.Decode}
)

// inspection is what the structural check found: the image's own bytes are
// b[:end] (a JPEG may be followed by validated structures that are dropped),
// orientation is the EXIF orientation to apply (1 = as stored), and jpeg the
// primary JPEG's frame information for the decode cost.
type inspection struct {
	end         int
	orientation int
	jpeg        *jpegFrame
}

// whole adapts a check for formats whose image must fill the upload.
func whole(check func([]byte) error) func([]byte) (inspection, error) {
	return func(b []byte) (inspection, error) {
		if err := check(b); err != nil {
			return inspection{}, err
		}
		return inspection{end: len(b), orientation: 1}, nil
	}
}

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
// Images whose decoding would need more than MaxDecodeBytes, or whose
// re-encoded form would exceed MaxEncodedBytes, are rejected with
// ErrTooComplex. Decoding waits for one of MaxConcurrentDecodes slots; if ctx
// ends first, Process returns ctx.Err(). Reading the upload happens before
// that wait, so callers must still bound request bodies and concurrent
// uploads.
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
		if isHEIF(raw) {
			return nil, ErrUnsupportedHEIC
		}
		return nil, fmt.Errorf("%w: only PNG, JPEG and WebP are accepted", ErrUnsupportedType)
	}
	if declaredType != "" {
		mt, _, err := mime.ParseMediaType(declaredType)
		if err != nil || (!strings.EqualFold(mt, f.contentType) && !strings.EqualFold(mt, "application/octet-stream")) {
			return nil, fmt.Errorf("%w: declared %q, detected %s", ErrTypeMismatch, declaredType, f.contentType)
		}
	}
	ins, err := f.inspect(raw)
	if err != nil {
		return nil, err
	}
	// Only the image itself is ever decoded, never the structures after it.
	data := raw[:ins.end]

	// Header-only check before allocating pixels (decompression-bomb defence).
	cfg, err := f.decodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, decodeError(err)
	}
	if err := checkDimensions(cfg.Width, cfg.Height); err != nil {
		return nil, err
	}
	// The limits are symmetric, but check the upright size too.
	if err := checkDimensions(orientedSize(cfg.Width, cfg.Height, ins.orientation)); err != nil {
		return nil, err
	}
	cost, err := decodeCost(f, data, cfg, ins)
	if err != nil {
		return nil, err
	}
	if cost > MaxDecodeBytes {
		return nil, fmt.Errorf("%w: decoding needs about %d MiB (at most %d)", ErrTooComplex, cost>>20, MaxDecodeBytes>>20)
	}

	if err := acquireSlot(ctx); err != nil {
		return nil, err
	}
	defer func() { <-decodeSlots }()

	img, err := f.decode(bytes.NewReader(data))
	if err != nil {
		return nil, decodeError(err)
	}
	if b := img.Bounds(); b.Dx() != cfg.Width || b.Dy() != cfg.Height {
		return nil, fmt.Errorf("%w: header and decoded dimensions differ", ErrCorrupt)
	}
	img = orient(img, ins.orientation)
	b := img.Bounds()

	out := limitedBuffer{max: encodedLimit}
	stored := ContentTypePNG
	switch f.contentType {
	case ContentTypeJPEG:
		stored = ContentTypeJPEG
		err = jpeg.Encode(&out, img, &jpeg.Options{Quality: JPEGQuality})
	default: // PNG, and WebP stored as lossless PNG.
		err = (&png.Encoder{CompressionLevel: png.DefaultCompression}).Encode(&out, img)
	}
	if errors.Is(err, errEncodedTooLarge) {
		return nil, fmt.Errorf("%w: re-encoded image exceeds %d MiB", ErrTooComplex, MaxEncodedBytes>>20)
	}
	if err != nil {
		return nil, fmt.Errorf("media: re-encode: %w", err)
	}

	sum := sha256.Sum256(out.buf.Bytes())
	return &Asset{
		processed:   true,
		data:        out.buf.Bytes(),
		sha256:      hex.EncodeToString(sum[:]),
		contentType: stored,
		sourceType:  f.contentType,
		width:       b.Dx(),
		height:      b.Dy(),
	}, nil
}

// decodeCost estimates the decode allocations of the image (cost.go).
func decodeCost(f format, data []byte, cfg image.Config, ins inspection) (int64, error) {
	switch f.contentType {
	case ContentTypeJPEG:
		return jpegCost(ins.jpeg, cfg.Width, cfg.Height, ins.orientation)
	case ContentTypePNG:
		return pngCost(data, cfg.Width, cfg.Height)
	default:
		return webpCost(cfg.Width, cfg.Height), nil
	}
}

var errEncodedTooLarge = errors.New("media: re-encoded image too large")

// encodedLimit is MaxEncodedBytes; tests lower it.
var encodedLimit = MaxEncodedBytes

// limitedBuffer collects encoder output up to max bytes. It has only Write,
// so encoders cannot bypass the limit through other bytes.Buffer methods.
type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.max {
		return 0, errEncodedTooLarge
	}
	return b.buf.Write(p)
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
