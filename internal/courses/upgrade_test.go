package courses_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hadden-me/learnoo/internal/courses"
	"github.com/hadden-me/learnoo/internal/database"
	"github.com/hadden-me/learnoo/internal/database/dbtest"
	"github.com/hadden-me/learnoo/migrations"
)

func migrationsUpTo(t *testing.T, v string) fs.FS {
	t.Helper()
	out := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		name := en.Name()
		if !strings.HasSuffix(name, ".sql") || name[:4] > v {
			continue
		}
		data, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = &fstest.MapFile{Data: data}
	}
	return out
}

// Upgrading a populated 0013 database keeps every existing ID and row:
// users, subjects, chapters, lessons, revisions, media, publication audit and
// curriculum. Courses start empty and work afterwards.
func TestUpgradeFrom0013KeepsExistingRows(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.EmptySchema(t)
	if _, err := database.Migrate(ctx, pool, migrationsUpTo(t, "0013")); err != nil {
		t.Fatal(err)
	}
	e := newEnvOn(t, pool)
	ch := e.chapter(e.math, "ch-01")
	p := e.edition(e.math, []int64{ch}, true)
	pub := e.publishedLesson(e.other, e.math, "fr", "Published before the upgrade")
	draft := e.lessonDraft(e.owner, e.math, "fr", "Draft before the upgrade")
	img := e.media(e.owner)
	snapshot := func() string {
		var s string
		if err := pool.QueryRow(ctx, `
			SELECT concat_ws('|',
			  (SELECT string_agg(id || ':' || email, ',' ORDER BY id) FROM users),
			  (SELECT string_agg(id || ':' || code, ',' ORDER BY id) FROM subjects),
			  (SELECT string_agg(id || ':' || subject_id || ':' || code, ',' ORDER BY id) FROM chapters),
			  (SELECT string_agg(id || ':' || public_id || ':' || coalesce(published_revision_id::text, '-'), ',' ORDER BY id) FROM lessons),
			  (SELECT string_agg(id || ':' || lesson_id || ':' || status || ':' || version, ',' ORDER BY id) FROM lesson_revisions),
			  (SELECT string_agg(id || ':' || storage_key, ',' ORDER BY id) FROM media_assets),
			  (SELECT string_agg(id || ':' || action || ':' || occurred_at, ',' ORDER BY id) FROM publication_audit),
			  (SELECT string_agg(id || ':' || action, ',' ORDER BY id) FROM curriculum_audit),
			  (SELECT string_agg(id || ':' || edition_id || ':' || subject_id, ',' ORDER BY id) FROM curriculum_assignments))`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	if _, err := database.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(); after != before {
		t.Fatalf("upgrade changed existing rows:\nbefore %s\nafter  %s", before, after)
	}
	if n := e.count(`SELECT count(*) FROM courses`); n != 0 {
		t.Fatalf("%d courses after upgrade", n)
	}
	// The upgraded database works with the existing rows.
	alt := "TEST alt"
	_, rev := e.course(e.math, "fr")
	e.mustSave(rev, courses.Draft{Title: "TEST upgraded", CoverMediaID: &img, CoverAlt: &alt, AssignmentIDs: []int64{p.assignment},
		Modules: []courses.ModuleInput{{Title: "m", ChapterID: &ch, Steps: []courses.StepInput{step(draft, "a"), step(pub, "b")}}}})
}
