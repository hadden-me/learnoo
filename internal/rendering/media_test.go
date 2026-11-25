package rendering

import (
	"errors"
	"strings"
	"testing"
)

func mediaDoc(blocks ...Node) Document {
	return Document{SchemaVersion: 1, Status: "unreviewed", Language: "fr", Title: "TEST", Blocks: blocks}
}

func TestMediaReferences(t *testing.T) {
	for src, want := range map[string]int64{"media:1": 1, "media:42": 42, "media:999999999999999": 999999999999999} {
		if id, ok := MediaID(src); !ok || id != want {
			t.Errorf("MediaID(%q) = %d, %v", src, id, ok)
		}
	}
	for _, src := range []string{"media:0", "media:01", "media:-1", "media:+1", "media:", "media:1 ", "Media:1", "media:1.0",
		"media:1000000000000000", "media:9007199254740993", "media:1/../2", "media://1", "/api/media/1", "media:١"} {
		if _, ok := MediaID(src); ok {
			t.Errorf("MediaID(%q) accepted", src)
		}
		if err := Validate(mediaDoc(Node{Type: "image", Src: src, Alt: "x"})); err == nil {
			t.Errorf("image src %q accepted", src)
		}
	}
	doc := mediaDoc(
		Node{Type: "image", Src: "media:7", Alt: "a"},
		Node{Type: "callout", Children: []Node{{Type: "image", Src: "media:3", Alt: "b", Caption: "c"}, {Type: "image", Src: "media:7", Alt: "a"}}},
		Node{Type: "image", Src: "/static/complex-plane.svg", Alt: "fixture"},
	)
	if err := Validate(doc); err != nil {
		t.Fatal(err)
	}
	if got := MediaIDs(doc); len(got) != 2 || got[0] != 3 || got[1] != 7 {
		t.Fatalf("MediaIDs = %v", got)
	}
}

func TestImageFieldRules(t *testing.T) {
	long := strings.Repeat("é", 501) // 1002 bytes
	for name, node := range map[string]Node{
		"missing alt":         {Type: "image", Src: "media:1"},
		"blank alt":           {Type: "image", Src: "media:1", Alt: " \t\u00a0\u3000"},
		"blank fixture alt":   {Type: "image", Src: "/static/complex-plane.svg", Alt: "\u2028\ufeff\u0085"},
		"alt too long":        {Type: "image", Src: "media:1", Alt: long},
		"caption too long":    {Type: "image", Src: "media:1", Alt: "x", Caption: long},
		"caption on callout":  {Type: "callout", Caption: "x", Children: []Node{{Type: "paragraph", Children: []Node{{Type: "text", Text: "t"}}}}},
		"image with children": {Type: "image", Src: "media:1", Alt: "x", Children: []Node{{Type: "text", Text: "t"}}},
		"image with text":     {Type: "image", Src: "media:1", Alt: "x", Text: "t"},
		"media in paragraph":  {Type: "paragraph", Children: []Node{{Type: "image", Src: "media:1", Alt: "x"}}},
	} {
		if err := Validate(mediaDoc(node)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRenderUploadedMedia(t *testing.T) {
	doc := mediaDoc(
		Node{Type: "image", Src: "media:7", Alt: "Un <schéma>", Caption: "Figure 1 & légende"},
		Node{Type: "image", Src: "media:8", Alt: "Sans légende"},
		Node{Type: "image", Src: "/static/complex-plane.svg", Alt: "Plan", Caption: "Légende du plan"},
	)
	parts, err := RenderLesson(doc, MediaPrefix("/learn/lessons/x/media/"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(parts.Body)
	for _, want := range []string{
		`<figure class="lesson-media"><img src="/learn/lessons/x/media/7" alt="Un &lt;schéma&gt;" loading="lazy"><figcaption>Figure 1 &amp; légende</figcaption></figure>`,
		`<figure class="lesson-media"><img src="/learn/lessons/x/media/8" alt="Sans légende" loading="lazy"></figure>`,
		`<figure><img src="/static/complex-plane.svg" alt="Plan" width="640" height="360" loading="lazy"><figcaption>Légende du plan</figcaption></figure>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, "media:") {
		t.Errorf("unresolved reference: %s", got)
	}
	// The document itself is not modified by rendering.
	if doc.Blocks[0].Src != "media:7" {
		t.Fatal("RenderLesson modified the document")
	}
	// Only the image URLs depend on the viewer's route.
	other, err := RenderLesson(doc, MediaPrefix("/api/media/"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.ReplaceAll(string(other.HTML()), "/api/media/", "/learn/lessons/x/media/") != string(parts.HTML()) {
		t.Fatal("renderings differ beyond the media URLs")
	}
	for name, render := range map[string]func() error{
		"Render":       func() error { _, err := Render(doc); return err },
		"RenderBlocks": func() error { _, err := RenderBlocks(doc, nil); return err },
		"RenderLesson": func() error { _, err := RenderLesson(doc, nil); return err },
	} {
		if err := render(); !errors.Is(err, ErrNoMediaURL) {
			t.Errorf("%s without a media URL: %v", name, err)
		}
	}
}

// Existing fixtures render the same with or without a media URL.
func TestFixturesIgnoreMediaURL(t *testing.T) {
	for _, name := range []string{"complex-numbers", "bidi", "failure"} {
		doc, _ := Fixture(name)
		a, err := RenderLesson(doc, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, err := RenderLesson(doc, MediaPrefix("/api/media/"))
		if err != nil || a != b {
			t.Fatalf("%s: %v", name, err)
		}
		// Issue #57: the alt text is never repeated as a caption.
		if name == "complex-numbers" && !strings.Contains(string(a.Body), `width="640" height="360" loading="lazy"></figure>`) {
			t.Fatalf("fixture image without caption: %s", a.Body)
		}
	}
}

// Issue #57: alternative text and caption are separate. A figure caption is
// rendered only from the optional caption, never from the alt text, for
// fixture and uploaded images alike.
func TestAltTextIsNotRepeatedAsCaption(t *testing.T) {
	doc := mediaDoc(
		Node{Type: "image", Src: "/static/complex-plane.svg", Alt: "Plan"},
		Node{Type: "image", Src: "media:4", Alt: "Schéma"},
		Node{Type: "image", Src: "/static/complex-plane.svg", Alt: "Plan annoté", Caption: "Le plan complexe"},
	)
	parts, err := RenderLesson(doc, MediaPrefix("/api/media/"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(parts.Body)
	if strings.Count(got, "<figcaption>") != 1 || !strings.Contains(got, "<figcaption>Le plan complexe</figcaption>") {
		t.Fatalf("captions: %s", got)
	}
	for _, alt := range []string{"Plan", "Schéma", "Plan annoté"} {
		if strings.Contains(got, ">"+alt+"<") {
			t.Errorf("alt %q repeated as text: %s", alt, got)
		}
	}
	page, err := Render(Document{SchemaVersion: 1, Status: "unreviewed", Language: "fr", Title: "TEST", Blocks: doc.Blocks[:1]})
	if err != nil || strings.Contains(string(page), "<figcaption>") {
		t.Fatalf("renderer page: %v %s", err, page)
	}
}
