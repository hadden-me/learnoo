package media

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
)

// Phone JPEGs often carry more than one image: the primary image ends with
// EOI and is followed by structures that its own metadata describes. Process
// decodes only the primary image and drops the rest, but it accepts a trailer
// only when every byte of it is accounted for by one of these, each checked
// structurally (never decoded):
//
//   - CIPA multi-picture (MPF) secondary images listed in the primary's APP2
//     MPF index (previews, Ultra HDR gain maps): each entry's offset and size
//     must frame exactly one well-formed JPEG segment stream (SOI, a frame, a
//     scan, EOI as its last two bytes).
//   - Items of a Google "Container" directory in the primary's XMP (Ultra HDR
//     gain maps, motion photos): laid out one after another from the primary's
//     EOI by their Item:Length and Item:Padding; image/jpeg items are checked as
//     above, the MotionPhoto video item as an ISO BMFF box list starting with
//     'ftyp' and filling its length exactly.
//   - A motion-photo video located by GCamera:MicroVideoOffset (bytes from the
//     end of the file), checked as an ISO BMFF box list.
//   - Zero bytes (0x00 only) between or after those structures, at most
//     MaxTrailingZeros in total.
//
// A video must be the last part and end exactly at the end of the file:
// nothing (no image, padding or zero byte) may follow it.
//
// Anything else after EOI (unknown, partially covered, overlapping or
// inconsistently declared data, appended files, scripts) is ErrTrailingData.

const (
	// MaxTrailingZeros bounds the zero padding accepted after a JPEG's EOI.
	MaxTrailingZeros = 1 << 20
	// maxTrailerParts bounds MPF entries and Container items.
	maxTrailerParts = 8
	// maxIFDEntries bounds the entries read from an EXIF or MPF IFD.
	maxIFDEntries = 512
)

var (
	exifID = []byte("Exif\x00\x00")
	xmpID  = []byte("http://ns.adobe.com/xap/1.0/\x00")
	mpfID  = []byte("MPF\x00")
)

const (
	nsCamera    = "http://ns.google.com/photos/1.0/camera/"
	nsContainer = "http://ns.google.com/photos/1.0/container/"
	nsItem      = "http://ns.google.com/photos/1.0/container/item/"
)

// jpegScan is the result of walking one JPEG segment stream.
type jpegScan struct {
	end         int  // offset just past EOI
	frame, scan bool // saw a SOF and an SOS segment
	// Collected for the primary image only.
	exif [][]byte // APP1 Exif payloads, after the identifier
	xmp  [][]byte // APP1 standard XMP packets, after the identifier
	mpf  []int    // absolute offsets of APP2 MPF segments' TIFF headers
	app2 [][]byte // the matching MPF payloads, from the TIFF header on
}

