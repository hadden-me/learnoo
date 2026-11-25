package studioapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
)

// strictDecode implements decodeJSON's rules on data, into v, a pointer to a
// request struct:
//
//   - data is exactly one JSON object; its keys are each in allowed,
//     compared byte for byte (encoding/json alone would match them
//     case-insensitively), and appear once; nothing follows it.
//   - null is accepted as a top-level value only for a pointer field (a
//     genuinely optional value, such as chapterId) without the
//     api:"nonnull" tag, and for a field whose type decodes JSON itself
//     (such as expectedPointer, which takes null as a value). It is refused
//     for every other field, including a json.RawMessage (content), and as
//     an element of an array field, where encoding/json would silently
//     store "", 0 or an empty list.
//   - Each value must decode into its field: integer fields take only
//     integer literals that fit (1.0, 1e0 and strings are refused).
//
// These rules apply to top-level values only. They are complete because a
// request type may hold nothing else: checkRequestType admits only scalar
// fields, pointers to scalars, arrays of scalars, json.RawMessage (whose
// content its consumer validates) and types with their own UnmarshalJSON,
// and it panics on first use of any other request type (nested structs or
// maps, embedded pointers, duplicate names, unknown api tags, or nonnull on
// a type that decodes itself), so a new request type fails closed in tests.
//
// Error messages name only JSON keys from the request and JSON types, never
// Go types or fields (issue #51).
func strictDecode(data []byte, v any, allowed []string) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		panic("studioapi: request target must be a non-nil pointer to a struct")
	}
	target := rv.Elem()
	fields := mustRequestFields(target.Type())

	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return errors.New("expected one JSON object")
	}
	type entry struct {
		key string
		raw json.RawMessage
	}
	var entries []entry
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return errors.New("malformed JSON")
		}
		key, _ := tok.(string)
		if _, known := fields[key]; !known || !slices.Contains(allowed, key) {
			return fmt.Errorf("unknown field %q", truncateKey(key))
		}
		if seen[key] {
			return fmt.Errorf("duplicate field %q", key)
		}
		seen[key] = true
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return errors.New("malformed JSON")
		}
		entries = append(entries, entry{key, raw})
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return errors.New("malformed JSON")
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("unexpected data after the JSON object")
	}
	// Keys are now known to be exact and unique; check and decode each
	// value into its own field.
	for _, e := range entries {
		f := fields[e.key]
		if err := checkNulls(e.key, e.raw, f); err != nil {
			return err
		}
		fv := target.FieldByIndex(f.index)
		fd := json.NewDecoder(bytes.NewReader(e.raw))
		fd.DisallowUnknownFields()
		if err := fd.Decode(fv.Addr().Interface()); err != nil {
			return fmt.Errorf("field %q %s", e.key, expectation(f.typ))
		}
	}
	return nil
}

// requestField is one top-level key of a request body.
type requestField struct {
	index    []int
	typ      reflect.Type
	nullable bool
}

var (
	rawMessageType  = reflect.TypeFor[json.RawMessage]()
	jsonUnmarshaler = reflect.TypeFor[json.Unmarshaler]()
)

// decodesItself reports a type with its own UnmarshalJSON, other than
// json.RawMessage (which accepts anything, null included, and so is held to
// the non-null rule here).
func decodesItself(t reflect.Type) bool {
	return t != rawMessageType && (t.Implements(jsonUnmarshaler) || reflect.PointerTo(t).Implements(jsonUnmarshaler))
}

var requestTypes sync.Map // reflect.Type -> map[string]requestField

// mustRequestFields returns the fields of request type t, checked once by
// checkRequestType. An unsupported type is a programming error: it panics,
// so the type cannot be decoded with weaker rules than documented.
func mustRequestFields(t reflect.Type) map[string]requestField {
	if f, ok := requestTypes.Load(t); ok {
		return f.(map[string]requestField)
	}
	fields, err := checkRequestType(t)
	if err != nil {
		panic("studioapi: unsupported request type: " + err.Error())
	}
	requestTypes.Store(t, fields)
	return fields
}

