package courses_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/hadden-me/learnoo/internal/courses"
	"github.com/hadden-me/learnoo/internal/lessons"
	"github.com/hadden-me/learnoo/internal/permissions"
)

func TestDraftLifecycle(t *testing.T) {
	e := newEnv(t)
	ch1 := e.chapter(e.math, "ch-01")
	p := e.edition(e.math, []int64{ch1}, true)
	own := e.lessonDraft(e.owner, e.math, "fr", "Own draft lesson")
	foreign := e.publishedLesson(e.other, e.math, "fr", "Published by another author")
	cover := e.media(e.owner)

	c, rev := e.course(e.math, "fr")
	if rev.Number != 1 || rev.Status != courses.StatusDraft || rev.Version != 1 || rev.BaseRevisionID != nil || rev.Kind != "ordinary" {
		t.Fatalf("first draft: %+v", rev)
	}
	alt := "TEST cover description"
	d := courses.Draft{
		Title: "  TEST course  ", Summary: "Line one\r\nline two", Objectives: "TEST objectives", Prerequisites: "",
		CoverMediaID: &cover, CoverAlt: &alt, AssignmentIDs: []int64{p.assignment},
		Modules: []courses.ModuleInput{
			{Title: "TEST module A", ChapterID: &ch1, Steps: []courses.StepInput{step(own, "Step one"), step(foreign, "Step two")}},
			{Title: "TEST module B"},
		},
	}
	saved := e.mustSave(rev, d)
	if saved.Version != 2 || saved.Title != "TEST course" || saved.Summary != "Line one\nline two" {
		t.Fatalf("saved: %+v", saved)
	}
	if len(saved.Assignments) != 1 || !saved.Assignments[0].Selectable || len(saved.Modules) != 2 {
		t.Fatalf("children: %+v", saved)
	}
	m := saved.Modules[0]
	if len(m.Steps) != 2 || m.Steps[0].Title != "Step one" || !m.Steps[0].OwnLesson || m.Steps[0].Eligible ||
		m.Steps[1].OwnLesson || !m.Steps[1].Eligible || len(saved.Modules[1].Steps) != 0 {
		t.Fatalf("steps: %+v", saved.Modules)
	}
	got, err := e.store.Revision(e.ctx, e.owner, rev.ID)
	if err != nil || got.Version != 2 || got.Modules[0].Key != m.Key {
		t.Fatalf("read back: %+v %v", got, err)
	}
	// Module keys are kept when sent back.
	d.Modules[0].Key = m.Key
	saved2 := e.mustSave(saved, d)
	if saved2.Modules[0].Key != m.Key {
		t.Fatalf("module key changed: %s -> %s", m.Key, saved2.Modules[0].Key)
	}

	// Another author sees nothing.
	_, err = e.store.Revision(e.ctx, e.other, rev.ID)
	wantErr(t, "other reads", err, courses.ErrNotFound)
	_, err = e.store.SaveDraft(e.ctx, e.other, rev.ID, saved2.Version, d)
	wantErr(t, "other saves", err, courses.ErrNotFound)
	wantErr(t, "other discards", e.store.DiscardDraft(e.ctx, e.other, rev.ID, saved2.Version), courses.ErrNotFound)
	wantErr(t, "other deletes", e.store.DeleteCourse(e.ctx, e.other, c.ID), courses.ErrNotFound)
	_, err = e.store.CreateDraft(e.ctx, e.other, c.ID)
	wantErr(t, "other drafts", err, courses.ErrNotFound)
	if list, _, err := e.store.Owned(e.ctx, e.other, 0); err != nil || len(list) != 0 {
		t.Fatalf("other's list: %+v %v", list, err)
	}
	list, _, err := e.store.Owned(e.ctx, e.owner, 0)
	if err != nil || len(list) != 1 || list[0].Latest == nil || list[0].Latest.Title != "TEST course" {
		t.Fatalf("owner's list: %+v %v", list, err)
	}

	// A second draft while one is open.
	_, err = e.store.CreateDraft(e.ctx, e.owner, c.ID)
	wantErr(t, "second draft", err, courses.ErrActiveCandidateExists)

	// Discard, then a new draft copies the latest remaining revision (none
	// here: it starts empty) with a new number.
	if err := e.store.DiscardDraft(e.ctx, e.owner, rev.ID, saved2.Version); err != nil {
		t.Fatal(err)
	}
	fresh, err := e.store.CreateDraft(e.ctx, e.owner, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Number != 2 || fresh.Title != "" || len(fresh.Modules) != 0 {
		t.Fatalf("fresh draft: %+v", fresh)
	}

	// Without the author permission: denied.
	if _, err := e.perms.Revoke(e.ctx, testOperator, e.owner, permissions.Author); err != nil {
		t.Fatal(err)
	}
	_, err = e.store.Revision(e.ctx, e.owner, fresh.ID)
	wantErr(t, "revoked", err, courses.ErrDenied)
}

func TestCreateDraftCopiesLatestRevision(t *testing.T) {
	e := newEnv(t)
	ch := e.chapter(e.math, "ch-01")
	p := e.edition(e.math, []int64{ch}, true)
	own := e.lessonDraft(e.owner, e.math, "fr", "Own lesson")
	cover := e.media(e.owner)
	alt := "TEST alt"
	c, rev := e.course(e.math, "fr")
	saved := e.mustSave(rev, courses.Draft{Title: "TEST copied", CoverMediaID: &cover, CoverAlt: &alt, AssignmentIDs: []int64{p.assignment},
		Modules: []courses.ModuleInput{{Title: "TEST m", ChapterID: &ch, Steps: []courses.StepInput{step(own, "s")}}}})
	// Changes requested (B04 path, simulated): the correction draft copies it.
	e.forceStatus(saved.ID, `status = 'changes_requested', submitted_at = now()`)
	next, err := e.store.CreateDraft(e.ctx, e.owner, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if next.Number != 2 || next.Title != "TEST copied" || next.CoverMediaID == nil || len(next.Assignments) != 1 ||
		len(next.Modules) != 1 || next.Modules[0].Key != saved.Modules[0].Key || len(next.Modules[0].Steps) != 1 {
		t.Fatalf("copied draft: %+v", next)
	}
	// The submitted revision is unchanged.
	old, err := e.store.Revision(e.ctx, e.owner, saved.ID)
	if err != nil || old.Version != saved.Version || len(old.Modules) != 1 {
		t.Fatalf("history changed: %+v %v", old, err)
	}
	// A course with history is never deleted; its draft can be discarded.
	wantErr(t, "delete with history", e.store.DeleteCourse(e.ctx, e.owner, c.ID), courses.ErrHasHistory)
	if err := e.store.DiscardDraft(e.ctx, e.owner, next.ID, next.Version); err != nil {
		t.Fatal(err)
	}
}

func TestStaleSaveOneWins(t *testing.T) {
	e := newEnv(t)
	_, rev := e.course(e.math, "fr")
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.save(rev, courses.Draft{Title: "TEST writer"})
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, courses.ErrVersionConflict):
			t.Errorf("loser: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d saves won", ok)
	}
	got, _ := e.store.Revision(e.ctx, e.owner, rev.ID)
	if got.Version != 2 {
		t.Fatalf("version %d", got.Version)
	}
}

