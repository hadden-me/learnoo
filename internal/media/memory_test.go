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
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sync"
	"testing"
	"time"
)

type worstCase struct {
	name, file string
	data       []byte
}

// phoneJPEG is a w x h baseline 4:2:0 JPEG from Go's encoder (as phones
// write them) with EXIF orientation 6 and an MPF Ultra HDR gain map.
func phoneJPEG(t testing.TB, w, h int) []byte {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := m.PixOffset(x, y)
			m.Pix[i], m.Pix[i+1], m.Pix[i+2], m.Pix[i+3] = uint8(x/16), uint8(y/16), uint8((x+y)/32), 0xFF
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, m, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	prim := insertJPEGSegment(b.Bytes(), 0xE1, orientation(binary.BigEndian, 6))
	return withMPF(prim, binary.BigEndian, [][]byte{gainMap(t)}, nil)
}

func png8(t testing.TB, w, h int) []byte {
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m.SetNRGBA(x, y, color.NRGBA{uint8(x / 16), uint8(y / 16), 0x80, 0xFF})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// worstCases are the measured inputs: the most expensive variants at the
// largest accepted size, and ordinary phone photos and screenshots.
func worstCases(t testing.TB) []worstCase {
	const n = 4000 // 4000 x 4000 = MaxPixels
	rot6 := [][]byte{app1(orientation(binary.BigEndian, 6))}
	spec := func(comps []jpegComp, progressive bool, adobe int, segs [][]byte) []byte {
		return encodeSpec(jpegSpec{w: n, h: n, comps: comps, progressive: progressive, jfif: adobe < 0, adobe: adobe, segments: segs})
	}
	return []worstCase{
		{"JPEG 16 MP progressive CMYK", "prog-cmyk.jpg", spec(cmyk, true, 0, nil)},
		{"JPEG 16 MP progressive YCCK", "prog-ycck.jpg", spec(cmyk, true, 2, nil)},
		{"JPEG 16 MP baseline CMYK", "base-cmyk.jpg", spec(cmyk, false, 0, nil)},
		{"JPEG 16 MP progressive RGB, orientation 6", "prog-rgb-rot6.jpg", spec(rgb444, true, 0, rot6)},
		{"JPEG 16 MP progressive 4:4:4, orientation 6", "prog-444-rot6.jpg", spec(ycc444, true, -1, rot6)},
		{"JPEG 16 MP progressive 4:4:4", "prog-444.jpg", spec(ycc444, true, -1, nil)},
		{"JPEG 16 MP progressive 4:2:0, orientation 6", "prog-420-rot6.jpg", spec(ycc420, true, -1, rot6)},
		{"JPEG 16 MP baseline 4:4:4, orientation 6", "base-444-rot6.jpg", spec(ycc444, false, -1, rot6)},
		{"JPEG 16 MP progressive grayscale, orientation 6", "prog-gray-rot6.jpg", spec(gray, true, -1, rot6)},
		{"Phone JPEG 12 MP baseline 4:2:0, orientation 6, Ultra HDR", "phone-12mp.jpg", phoneJPEG(t, 4000, 3000)},
		{"Phone JPEG 16 MP baseline 4:2:0, orientation 6, Ultra HDR", "phone-16mp.jpg", phoneJPEG(t, n, n)},
		{"PNG 16 MP 8-bit RGBA", "png8.png", png8(t, n, n)},
		{"PNG 16 MP 16-bit RGBA", "png16.png", png16(t, n, n)},
		{"PNG 16 MP 16-bit RGBA, Adam7 interlaced", "png16-adam7.png", adam7PNG16(n, n)},
		{"WebP lossless 16 MP", "lossless.webp", webpLossless(n, n, false)},
		{"WebP lossless 16 MP, 16-colour palette", "lossless-palette.webp", webpLossless(n, n, true)},
		{"WebP lossy 16 MP", "lossy.webp", webpLossy(n, n)},
		{"WebP lossless 256 x 256, 2600 Huffman groups", "groups-256.webp", webpManyGroups(256, 256, false)},
		{"WebP lossless 2400 x 2400, 2600 Huffman groups", "groups-6mp.webp", webpManyGroups(2400, 2400, false)},
		{"WebP lossless 2400 x 2400, 2600 Huffman groups, palette", "groups-6mp-palette.webp", webpManyGroups(2400, 2400, true)},
		{"WebP lossless 4096 x 3906, 2600 Huffman groups", "groups-16mp.webp", webpManyGroups(4096, 3906, false)},
		{"WebP lossless 4096 x 3906, 2600 Huffman groups, palette", "groups-16mp-palette.webp", webpManyGroups(4096, 3906, true)},
		{"WebP lossy 256 x 256, alpha with 2600 Huffman groups", "alpha-groups-256.webp", webpAlphaManyGroups(256, 256)},
		{"WebP lossy 2048 x 2048, alpha with 2600 Huffman groups", "alpha-groups-4mp.webp", webpAlphaManyGroups(2048, 2048)},
	}
}

// heapPeak runs f while sampling the heap and returns the largest amount of
// heap object memory (live plus not yet swept) above the starting point, and
// the bytes allocated by f. With a small GOGC the sample approximates the
// live heap; with the default it shows what the collector lets accumulate.
func heapPeak(gcPercent int, f func()) (peak, allocated uint64) {
	samples := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}, {Name: "/gc/heap/allocs:bytes"}}
	read := func() (uint64, uint64) {
		metrics.Read(samples)
		return samples[0].Value.Uint64(), samples[1].Value.Uint64()
	}
	old := debug.SetGCPercent(gcPercent)
	defer debug.SetGCPercent(old)
	runtime.GC()
	base, alloc0 := read()
	var max uint64
	var mu sync.Mutex
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			v, _ := read()
			mu.Lock()
			if v > max {
				max = v
			}
			mu.Unlock()
			select {
			case <-done:
				return
			default:
				time.Sleep(200 * time.Microsecond)
			}
		}
	}()
	f()
	close(done)
	<-stopped
	_, alloc1 := read()
	if max < base {
		return 0, alloc1 - alloc0
	}
	return max - base, alloc1 - alloc0
}

