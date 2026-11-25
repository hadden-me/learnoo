package studioapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestWriteLessonErrorCodes(t *testing.T) {
	h := New(Config{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	wrapped := func(code string) error { return fmt.Errorf("save: %w", &pgconn.PgError{Code: code}) }
	for name, c := range map[string]struct {
		err    error
		own    bool
		status int
		code   string
	}{
		"deadlock":              {wrapped("40P01"), true, http.StatusConflict, "retry"},
		"serialization":         {wrapped("40001"), false, http.StatusConflict, "retry"},
		"deadlock in RequireTx": {fmt.Errorf("%w", errors.Join(lessons.ErrDenied, &pgconn.PgError{Code: "40P01"})), true, http.StatusConflict, "retry"},
		"other database error":  {wrapped("23514"), true, http.StatusInternalServerError, "internal"},
		"denied, own revision":  {lessons.ErrDenied, true, http.StatusNotFound, "not_found"},
		"denied, create":        {lessons.ErrDenied, false, http.StatusForbidden, "forbidden"},
		"not found":             {lessons.ErrNotFound, true, http.StatusNotFound, "not_found"},
		"invalid content":       {fmt.Errorf("%w: content is not valid UTF-8", lessons.ErrInvalidContent), true, http.StatusUnprocessableEntity, "invalid_content"},
		"archived":              {lessons.ErrArchived, true, http.StatusConflict, "archived"},
		"active candidate":      {lessons.ErrActiveCandidateExists, false, http.StatusConflict, "active_candidate_exists"},
		"immutable":             {lessons.ErrImmutable, true, http.StatusConflict, "immutable"},
	} {
		w := httptest.NewRecorder()
		h.writeLessonError(w, "test", c.err, c.own)
		var b struct{ Error struct{ Code string } }
		_ = json.Unmarshal(w.Body.Bytes(), &b)
		if w.Code != c.status || b.Error.Code != c.code {
			t.Errorf("%s: %d %s, want %d %s", name, w.Code, b.Error.Code, c.status, c.code)
		}
	}
}

func TestDeniedWithDatabaseCauseIsInternal(t *testing.T) {
	h := New(Config{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	for name, err := range map[string]error{
		"pg error":    errors.Join(lessons.ErrDenied, &pgconn.PgError{Code: "57014"}),
		"other cause": errors.Join(lessons.ErrDenied, io.ErrUnexpectedEOF),
	} {
		for _, own := range []bool{true, false} {
			w := httptest.NewRecorder()
			h.writeLessonError(w, "test", err, own)
			if w.Code != http.StatusInternalServerError {
				t.Errorf("%s (own=%v): %d %s", name, own, w.Code, w.Body)
			}
		}
	}
}