func TestConcurrentDraftCreationExactlyOne(t *testing.T) {
	e := newEnv(t)
	c, rev := e.course(e.math, "fr")
	if err := e.store.DiscardDraft(e.ctx, e.owner, rev.ID, rev.Version); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.store.CreateDraft(e.ctx, e.owner, c.ID)
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, courses.ErrActiveCandidateExists):
			t.Errorf("loser: %v", err)
		}
	}
	if ok != 1 || e.count(`SELECT count(*) FROM course_revisions WHERE course_id = $1`, c.ID) != 1 {
		t.Fatalf("%d drafts created", ok)
	}
}

func TestStepRules(t *testing.T) {
	e := newEnv(t)
	_, rev := e.course(e.math, "fr")
	own := e.lessonDraft(e.owner, e.math, "fr", "Own")
	ownAr := e.lessonDraft(e.owner, e.math, "ar", "Own Arabic")
	ownPhysics := e.lessonDraft(e.owner, e.physics, "fr", "Own physics")
	otherDraft := e.lessonDraft(e.other, e.math, "fr", "Other unpublished")
	otherPub := e.publishedLesson(e.other, e.math, "fr", "Other published")
	// An approved revision of another author's published lesson that was
	// never published itself is not eligible.
	otherNext, err := e.lessons.CreateDraft(e.ctx, e.other, otherPub.LessonID, lessons.Fields{Content: doc("Other next", "fr")})
	if err != nil {
		t.Fatal(err)
	}
	e.approve(otherNext)
	ownOther := e.lessonDraft(e.owner, e.math, "fr", "Own second")

	one := func(steps ...courses.StepInput) courses.Draft {
		return courses.Draft{Modules: []courses.ModuleInput{{Title: "TEST m", Steps: steps}}}
	}
	for name, d := range map[string]courses.Draft{
		"duplicate lesson":             one(step(own, "a"), step(own, "b")),
		"duplicate across modules":     {Modules: []courses.ModuleInput{{Title: "m1", Steps: []courses.StepInput{step(own, "a")}}, {Title: "m2", Steps: []courses.StepInput{step(own, "b")}}}},
		"cross-language":               one(step(ownAr, "a")),
		"cross-subject":                one(step(ownPhysics, "a")),
		"other's unpublished lesson":   one(step(otherDraft, "a")),
		"other's unpublished revision": one(step(otherNext, "a")),
		"revision of another lesson":   one(courses.StepInput{LessonID: own.LessonID, RevisionID: ownOther.ID, Title: "a"}),
		"unknown lesson":               one(courses.StepInput{LessonID: 999999, RevisionID: own.ID, Title: "a"}),
	} {
		_, err := e.save(rev, d)
		wantErr(t, name, err, courses.ErrInvalidStep)
	}
	_, err = e.save(rev, one(courses.StepInput{LessonID: own.LessonID, RevisionID: own.ID, Title: " "}))
	wantErr(t, "blank step title", err, courses.ErrInvalidInput)
	if got, _ := e.store.Revision(e.ctx, e.owner, rev.ID); got.Version != 1 || len(got.Modules) != 0 {
		t.Fatalf("refused saves changed the draft: %+v", got)
	}

	// Allowed: any revision of the owner's lesson, the eligible revision of
	// another author's published lesson.
	saved := e.mustSave(rev, one(step(own, "a"), step(otherPub, "b")))
	// Once the other lesson is unpublished, its step no longer saves.
	e.unpublish(otherPub.LessonID)
	_, err = e.save(saved, one(step(own, "a"), step(otherPub, "b")))
	wantErr(t, "unpublished after attach", err, courses.ErrInvalidStep)
	got, _ := e.store.Revision(e.ctx, e.owner, rev.ID)
	if got.Modules[0].Steps[1].Eligible {
		t.Fatal("unpublished lesson's step reported eligible")
	}
}