const mib = 1 << 20

// TestDecodeMemoryReport prints the per-Process heap figures of
// docs/measurements/upload-memory.md (go test -run DecodeMemoryReport -v
// with LEARNOO_MEMORY_REPORT=1).
func TestDecodeMemoryReport(t *testing.T) {
	if os.Getenv("LEARNOO_MEMORY_REPORT") == "" {
		t.Skip("set LEARNOO_MEMORY_REPORT=1 to print the measurements")
	}
	t.Logf("%s %s/%s, GOMAXPROCS %d", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.GOMAXPROCS(0))
	t.Logf("| Input | Upload | Estimate | Decode+orient live (GOGC=1) | Process result | Process live (GOGC=1) | Process heap (GOGC=100) | Process allocated |")
	for _, c := range worstCases(t) {
		est, err := estimate(t, c.data)
		estimated, decoded := "-", "-"
		if err == nil {
			estimated = fmt.Sprintf("%.0f MiB", float64(est)/mib)
			f, _ := sniff(c.data)
			ins, _ := f.inspect(c.data)
			d, _ := heapPeak(1, func() {
				img, err := f.decode(bytes.NewReader(c.data[:ins.end]))
				if err == nil {
					img = orient(img, ins.orientation)
				}
				runtime.KeepAlive(img)
			})
			decoded = fmt.Sprintf("%.0f MiB", float64(d)/mib)
		}
		res := "accepted"
		live, alloc := heapPeak(1, func() { _, err = Process(context.Background(), bytes.NewReader(c.data), "") })
		if err != nil {
			res = err.Error()
		}
		heap, _ := heapPeak(100, func() { _, _ = Process(context.Background(), bytes.NewReader(c.data), "") })
		t.Logf("| %s | %.2f MiB | %s | %s | %s | %.0f MiB | %.0f MiB | %.0f MiB |", c.name, float64(len(c.data))/mib, estimated, decoded, res,
			float64(live)/mib, float64(heap)/mib, float64(alloc)/mib)
	}
}

// TestWriteWorstCaseInputs writes the inputs for the whole-process
// measurement (scripts/upload-memory) to $LEARNOO_WORST_CASE_DIR.
func TestWriteWorstCaseInputs(t *testing.T) {
	dir := os.Getenv("LEARNOO_WORST_CASE_DIR")
	if dir == "" {
		t.Skip("set LEARNOO_WORST_CASE_DIR to write the inputs")
	}
	for _, c := range worstCases(t) {
		if err := os.WriteFile(filepath.Join(dir, c.file), c.data, 0o644); err != nil {
			t.Fatal(err)
		}
		fmt.Println(c.file, len(c.data))
	}
}

// estimate runs the checks Process makes before decoding and returns the
// decode cost estimate.
func estimate(t *testing.T, data []byte) (int64, error) {
	t.Helper()
	f, ok := sniff(data)
	if !ok {
		t.Fatal("unknown format")
	}
	ins, err := f.inspect(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := f.decodeConfig(bytes.NewReader(data[:ins.end]))
	if err != nil {
		t.Fatal(err)
	}
	return decodeCost(f, data[:ins.end], cfg, ins)
}

// The decode cost estimate is an upper bound of the measured heap of the
// decode and orientation steps, for every worst-case input (also those that
// Process rejects for their cost), and every accepted input stays within
// MaxDecodeBytes plus the bounded re-encoding buffer through all of Process.
// About 20 seconds; skipped with -short and -race.
func TestDecodeMemoryWithinEstimate(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("heap measurement: not with -short or -race")
	}
	// A bytes.Buffer that ends at MaxEncodedBytes may briefly hold its old and
	// new backing arrays.
	const processBound = MaxDecodeBytes + 3*MaxEncodedBytes
	const slack = 4 << 20 // row buffers, decoder state, sampling noise
	for _, c := range worstCases(t) {
		est, err := estimate(t, c.data)
		if errors.Is(err, ErrUnsupportedCMYK) {
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		f, _ := sniff(c.data)
		ins, _ := f.inspect(c.data)
		var derr error
		decoded, _ := heapPeak(1, func() {
			var img image.Image
			img, derr = f.decode(bytes.NewReader(c.data[:ins.end]))
			if derr == nil {
				img = orient(img, ins.orientation)
			}
			runtime.KeepAlive(img)
		})
		if derr != nil {
			t.Fatalf("%s: decode: %v", c.name, derr)
		}
		if int64(decoded) > est+slack {
			t.Errorf("%s: decode and orientation used %d MiB, estimated %d MiB", c.name, decoded>>20, est>>20)
		}
		if est > MaxDecodeBytes {
			continue
		}
		var perr error
		processed, _ := heapPeak(1, func() { _, perr = Process(context.Background(), bytes.NewReader(c.data), "") })
		if perr != nil {
			t.Errorf("%s: estimated %d MiB but rejected: %v", c.name, est>>20, perr)
		}
		if processed > processBound+slack {
			t.Errorf("%s: Process used %d MiB", c.name, processed>>20)
		}
	}
}