// walkJPEG walks the marker segments and entropy-coded data of the JPEG that
// starts at b[0] (SOI) and stops at its EOI. Segment payloads (including EXIF
// thumbnails that contain their own SOI/EOI) are skipped by length. With
// collect, it records the metadata segments Process uses. Errors wrap
// ErrCorrupt.
func walkJPEG(b []byte, collect bool) (jpegScan, error) {
	var s jpegScan
	n := len(b)
	if n < 2 || b[0] != 0xFF || b[1] != 0xD8 {
		return s, fmt.Errorf("%w: jpeg SOI expected", ErrCorrupt)
	}
	pos := 2
	for {
		if pos >= n || b[pos] != 0xFF {
			return s, fmt.Errorf("%w: jpeg marker expected", ErrCorrupt)
		}
		for pos < n && b[pos] == 0xFF { // fill bytes
			pos++
		}
		if pos >= n {
			return s, fmt.Errorf("%w: jpeg ends without EOI", ErrCorrupt)
		}
		m := b[pos]
		pos++
		switch {
		case m == 0xD9: // EOI
			s.end = pos
			return s, nil
		case m == 0x01 || (m >= 0xD0 && m <= 0xD7): // no length
			continue
		case m == 0x00 || m == 0xD8:
			return s, fmt.Errorf("%w: unexpected jpeg marker 0x%02X", ErrCorrupt, m)
		}
		if pos+2 > n {
			return s, fmt.Errorf("%w: jpeg segment truncated", ErrCorrupt)
		}
		length := int(binary.BigEndian.Uint16(b[pos:]))
		if length < 2 || pos+length > n {
			return s, fmt.Errorf("%w: jpeg segment length invalid", ErrCorrupt)
		}
		payload := b[pos+2 : pos+length]
		if m >= 0xC0 && m <= 0xCF && m != 0xC4 && m != 0xC8 && m != 0xCC {
			s.frame = true
		}
		if collect {
			switch {
			case m == 0xE1 && bytes.HasPrefix(payload, exifID):
				s.exif = append(s.exif, payload[len(exifID):])
			case m == 0xE1 && bytes.HasPrefix(payload, xmpID):
				s.xmp = append(s.xmp, payload[len(xmpID):])
			case m == 0xE2 && bytes.HasPrefix(payload, mpfID):
				s.mpf = append(s.mpf, pos+2+len(mpfID))
				s.app2 = append(s.app2, payload[len(mpfID):])
			}
		}
		pos += length
		if m != 0xDA { // not SOS
			continue
		}
		s.scan = true
		// Entropy-coded data runs until a marker other than a stuffed 0xFF00
		// or a restart marker.
		for {
			if pos+1 >= n {
				return s, fmt.Errorf("%w: jpeg ends without EOI", ErrCorrupt)
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

// inspectJPEG validates the primary image's segment structure and everything
// after its EOI, and reads its EXIF orientation.
func inspectJPEG(b []byte) (inspection, error) {
	s, err := walkJPEG(b, true)
	if err != nil {
		return inspection{}, err
	}
	if err := checkJPEGTrailer(b, s); err != nil {
		return inspection{}, err
	}
	return inspection{end: s.end, orientation: exifOrientation(s.exif)}, nil
}

type partKind int

const (
	partJPEG partKind = iota + 1
	partVideo
	partZeros
)

type partSource int

const (
	fromMPF partSource = iota + 1
	fromContainer
	fromMicroVideo
)

// trailerPart is a validated byte range [start, end) after the primary EOI.
type trailerPart struct {
	start, end int
	kind       partKind
	source     partSource
}

func trailingErr(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrTrailingData}, args...)...)
}

// checkJPEGTrailer accepts the bytes after the primary EOI only if validated
// parts and zero padding cover them exactly.
func checkJPEGTrailer(b []byte, s jpegScan) error {
	n := len(b)
	if s.end == n {
		return nil
	}
	var parts []trailerPart
	switch len(s.mpf) {
	case 0:
	case 1:
		p, err := mpfParts(b, s.mpf[0], s.app2[0], s.end)
		if err != nil {
			return err
		}
		parts = append(parts, p...)
	default:
		return trailingErr("%d MPF segments", len(s.mpf))
	}
	// Several XMP packets or one that is not well-formed XML claim nothing:
	// the trailer must then be covered by other parts.
	if len(s.xmp) == 1 {
		x, err := parseXMP(s.xmp[0])
		if err == nil {
			p, err := xmpParts(b, x, s.end)
			if err != nil {
				return err
			}
			parts = append(parts, p...)
		}
	}
	return coverTrailer(b, s.end, parts)
}

// coverTrailer requires parts and zero bytes to tile b[eoi:] exactly. The
// same range may be declared by two different sources (an Ultra HDR gain map
// is both an MPF entry and a Container item) if both say it is the same kind.
func coverTrailer(b []byte, eoi int, parts []trailerPart) error {
	slices.SortFunc(parts, func(x, y trailerPart) int {
		if x.start != y.start {
			return x.start - y.start
		}
		return x.end - y.end
	})
	pos, zeros := eoi, 0
	var prev *trailerPart
	for i := range parts {
		p := &parts[i]
		if prev != nil && p.start == prev.start && p.end == prev.end && p.kind == prev.kind && p.source != prev.source {
			continue
		}
		if prev != nil && prev.kind == partVideo {
			return trailingErr("data after the motion-photo video")
		}
		if p.start < pos {
			return trailingErr("overlapping parts at offset %d", p.start)
		}
		if !allZero(b[pos:p.start]) {
			return trailingErr("undeclared data at offset %d", pos)
		}
		zeros += p.start - pos
		if p.kind == partZeros {
			zeros += p.end - p.start
		}
		pos = p.end
		prev = p
	}
	if prev != nil && prev.kind == partVideo && pos != len(b) {
		return trailingErr("data after the motion-photo video")
	}
	if !allZero(b[pos:]) {
		return trailingErr("undeclared data at offset %d", pos)
	}
	zeros += len(b) - pos
	if zeros > MaxTrailingZeros {
		return trailingErr("%d bytes of zero padding", zeros)
	}
	return nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// checkEmbeddedJPEG requires b to be exactly one well-formed JPEG segment
// stream with a frame and a scan. Its pixels are never decoded.
func checkEmbeddedJPEG(b []byte) error {
	s, err := walkJPEG(b, false)
	if err != nil {
		return trailingErr("embedded image: %v", err)
	}
	if s.end != len(b) || !s.frame || !s.scan {
		return trailingErr("embedded image does not fill its declared length")
	}
	return nil
}

// checkBMFF requires b to be a list of ISO BMFF boxes that starts with 'ftyp'
// and fills b exactly. Box contents are never parsed. A box size of 0 ("to
// the end") is accepted only for a final 'mdat' box, as streaming writers
// produce; the region it extends to is already bounded by the declared length.
func checkBMFF(b []byte) error {
	n := uint64(len(b))
	pos := uint64(0)
	for pos < n {
		if n-pos < 8 {
			return trailingErr("video box header truncated")
		}
		size := uint64(binary.BigEndian.Uint32(b[pos:]))
		typ := b[pos+4 : pos+8]
		header := uint64(8)
		switch size {
		case 0: // extends to the end: only a final mdat
			if string(typ) != "mdat" {
				return trailingErr("video box %q without a size", typ)
			}
			size = n - pos
		case 1: // 64-bit size
			if n-pos < 16 {
				return trailingErr("video box header truncated")
			}
			size = binary.BigEndian.Uint64(b[pos+8:])
			header = 16
		}
		if size < header || size > n-pos {
			return trailingErr("video box size invalid")
		}
		for _, c := range typ {
			if c < 0x20 || c > 0x7E {
				return trailingErr("video box type invalid")
			}
		}
		if pos == 0 && (string(typ) != "ftyp" || size < header+8) {
			return trailingErr("video does not start with an ftyp box")
		}
		pos += size
	}
	if pos == 0 {
		return trailingErr("empty video")
	}
	return nil
}

// tiff reads a TIFF structure (EXIF or MPF) with strict bounds.
type tiff struct {
	b     []byte
	order binary.ByteOrder
}

func newTIFF(b []byte) (tiff, bool) {
	if len(b) < 8 {
		return tiff{}, false
	}
	var t tiff
	switch string(b[:4]) {
	case "II*\x00":
		t.order = binary.LittleEndian
	case "MM\x00*":
		t.order = binary.BigEndian
	default:
		return tiff{}, false
	}
	t.b = b
	return t, true
}

type ifdEntry struct {
	tag, typ     uint16
	count, value uint32
	raw          []byte // the 4-byte value field
}

// ifd0 returns the entries of the first IFD.
func (t tiff) ifd0() ([]ifdEntry, bool) {
	off := uint64(t.order.Uint32(t.b[4:]))
	n := uint64(len(t.b))
	if off < 8 || off+2 > n {
		return nil, false
	}
	count := uint64(t.order.Uint16(t.b[off:]))
	if count > maxIFDEntries || off+2+count*12 > n {
		return nil, false
	}
	entries := make([]ifdEntry, count)
	for i := range entries {
		e := t.b[off+2+uint64(i)*12:]
		entries[i] = ifdEntry{
			tag: t.order.Uint16(e), typ: t.order.Uint16(e[2:]),
			count: t.order.Uint32(e[4:]), value: t.order.Uint32(e[8:]), raw: e[8:12],
		}
	}
	return entries, true
}

// mpfParts validates the MPF index whose TIFF header is at absolute offset
// base and returns its secondary images.
func mpfParts(b []byte, base int, payload []byte, eoi int) ([]trailerPart, error) {
	t, ok := newTIFF(payload)
	if !ok {
		return nil, trailingErr("MPF header invalid")
	}
	entries, ok := t.ifd0()
	if !ok {
		return nil, trailingErr("MPF index invalid")
	}
	var version, number, entryTag *ifdEntry
	for i := range entries {
		e := &entries[i]
		var slot **ifdEntry
		switch e.tag {
		case 0xB000:
			slot = &version
		case 0xB001:
			slot = &number
		case 0xB002:
			slot = &entryTag
		default:
			continue
		}
		if *slot != nil {
			return nil, trailingErr("duplicate MPF tag 0x%04X", e.tag)
		}
		*slot = e
	}
	if version == nil || version.typ != 7 || version.count != 4 || string(version.raw) != "0100" {
		return nil, trailingErr("MPF version missing or unsupported")
	}
	if number == nil || number.typ != 4 || number.count != 1 || number.value < 1 || number.value > maxTrailerParts {
		return nil, trailingErr("MPF image count missing or out of range")
	}
	images := uint64(number.value)
	if entryTag == nil || entryTag.typ != 7 || uint64(entryTag.count) != 16*images {
		return nil, trailingErr("MPF entries missing or inconsistent")
	}
	off := uint64(entryTag.value)
	if off < 8 || off+16*images > uint64(len(payload)) {
		return nil, trailingErr("MPF entries outside the segment")
	}
	parts := make([]trailerPart, 0, images-1)
	for i := uint64(0); i < images; i++ {
		e := payload[off+16*i:]
		size := uint64(t.order.Uint32(e[4:]))
		offset := uint64(t.order.Uint32(e[8:]))
		if i == 0 {
			// The primary image: offset 0 by definition. Its size is not
			// used: the primary's extent is where its own EOI is.
			if offset != 0 {
				return nil, trailingErr("MPF primary entry has an offset")
			}
			continue
		}
		start := uint64(base) + offset
		if size == 0 || offset == 0 || start < uint64(eoi) || start+size > uint64(len(b)) {
			return nil, trailingErr("MPF entry %d outside the trailer", i)
		}
		if err := checkEmbeddedJPEG(b[start : start+size]); err != nil {
			return nil, fmt.Errorf("MPF entry %d: %w", i, err)
		}
		parts = append(parts, trailerPart{int(start), int(start + size), partJPEG, fromMPF})
	}
	return parts, nil
}

// xmpInfo is what the primary's XMP says about the trailer.
type xmpInfo struct {
	directories      int
	items            []containerItem
	microVideo       []string
	microVideoOffset []string
}

type containerItem struct {
	mime, semantic, length, padding string
	inDirectory                     bool
}

// parseXMP reads the attributes Process needs from an XMP packet (at most one
// JPEG segment, so bounded). Properties written as elements rather than
// attributes are not recognized; their trailer then stays undeclared.
func parseXMP(packet []byte) (xmpInfo, error) {
	var x xmpInfo
	d := xml.NewDecoder(bytes.NewReader(packet))
	d.Strict = true
	depth, dirDepth := 0, 0
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			return x, nil
		}
		if err != nil {
			return xmpInfo{}, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			for _, a := range t.Attr {
				if a.Name.Space != nsCamera {
					continue
				}
				switch a.Name.Local {
				case "MicroVideo":
					x.microVideo = append(x.microVideo, a.Value)
				case "MicroVideoOffset":
					x.microVideoOffset = append(x.microVideoOffset, a.Value)
				}
			}
			if t.Name.Space != nsContainer {
				continue
			}
			switch t.Name.Local {
			case "Directory":
				x.directories++
				if dirDepth == 0 {
					dirDepth = depth
				}
			case "Item":
				if len(x.items) == maxTrailerParts+1 {
					return xmpInfo{}, errors.New("too many container items")
				}
				it := containerItem{inDirectory: dirDepth != 0}
				for _, a := range t.Attr {
					if a.Name.Space != nsItem {
						continue
					}
					switch a.Name.Local {
					case "Mime":
						it.mime = a.Value
					case "Semantic":
						it.semantic = a.Value
					case "Length":
						it.length = a.Value
					case "Padding":
						it.padding = a.Value
					}
				}
				x.items = append(x.items, it)
			}
		case xml.EndElement:
			if depth == dirDepth {
				dirDepth = 0
			}
			depth--
		}
	}
}