func TestPlacementRules(t *testing.T) {
	e := newEnv(t)
	ch1, ch2, ch3 := e.chapter(e.math, "ch-01"), e.chapter(e.math, "ch-02"), e.chapter(e.math, "ch-03")
	physCh := e.chapter(e.physics, "ch-01")
	cur := e.edition(e.math, []int64{ch1, ch2}, true)
	draftEd := e.edition(e.math, []int64{ch3}, false)
	phys := e.edition(e.physics, []int64{physCh}, true)
	// A superseded edition: make a successor current in the same section.
	old := e.edition(e.math, []int64{ch3}, true)
	e.editionIn(old.level, old.section, e.math, []int64{ch1}, true)
	retiredEd := e.edition(e.math, []int64{ch3}, true)
	if _, err := e.cur.RetireEdition(e.ctx, e.admin, retiredEd.edition.ID, e.versionOf(retiredEd.edition.ID)); err != nil {
		t.Fatal(err)
	}
	_, rev := e.course(e.math, "fr")

	mod := func(ch *int64) []courses.ModuleInput { return []courses.ModuleInput{{Title: "TEST m", ChapterID: ch}} }
	cases := []struct {
		name string
		d    courses.Draft
		want error
	}{
		{"draft edition", courses.Draft{AssignmentIDs: []int64{draftEd.assignment}}, courses.ErrPlacementInvalid},
		{"superseded edition, newly selected", courses.Draft{AssignmentIDs: []int64{old.assignment}}, courses.ErrPlacementInvalid},
		{"retired edition", courses.Draft{AssignmentIDs: []int64{retiredEd.assignment}}, courses.ErrPlacementInvalid},
		{"other subject's assignment", courses.Draft{AssignmentIDs: []int64{phys.assignment}}, courses.ErrPlacementInvalid},
		{"module chapter outside assignments", courses.Draft{AssignmentIDs: []int64{cur.assignment}, Modules: mod(&ch3)}, courses.ErrPlacementInvalid},
		{"module chapter without assignments", courses.Draft{Modules: mod(&ch1)}, courses.ErrPlacementInvalid},
		{"module chapter of another subject", courses.Draft{AssignmentIDs: []int64{cur.assignment}, Modules: mod(&physCh)}, courses.ErrPlacementInvalid},
		{"duplicate assignment", courses.Draft{AssignmentIDs: []int64{cur.assignment, cur.assignment}}, courses.ErrPlacementInvalid},
	}
	for _, tc := range cases {
		_, err := e.save(rev, tc.d)
		wantErr(t, tc.name, err, tc.want)
	}
	saved := e.mustSave(rev, courses.Draft{AssignmentIDs: []int64{cur.assignment}, Modules: mod(&ch2)})

	// Retiring the chapter afterwards: a later save refuses it (not inherited
	// from a comparison revision).
	if err := e.cur.RetireChapter(e.ctx, e.admin, ch2); err != nil {
		t.Fatal(err)
	}
	_, err := e.save(saved, courses.Draft{AssignmentIDs: []int64{cur.assignment}, Modules: mod(&ch2)})
	wantErr(t, "retired chapter", err, courses.ErrPlacementInvalid)

	// Retiring the edition: the placement is flagged on read and refused on
	// the next save.
	if _, err := e.cur.RetireEdition(e.ctx, e.admin, cur.edition.ID, e.versionOf(cur.edition.ID)); err != nil {
		t.Fatal(err)
	}
	got, _ := e.store.Revision(e.ctx, e.owner, rev.ID)
	if !got.Assignments[0].Retired || got.Assignments[0].Selectable || got.Assignments[0].EditionState != "retired" {
		t.Fatalf("retired placement not flagged: %+v", got.Assignments)
	}
	_, err = e.save(saved, courses.Draft{AssignmentIDs: []int64{cur.assignment}})
	wantErr(t, "retired assignment at save", err, courses.ErrPlacementInvalid)
}

