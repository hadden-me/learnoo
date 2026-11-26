// Package coursetest builds published TEST courses for database-backed tests
// of the reader and error reports (LMS-B08). It goes through the real
// stores (curriculum, courses), so every database guard applies. It is
// imported only by tests.
package coursetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/hadden-me/learnoo/internal/courses"
	"github.com/hadden-me/learnoo/internal/curriculum"
	"github.com/hadden-me/learnoo/internal/permissions"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Step pins one published lesson revision under a course-authored title.
type Step struct {
	LessonID   int64
	RevisionID int64
	Title      string
}

// Module is a titled group of steps.
type Module struct {
	Title string
	Steps []Step
}

// Spec describes a course. Admin is an active administrator (curriculum),
// Owner an active author, Reviewer an eligible course reviewer of the
// subject (neither the owner nor the author of a pinned lesson revision),
// Publisher an active publisher. Every pinned revision must be eligible.
type Spec struct {
	Admin, Owner, Reviewer, Publisher int64
	SubjectID                         int64
	Language                          string // "fr" when empty
	Title                             string
	Summary                           string
	Modules                           []Module
	// SectionID places the course in the current edition of an existing
	// section; zero creates a new TEST level, section and current edition.
	SectionID int64
}

// Course is a published course.
type Course struct {
	ID             int64
	PublicID       string
	RevisionID     int64
	RevisionNumber int
	LevelID        int64
	SectionID      int64
	EditionID      int64
	AssignmentID   int64
}

func code(prefix string) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}

// Section creates a TEST level and section with a current edition assigning
// subjectID, named with the given French labels, and returns the level,
// section, edition and assignment IDs.
func Section(t testing.TB, db *pgxpool.Pool, admin, subjectID int64, levelFR, sectionFR string) (level, section, edition, assignment int64) {
	t.Helper()
	ctx := context.Background()
	cur := curriculum.NewStore(db)
	actor := permissions.UserActor(admin)
	lv, err := cur.CreateLevel(ctx, actor, curriculum.Entry{Code: code("test-l"), NameFR: levelFR, NameAR: "مستوى تجريبي"})
	if err != nil {
		t.Fatal(err)
	}
	sec, err := cur.CreateSection(ctx, actor, lv.ID, curriculum.Entry{Code: code("test-s"), NameFR: sectionFR, NameAR: "شعبة تجريبية"})
	if err != nil {
		t.Fatal(err)
	}
	ed, err := cur.CreateEdition(ctx, actor, sec.ID, curriculum.NewEdition{Code: code("test-e"), LabelFR: "TEST édition", LabelAR: "طبعة تجريبية"})
	if err != nil {
		t.Fatal(err)
	}
	ed, asg, err := cur.AddAssignment(ctx, actor, ed.ID, ed.Version, subjectID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = cur.MakeCurrent(ctx, actor, ed.ID, ed.Version, nil); err != nil {
		t.Fatal(err)
	}
	return lv.ID, sec.ID, ed.ID, asg.ID
}

func (s Spec) draft(assignment int64) courses.Draft {
	title, summary := s.Title, s.Summary
	if title == "" {
		title = "TEST cours"
	}
	if summary == "" {
		summary = "Résumé de test."
	}
	d := courses.Draft{Title: title, Summary: summary, Objectives: "Objectifs de test.", AssignmentIDs: []int64{assignment}}
	for _, m := range s.Modules {
		mi := courses.ModuleInput{Title: m.Title}
		for _, st := range m.Steps {
			mi.Steps = append(mi.Steps, courses.StepInput{LessonID: st.LessonID, RevisionID: st.RevisionID, Title: st.Title})
		}
		d.Modules = append(d.Modules, mi)
	}
	return d
}

// Publish creates the course and takes its first revision through
// submission, approval and publication.
func Publish(t testing.TB, db *pgxpool.Pool, s Spec) Course {
	t.Helper()
	ctx := context.Background()
	var c Course
	if s.SectionID == 0 {
		c.LevelID, c.SectionID, c.EditionID, c.AssignmentID = Section(t, db, s.Admin, s.SubjectID, "TEST Niveau", "TEST Section")
	} else {
		c.SectionID = s.SectionID
		if err := db.QueryRow(ctx, `
			SELECT sec.level_id, e.id, a.id
			  FROM academic_sections sec
			  JOIN curriculum_editions e ON e.section_id = sec.id AND e.state = 'current'
			  JOIN curriculum_assignments a ON a.edition_id = e.id AND a.subject_id = $2
			 WHERE sec.id = $1`, s.SectionID, s.SubjectID).Scan(&c.LevelID, &c.EditionID, &c.AssignmentID); err != nil {
			t.Fatal(err)
		}
	}
	lang := s.Language
	if lang == "" {
		lang = "fr"
	}
	store := courses.NewStore(db)
	course, rev, err := store.CreateCourse(ctx, s.Owner, s.SubjectID, lang)
	if err != nil {
		t.Fatal(err)
	}
	c.ID, c.PublicID = course.ID, course.PublicID
	c.RevisionID, c.RevisionNumber = publishDraft(t, db, s, rev, c.AssignmentID, nil)
	return c
}

func publishDraft(t testing.TB, db *pgxpool.Pool, s Spec, rev courses.Revision, assignment int64, expected *int64) (int64, int) {
	t.Helper()
	ctx := context.Background()
	store := courses.NewStore(db)
	rev, err := store.SaveDraft(ctx, s.Owner, rev.ID, rev.Version, s.draft(assignment))
	if err != nil {
		t.Fatalf("save course draft: %v", err)
	}
	if rev, err = store.Submit(ctx, s.Owner, rev.ID, rev.Version); err != nil {
		t.Fatalf("submit course: %v", err)
	}
	if _, err = store.Approve(ctx, s.Reviewer, rev.ID, ""); err != nil {
		t.Fatalf("approve course: %v", err)
	}
	if _, err = store.Publish(ctx, s.Publisher, rev.ID, expected); err != nil {
		t.Fatalf("publish course: %v", err)
	}
	return rev.ID, rev.Number
}

// Republish publishes a successor revision of c with the content of s (same
// placement) and returns the updated course.
func Republish(t testing.TB, db *pgxpool.Pool, c Course, s Spec) Course {
	t.Helper()
	ctx := context.Background()
	rev, err := courses.NewStore(db).CreateDraft(ctx, s.Owner, c.ID)
	if err != nil {
		t.Fatalf("create course draft: %v", err)
	}
	prev := c.RevisionID
	c.RevisionID, c.RevisionNumber = publishDraft(t, db, s, rev, c.AssignmentID, &prev)
	return c
}

// Suspend suspends c as admin (the course's emergency unpublication).
func Suspend(t testing.TB, db *pgxpool.Pool, admin int64, c Course) {
	t.Helper()
	cur := c.RevisionID
	if _, err := courses.NewStore(db).Suspend(context.Background(), admin, c.ID, &cur, "TEST suspension"); err != nil {
		t.Fatalf("suspend course: %v", err)
	}
}

// Archive archives c as admin.
func Archive(t testing.TB, db *pgxpool.Pool, admin int64, c Course) {
	t.Helper()
	cur := c.RevisionID
	if _, err := courses.NewStore(db).Archive(context.Background(), admin, c.ID, &cur, "TEST archive"); err != nil {
		t.Fatalf("archive course: %v", err)
	}
}
