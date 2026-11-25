// Package rendering implements the restricted, experimental lesson format.
package rendering

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"regexp"
	"slices"
	"strconv"

	"github.com/hadden-me/learnoo/internal/strictjson"
)

//go:embed page.html lesson.html static/* fixtures/*.json
var Files embed.FS

const MaxDocumentBytes = 128 << 10

type Document struct {
	SchemaVersion int    `json:"schemaVersion"`
	Title         string `json:"title"`
	Language      string `json:"language"`
	Status        string `json:"status"`
	Objective     string `json:"objective"`
	Blocks        []Node `json:"blocks"`
}

type Node struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Title    string `json:"title,omitempty"`
	Lang     string `json:"lang,omitempty"`
	Dir      string `json:"dir,omitempty"`
	Src      string `json:"src,omitempty"`
	Alt      string `json:"alt,omitempty"`
	Caption  string `json:"caption,omitempty"`
	VideoID  string `json:"videoId,omitempty"`
	Children []Node `json:"children,omitempty"`
}

var youtubeID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// fixtureImages are the renderer's own local assets an image may name.
var fixtureImages = map[string]bool{"/static/complex-plane.svg": true}

// mediaRef is an embedded reference to an uploaded asset (media_assets.id):
// "media:" and a positive decimal ID without sign or leading zeros that fits
// below 2^53 (at most 15 digits), so the Studio holds it exactly as a
// JavaScript number. Media IDs are database identities; 10^15 is unreachable.
var mediaRef = regexp.MustCompile(`^media:[1-9][0-9]{0,14}$`)

// blankText matches strings made only of white space: the union of Go's
// unicode.IsSpace and JavaScript's String.prototype.trim set, listed by code
// point so that the Studio (lesson.ts BLANK) gives the same verdict.
var blankText = regexp.MustCompile(`^[\t\n\v\f\r \x{85}\x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}]*$`)

// MediaRefPrefix starts the src of an image that embeds an uploaded asset.
const MediaRefPrefix = "media:"

// MediaID returns the uploaded asset ID an image src refers to, if src is a
// well-formed media reference.
func MediaID(src string) (int64, bool) {
	if !mediaRef.MatchString(src) {
		return 0, false
	}
	id, err := strconv.ParseInt(src[len(MediaRefPrefix):], 10, 64)
	return id, err == nil
}

// MediaIDs returns the distinct uploaded asset IDs embedded in doc, sorted.
// It does not validate doc.
func MediaIDs(doc Document) []int64 {
	var ids []int64
	var walk func([]Node)
	walk = func(nodes []Node) {
		for _, n := range nodes {
			if id, ok := MediaID(n.Src); ok && n.Type == "image" {
				ids = append(ids, id)
			}
			walk(n.Children)
		}
	}
	walk(doc.Blocks)
	slices.Sort(ids)
	return slices.Compact(ids)
}

var page = template.Must(template.New("").Funcs(template.FuncMap{
	"fixtureImage": func(src string) bool { return fixtureImages[src] },
}).ParseFS(Files, "page.html", "lesson.html"))

func Decode(r io.Reader) (Document, error) {
	var doc Document
	data, err := io.ReadAll(io.LimitReader(r, MaxDocumentBytes+1))
	if err != nil {
		return doc, err
	}
	if len(data) > MaxDocumentBytes {
		return doc, errors.New("document exceeds 128 KiB")
	}
	if err := strictjson.Unmarshal(data, &doc); err != nil {
		return Document{}, fmt.Errorf("invalid JSON: %w", err)
	}
	return doc, Validate(doc)
}