func (e *env) versionOf(edition int64) int {
	return e.count(`SELECT version FROM curriculum_editions WHERE id = $1`, edition)
}

func TestCoverAndFieldRules(t *testing.T) {
	e := newEnv(t)
	_, rev := e.course(e.math, "fr")
	mine, theirs := e.media(e.owner), e.media(e.other)
	alt := "TEST alt"
	_, err := e.save(rev, courses.Draft{CoverMediaID: &theirs, CoverAlt: &alt})
	wantErr(t, "someone else's upload", err, courses.ErrUnknownMedia)
	_, err = e.save(rev, courses.Draft{CoverMediaID: &mine})
	wantErr(t, "cover without alt", err, courses.ErrInvalidInput)
	_, err = e.save(rev, courses.Draft{CoverAlt: &alt})
	wantErr(t, "alt without cover", err, courses.ErrInvalidInput)
	long := make([]byte, 201)
	for i := range long {
		long[i] = 'a'
	}
	for name, d := range map[string]courses.Draft{
		"long title":       {Title: string(long)},
		"multi-line title": {Title: "a\nb"},
		"bidi control":     {Summary: "a‮b"},
		"NUL":              {Objectives: "a\x00b"},
		"line separator":   {Prerequisites: "a b"},
		"blank module":     {Modules: []courses.ModuleInput{{Title: "  "}}},
		"bad module key":   {Modules: []courses.ModuleInput{{Key: "NOT-A-UUID", Title: "m"}}},
		"duplicate key":    {Modules: []courses.ModuleInput{{Key: "00000000-0000-4000-8000-000000000001", Title: "a"}, {Key: "00000000-0000-4000-8000-000000000001", Title: "b"}}},
	} {
		_, err := e.save(rev, d)
		wantErr(t, name, err, courses.ErrInvalidInput)
	}
	too := make([]courses.ModuleInput, courses.MaxModules+1)
	for i := range too {
		too[i] = courses.ModuleInput{Title: "m"}
	}
	_, err = e.save(rev, courses.Draft{Modules: too})
	wantErr(t, "too many modules", err, courses.ErrInvalidInput)
	saved := e.mustSave(rev, courses.Draft{CoverMediaID: &mine, CoverAlt: &alt})
	if saved.CoverMediaID == nil || *saved.CoverMediaID != mine {
		t.Fatalf("cover: %+v", saved)
	}
	_, _, err = e.store.CreateCourse(e.ctx, e.owner, e.math, "en")
	wantErr(t, "language", err, courses.ErrInvalidInput)
	_, _, err = e.store.CreateCourse(e.ctx, e.owner, 999999, "fr")
	wantErr(t, "subject", err, courses.ErrUnknownSubject)
	_, _, err = e.store.CreateCourse(e.ctx, e.other+e.owner+1000, e.math, "fr")
	wantErr(t, "no permission", err, courses.ErrDenied)
}

