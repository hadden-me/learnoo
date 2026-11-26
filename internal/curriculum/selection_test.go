package curriculum_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hadden-me/learnoo/internal/catalogue"
	"github.com/hadden-me/learnoo/internal/curriculum"
	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/hadden-me/learnoo/internal/review"
)

func lessonDoc(title string) []byte {
	b, err := json.Marshal(map[string]any{
		"schemaVersion": 1, "title": title, "language": "fr", "status": "unreviewed",
		"objective": "Objectif de test.",
		"blocks": []any{map[string]any{"type": "paragraph", "children": []any{
			map[string]any{"type": "text", "text": "Texte de test : " + title}}}},
	})
	if err != nil {
		panic(err)
	}
	return b
}

// A retired subject or chapter cannot be newly selected anywhere: catalogue
// additions, reviewer grants, new lessons and drafts (except a chapter kept
// from the draft's base revision), and making an edition current.
func TestRetiredEntriesCannotBeNewlySelected(t *testing.T) {
	e := newEnv(t)
	ls := lessons.NewStore(e.db)
	rs := review.NewStore(e.db)
	cat := catalogue.NewStore(e.db)
	reviewer := e.user("reviewer")
	if _, err := e.perms.GrantReviewer(e.ctx, testOperator, reviewer, e.math); err != nil {
		t.Fatal(err)
	}
	kept, other := e.chapter(e.math, "ch-01"), e.chapter(e.math, "ch-02")

	// A published lesson whose revision names kept.
	rev, err := ls.CreateLesson(e.ctx, e.authorID, lessons.NewLesson{SubjectID: e.math, Language: "fr",
		Fields: lessons.Fields{Content: lessonDoc("TEST leçon"), ChapterID: &kept}})
	if err != nil {
		t.Fatal(err)
	}
	if rev, err = ls.Submit(e.ctx, e.authorID, rev.ID, rev.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.Approve(e.ctx, reviewer, rev.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.Publish(e.ctx, e.authorID, rev.ID); err != nil {
		t.Fatal(err)
	}
	for _, c := range []int64{kept, other} {
		if err := e.store.RetireChapter(e.ctx, e.admin, c); err != nil {
			t.Fatal(err)
		}
	}

	// Drafts: the base revision's retired chapter is kept; another retired
	// chapter is refused on creation and on save.
	_, err = ls.CreateDraft(e.ctx, e.authorID, rev.LessonID, lessons.Fields{Content: lessonDoc("TEST suite"), ChapterID: &other})
	wantErr(t, "draft with another retired chapter", err, lessons.ErrRetiredChapter)
	draft, err := ls.CreateDraft(e.ctx, e.authorID, rev.LessonID, lessons.Fields{Content: lessonDoc("TEST suite"), ChapterID: &kept})
	if err != nil {
		t.Fatalf("draft keeping its base chapter: %v", err)
	}
	if draft, err = ls.SaveDraft(e.ctx, e.authorID, draft.ID, draft.Version, lessons.Fields{Content: lessonDoc("TEST suite 2"), ChapterID: &kept}); err != nil {
		t.Fatalf("save keeping the base chapter: %v", err)
	}
	_, err = ls.SaveDraft(e.ctx, e.authorID, draft.ID, draft.Version, lessons.Fields{Content: lessonDoc("TEST suite 3"), ChapterID: &other})
	wantErr(t, "save setting another retired chapter", err, lessons.ErrRetiredChapter)
	_, err = ls.CreateLesson(e.ctx, e.authorID, lessons.NewLesson{SubjectID: e.math, Language: "fr",
		Fields: lessons.Fields{Content: lessonDoc("TEST neuve"), ChapterID: &kept}})
	wantErr(t, "new lesson with a retired chapter", err, lessons.ErrRetiredChapter)

	asRole := e.leastPrivilegeRole()
	wantGuard(t, "direct save setting a retired chapter", asRole(
		`UPDATE S.lesson_revisions SET chapter_id = `+itoa(other)+`, version = version + 1 WHERE id = `+itoa(draft.ID)),
		"a draft keeps it only from its base revision")
	if err := asRole(`UPDATE S.lesson_revisions SET chapter_id = ` + itoa(kept) + `, version = version + 1 WHERE id = ` + itoa(draft.ID)); err != nil {
		t.Fatalf("direct save keeping the base chapter: %v", err)
	}

	// Subjects: no new chapter, lesson or reviewer grant; existing grants and
	// drafts of existing lessons stay.
	if err := e.store.RetireSubject(e.ctx, e.admin, e.math); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = cat.AddChapter(e.ctx, testOperator, "test-math", "ch-new")
	wantErr(t, "chapter in a retired subject", err, catalogue.ErrSubjectRetired)
	if _, _, created, err := cat.AddChapter(e.ctx, testOperator, "test-math", "ch-01"); err != nil || created {
		t.Fatalf("re-adding an existing chapter: created %v, %v", created, err)
	}
	wantGuard(t, "direct chapter in a retired subject", asRole(
		`WITH c AS (INSERT INTO S.chapters (subject_id, code) VALUES (`+itoa(e.math)+`, 'ch-direct') RETURNING id, subject_id)
		 INSERT INTO S.catalogue_audit (operator_label, action, subject_id, chapter_id) SELECT 'x', 'add_chapter', subject_id, id FROM c`),
		"no chapter can be added")
	_, err = ls.CreateLesson(e.ctx, e.authorID, lessons.NewLesson{SubjectID: e.math, Language: "fr",
		Fields: lessons.Fields{Content: lessonDoc("TEST autre")}})
	wantErr(t, "lesson in a retired subject", err, lessons.ErrRetiredSubject)
	if _, err := ls.SaveDraft(e.ctx, e.authorID, draft.ID, draft.Version+1, lessons.Fields{Content: lessonDoc("TEST suite 4"), ChapterID: &kept}); err != nil {
		t.Fatalf("draft of an existing lesson in a retired subject: %v", err)
	}
	newcomer := e.user("newcomer")
	_, err = e.perms.GrantReviewer(e.ctx, testOperator, newcomer, e.math)
	wantErr(t, "reviewer grant on a retired subject", err, permissions.ErrRetiredSubject)
	if changed, err := e.perms.GrantReviewer(e.ctx, testOperator, reviewer, e.math); err != nil || changed {
		t.Fatalf("re-granting an existing reviewer: changed %v, %v", changed, err)
	}
	wantGuard(t, "direct reviewer grant on a retired subject", asRole(
		`INSERT INTO S.reviewer_assignments (user_id, subject_id) VALUES (`+itoa(newcomer)+`, `+itoa(e.math)+`)`),
		"no reviewer can be newly assigned")
	if _, err := e.perms.RevokeReviewer(e.ctx, testOperator, reviewer, e.math); err != nil {
		t.Fatalf("revoking on a retired subject: %v", err)
	}

	// The Studio catalogue shows the retirement.
	subjects, err := cat.Subjects(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sj := range subjects {
		if (sj.ID == e.math) != (sj.RetiredAt != nil) {
			t.Errorf("subject %s retiredAt %v", sj.Code, sj.RetiredAt)
		}
	}
	_, chapters, err := cat.Chapters(e.ctx, "test-math")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chapters {
		if c.RetiredAt == nil {
			t.Errorf("chapter %s not shown retired", c.Code)
		}
	}
}

// Making an edition current is refused while it assigns a retired subject or
// chapter, naming them (store and guard).
func TestMakeCurrentRefusesRetiredSelections(t *testing.T) {
	e := newEnv(t)
	_, s, ed := e.hierarchy()
	k := e.chapter(e.math, "ch-01")
	ed, a, err := e.store.AddAssignment(e.ctx, e.admin, ed.ID, ed.Version, e.math)
	if err != nil {
		t.Fatal(err)
	}
	if ed, err = e.store.SetChapters(e.ctx, e.admin, ed.ID, a.ID, ed.Version, []int64{k}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.RetireChapter(e.ctx, e.admin, k); err != nil {
		t.Fatal(err)
	}
	_, _, err = e.store.MakeCurrent(e.ctx, e.admin, ed.ID, ed.Version, nil)
	wantErr(t, "retired chapter", err, curriculum.ErrPlacementInvalid)
	if err == nil || !strings.Contains(err.Error(), "chapter test-math/ch-01") {
		t.Fatalf("error does not name the chapter: %v", err)
	}
	if err := e.store.RetireSubject(e.ctx, e.admin, e.math); err != nil {
		t.Fatal(err)
	}
	_, _, err = e.store.MakeCurrent(e.ctx, e.admin, ed.ID, ed.Version, nil)
	if err == nil || !strings.Contains(err.Error(), "subject test-math") {
		t.Fatalf("error does not name the subject: %v", err)
	}
	asRole := e.leastPrivilegeRole()
	wantGuard(t, "direct make-current", asRole(
		auditSQL("edition", ed.ID, &ed.ID, "make_current", stateJSON("draft"), stateJSON("current")),
		`UPDATE S.curriculum_editions SET state = 'current', current_since = now(), version = version + 1 WHERE id = `+itoa(ed.ID)),
		"assigns retired entries")
	if n := e.count(`SELECT count(*) FROM curriculum_editions WHERE section_id = $1 AND state = 'current'`, s.ID); n != 0 {
		t.Fatalf("%d current editions", n)
	}
}
