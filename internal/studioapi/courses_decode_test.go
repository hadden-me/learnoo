package studioapi

import (
	"strings"
	"testing"
)

// Course request types (courses.go), checked by
// TestRequestTypesAreSupported.
func init() {
	checkedRequestTypes = append(checkedRequestTypes,
		requestType{&createCourseRequest{}, createCourseKeys},
		requestType{&saveCourseRequest{}, saveCourseKeys},
		requestType{&moduleRequest{}, moduleKeys},
		requestType{&stepRequest{}, stepKeys},
	)
}

// Course save bodies are strict at every level: the top level through
// decodeJSON, modules and steps through draft().
func TestCourseSaveDecoding(t *testing.T) {
	const head = `{"expectedVersion":1,"title":"t","summary":"","objectives":"","prerequisites":"","assignmentIds":[3],"modules":`
	var ok saveCourseRequest
	if err := strictDecode([]byte(head+`[{"key":null,"title":"m","chapterId":null,"steps":[{"lessonId":1,"revisionId":2,"title":"s"}]}]}`), &ok, saveCourseKeys); err != nil {
		t.Fatal(err)
	}
	d, err := ok.draft()
	if err != nil || len(d.Modules) != 1 || d.Modules[0].Key != "" || len(d.Modules[0].Steps) != 1 || d.Modules[0].Steps[0].RevisionID != 2 {
		t.Fatalf("draft %+v %v", d, err)
	}
	for name, tc := range map[string]struct{ modules, want string }{
		"modules null element": {`[null]`, "array of objects"},
		"steps object":         {`[{"title":"m","steps":{}}]`, "array of objects"},
		"module missing steps": {`[{"title":"m"}]`, "title and steps are required"},
		"module empty key":     {`[{"key":"","title":"m","steps":[]}]`, "key must be a UUID"},
		"step missing title":   {`[{"title":"m","steps":[{"lessonId":1,"revisionId":2}]}]`, "are required"},
		"step null lesson":     {`[{"title":"m","steps":[{"lessonId":null,"revisionId":2,"title":"s"}]}]`, "lessonId"},
		"step extra key":       {`[{"title":"m","steps":[{"lessonId":1,"revisionId":2,"title":"s","x":1}]}]`, `unknown field "x"`},
	} {
		var req saveCourseRequest
		if err := strictDecode([]byte(head+tc.modules+`}`), &req, saveCourseKeys); err != nil {
			t.Fatalf("%s: top level: %v", name, err)
		}
		if _, err := req.draft(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", name, err, tc.want)
		}
	}
	var create createCourseRequest
	if err := strictDecode([]byte(`{"subjectId":1,"language":null}`), &create, createCourseKeys); err == nil {
		t.Error("null language accepted")
	}
}
