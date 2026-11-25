package studioapi

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The cases of issue #51: null only where a field is genuinely optional,
// integers only as integer literals, and errors in JSON terms only.

const okDoc = `{"type":"doc"}`

func TestStrictDecodeRefusesNullForRequiredFields(t *testing.T) {
	for name, c := range map[string]struct {
		body    string
		v       any
		allowed []string
		key     string
	}{
		"create prerequisites": {`{"subjectId":1,"language":"fr","content":` + okDoc + `,"prerequisites":null}`, &createLessonRequest{}, createLessonKeys, "prerequisites"},
		"create mediaIds":      {`{"subjectId":1,"language":"fr","content":` + okDoc + `,"mediaIds":null}`, &createLessonRequest{}, createLessonKeys, "mediaIds"},
		"create subjectId":     {`{"subjectId":null,"language":"fr","content":` + okDoc + `}`, &createLessonRequest{}, createLessonKeys, "subjectId"},
		"create language":      {`{"subjectId":1,"language":null,"content":` + okDoc + `}`, &createLessonRequest{}, createLessonKeys, "language"},
		"create content":       {`{"subjectId":1,"language":"fr","content":null}`, &createLessonRequest{}, createLessonKeys, "content"},
		"draft prerequisites":  {`{"content":` + okDoc + `,"prerequisites":null}`, &fieldsJSON{}, fieldsKeys, "prerequisites"},
		"draft mediaIds":       {`{"content":` + okDoc + `,"mediaIds":null}`, &fieldsJSON{}, fieldsKeys, "mediaIds"},
		"draft content":        {`{"content":null}`, &fieldsJSON{}, fieldsKeys, "content"},
		"save expectedVersion": {`{"expectedVersion":null,"content":` + okDoc + `}`, &saveRequest{}, saveKeys, "expectedVersion"},
		"save mediaIds":        {`{"expectedVersion":1,"content":` + okDoc + `,"mediaIds":null}`, &saveRequest{}, saveKeys, "mediaIds"},
		"save prerequisites":   {`{"expectedVersion":1,"content":` + okDoc + `,"prerequisites":null}`, &saveRequest{}, saveKeys, "prerequisites"},
		"submit version":       {`{"expectedVersion":null}`, &submitRequest{}, submitKeys, "expectedVersion"},
	} {
		err := strictDecode([]byte(c.body), c.v, c.allowed)
		if err == nil {
			t.Errorf("%s: null accepted", name)
			continue
		}
		if want := `field "` + c.key + `" must not be null`; err.Error() != want {
			t.Errorf("%s: %q, want %q", name, err, want)
		}
	}
	for name, body := range map[string]string{
		"null element":             `{"content":` + okDoc + `,"mediaIds":[1,null]}`,
		"only a null element":      `{"content":` + okDoc + `,"mediaIds":[null]}`,
		"null element with spaces": `{"content":` + okDoc + `,"mediaIds":[ null ]}`,
	} {
		var req fieldsJSON
		err := strictDecode([]byte(body), &req, fieldsKeys)
		if err == nil || err.Error() != `field "mediaIds" must not contain null` {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestStrictDecodeAcceptsNullForOptionalFields(t *testing.T) {
	var req createLessonRequest
	body := `{"subjectId":1,"language":"fr","content":` + okDoc + `,"chapterId":null,"translationOf":null,"prerequisites":"p","mediaIds":[]}`
	if err := strictDecode([]byte(body), &req, createLessonKeys); err != nil {
		t.Fatal(err)
	}
	if req.ChapterID != nil || req.TranslationOf != nil || req.SubjectID != 1 || req.Language != "fr" ||
		req.Prerequisites != "p" || req.MediaIDs == nil || len(req.MediaIDs) != 0 || string(req.Content) != okDoc {
		t.Fatalf("decoded %+v", req)
	}
	var save saveRequest
	if err := strictDecode([]byte(`{"expectedVersion":3,"content":`+okDoc+`,"chapterId":7,"mediaIds":[4,5]}`), &save, saveKeys); err != nil {
		t.Fatal(err)
	}
	if save.ExpectedVersion == nil || *save.ExpectedVersion != 3 || save.ChapterID == nil || *save.ChapterID != 7 ||
		len(save.MediaIDs) != 2 || save.MediaIDs[1] != 5 {
		t.Fatalf("decoded %+v", save)
	}
	// Absent keys stay absent; the handlers report missing required ones.
	var submit submitRequest
	if err := strictDecode([]byte(`{}`), &submit, submitKeys); err != nil || submit.ExpectedVersion != nil {
		t.Fatalf("empty object: %v %+v", err, submit)
	}
}

func TestStrictDecodeRefusesNonIntegers(t *testing.T) {
	for name, c := range map[string]struct {
		body    string
		v       any
		allowed []string
		want    string
	}{
		"mediaIds fraction":    {`{"content":` + okDoc + `,"mediaIds":[1.0]}`, &fieldsJSON{}, fieldsKeys, `field "mediaIds" must be an array of integers`},
		"mediaIds exponent":    {`{"content":` + okDoc + `,"mediaIds":[1e0]}`, &fieldsJSON{}, fieldsKeys, `field "mediaIds" must be an array of integers`},
		"mediaIds string":      {`{"content":` + okDoc + `,"mediaIds":["1"]}`, &fieldsJSON{}, fieldsKeys, `field "mediaIds" must be an array of integers`},
		"mediaIds not array":   {`{"content":` + okDoc + `,"mediaIds":1}`, &fieldsJSON{}, fieldsKeys, `field "mediaIds" must be an array of integers`},
		"chapterId fraction":   {`{"content":` + okDoc + `,"chapterId":2.5}`, &fieldsJSON{}, fieldsKeys, `field "chapterId" must be an integer`},
		"subjectId fraction":   {`{"subjectId":1.0,"language":"fr","content":` + okDoc + `}`, &createLessonRequest{}, createLessonKeys, `field "subjectId" must be an integer`},
		"subjectId exponent":   {`{"subjectId":1e0,"language":"fr","content":` + okDoc + `}`, &createLessonRequest{}, createLessonKeys, `field "subjectId" must be an integer`},
		"subjectId overflow":   {`{"subjectId":99999999999999999999,"language":"fr","content":` + okDoc + `}`, &createLessonRequest{}, createLessonKeys, `field "subjectId" must be an integer`},
		"translationOf string": {`{"subjectId":1,"language":"fr","content":` + okDoc + `,"translationOf":"2"}`, &createLessonRequest{}, createLessonKeys, `field "translationOf" must be an integer`},
		"language number":      {`{"subjectId":1,"language":1,"content":` + okDoc + `}`, &createLessonRequest{}, createLessonKeys, `field "language" must be a string`},
		"prerequisites array":  {`{"content":` + okDoc + `,"prerequisites":[]}`, &fieldsJSON{}, fieldsKeys, `field "prerequisites" must be a string`},
		"version fraction":     {`{"expectedVersion":1.5}`, &submitRequest{}, submitKeys, `field "expectedVersion" must be an integer`},
		"version string":       {`{"expectedVersion":"1"}`, &submitRequest{}, submitKeys, `field "expectedVersion" must be an integer`},
	} {
		err := strictDecode([]byte(c.body), c.v, c.allowed)
		if err == nil || err.Error() != c.want {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

// Error messages never name Go types, packages or struct fields.
func TestStrictDecodeMessagesUseJSONNames(t *testing.T) {
	bodies := []string{
		`{"content":` + okDoc + `,"mediaIds":[1.0]}`,
		`{"content":` + okDoc + `,"mediaIds":[true]}`,
		`{"content":` + okDoc + `,"mediaIds":{"a":1}}`,
		`{"content":` + okDoc + `,"chapterId":{}}`,
		`{"content":` + okDoc + `,"prerequisites":null}`,
		`{"content":` + okDoc + `,"Content":` + okDoc + `}`,
		`{"content":` + okDoc + `,"MediaIDs":[1]}`,
		`{"content":` + okDoc + `,"content":` + okDoc + `}`,
		`{"content":` + okDoc + `} x`,
	}
	for _, body := range bodies {
		var req fieldsJSON
		err := strictDecode([]byte(body), &req, fieldsKeys)
		if err == nil {
			t.Errorf("%s: accepted", body)
			continue
		}
		for _, leak := range []string{"Go ", "fieldsJSON", "studioapi", "int64", "[]", "json:", "RawMessage", "struct"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s: message %q contains %q", body, err, leak)
			}
		}
	}
}

// The embedded struct is not a key of its own.
func TestStrictDecodeRefusesEmbeddedStructName(t *testing.T) {
	var req saveRequest
	err := strictDecode([]byte(`{"expectedVersion":1,"fieldsJSON":{"content":{}}}`), &req, saveKeys)
	if err == nil || err.Error() != `unknown field "fieldsJSON"` {
		t.Fatalf("%v", err)
	}
}

// pointerLike stands for a field type with its own decoding, such as the
// required, nullable expectedPointer: it receives null itself.
type pointerLike struct {
	set  bool
	null bool
}

func (p *pointerLike) UnmarshalJSON(b []byte) error {
	p.set = true
	if string(b) == "null" {
		p.null = true
		return nil
	}
	var id int64
	return json.Unmarshal(b, &id)
}

func TestStrictDecodeLeavesNullToCustomTypes(t *testing.T) {
	type request struct {
		P pointerLike `json:"p"`
	}
	var r request
	if err := strictDecode([]byte(`{"p":null}`), &r, []string{"p"}); err != nil || !r.P.set || !r.P.null {
		t.Fatalf("null: %v %+v", err, r)
	}
	r = request{}
	if err := strictDecode([]byte(`{}`), &r, []string{"p"}); err != nil || r.P.set {
		t.Fatalf("absent: %v %+v", err, r)
	}
	err := strictDecode([]byte(`{"p":1.5}`), &request{}, []string{"p"})
	if err == nil || err.Error() != `field "p" has the wrong type` {
		t.Fatalf("fraction: %v", err)
	}
}

// requestType pairs a request body type with its allowed keys. Each file
// that adds request types appends them in its own test file (init), so
// every type decodeJSON is used with is checked here.
type requestType struct {
	v    any
	keys []string
}

var checkedRequestTypes = []requestType{
	{&createLessonRequest{}, createLessonKeys},
	{&fieldsJSON{}, fieldsKeys},
	{&saveRequest{}, saveKeys},
	{&submitRequest{}, submitKeys},
}

// Every request type is supported by strictDecode, and its allowed keys are
// exactly its JSON fields.
func TestRequestTypesAreSupported(t *testing.T) {
	for _, rt := range checkedRequestTypes {
		typ := reflect.TypeOf(rt.v).Elem()
		fields, err := checkRequestType(typ)
		if err != nil {
			t.Errorf("%s: %v", typ, err)
			continue
		}
		names := slices.Sorted(maps.Keys(fields))
		keys := slices.Sorted(slices.Values(rt.keys))
		if !slices.Equal(names, keys) {
			t.Errorf("%s: fields %v, allowed keys %v", typ, names, keys)
		}
	}
}

type TestNonNullX struct {
	X *int `json:"X" api:"nonnull"`
}

type TestOtherX struct {
	X int
}

type innerPlain struct {
	Y int `json:"y"`
}

// Request types the strict rules would not cover are refused: the decoder
// panics on first use instead of decoding them more loosely.
func TestUnsupportedRequestTypesFailClosed(t *testing.T) {
	for name, v := range map[string]any{
		"nested struct": &struct {
			A innerPlain `json:"a"`
		}{},
		"pointer to struct": &struct {
			A *innerPlain `json:"a"`
		}{},
		"map": &struct {
			A map[string]int `json:"a"`
		}{},
		"slice of structs": &struct {
			A []innerPlain `json:"a"`
		}{},
		"interface": &struct {
			A any `json:"a"`
		}{},
		"byte slice": &struct {
			A []byte `json:"a"`
		}{},
		"string option": &struct {
			A int `json:"a,string"`
		}{},
		"embedded pointer": &struct {
			*innerPlain
		}{},
		"tagged embedded struct": &struct {
			innerPlain `json:"inner"`
		}{},
		// A shallower field would hide the embedded nonnull one in
		// encoding/json; duplicates are refused instead. (The shallower
		// names are implicit, so go vet's tag check does not reject the
		// test itself.)
		"name conflict across depths": &struct {
			X *int
			TestNonNullX
		}{},
		"name conflict, same depth": &struct {
			TestNonNullX
			TestOtherX
		}{},
		"nonnull on a custom type": &struct {
			P *pointerLike `json:"p" api:"nonnull"`
		}{},
		"nonnull on a custom value": &struct {
			P pointerLike `json:"p" api:"nonnull"`
		}{},
		"nonnull on a non-pointer": &struct {
			A int `json:"a" api:"nonnull"`
		}{},
		"unknown api tag": &struct {
			A *int `json:"a" api:"required"`
		}{},
		"not a struct": new(int),
	} {
		if _, err := checkRequestType(reflect.TypeOf(v).Elem()); err == nil {
			t.Errorf("%s: accepted", name)
		}
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: strictDecode did not panic", name)
				}
			}()
			_ = strictDecode([]byte(`{}`), v, nil)
		}()
	}
	// Embedded non-pointer structs are promoted, with nonnull kept.
	var ok struct {
		TestNonNullX
		innerPlain
	}
	if err := strictDecode([]byte(`{"X":null,"y":1}`), &ok, []string{"X", "y"}); err == nil || err.Error() != `field "X" must not be null` {
		t.Fatalf("promoted nonnull: %v", err)
	}
}