func Validate(doc Document) error {
	if doc.SchemaVersion != 1 || doc.Status != "unreviewed" {
		return errors.New("prototype accepts only schema 1, unreviewed fixtures")
	}
	if doc.Language != "fr" && doc.Language != "ar" {
		return errors.New("unsupported document language")
	}
	if len(doc.Title) == 0 || len(doc.Title) > 240 || len(doc.Objective) > 1000 {
		return errors.New("invalid title or objective")
	}
	if len(doc.Blocks) == 0 {
		return errors.New("empty document")
	}
	count := 0
	var walk func([]Node, string, int) error
	walk = func(nodes []Node, parent string, depth int) error {
		if depth > 12 {
			return errors.New("content nesting exceeds limit")
		}
		for _, n := range nodes {
			count++
			if count > 500 {
				return errors.New("too many nodes")
			}
			if len(n.Text) > 8000 || len(n.Title) > 240 || len(n.Alt) > 1000 || len(n.Caption) > 1000 {
				return errors.New("node text exceeds limit")
			}
			if n.Lang != "" && n.Lang != "ar" && n.Lang != "fr" {
				return errors.New("unsupported language")
			}
			if n.Dir != "" && n.Dir != "rtl" && n.Dir != "ltr" && n.Dir != "auto" {
				return errors.New("invalid text direction")
			}
			inline := n.Type == "text" || n.Type == "math" || n.Type == "strong"
			inlineParent := parent == "paragraph" || parent == "heading" || parent == "strong"
			if inlineParent != inline {
				return fmt.Errorf("invalid %s inside %s", n.Type, parent)
			}
			if parent == "list" && n.Type != "item" {
				return errors.New("list requires items")
			}
			if n.Type == "item" && parent != "list" {
				return errors.New("item outside list")
			}
			if n.Type == "solution" && parent != "exercise" {
				return errors.New("solution outside exercise")
			}
			if n.Src != "" && n.Type != "image" || n.Alt != "" && n.Type != "image" || n.Caption != "" && n.Type != "image" || n.VideoID != "" && n.Type != "youtube" {
				return errors.New("unexpected media attributes")
			}
			switch n.Type {
			case "text", "math", "displayMath":
				if len(n.Children) > 0 || n.Text == "" {
					return errors.New("invalid leaf node")
				}
				if (n.Type == "math" || n.Type == "displayMath") && len(n.Text) > 2000 {
					return errors.New("formula too long")
				}
			case "paragraph", "heading", "strong", "list", "item", "callout", "exercise", "solution":
				if n.Text != "" || len(n.Children) == 0 {
					return errors.New("container requires children, not text")
				}
			case "image":
				if !fixtureImages[n.Src] && !mediaRef.MatchString(n.Src) || blankText.MatchString(n.Alt) || len(n.Children) > 0 || n.Text != "" {
					return errors.New("image must reference an approved local fixture asset or an uploaded asset (media:<id>) with alt text")
				}
			case "youtube":
				if (n.VideoID != "" && !youtubeID.MatchString(n.VideoID)) || len(n.Children) > 0 || n.Text != "" {
					return errors.New("invalid YouTube reference")
				}
			default:
				return fmt.Errorf("unsupported node type: %s", n.Type)
			}
			if err := walk(n.Children, n.Type, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(doc.Blocks, "document", 0)
}

// MediaURL maps an embedded uploaded asset ID to the URL the viewer of the
// rendered page fetches it from. The URL depends on who views the page (an
// author, a reviewer or a student each have their own access-checked route),
// so the caller supplies it; the rest of the markup does not depend on it.
type MediaURL func(id int64) string

// MediaPrefix returns a MediaURL that appends the decimal ID to prefix, for
// example "/api/media/".
func MediaPrefix(prefix string) MediaURL {
	return func(id int64) string { return prefix + strconv.FormatInt(id, 10) }
}

// ErrNoMediaURL means a document embeds uploaded media but the renderer was
// given no MediaURL (the local renderer experiment has none).
var ErrNoMediaURL = errors.New("document embeds uploaded media, which this renderer cannot show")

// resolveMedia returns a copy of doc whose uploaded-media image sources are
// replaced by their URLs. doc must be valid. The original is not modified.
func resolveMedia(doc Document, media MediaURL) (Document, error) {
	var resolve func([]Node) ([]Node, error)
	resolve = func(nodes []Node) ([]Node, error) {
		if len(nodes) == 0 {
			return nodes, nil
		}
		out := make([]Node, len(nodes))
		for i, n := range nodes {
			if id, ok := MediaID(n.Src); ok && n.Type == "image" {
				if media == nil {
					return nil, ErrNoMediaURL
				}
				n.Src = media(id)
			}
			children, err := resolve(n.Children)
			if err != nil {
				return nil, err
			}
			n.Children = children
			out[i] = n
		}
		return out, nil
	}
	blocks, err := resolve(doc.Blocks)
	if err != nil {
		return Document{}, err
	}
	doc.Blocks = blocks
	return doc, nil
}

// Render renders the local renderer experiment's full page. It has no media
// routes, so a document that embeds uploaded media returns ErrNoMediaURL.
func Render(doc Document) ([]byte, error) {
	if err := Validate(doc); err != nil {
		return nil, err
	}
	doc, err := resolveMedia(doc, nil)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	err = page.ExecuteTemplate(&out, "page", doc)
	return out.Bytes(), err
}

// RenderBlocks validates doc and renders only its blocks: the canonical
// lesson body that Render places inside the page's article element. The
// authenticated reader (internal/reader) wraps it in its own page, so reader
// and preview share one node renderer. Uploaded media is shown through media
// (nil: a document embedding uploaded media returns ErrNoMediaURL).
func RenderBlocks(doc Document, media MediaURL) (template.HTML, error) {
	if err := Validate(doc); err != nil {
		return "", err
	}
	doc, err := resolveMedia(doc, media)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := page.ExecuteTemplate(&out, "blocks", doc); err != nil {
		return "", err
	}
	// The output of html/template is safe HTML by construction.
	return template.HTML(out.String()), nil
}

// Dir returns the base text direction of a lesson language.
func Dir(lang string) string {
	if lang == "ar" {
		return "rtl"
	}
	return "ltr"
}

// LessonParts is the canonical markup of a lesson's heading, objective and
// body, shared by the reader's lesson page and the Studio preview so that
// both show the same HTML for the same document.
type LessonParts struct {
	// Heading is the h1 title.
	Heading template.HTML
	// Objective is the objective paragraph, empty without an objective.
	Objective template.HTML
	// Body is the article element wrapping RenderBlocks' output.
	Body template.HTML
}

// HTML concatenates the parts in reading order: the Studio preview fragment.
func (p LessonParts) HTML() template.HTML { return p.Heading + p.Objective + p.Body }

// RenderLesson validates doc and renders its LessonParts. Language and
// direction come from the document's language. Uploaded media is shown
// through media, the viewer's media route (nil: a document embedding
// uploaded media returns ErrNoMediaURL); documents without uploaded media
// render identically whatever media is.
func RenderLesson(doc Document, media MediaURL) (LessonParts, error) {
	if err := Validate(doc); err != nil {
		return LessonParts{}, err
	}
	doc, err := resolveMedia(doc, media)
	if err != nil {
		return LessonParts{}, err
	}
	data := struct {
		Document
		Dir string
	}{doc, Dir(doc.Language)}
	var parts LessonParts
	for _, p := range []struct {
		name string
		out  *template.HTML
	}{{"lesson-heading", &parts.Heading}, {"lesson-objective", &parts.Objective}, {"lesson-body", &parts.Body}} {
		var buf bytes.Buffer
		if err := page.ExecuteTemplate(&buf, p.name, data); err != nil {
			return LessonParts{}, err
		}
		// The output of html/template is safe HTML by construction.
		*p.out = template.HTML(buf.String())
	}
	return parts, nil
}

func Fixture(name string) (Document, error) {
	data, err := Files.ReadFile("fixtures/" + name + ".json")
	if err != nil {
		return Document{}, err
	}
	return Decode(bytes.NewReader(data))
}
