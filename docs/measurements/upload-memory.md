# Upload memory measurements

Basis for the image-upload memory budget recorded in [DECISIONS.md](../DECISIONS.md#media-upload-limits-lrn-006) ("Limits", resolution of 2026-11-26) and for the app memory guidance in [DEPLOYMENT.md](../DEPLOYMENT.md#app-memory). Measured on 2026-11-26 on a development machine, not on the shared server.

## Conditions

- Apple M1 Pro (10 cores), 32 GiB, macOS; Go 1.25.4 (`darwin/arm64` for the per-call and host runs, `linux/arm64` with `CGO_ENABLED=0` for the container runs).
- Containers: Docker Desktop 27.3.1 (Linux VM with 8 GiB), image `gcr.io/distroless/static-debian12:nonroot` (the runtime base of the `Dockerfile`, same digest) with the server binary; `--memory 512m --memory-swap 512m --cpus 1 --pids-limit 200 --read-only --cap-drop ALL`, matching the app service of `deploy/compose.production.yaml` with the EXAMPLE values. PostgreSQL 16.13 on the host (a disposable schema). The cgroup was read from a `busybox:1.36.1` helper container.
- "Before" is `origin/develop` at 8be9f4b (2 concurrent decodes, CMYK accepted, no decode estimate, no `GOMEMLIMIT`); "after" is this change.
- `GOGC` unset (100) unless stated.

## Inputs

All generated in Go by `internal/media/worstcase_test.go` and `memory_test.go` (no downloaded files); `LEARNOO_WORST_CASE_DIR=dir go test -run WriteWorstCaseInputs ./internal/media` writes them. Decoding memory depends on the headers (dimensions, components, sampling, progressive, bit depth, interlacing), not on pixel values, so the JPEGs carry a simple coefficient pattern and stay well under 5 MiB.

| File | Image | Upload size |
| --- | --- | --- |
| `prog-cmyk.jpg`, `prog-ycck.jpg`, `base-cmyk.jpg` | 4000 × 4000 four-component JPEG (Adobe transform 0 or 2), progressive or baseline | 3.4 MiB |
| `prog-rgb-rot6.jpg` | 4000 × 4000 progressive JPEG stored as RGB (Adobe transform 0), EXIF orientation 6 | 2.5 MiB |
| `prog-444-rot6.jpg`, `prog-444.jpg` | 4000 × 4000 progressive YCbCr 4:4:4, with and without orientation 6 | 2.5 MiB |
| `prog-420-rot6.jpg` | 4000 × 4000 progressive 4:2:0, orientation 6 | 1.2 MiB |
| `base-444-rot6.jpg` | 4000 × 4000 baseline 4:4:4, orientation 6 | 2.5 MiB |
| `prog-gray-rot6.jpg` | 4000 × 4000 progressive grayscale, orientation 6 | 0.8 MiB |
| `phone-12mp.jpg`, `phone-16mp.jpg` | 4000 × 3000 and 4000 × 4000 baseline 4:2:0 from Go's encoder (as phones write them), orientation 6, MPF Ultra HDR gain map after the primary | 0.2–0.3 MiB |
| `png8.png`, `png16.png` | 4000 × 4000 RGBA, 8 and 16 bits per sample | 0.1 / 1.1 MiB |
| `png16-adam7.png` | 4000 × 4000 RGBA 16-bit, Adam7 interlaced | 0.3 MiB |
| `lossless.webp`, `lossless-palette.webp` | 4000 × 4000 VP8L, without transforms and with a 16-colour colour-indexing transform (the transform for which Go's decoder allocates a second full-size buffer) | < 100 B |
| `lossy.webp` | 4000 × 4000 lossy key frame with zero-filled partitions (decoded as a flat image) | 0.1 MiB |
| `groups-*.webp` | Adversarial lossless WebP (review finding of 2026-11-26): 2600 Huffman groups (the most Go's decoder accepts), each referenced by a tile of the meta image so all are built, five complete trees per group and an 11-bit colour cache; 256 × 256, 2400 × 2400 and 4096 × 3906, with and without the palette transform | 0.6 MiB |
| `alpha-groups-*.webp` | Lossy frame (as above) with that stream as lossless-compressed alpha; 256 × 256 and 2048 × 2048 | 0.7 MiB |

## One `Process` call

`LEARNOO_MEMORY_REPORT=1 go test -run DecodeMemoryReport -v ./internal/media`. "Live" samples the heap's object bytes every 0.2 ms with `GOGC=1`, which approximates the live heap; "heap" samples the same at `GOGC=100` (what the collector lets accumulate without a memory limit); "allocated" is all bytes allocated by the call. MiB above the starting heap.

Before:

| Input | Result | Live | Heap | Allocated |
| --- | --- | --- | --- | --- |
| progressive CMYK / YCCK | accepted | 375 | 433 | 449 |
| baseline CMYK | accepted | 128 | 189 | 205 |
| progressive RGB, orientation 6 | accepted | 354 | 354 | 427 |
| progressive 4:4:4, orientation 6 | accepted | 293 | 297 | 307 |
| progressive 4:4:4 | accepted | 236 | 236 | 246 |
| progressive 4:2:0, orientation 6 | accepted | 177 | 179 | 184 |
| baseline 4:4:4, orientation 6 | accepted | 109 | 119 | 124 |
| progressive grayscale, orientation 6 | accepted | 92 | 95 | 98 |
| phone 12 MP | accepted | 64 | 65 | 65 |
| phone 16 MP | accepted | 85 | 86 | 86 |
| PNG 8-bit | accepted | 62 | 62 | 62 |
| PNG 16-bit | accepted | 126 | 248 | 253 |
| WebP lossless / with palette | accepted | 62 / 93 | 62 / 93 | 62 / 93 |

After. "Estimate" is `decodeCost` from the headers; "decode + orient" measures the decoder and the orientation step alone (live, `GOGC=1`), also for inputs that `Process` now refuses, to check the estimate:

| Input | Estimate | Decode + orient | Result | Live | Heap | Allocated |
| --- | --- | --- | --- | --- | --- | --- |
| progressive CMYK / YCCK, baseline CMYK | – | – | `415 unsupported_cmyk` | 7 | 20 | 20 |
| progressive RGB, orientation 6 | 351 | 351 | `422 image_too_complex` | 5 | 13 | 13 |
| progressive 4:4:4, orientation 6 | 290 | 290 | `422 image_too_complex` | 5 | 13 | 13 |
| progressive 4:4:4 | 229 | 229 | `422 image_too_complex` | 6 | 13 | 13 |
| progressive 4:2:0, orientation 6 | 175 | 176 | accepted | 177 | 179 | 184 |
| baseline 4:4:4, orientation 6 | 107 | 107 | accepted | 109 | 113 | 124 |
| progressive grayscale, orientation 6 | 92 | 92 | accepted | 92 | 94 | 98 |
| phone 12 MP | 63 | 63 | accepted | 64 | 64 | 65 |
| phone 16 MP | 84 | 84 | accepted | 85 | 85 | 86 |
| PNG 8-bit | 61 | 61 | accepted | 62 | 62 | 62 |
| PNG 16-bit | 122 | 122 | accepted | 126 | 248 | 253 |
| PNG 16-bit Adam7 | 244 | 214 | `422 image_too_complex` | 1 | 1 | 1 |
| WebP lossless 16 MP | 244 | 61 | `422 image_too_complex` | 0 | 0 | 0 |
| WebP lossless 16 MP with palette | 244 | 92 | `422 image_too_complex` | 0 | 0 | 0 |
| WebP lossy 16 MP | 24 | 23 | accepted | 25 | 67 | 74 |
| WebP 2600 groups, 256 × 256 | 142 | 141 | accepted | 142 | 154 | 209 |
| WebP 2600 groups, 2400 × 2400 | 178 | 162 | accepted | 164 | 176 | 231 |
| WebP 2600 groups, 2400 × 2400, palette | 178 | 173 | accepted | 174 | 187 | 242 |
| WebP 2600 groups, 4096 × 3906 | 244 | 201 | `422 image_too_complex` | 2 | 3 | 3 |
| WebP 2600 groups, 4096 × 3906, palette | 244 | 232 | `422 image_too_complex` | 2 | 3 | 3 |
| WebP lossy, alpha with 2600 groups, 256 × 256 | 142 | 141 | accepted | 142 | 157 | 211 |
| WebP lossy, alpha with 2600 groups, 2048 × 2048 | 178 | 160 | accepted | 161 | 245 | 300 |

Before the WebP correction the estimate was 9 bytes per pixel: 0.6 MiB for the 256 × 256 group file (measured 141 MiB) and 137 MiB for the accepted 4096 × 3906 one (measured 201 MiB, 232 MiB with the palette), so `TestDecodeMemoryWithinEstimate` fails against it.

Notes:

- The estimate is the sum of the large allocations, so it matches the measured decode within about 1 MiB where the model is exact (JPEG, non-interlaced PNG) and is above it elsewhere. The exception is the WebP Huffman tables: their per-tree code-length buffers are released before the next tree and are counted once, so "allocated" exceeds the estimate for the group files (up to 300 MiB) while the live heap stays below it; that garbage is short-lived small objects, which the collector reclaims under `GOMEMLIMIT`. `TestDecodeMemoryWithinEstimate` asserts estimate + 4 MiB ≥ measured for every input and that every accepted input stays within `MaxDecodeBytes` + 3 × `MaxEncodedBytes` through `Process` (about 20 s; skipped with `-short` and `-race`).
- The 16-bit PNG "heap" figure is short-lived garbage: Go's PNG encoder boxes every 16-bit pixel. Its live heap is 126 MiB, but without a memory limit the collector lets the heap reach twice that; this is one reason `GOMEMLIMIT` is set.
- The progressive JPEG coefficients (one `int32` per sample, for every component, for the whole decode) are what makes progressive 4:4:4 and RGB expensive; baseline JPEGs of the same size need a fifth of it.

## Whole server

`scripts/upload-memory` migrates a disposable schema, creates a throwaway author with a random password, keeps 4 uploads (`MaxConcurrentUploads`) in flight in a loop over the inputs and runs 2 readers fetching stored media and `/healthz`. "Worst" is the four most expensive inputs (progressive CMYK, YCCK, RGB with rotation, 4:4:4 with rotation); "accepted" the four most expensive inputs the change still accepts (progressive 4:2:0 with rotation, 16-bit PNG, baseline 4:4:4 with rotation, phone 16 MP); "phone" the phone photos and the 8-bit PNG.

### In a 512 MiB container

A helper container reads the app container's cgroup every 0.1 s: the largest `anon` value of `memory.stat` (the process's own memory) and the last `memory.peak` (largest `memory.current`, which also counts the page cache of written media and is what the limit enforces). Docker's `OOMKilled` flag is the decisive result. 45 s per run (60 s for the "after" runs with accepted inputs).

| Server | Inputs | `GOMEMLIMIT` | Peak anon | `memory.peak` | Result |
| --- | --- | --- | --- | --- | --- |
| before | worst | unset | 485 MiB | 512 MiB | OOM-killed after 0.7 s |
| before | worst | 384MiB | ≥ 467 MiB | ≥ 471 MiB | OOM-killed after 0.7 s |
| before | accepted | unset | 508 MiB | 512 MiB | OOM-killed after 3.3 s (3 uploads done) |
| before | phone | unset | 431 MiB | 446 MiB | survived, 125 uploads |
| after | worst | derived (384 MiB) | 36 MiB | 45 MiB | survived, 2 324 uploads refused (415/422) |
| after | accepted | derived (384 MiB) | 383 MiB | 423 MiB | survived, 67 uploads |
| after | accepted | `off` | 435 MiB | 477 MiB | survived, 69 uploads |
| after | all 16 inputs | derived (384 MiB) | 384 MiB | 425 MiB | survived, 142 uploads (82 stored, 60 refused) |
| after | phone | derived (384 MiB) | 233 MiB | 263 MiB | survived, 102 uploads |
| after | accepted, 180 s | derived (384 MiB) | 388 MiB | 436 MiB | survived, 195 uploads |
| after WebP fix | accepted incl. WebP group files (2400², 2048² alpha, 256²), progressive 4:2:0, 16-bit PNG, 120 s | derived (384 MiB) | 382 MiB | 416 MiB | survived, 150 uploads |
| after WebP fix | all 24 inputs, 60 s | derived (384 MiB) | 382 MiB | 438 MiB | survived, 165 uploads (106 stored, 59 refused) |
| after WebP fix | refused WebP (16 MP lossless, group files), 30 s | derived (384 MiB) | 43 MiB | 45 MiB | survived, 13 092 refused |

With the derived limit the process grows to its `GOMEMLIMIT` (anon about 384 MiB) and the collector then keeps it there: that is the limit working, not the live heap, which stayed at most 131 MiB after collections in the host runs. Without a limit (`off`) the same load reached 435 MiB of 512 MiB, too close to rely on.

### On the host (macOS)

Process RSS sampled with `ps` every 50 ms, and the Go heap from `GODEBUG=gctrace=1` (largest heap when a collection started, largest heap left live after one), 30 s per run. macOS RSS keeps pages the Go runtime has already returned to the system, so it overstates use and never falls; the gctrace and container figures are the ones to compare with the limits.

| Server | Inputs | `GOMEMLIMIT` | RSS peak | Heap at GC start | Live after GC | Uploads |
| --- | --- | --- | --- | --- | --- | --- |
| before | worst | unset | 1624 MiB | 1428 MiB | 689 MiB | 86 × 201 |
| before | worst | 384MiB | 925 MiB | 732 MiB | 732 MiB | 57 × 201 (collector thrashing: live heap above the limit) |
| after | worst | unset | 135 MiB | 89 MiB | 52 MiB | 14 244 refused (415/422) |
| after | worst | 384MiB | 152 MiB | 105 MiB | 61 MiB | 19 149 refused |
| after | accepted | unset | 524 MiB | 333 MiB | 131 MiB | 48 × 201 |
| after | accepted | 384MiB | 503 MiB | 327 MiB | 131 MiB | 47 × 201 |
| after WebP fix | accepted incl. WebP group files | 384MiB | 611 MiB | 332 MiB | 157 MiB | 52 × 201 |

## Budget

Computed, with one decode at a time: decode (at most `MaxDecodeBytes` = 192 MiB, by the estimate) + its re-encoded output (at most 3 × 16 MiB while a `bytes.Buffer` grows to `MaxEncodedBytes` and the exact-size copy is made) + its upload body (5 MiB, about twice that while `io.ReadAll` grows) + the three other upload slots, each holding a body or a finished asset (at most 16 MiB each; `Store.Put` compares duplicates in 32 KiB chunks) + idle heap (about 20 MiB at start) ≈ 320 MiB of live heap in the worst case, against a `GOMEMLIMIT` of 384 MiB (three quarters of 512 MiB). The remaining quarter of the container covers the binary, page cache of written media and memory outside the Go heap. Measured: the largest live heap left after a collection in the server runs was 157 MiB, the largest heap at the start of a collection 333 MiB, the largest container `anon` 388 MiB and `memory.peak` 438 MiB, with no OOM kill after the change.

## Reproducing

```sh
DIR=$(mktemp -d)
LEARNOO_WORST_CASE_DIR=$DIR go test -count=1 -run WriteWorstCaseInputs ./internal/media
go build -o /tmp/learnoo-server ./cmd/server
psql "$TEST_DB" -c 'CREATE SCHEMA memmeasure'
go run ./scripts/upload-memory -server /tmp/learnoo-server -inputs $DIR \
  -db "$TEST_DB&search_path=memmeasure" -duration 30s -gctrace -stderr /tmp/gc.txt [-gomemlimit 384MiB] [-only a.jpg,b.png]
# In a container: run the image with --memory 512m --memory-swap 512m --cpus 1, the
# same database and LEARNOO_ADDR=0.0.0.0:8090 LEARNOO_ALLOW_NON_LOOPBACK=1, then
# add -url http://127.0.0.1:<port> -container <name>.
psql "$TEST_DB" -c 'DROP SCHEMA memmeasure CASCADE'
```
