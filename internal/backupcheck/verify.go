// Package backupcheck verifies a restored Learnoo database and media
// directory (LRN-010): migrations are current, every media asset row has its
// file with the recorded size and SHA-256, unexpected files are reported,
// lesson and course publication pointers are consistent and every submitted
// lesson and course revision has its submission audit row. It only reads;
// it never repairs or deletes anything.
package backupcheck

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/hadden-me/learnoo/internal/database"
	"github.com/hadden-me/learnoo/internal/media"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TableCount is the number of rows in one table of the application schema.
type TableCount struct {
	Table string
	Rows  int64
}

// Report is the outcome of Verify. Problems make the verification fail;
// unexpected and temporary files are reported only (they fail it in strict
// mode, see Failed).
type Report struct {
	Counts     []TableCount
	Assets     int      // media_assets rows checked
	Referenced int      // distinct assets referenced by revision_media
	Published  int      // lessons with a published pointer
	Courses    int      // courses with a published pointer
	Problems   []string // failed checks
	Unexpected []string // files in the media directory no asset row names
	Temporary  []string // leftover ".tmp-" files from interrupted writes
}

// Failed reports whether the verification failed. In strict mode unexpected
// and temporary files fail it too.
func (r *Report) Failed(strict bool) bool {
	return len(r.Problems) > 0 || (strict && len(r.Unexpected)+len(r.Temporary) > 0)
}

func (r *Report) problem(format string, args ...any) {
	r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
}

// Verify checks the database behind pool (its search_path schema) against the
// media directory mediaDir and the embedded migrations migFS. The returned
// error is reserved for failures to run the checks (connection, I/O on the
// directory itself); findings are in the Report.
func Verify(ctx context.Context, pool *pgxpool.Pool, mediaDir string, migFS fs.FS) (*Report, error) {
	r := &Report{}
	if err := database.Check(ctx, pool, migFS); err != nil {
		// Without the current schema the other queries may not apply.
		r.problem("migrations: %v", err)
		return r, nil
	}
	root, err := os.OpenRoot(mediaDir)
	if err != nil {
		return nil, fmt.Errorf("open media directory: %w", err)
	}
	defer root.Close()

	if r.Counts, err = countRows(ctx, pool); err != nil {
		return nil, err
	}
	expected, err := checkAssets(ctx, pool, root, r)
	if err != nil {
		return nil, err
	}
	if err := scanFiles(root, expected, r); err != nil {
		return nil, err
	}
	if err := checkPublication(ctx, pool, r); err != nil {
		return nil, err
	}
	if err := checkSubmissions(ctx, pool, r); err != nil {
		return nil, err
	}
	if err := checkCourses(ctx, pool, r); err != nil {
		return nil, err
	}
	return r, nil
}

func countRows(ctx context.Context, pool *pgxpool.Pool) ([]TableCount, error) {
	rows, err := pool.Query(ctx, `
		SELECT tablename FROM pg_catalog.pg_tables
		 WHERE schemaname = current_schema() ORDER BY tablename`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	out := make([]TableCount, 0, len(names))
	for _, n := range names {
		var c int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{n}.Sanitize()).Scan(&c); err != nil {
			return nil, fmt.Errorf("count %s: %w", n, err)
		}
		out = append(out, TableCount{Table: n, Rows: c})
	}
	return out, nil
}

// checkAssets verifies every media_assets row against its file and returns
// the set of expected relative paths.
func checkAssets(ctx context.Context, pool *pgxpool.Pool, root *os.Root, r *Report) (map[string]bool, error) {
	rows, err := pool.Query(ctx, `
		SELECT a.id, a.sha256, a.byte_size, a.content_type, a.storage_key,
		       EXISTS (SELECT 1 FROM revision_media m WHERE m.media_asset_id = a.id)
		  FROM media_assets a ORDER BY a.id`)
	if err != nil {
		return nil, fmt.Errorf("read media_assets: %w", err)
	}
	type asset struct {
		id         int64
		sum        []byte
		size       int64
		ctype, key string
		referenced bool
	}
	assets, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (asset, error) {
		var a asset
		err := row.Scan(&a.id, &a.sum, &a.size, &a.ctype, &a.key, &a.referenced)
		return a, err
	})
	if err != nil {
		return nil, fmt.Errorf("read media_assets: %w", err)
	}
	expected := map[string]bool{}
	for _, a := range assets {
		r.Assets++
		if a.referenced {
			r.Referenced++
		}
		label := fmt.Sprintf("media asset %d (%s)", a.id, a.key)
		rel, err := media.RelPath(a.key)
		if err != nil {
			r.problem("%s: storage key is not a valid content key", label)
			continue
		}
		expected[rel] = true
		if want, _ := media.ContentTypeForKey(a.key); want != a.ctype {
			r.problem("%s: content type %s does not match the key", label, a.ctype)
		}
		if !strings.HasPrefix(a.key, hex.EncodeToString(a.sum)+".") {
			r.problem("%s: recorded SHA-256 does not match the key", label)
		}
		sum, size, err := hashFile(root, rel)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			r.problem("%s: file %s is missing", label, rel)
		case err != nil:
			r.problem("%s: file %s unreadable: %v", label, rel, err)
		case size != a.size:
			r.problem("%s: file %s has %d bytes, recorded %d", label, rel, size, a.size)
		case !bytes.Equal(sum, a.sum):
			r.problem("%s: file %s does not match the recorded SHA-256", label, rel)
		}
	}
	return expected, nil
}

