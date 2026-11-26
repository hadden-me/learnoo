package reader

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hadden-me/learnoo/internal/rendering"
)

// Fingerprinted public assets (issue #88). The reader's generic styles and
// scripts and the vendored KaTeX contain no user or lesson data, so they are
// served at content-addressed URLs that any cache may keep:
//
//	/assets/<version>/<name>
//
// where <name> is the asset's legacy /static name (site.css,
// vendor/katex/katex.min.css…) and <version> is the first 16 hex digits of
// a SHA-256: of the file itself, or, for KaTeX, of the whole vendor/katex
// directory, so that the fonts katex.min.css loads by relative URL resolve
// under the same version. The manifest is computed from the embedded files
// when the server starts. Only the current version of each name is served;
// any other version (an older build, a typo) gets 404, never a redirect, and
// nothing under /assets/ reads the session or sets a cookie.
//
// The unversioned /static/ paths keep working, with private, no-store, for
// anything that still names them (the renderer experiment's page, the Studio
// development server).

// AssetPrefix starts every fingerprinted asset URL. It is a fixed prefix,
// distinct from /static/, so that a CDN rule can select these paths with a
// plain "starts with" match (docs/DEPLOYMENT.md).
const AssetPrefix = "/assets/"

// AssetCacheControl is sent on fingerprinted assets: any cache may keep them
// for a year without revalidation, since their URL changes with their
// content.
const AssetCacheControl = "public, max-age=31536000, immutable"

// katexDir is the vendored KaTeX directory (scripts/copy-katex.mjs), one
// versioned group.
const katexDir = "vendor/katex/"

// publicRendering lists the generic rendering styles and scripts; with
// reader.css and the KaTeX directory they are the public assets.
var publicRendering = map[string]bool{"tokens.css": true, "site.css": true, "math.js": true}

// assetTypes are the content types of the public asset kinds; other files
// (KaTeX's LICENSE) get no fingerprinted URL.
var assetTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".woff2": "font/woff2",
	".woff":  "font/woff",
	".ttf":   "font/ttf",
}

// versionPattern matches the <version> segment.
var versionPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// isPublicAsset reports whether name is a public asset: served without a
// session under /static/ and, when present, fingerprinted under /assets/.
func isPublicAsset(name string) bool {
	return name == "reader.css" || publicRendering[name] || strings.HasPrefix(name, katexDir)
}

type asset struct {
	version string
	etag    string
	ctype   string
	data    []byte
}

// manifest maps public asset names to their current version and content.
type manifest map[string]asset

// staticFS returns the two embedded static directories.
func staticFS() (readerFS, renderingFS fs.FS) {
	readerFS, _ = fs.Sub(files, "static")
	renderingFS, _ = fs.Sub(rendering.Files, "static")
	return readerFS, renderingFS
}

// embeddedAssets is the manifest of the embedded files, built once.
var embeddedAssets = sync.OnceValues(func() (manifest, error) {
	readerFS, renderingFS := staticFS()
	return buildManifest(readerFS, renderingFS)
})

// buildManifest fingerprints reader.css from readerFS and the generic
// rendering assets and KaTeX directory from renderingFS. reader.css and the
// rendering files are required; the KaTeX directory is absent from a
// checkout where scripts/copy-katex.mjs has not run (Go tests), and then
// simply has no fingerprinted URLs.
func buildManifest(readerFS, renderingFS fs.FS) (manifest, error) {
	m := manifest{}
	single := func(fsys fs.FS, name string) error {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("asset %s: %w", name, err)
		}
		return m.add(name, digest(data)[:16], data)
	}
	if err := single(readerFS, "reader.css"); err != nil {
		return nil, err
	}
	for _, name := range slices.Sorted(maps.Keys(publicRendering)) {
		if err := single(renderingFS, name); err != nil {
			return nil, err
		}
	}

	// KaTeX: every served file shares the digest of the directory, computed
	// over the sorted names, sizes and contents of those files.
	var names []string
	err := fs.WalkDir(renderingFS, strings.TrimSuffix(katexDir, "/"), func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() && assetTypes[path.Ext(name)] != "" {
			names = append(names, name)
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, fmt.Errorf("asset %s: %w", katexDir, err)
	}
	slices.Sort(names)
	group := sha256.New()
	contents := make([][]byte, len(names))
	for i, name := range names {
		data, err := fs.ReadFile(renderingFS, name)
		if err != nil {
			return nil, fmt.Errorf("asset %s: %w", name, err)
		}
		contents[i] = data
		fmt.Fprintf(group, "%s\x00%d\x00", name, len(data))
		group.Write(data)
	}
	version := hex.EncodeToString(group.Sum(nil))[:16]
	for i, name := range names {
		if err := m.add(name, version, contents[i]); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m manifest) add(name, version string, data []byte) error {
	ctype := assetTypes[path.Ext(name)]
	if ctype == "" || !isPublicAsset(name) {
		return fmt.Errorf("asset %s: not a public asset type", name)
	}
	m[name] = asset{version: version, etag: `"` + digest(data) + `"`, ctype: ctype, data: data}
	return nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// url returns the fingerprinted URL of a public asset, or its unversioned
// /static/ URL when the file is missing from the build (KaTeX not copied);
// that URL answers 404 too, as before. A name that is not a public asset is
// a template defect and returns an error.
func (m manifest) url(name string) (string, error) {
	if a, ok := m[name]; ok {
		return AssetPrefix + a.version + "/" + name, nil
	}
	if isPublicAsset(name) {
		return "/static/" + name, nil
	}
	return "", fmt.Errorf("%q is not a public asset", name)
}

// AssetURL returns the URL pages use for the public asset name (for example
// "vendor/katex/katex.min.js"): fingerprinted under /assets/ when the file is
// in the build.
func (h *Handlers) AssetURL(name string) (string, error) { return h.assets.url(name) }

// serveAsset answers GET and HEAD /assets/{version}/{path...}. It is mounted
// without the session middleware: the response is the same for everyone, so
// it does not vary on the cookie and sets none. Unknown names and versions
// other than the current one get the server's JSON 404 with its no-store
// header.
func (h *Handlers) serveAsset(w http.ResponseWriter, r *http.Request) {
	a, ok := h.assets[r.PathValue("path")]
	if !ok || !versionPattern.MatchString(r.PathValue("version")) || r.PathValue("version") != a.version {
		staticNotFound(w)
		return
	}
	hdr := w.Header()
	hdr.Del("Vary")
	hdr.Set("Cache-Control", AssetCacheControl)
	hdr.Set("Content-Type", a.ctype)
	hdr.Set("ETag", a.etag)
	// A zero time sends no Last-Modified; ServeContent answers
	// If-None-Match with 304 from the ETag.
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(a.data))
}
