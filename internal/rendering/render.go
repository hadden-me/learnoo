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
	VideoID  string `json:"videoId,omitempty"`
	Children []Node `json:"children,omitempty"`
}

var youtubeID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
var page = template.Must(template.ParseFS(Files, "page.html", "lesson.html"))

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
			if len(n.Text) > 8000 || len(n.Title) > 240 || len(n.Alt) > 1000 {
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
			if n.Src != "" && n.Type != "image" || n.Alt != "" && n.Type != "image" || n.VideoID != "" && n.Type != "youtube" {
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
				if n.Src != "/static/complex-plane.svg" || n.Alt == "" || len(n.Children) > 0 || n.Text != "" {
					return errors.New("image must reference an approved local fixture asset with alt text")
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

func Render(doc Document) ([]byte, error) {
	if err := Validate(doc); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	err := page.ExecuteTemplate(&out, "page", doc)
	return out.Bytes(), err
}

// RenderBlocks validates doc and renders only its blocks: the canonical
// lesson body that Render places inside the page's article element. The
// authenticated reader (internal/reader) wraps it in its own page, so reader
// and preview share one node renderer.
func RenderBlocks(doc Document) (template.HTML, error) {
	if err := Validate(doc); err != nil {
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
// direction come from the document's language.
func RenderLesson(doc Document) (LessonParts, error) {
	if err := Validate(doc); err != nil {
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