// parseLength reads a decimal byte count no larger than the upload limit.
func parseLength(s string, required bool) (int, bool) {
	if s == "" {
		return 0, !required
	}
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil || v > MaxUploadBytes || (required && v == 0) {
		return 0, false
	}
	return int(v), true
}

// xmpParts turns the XMP's Container directory and MicroVideo offset into
// validated trailer parts.
func xmpParts(b []byte, x xmpInfo, eoi int) ([]trailerPart, error) {
	n := len(b)
	var parts []trailerPart
	if x.directories > 1 {
		return nil, trailingErr("%d container directories", x.directories)
	}
	if x.directories == 1 {
		if len(x.items) < 1 || len(x.items) > maxTrailerParts {
			return nil, trailingErr("container directory has %d items", len(x.items))
		}
		if x.items[0].semantic != "Primary" {
			return nil, trailingErr("container directory does not start with the primary image")
		}
		pos := eoi
		for i, it := range x.items {
			if !it.inDirectory {
				return nil, trailingErr("container item outside the directory")
			}
			padding, ok := parseLength(it.padding, false)
			if !ok {
				return nil, trailingErr("container item %d padding invalid", i)
			}
			if i > 0 {
				length, ok := parseLength(it.length, true)
				if !ok || length > n-pos {
					return nil, trailingErr("container item %d length invalid", i)
				}
				p := trailerPart{pos, pos + length, 0, fromContainer}
				var err error
				switch {
				case it.semantic == "Primary":
					err = trailingErr("second primary container item")
				case it.mime == "image/jpeg":
					p.kind = partJPEG
					err = checkEmbeddedJPEG(b[p.start:p.end])
				case it.semantic == "MotionPhoto" && (it.mime == "video/mp4" || it.mime == "video/quicktime"):
					p.kind = partVideo
					err = checkBMFF(b[p.start:p.end])
				default:
					err = trailingErr("container item %d (%q, %q) not supported", i, it.semantic, it.mime)
				}
				if err != nil {
					return nil, err
				}
				parts = append(parts, p)
				pos = p.end
			}
			if padding > n-pos {
				return nil, trailingErr("container item %d padding beyond the file", i)
			}
			if padding > 0 {
				if !allZero(b[pos : pos+padding]) {
					return nil, trailingErr("container item %d padding is not zero", i)
				}
				parts = append(parts, trailerPart{pos, pos + padding, partZeros, fromContainer})
				pos += padding
			}
		}
	} else if len(x.items) > 0 {
		return nil, trailingErr("container item outside a directory")
	}

	if len(x.microVideo) > 1 || len(x.microVideoOffset) > 1 {
		return nil, trailingErr("duplicate MicroVideo properties")
	}
	if len(x.microVideo) == 1 && x.microVideo[0] == "1" && len(x.microVideoOffset) == 1 {
		v, ok := parseLength(x.microVideoOffset[0], true)
		if !ok || v > n-eoi {
			return nil, trailingErr("MicroVideoOffset invalid")
		}
		if err := checkBMFF(b[n-v:]); err != nil {
			return nil, err
		}
		parts = append(parts, trailerPart{n - v, n, partVideo, fromMicroVideo})
	}
	return parts, nil
}

// exifOrientation returns the EXIF orientation (1 to 8) of the primary image,
// or 1 (as stored) when there is no single well-formed Exif segment with one
// valid Orientation tag in IFD0. Ambiguous or malformed EXIF is ignored rather
// than rejected: it is never stored, and the pixels decode the same way.
func exifOrientation(segments [][]byte) int {
	if len(segments) != 1 {
		return 1
	}
	t, ok := newTIFF(segments[0])
	if !ok {
		return 1
	}
	entries, ok := t.ifd0()
	if !ok {
		return 1
	}
	o := 0
	for _, e := range entries {
		if e.tag != 0x0112 {
			continue
		}
		if o != 0 || e.typ != 3 || e.count != 1 {
			return 1
		}
		o = int(t.order.Uint16(e.raw))
		if o < 1 || o > 8 {
			return 1
		}
	}
	if o == 0 {
		return 1
	}
	return o
}
