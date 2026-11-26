package media

import (
	"encoding/binary"
	"image"
	"image/color"
)

// orientedSize is the upright size of a w x h image with EXIF orientation o.
func orientedSize(w, h, o int) (int, int) {
	if o >= 5 && o <= 8 {
		return h, w
	}
	return w, h
}

// orient returns img transformed so that it displays upright for EXIF
// orientation o (1 to 8); for 1 or any other value it returns img unchanged.
// The result is a new image of the same pixel count: *image.Gray for
// grayscale input, *image.RGBA otherwise. The JPEG decoder's output types
// (*image.YCbCr, *image.Gray, and *image.RGBA for JPEGs stored as RGB) are
// read directly from their pixel buffers, without per-pixel allocations.
// (Four-component JPEGs are rejected before decoding; see jpegCost.)
func orient(img image.Image, o int) image.Image {
	if o < 2 || o > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := orientedSize(w, h, o)
	// to maps source pixel (x, y), relative to b.Min, to its upright position.
	to := func(x, y int) (int, int) {
		switch o {
		case 2: // mirrored horizontally
			return w - 1 - x, y
		case 3: // rotated 180
			return w - 1 - x, h - 1 - y
		case 4: // mirrored vertically
			return x, h - 1 - y
		case 5: // transposed
			return y, x
		case 6: // rotate 90 clockwise to display
			return h - 1 - y, x
		case 7: // transversed
			return h - 1 - y, w - 1 - x
		default: // 8: rotate 90 counter-clockwise to display
			return y, w - 1 - x
		}
	}
	switch src := img.(type) {
	case *image.Gray:
		dst := image.NewGray(image.Rect(0, 0, dw, dh))
		for y := 0; y < h; y++ {
			row := src.Pix[src.PixOffset(b.Min.X, b.Min.Y+y):]
			for x := 0; x < w; x++ {
				dx, dy := to(x, y)
				dst.Pix[dy*dst.Stride+dx] = row[x]
			}
		}
		return dst
	case *image.YCbCr:
		dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				yi := src.YOffset(b.Min.X+x, b.Min.Y+y)
				ci := src.COffset(b.Min.X+x, b.Min.Y+y)
				r, g, bl := color.YCbCrToRGB(src.Y[yi], src.Cb[ci], src.Cr[ci])
				dx, dy := to(x, y)
				i := dst.PixOffset(dx, dy)
				dst.Pix[i], dst.Pix[i+1], dst.Pix[i+2], dst.Pix[i+3] = r, g, bl, 0xFF
			}
		}
		return dst
	case *image.RGBA: // JPEGs stored as RGB
		dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
		for y := 0; y < h; y++ {
			row := src.Pix[src.PixOffset(b.Min.X, b.Min.Y+y):]
			for x := 0; x < w; x++ {
				dx, dy := to(x, y)
				copy(dst.Pix[dst.PixOffset(dx, dy):][:4], row[4*x:4*x+4])
			}
		}
		return dst
	default: // not produced by the JPEG decoder; slow but correct
		dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				dx, dy := to(x, y)
				dst.Set(dx, dy, img.At(b.Min.X+x, b.Min.Y+y))
			}
		}
		return dst
	}
}

// isHEIF reports an ISO BMFF file whose 'ftyp' box names a HEIC/HEIF brand
// (iPhone photos by default). AVIF, which shares the container, is not
// reported.
func isHEIF(b []byte) bool {
	if len(b) < 16 || string(b[4:8]) != "ftyp" {
		return false
	}
	size := int(binary.BigEndian.Uint32(b))
	if size < 16 || size > len(b) || size > 1024 {
		size = min(len(b), 1024)
	}
	heif := false
	for i := 8; i+4 <= size; i += 4 {
		if i == 12 { // minor version
			continue
		}
		switch string(b[i : i+4]) {
		case "heic", "heix", "heim", "heis", "hevc", "hevx", "hevm", "hevs", "mif1", "msf1", "mif2":
			heif = true
		case "avif", "avis":
			return false
		}
	}
	return heif
}
