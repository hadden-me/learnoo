package studioapi

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hadden-me/learnoo/internal/database/dbtest"
)

func TestTruncateKeyBoundsEcho(t *testing.T) {
	if got := truncateKey("expectedVersion"); got != "expectedVersion" {
		t.Fatalf("short key changed: %q", got)
	}
	long := strings.Repeat("é", 200_000)
	got := truncateKey(long)
	if len(got) > 64+len("…") || !utf8.ValidString(got) {
		t.Fatalf("long key echoed as %d bytes (valid UTF-8: %v)", len(got), utf8.ValidString(got))
	}
}

func TestMediaUploadsAppendOnly(t *testing.T) {
	db := dbtest.Migrated(t)
	ctx := context.Background()
	var user, asset int64
	if err := db.QueryRow(ctx, `INSERT INTO users (email, display_name, password_hash)
		VALUES ('uploader@example.test', 'Simulated Uploader', '$argon2id$unused') RETURNING id`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO media_assets
		(sha256, content_type, byte_size, width, height, storage_key, created_at, created_by)
		VALUES (decode(repeat('ab', 32), 'hex'), 'image/png', 1, 1, 1, 'ab/cd/x.png', now(), $1)
		RETURNING id`, user).Scan(&asset); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO media_uploads (user_id, media_asset_id, created_at) VALUES ($1, $2, now())`, user, asset); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE media_uploads SET created_at = now()`,
		`DELETE FROM media_uploads`,
		`TRUNCATE media_uploads`,
	} {
		if _, err := db.Exec(ctx, stmt); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: want append-only error, got %v", stmt, err)
		}
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM media_uploads`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("links after tampering: %d, %v", n, err)
	}
}
