package reader

// Interface localization of the reader and public pages (LMS-B09,
// internal/i18n). Every page template is parsed once per locale with the
// locale's functions bound, so templates call {{t "key"}} anywhere,
// including nested templates, without threading the locale through data.

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/hadden-me/learnoo/internal/i18n"
)

// localeFuncs are the template functions bound to locale l. public selects
// how links keep the locale: public pages carry it in the URL (href adds
// ?lang=ar), session pages keep it in the cookie (href is the identity).
func localeFuncs(l i18n.Locale, public bool) template.FuncMap {
	return template.FuncMap{
		"ui":        func() string { return string(l) },
		"other":     func() string { return string(l.Other()) },
		"otherName": func() string { return l.Other().Name() },
		"t": func(key string, args ...any) template.HTML {
			return htmlMessage(i18n.Message(l, key), args...)
		},
		"tn": func(key string, n int, args ...any) template.HTML {
			// The count first, so that an argument containing "{n}" is never rewritten.
			return htmlMessage(strings.ReplaceAll(i18n.Message(l, i18n.PluralKey(l, key, n)), "{n}", fmt.Sprint(n)), args...)
		},
		// pick chooses between a French and an Arabic catalogue name
		// (levels, sections, editions: both are required).
		"pick": func(fr, ar string) string {
			if l == i18n.AR && ar != "" {
				return ar
			}
			return fr
		},
		"href": func(parts ...string) string {
			target := strings.Join(parts, "")
			if public {
				return i18n.PublicURL(l, target)
			}
			return target
		},
		// Arrows point backwards and forwards in the reading direction.
		"prevArrow": func() string {
			if l.Dir() == "rtl" {
				return "→"
			}
			return "←"
		},
		"nextArrow": func() string {
			if l.Dir() == "rtl" {
				return "←"
			}
			return "→"
		},
	}
}

// markupFuncs build small trusted fragments passed as message arguments.
var markupFuncs = template.FuncMap{
	// bdi isolates a name (any direction); an optional class is added.
	"bdi": func(text string, class ...string) template.HTML {
		if len(class) > 0 {
			return template.HTML(`<bdi class="` + template.HTMLEscapeString(class[0]) + `">` + template.HTMLEscapeString(text) + `</bdi>`)
		}
		return template.HTML(`<bdi>` + template.HTMLEscapeString(text) + `</bdi>`)
	},
	"strong": func(text string) template.HTML {
		return template.HTML(`<strong>` + template.HTMLEscapeString(text) + `</strong>`)
	},
	// langspan marks content text with its own language and direction.
	"langspan": func(text, lang string) template.HTML {
		return template.HTML(`<span lang="` + template.HTMLEscapeString(lang) + `" dir="` + Dir(lang) + `">` + template.HTMLEscapeString(text) + `</span>`)
	},
	// link is a same-site link; href must be a plain local path.
	"link": func(href string, text any) template.HTML {
		if !strings.HasPrefix(href, "/") || strings.HasPrefix(href, "//") || strings.Contains(href, `\`) {
			href = "/"
		}
		return template.HTML(`<a href="` + template.HTMLEscapeString(href) + `">` + htmlArg(text) + `</a>`)
	},
}

// htmlMessage escapes a catalog message and replaces its {0}, {1}…
// placeholders: template.HTML arguments as they are, anything else escaped.
func htmlMessage(text string, args ...any) template.HTML {
	return template.HTML(i18n.Format(template.HTMLEscapeString(text), htmlArg, args...))
}

func htmlArg(a any) string {
	if h, ok := a.(template.HTML); ok {
		return string(h)
	}
	return template.HTMLEscapeString(fmt.Sprint(a))
}

// localized parses one template set per locale: base files then page.
func localized(funcs template.FuncMap, public bool, base []string, page string) (map[i18n.Locale]*template.Template, error) {
	out := map[i18n.Locale]*template.Template{}
	for _, l := range i18n.Locales {
		t := template.New("").Funcs(funcs).Funcs(markupFuncs).Funcs(localeFuncs(l, public))
		t, err := t.ParseFS(files, append(append([]string{}, base...), "templates/"+page+".html")...)
		if err != nil {
			return nil, err
		}
		out[l] = t
	}
	return out, nil
}

// locale is the request's interface locale (i18n.Middleware or, for the
// public pages, the URL).
func locale(r *http.Request) i18n.Locale { return i18n.From(r) }

// msg is the request locale's text of key.
func msg(r *http.Request, key string, args ...any) string { return i18n.T(locale(r), key, args...) }

// stateKey is the message key of a curriculum edition state.
func stateKey(state string) string {
	switch state {
	case "current":
		return "edition.state.current"
	case "superseded":
		return "edition.state.superseded"
	case "retired":
		return "edition.state.retired"
	}
	return "edition.state.unknown"
}

// languageKey is the message key of a content language's name.
func languageKey(lang string) string {
	switch lang {
	case "fr":
		return "language.fr"
	case "ar":
		return "language.ar"
	}
	return "language.other"
}