// checkRequestType maps the JSON name of each exported field of struct
// type t, including fields of embedded (non-pointer) structs, to its
// description, or reports why t is not a supported request type. Names must
// be unique across all depths, so encoding/json's promotion rules (a
// shallower field hides a deeper one) never decide which field a key fills.
func checkRequestType(t reflect.Type) (map[string]requestField, error) {
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("%s is not a struct", t)
	}
	out := map[string]requestField{}
	var walk func(t reflect.Type, prefix []int) error
	walk = func(t reflect.Type, prefix []int) error {
		for i := range t.NumField() {
			sf := t.Field(i)
			index := append(slices.Clone(prefix), i)
			tag := sf.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name, opts, _ := strings.Cut(tag, ",")
			if opts != "" && opts != "omitempty" {
				return fmt.Errorf("%s.%s: json option %q", t, sf.Name, opts)
			}
			if sf.Anonymous {
				if name != "" || sf.Type.Kind() != reflect.Struct || decodesItself(sf.Type) {
					return fmt.Errorf("%s.%s: only untagged embedded structs are supported", t, sf.Name)
				}
				if err := walk(sf.Type, index); err != nil {
					return err
				}
				continue
			}
			if !sf.IsExported() {
				continue
			}
			if name == "" {
				name = sf.Name
			}
			if _, dup := out[name]; dup {
				return fmt.Errorf("%s.%s: duplicate JSON name %q", t, sf.Name, name)
			}
			api := sf.Tag.Get("api")
			switch {
			case api != "" && api != "nonnull":
				return fmt.Errorf("%s.%s: unknown api tag %q", t, sf.Name, api)
			case decodesItself(sf.Type) || (sf.Type.Kind() == reflect.Pointer && decodesItself(sf.Type.Elem())):
				if api != "" {
					return fmt.Errorf("%s.%s: nonnull on a type that decodes JSON itself", t, sf.Name)
				}
				out[name] = requestField{index: index, typ: sf.Type, nullable: true}
				continue
			case sf.Type == rawMessageType || scalar(sf.Type) || (sf.Type.Kind() == reflect.Slice && scalar(sf.Type.Elem()) && sf.Type.Elem().Kind() != reflect.Uint8):
				if api != "" {
					return fmt.Errorf("%s.%s: nonnull on a field that is never nullable", t, sf.Name)
				}
				out[name] = requestField{index: index, typ: sf.Type}
			case sf.Type.Kind() == reflect.Pointer && scalar(sf.Type.Elem()):
				out[name] = requestField{index: index, typ: sf.Type, nullable: api != "nonnull"}
			default:
				return fmt.Errorf("%s.%s: unsupported field type %s", t, sf.Name, sf.Type)
			}
		}
		return nil
	}
	if err := walk(t, nil); err != nil {
		return nil, err
	}
	return out, nil
}

// scalar reports a JSON string, number or boolean type without custom
// decoding. ([]byte, which encoding/json reads as base64, is refused by the
// caller.)
func scalar(t reflect.Type) bool {
	return jsonKind(t) != "" && t.Kind() != reflect.Pointer && !decodesItself(t)
}

// checkNulls refuses null where f does not accept it: as the value, and as
// an element of an array whose elements are not pointers.
func checkNulls(key string, raw json.RawMessage, f requestField) error {
	isNull := func(b []byte) bool { return string(bytes.TrimSpace(b)) == "null" }
	if isNull(raw) {
		if f.nullable {
			return nil
		}
		return fmt.Errorf("field %q must not be null", key)
	}
	t := f.typ
	if decodesItself(t) {
		return nil
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == rawMessageType || (t.Kind() != reflect.Slice && t.Kind() != reflect.Array) || t.Elem().Kind() == reflect.Pointer {
		return nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil // not an array: reported when decoding
	}
	if slices.ContainsFunc(elems, func(e json.RawMessage) bool { return isNull(e) }) {
		return fmt.Errorf("field %q must not contain null", key)
	}
	return nil
}

// expectation completes "field %q …" for a value that does not decode into
// t, in JSON terms.
func expectation(t reflect.Type) string {
	if decodesItself(t) {
		return "has the wrong type"
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t != rawMessageType && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
		if what := jsonKind(t.Elem()); what != "" {
			return "must be an array of " + what + "s"
		}
		return "must be an array"
	}
	if what := jsonKind(t); what != "" {
		return "must be " + article(what) + " " + what
	}
	return "has the wrong type"
}

func jsonKind(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	}
	return ""
}

func article(word string) string {
	if strings.ContainsRune("aeiou", rune(word[0])) {
		return "an"
	}
	return "a"
}