// Deleting a never-submitted course removes course rows only.
func TestDeleteKeepsLessonsAndMedia(t *testing.T) {
	e := newEnv(t)
	ch := e.chapter(e.math, "ch-01")
	p := e.edition(e.math, []int64{ch}, true)
	own := e.lessonDraft(e.owner, e.math, "fr", "Own")
	foreign := e.publishedLesson(e.other, e.math, "fr", "Foreign")
	cover := e.media(e.owner)
	alt := "TEST alt"
	c, rev := e.course(e.math, "fr")
	e.mustSave(rev, courses.Draft{CoverMediaID: &cover, CoverAlt: &alt, AssignmentIDs: []int64{p.assignment},
		Modules: []courses.ModuleInput{{Title: "m", ChapterID: &ch, Steps: []courses.StepInput{step(own, "a"), step(foreign, "b")}}}})
	before := e.count(`SELECT (SELECT count(*) FROM lessons) + (SELECT count(*) FROM lesson_revisions) + (SELECT count(*) FROM media_assets)
		+ (SELECT count(*) FROM media_uploads) + (SELECT count(*) FROM curriculum_assignments)`)
	if err := e.store.DeleteCourse(e.ctx, e.owner, c.ID); err != nil {
		t.Fatal(err)
	}
	after := e.count(`SELECT (SELECT count(*) FROM lessons) + (SELECT count(*) FROM lesson_revisions) + (SELECT count(*) FROM media_assets)
		+ (SELECT count(*) FROM media_uploads) + (SELECT count(*) FROM curriculum_assignments)`)
	if before != after {
		t.Fatalf("delete removed shared rows: %d -> %d", before, after)
	}
	if n := e.count(`SELECT (SELECT count(*) FROM courses) + (SELECT count(*) FROM course_revisions) + (SELECT count(*) FROM course_modules)
		+ (SELECT count(*) FROM course_steps) + (SELECT count(*) FROM course_revision_assignments)`); n != 0 {
		t.Fatalf("%d course rows left", n)
	}
	wantErr(t, "delete again", e.store.DeleteCourse(e.ctx, e.owner, c.ID), courses.ErrNotFound)
}

