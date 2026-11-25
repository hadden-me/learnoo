package rendering

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestFixtures(t *testing.T) {
	for _, name := range []string{"complex-numbers", "bidi", "failure"} {
		t.Run(name, func(t *testing.T) {
			doc, err := Fixture(name)
			if err != nil {
				t.Fatal(err)
			}
			html, err := Render(doc)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(html, []byte("Contenu non relu")) {
				t.Fatal("missing unreviewed warning")
			}
		})
	}
}
func TestUnsafeContent(t *testing.T) {
	valid, _ := Fixture("complex-numbers")
	cases := map[string]Node{
		"raw html":               {Type: "html", Text: "<script>alert(1)</script>"},
		"javascript image":       {Type: "image", Src: "javascript:alert(1)", Alt: "x"},
		"remote tracking image":  {Type: "image", Src: "https://example.com/x.png", Alt: "x"},
		"traversal":              {Type: "image", Src: "/static/../secret", Alt: "x"},
		"video URL injection":    {Type: "youtube", VideoID: "\" onload=alert(1)"},
		"unescaped direction":    {Type: "paragraph", Dir: "rtl\" onclick=alert(1)", Children: []Node{{Type: "text", Text: "x"}}},
		"block inside paragraph": {Type: "paragraph", Children: []Node{{Type: "displayMath", Text: "x"}}},
	}
	for name, node := range cases {
		t.Run(name, func(t *testing.T) {
			doc := valid
			doc.Blocks = []Node{node}
			if _, err := Render(doc); err == nil {
				t.Fatal("unsafe input accepted")
			}
		})
	}
}
func TestTextEscaping(t *testing.T) {
	doc, _ := Fixture("complex-numbers")
	doc.Title = "<script>alert(1)</script>"
	doc.Blocks = []Node{{Type: "paragraph", Children: []Node{{Type: "text", Text: "<img src=x onerror=alert(1)>"}}}}
	html, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(html), "<script>alert") || strings.Contains(string(html), "<img src=x") {
		t.Fatal("unescaped HTML")
	}
	if !bytes.Contains(html, []byte("&lt;img")) {
		t.Fatal("escaped text missing")
	}
}
func TestDecodeBoundaries(t *testing.T) {
	doc, _ := Fixture("complex-numbers")
	raw, _ := json.Marshal(doc)
	for name, data := range map[string][]byte{"trailing": append(append([]byte{}, raw...), raw...), "oversize": []byte(strings.Repeat(" ", MaxDocumentBytes+1)), "unknown": bytes.Replace(raw, []byte(`"title":`), []byte(`"unrecognized":1,"title":`), 1)} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(bytes.NewReader(data)); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
	doc.Status = "approved"
	if _, err := Render(doc); err == nil {
		t.Fatal("prototype accepted approved status")
	}
	node := Node{Type: "text", Text: "x"}
	for i := 0; i < 20; i++ {
		node = Node{Type: "strong", Children: []Node{node}}
	}
	doc.Status = "unreviewed"
	doc.Blocks = []Node{{Type: "paragraph", Children: []Node{node}}}
	if _, err := Render(doc); err == nil {
		t.Fatal("excessive depth accepted")
	}
}
func BenchmarkRenderLesson(b *testing.B) {
	doc, _ := Fixture("complex-numbers")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Render(doc); err != nil {
			b.Fatal(err)
		}
	}
}

// The reader embeds RenderBlocks; the preview page embeds the same blocks.
func TestRenderBlocksMatchesPageBody(t *testing.T) {
	for _, name := range []string{"complex-numbers", "bidi", "failure"} {
		doc, err := Fixture(name)
		if err != nil {
			t.Fatal(err)
		}
		page, err := Render(doc)
		if err != nil {
			t.Fatal(err)
		}
		body, err := RenderBlocks(doc)
		if err != nil {
			t.Fatal(err)
		}
		if body == "" || !strings.Contains(string(page), `<article aria-label="Contenu de la leçon">`+string(body)+`</article>`) {
			t.Errorf("%s: blocks differ from the page body", name)
		}
	}
	if _, err := RenderBlocks(Document{SchemaVersion: 1, Status: "unreviewed", Language: "fr", Title: "x"}); err == nil {
		t.Error("invalid document rendered")
	}
}

// Nodes without a language or direction inherit them: empty lang/dir
// attributes would reset the inherited language (lang="" means unknown).
func TestUnlabelledNodesInheritLanguage(t *testing.T) {
	doc := Document{SchemaVersion: 1, Status: "unreviewed", Language: "ar", Title: "اختبار", Blocks: []Node{
		{Type: "heading", Children: []Node{{Type: "text", Text: "عنوان"}}},
		{Type: "paragraph", Children: []Node{{Type: "text", Text: "نص"}, {Type: "text", Text: "terme", Lang: "fr", Dir: "ltr"}}},
		{Type: "paragraph", Lang: "fr", Dir: "ltr", Children: []Node{{Type: "text", Text: "texte"}}},
	}}
	body, err := RenderBlocks(doc)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if strings.Contains(got, `lang=""`) || strings.Contains(got, `dir=""`) {
		t.Fatalf("empty lang/dir attributes: %s", got)
	}
	for _, want := range []string{`<h2><bdi>عنوان</bdi></h2>`, `<p><bdi>نص</bdi><bdi lang="fr" dir="ltr">terme</bdi></p>`, `<p lang="fr" dir="ltr"><bdi>texte</bdi></p>`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	for _, name := range []string{"complex-numbers", "bidi", "failure"} {
		doc, _ := Fixture(name)
		page, err := Render(doc)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(page), `lang=""`) || strings.Contains(string(page), `dir=""`) {
			t.Errorf("%s: empty lang/dir attributes", name)
		}
	}
}

// The click-to-load button and its notice share one wrapper, which math.js
// replaces as a whole with the player (issue #54).
func TestYouTubeControlWrapsNotice(t *testing.T) {
	doc, _ := Fixture("complex-numbers")
	doc.Blocks = []Node{{Type: "youtube", Title: "Vidéo", VideoID: "abcdefghijk"}, {Type: "youtube", Title: "À choisir"}}
	html, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	want := `<div class="video-control"><button type="button" class="video-load" data-video-id="abcdefghijk">Charger la vidéo YouTube</button><p class="video-notice">La vidéo ne se charge qu’après votre choix.</p></div>`
	if !bytes.Contains(html, []byte(want)) {
		t.Fatalf("video control markup missing: %s", html)
	}
	if n := bytes.Count(html, []byte(`class="video-control"`)); n != 1 {
		t.Fatalf("pending video slot must not render a control, got %d", n)
	}
}