// hashFile returns the SHA-256 and size of the regular file rel; anything
// else (directory, symbolic link, device) is an error.
func hashFile(root *os.Root, rel string) ([]byte, int64, error) {
	fi, err := root.Lstat(rel)
	if err != nil {
		return nil, 0, err
	}
	if !fi.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("not a regular file (%s)", fi.Mode().Type())
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return nil, 0, err
	}
	return h.Sum(nil), n, nil
}

// scanFiles lists every non-directory entry no asset row expects.
func scanFiles(root *os.Root, expected map[string]bool, r *Report) error {
	err := fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch {
		case expected[p] && d.Type().IsRegular():
		case strings.HasPrefix(path.Base(p), media.TempPrefix) && d.Type().IsRegular():
			r.Temporary = append(r.Temporary, p)
		default:
			r.Unexpected = append(r.Unexpected, p)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("scan media directory: %w", err)
	}
	sort.Strings(r.Unexpected)
	sort.Strings(r.Temporary)
	return nil
}

// checkPublication verifies each published pointer names an approved,
// non-withdrawn revision of its own lesson, and that the latest pointer
// audit row of every audited lesson matches the pointer.
func checkPublication(ctx context.Context, pool *pgxpool.Pool, r *Report) error {
	rows, err := pool.Query(ctx, `
		SELECT l.id, l.published_revision_id,
		       CASE
		         WHEN rv.id IS NULL THEN 'names a missing revision or one of another lesson'
		         WHEN rv.status <> 'approved' THEN 'names a revision with status ' || rv.status
		         WHEN EXISTS (SELECT 1 FROM revision_withdrawals w WHERE w.revision_id = rv.id) THEN 'names a withdrawn revision'
		       END
		  FROM lessons l
		  LEFT JOIN lesson_revisions rv ON rv.id = l.published_revision_id AND rv.lesson_id = l.id
		 WHERE l.published_revision_id IS NOT NULL
		UNION ALL
		SELECT l.id, l.published_revision_id, 'differs from the latest publication audit row'
		  FROM lessons l
		  JOIN LATERAL (SELECT p.revision_id FROM publication_audit p
		                 WHERE p.lesson_id = l.id ORDER BY p.id DESC LIMIT 1) last ON true
		 WHERE last.revision_id IS DISTINCT FROM l.published_revision_id
		ORDER BY 1`)
	if err != nil {
		return fmt.Errorf("check publication pointers: %w", err)
	}
	type finding struct {
		lesson int64
		rev    *int64
		what   *string
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (finding, error) {
		var f finding
		err := row.Scan(&f.lesson, &f.rev, &f.what)
		return f, err
	})
	if err != nil {
		return fmt.Errorf("check publication pointers: %w", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM lessons WHERE published_revision_id IS NOT NULL`).Scan(&r.Published); err != nil {
		return fmt.Errorf("count published lessons: %w", err)
	}
	for _, f := range found {
		if f.what == nil {
			continue
		}
		rev := "none"
		if f.rev != nil {
			rev = fmt.Sprint(*f.rev)
		}
		r.problem("lesson %d: published pointer (%s) %s", f.lesson, rev, *f.what)
	}
	return nil
}

// checkSubmissions verifies that every submitted revision (any status but
// draft) has its submission audit row (migrations/0011), recording its
// author and its submitted_at, and that no draft has one.
func checkSubmissions(ctx context.Context, pool *pgxpool.Pool, r *Report) error {
	rows, err := pool.Query(ctx, `
		SELECT rv.id,
		       CASE
		         WHEN rv.status = 'draft' THEN 'is a draft with a submission audit row'
		         WHEN a.id IS NULL THEN 'has no submission audit row'
		         WHEN a.lesson_id <> rv.lesson_id OR a.actor_id <> rv.author_id OR a.submitted_at IS DISTINCT FROM rv.submitted_at
		           THEN 'differs from its submission audit row'
		       END
		  FROM lesson_revisions rv
		  LEFT JOIN submission_audit a ON a.revision_id = rv.id
		 WHERE rv.status <> 'draft' OR a.id IS NOT NULL
		 ORDER BY rv.id`)
	if err != nil {
		return fmt.Errorf("check submissions: %w", err)
	}
	type finding struct {
		rev  int64
		what *string
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (finding, error) {
		var f finding
		err := row.Scan(&f.rev, &f.what)
		return f, err
	})
	if err != nil {
		return fmt.Errorf("check submissions: %w", err)
	}
	for _, f := range found {
		if f.what != nil {
			r.problem("revision %d: %s", f.rev, *f.what)
		}
	}
	return nil
}

// checkCourses applies the lesson checks to courses (migrations/0014 and
// 0015): each published pointer names an approved revision of its own course
// that was not withdrawn, on a course that is not archived, and equals the
// latest course publication audit row (publish, restore, suspend and archive
// all write one); every submitted course revision has its submission audit
// row, recording its course, author and submitted_at, and no draft has one.
func checkCourses(ctx context.Context, pool *pgxpool.Pool, r *Report) error {
	rows, err := pool.Query(ctx, `
		SELECT 'course ' || c.id || ': published pointer (' || c.published_revision_id || ') ' ||
		       CASE
		         WHEN rv.id IS NULL THEN 'names a missing revision or one of another course'
		         WHEN rv.status <> 'approved' THEN 'names a revision with status ' || rv.status
		         WHEN rv.withdrawn_at IS NOT NULL THEN 'names a withdrawn revision'
		         -- Unreachable while the CHECK courses_archived_unpublished
		         -- (0015) holds; kept in case that constraint is ever dropped.
		         ELSE 'is set on an archived course'
		       END
		  FROM courses c
		  LEFT JOIN course_revisions rv ON rv.id = c.published_revision_id AND rv.course_id = c.id
		 WHERE c.published_revision_id IS NOT NULL
		   AND (rv.id IS NULL OR rv.status <> 'approved' OR rv.withdrawn_at IS NOT NULL OR c.archived_at IS NOT NULL)
		UNION ALL
		SELECT 'course ' || c.id || ': published pointer (' || coalesce(c.published_revision_id::text, 'none') ||
		       ') differs from the latest course publication audit row'
		  FROM courses c
		  JOIN LATERAL (SELECT p.revision_id FROM course_publication_audit p
		                 WHERE p.course_id = c.id ORDER BY p.id DESC LIMIT 1) last ON true
		 WHERE last.revision_id IS DISTINCT FROM c.published_revision_id
		UNION ALL
		SELECT 'course ' || c.id || ': published pointer (' || c.published_revision_id || ') has no course publication audit row'
		  FROM courses c
		 WHERE c.published_revision_id IS NOT NULL
		   AND NOT EXISTS (SELECT 1 FROM course_publication_audit p WHERE p.course_id = c.id)
		UNION ALL
		SELECT 'course revision ' || rv.id || ' ' ||
		       CASE
		         WHEN rv.status = 'draft' THEN 'is a draft with a submission audit row'
		         WHEN a.id IS NULL THEN 'has no submission audit row'
		         ELSE 'differs from its submission audit row'
		       END
		  FROM course_revisions rv
		  LEFT JOIN course_submission_audit a ON a.revision_id = rv.id
		 WHERE (rv.status <> 'draft' OR a.id IS NOT NULL)
		   AND (rv.status = 'draft' OR a.id IS NULL OR a.course_id <> rv.course_id OR a.actor_id <> rv.author_id
		        OR a.submitted_at IS DISTINCT FROM rv.submitted_at)
		ORDER BY 1`)
	if err != nil {
		return fmt.Errorf("check courses: %w", err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("check courses: %w", err)
	}
	r.Problems = append(r.Problems, found...)
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM courses WHERE published_revision_id IS NOT NULL`).Scan(&r.Courses); err != nil {
		return fmt.Errorf("count published courses: %w", err)
	}
	return nil
}

// WriteText prints the report for operators. It contains table names, row
// counts, asset IDs and content keys, never row contents.
func (r *Report) WriteText(w io.Writer, strict bool) {
	if len(r.Counts) == 0 && len(r.Problems) > 0 {
		fmt.Fprintln(w, "migrations: NOT current")
	} else {
		fmt.Fprintln(w, "migrations: up to date")
	}
	for _, c := range r.Counts {
		fmt.Fprintf(w, "rows %-28s %d\n", c.Table, c.Rows)
	}
	fmt.Fprintf(w, "media assets checked: %d (%d referenced by revisions)\n", r.Assets, r.Referenced)
	fmt.Fprintf(w, "published lessons: %d\n", r.Published)
	fmt.Fprintf(w, "published courses: %d\n", r.Courses)
	fmt.Fprintf(w, "unexpected files: %d\n", len(r.Unexpected))
	for _, p := range r.Unexpected {
		fmt.Fprintln(w, "  unexpected:", p)
	}
	fmt.Fprintf(w, "temporary files: %d\n", len(r.Temporary))
	for _, p := range r.Temporary {
		fmt.Fprintln(w, "  temporary:", p)
	}
	for _, p := range r.Problems {
		fmt.Fprintln(w, "PROBLEM:", p)
	}
	if r.Failed(strict) {
		fmt.Fprintln(w, "verification FAILED")
	} else {
		fmt.Fprintln(w, "verification passed")
	}
}