// A lesson draft pinned by a course draft cannot be discarded.
func TestLessonDraftDiscardWhileReferenced(t *testing.T) {
	e := newEnv(t)
	own := e.lessonDraft(e.owner, e.math, "fr", "Own")
	_, rev := e.course(e.math, "fr")
	saved := e.mustSave(rev, courses.Draft{Modules: []courses.ModuleInput{{Title: "m", Steps: []courses.StepInput{step(own, "a")}}}})
	err := e.lessons.DiscardDraft(e.ctx, e.owner, own.ID, own.Version)
	wantErr(t, "discard referenced", err, lessons.ErrReferencedByCourse)
	e.mustSave(saved, courses.Draft{})
	if err := e.lessons.DiscardDraft(e.ctx, e.owner, own.ID, own.Version); err != nil {
		t.Fatalf("discard after removal: %v", err)
	}
}

func TestAttachableLessonsAndEligibleRevisions(t *testing.T) {
	e := newEnv(t)
	c, _ := e.course(e.math, "fr")
	own := e.lessonDraft(e.owner, e.math, "fr", "Algèbre propre")
	otherPub := e.publishedLesson(e.other, e.math, "fr", "Algebre publiee")
	e.lessonDraft(e.other, e.math, "fr", "Algebre brouillon")       // unpublished: hidden
	e.publishedLesson(e.other, e.math, "ar", "Algebre arabe")       // other language: hidden
	e.publishedLesson(e.other, e.physics, "fr", "Algebre physique") // other subject: hidden
	list, next, err := e.store.AttachableLessons(e.ctx, e.owner, c.ID, "", 0)
	if err != nil || next != nil || len(list) != 2 || list[0].LessonID != own.LessonID || !list[0].Own || list[0].Latest == nil ||
		list[1].LessonID != otherPub.LessonID || list[1].Own || list[1].Latest != nil {
		t.Fatalf("attachable: %+v %v %v", list, next, err)
	}
	// Folded search: "ALGEBRE" matches "Algèbre".
	list, _, err = e.store.AttachableLessons(e.ctx, e.owner, c.ID, "ALGEBRE PROPRE", 0)
	if err != nil || len(list) != 1 || list[0].LessonID != own.LessonID {
		t.Fatalf("search: %+v %v", list, err)
	}
	list, _, err = e.store.AttachableLessons(e.ctx, e.owner, c.ID, "", own.LessonID)
	if err != nil || len(list) != 1 || list[0].LessonID != otherPub.LessonID {
		t.Fatalf("cursor: %+v %v", list, err)
	}
	_, _, err = e.store.AttachableLessons(e.ctx, e.other, c.ID, "", 0)
	wantErr(t, "other's course", err, courses.ErrNotFound)

	revs, err := e.store.EligibleRevisions(e.ctx, e.owner, otherPub.LessonID)
	if err != nil || len(revs) != 1 || revs[0].ID != otherPub.ID || !revs[0].Eligible {
		t.Fatalf("eligible: %+v %v", revs, err)
	}
	revs, err = e.store.EligibleRevisions(e.ctx, e.owner, own.LessonID)
	if err != nil || len(revs) != 1 || revs[0].Eligible {
		t.Fatalf("own revisions: %+v %v", revs, err)
	}
	_, err = e.store.EligibleRevisions(e.ctx, e.other, own.LessonID)
	wantErr(t, "unpublished lesson of another", err, courses.ErrNotFound)
}
