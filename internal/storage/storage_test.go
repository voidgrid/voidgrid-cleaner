package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestMigrateAppliesAllStatementsOnce(t *testing.T) {
	db := openTest(t)
	// Every table in 0001 exists, so the multi-statement file ran whole.
	for _, table := range []string{"settings", "users", "sessions"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s: n=%d err=%v", table, n, err)
		}
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestSettingKeepsFirstValue(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	a, _ := db.Setting(ctx, "k", func() string { return "first" })
	b, err := db.Setting(ctx, "k", func() string { return "second" })
	if err != nil || a != "first" || b != "first" {
		t.Fatalf("got %q %q %v", a, b, err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if err := db.UpsertUser(ctx, User{SeerrID: 7, Name: "alice", IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE users SET auto_approve = 1 WHERE seerr_user_id = 7`); err != nil {
		t.Fatal(err)
	}
	// A refresh from Seerr must not reset the app's own auto-approve flag.
	if err := db.UpsertUser(ctx, User{SeerrID: 7, Name: "alice2"}); err != nil {
		t.Fatal(err)
	}
	tok, err := db.CreateSession(ctx, 7, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	u, err := db.SessionUser(ctx, tok)
	if err != nil || u.Name != "alice2" || u.IsAdmin || !u.AutoApprove {
		t.Fatalf("got %+v %v", u, err)
	}
	if _, err := db.SessionUser(ctx, "bogus"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("bogus token: %v", err)
	}
	expired, _ := db.CreateSession(ctx, 7, -time.Second)
	if _, err := db.SessionUser(ctx, expired); !errors.Is(err, ErrNoSession) {
		t.Fatalf("expired token: %v", err)
	}
	if err := db.DeleteSession(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SessionUser(ctx, tok); !errors.Is(err, ErrNoSession) {
		t.Fatalf("after logout: %v", err)
	}
}
