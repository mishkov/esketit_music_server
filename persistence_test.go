package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSQLiteMigrationsFreshAndRepeatable(t *testing.T) {
	store, err := newTrackStore(filepath.Join(t.TempDir(), "fresh.sqlite"))
	if err != nil {
		t.Fatalf("newTrackStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.db.Close() })

	wantVersion := defaultSchemaMigrations()[len(defaultSchemaMigrations())-1].version
	version, err := currentSchemaVersion(context.Background(), store.db)
	if err != nil {
		t.Fatalf("currentSchemaVersion() error = %v", err)
	}
	if version != wantVersion {
		t.Fatalf("schema version = %d, want %d", version, wantVersion)
	}
	if err := runSQLiteMigrations(context.Background(), store.db, defaultSchemaMigrations()); err != nil {
		t.Fatalf("repeat runSQLiteMigrations() error = %v", err)
	}
	var applied int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count schema migrations: %v", err)
	}
	if applied != len(defaultSchemaMigrations()) {
		t.Fatalf("applied migrations = %d, want %d", applied, len(defaultSchemaMigrations()))
	}

	for attempt := 0; attempt < 2; attempt++ {
		connection, err := store.db.Conn(context.Background())
		if err != nil {
			t.Fatalf("db.Conn() error = %v", err)
		}
		var foreignKeys int
		if err := connection.QueryRowContext(context.Background(), `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
			_ = connection.Close()
			t.Fatalf("PRAGMA foreign_keys error = %v", err)
		}
		if err := connection.Close(); err != nil {
			t.Fatalf("connection.Close() error = %v", err)
		}
		if foreignKeys != 1 {
			t.Fatalf("foreign_keys connection %d = %d, want 1", attempt, foreignKeys)
		}
	}
	if _, err := store.db.Exec(`CREATE TABLE future_user_roles (
		user_id INTEGER NOT NULL REFERENCES users(id)
	)`); err != nil {
		t.Fatalf("create future RBAC reference table: %v", err)
	}
	if _, err := store.db.Exec(`INSERT INTO future_user_roles (user_id) VALUES (999999)`); err == nil {
		t.Fatal("foreign-key violating insert succeeded")
	}
}

func TestSQLiteMigrationFailureRollsBackAndIsNotRecorded(t *testing.T) {
	db, err := openSQLiteDB(filepath.Join(t.TempDir(), "failed.sqlite"))
	if err != nil {
		t.Fatalf("openSQLiteDB() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	boom := errors.New("injected migration failure")
	err = runSQLiteMigrations(context.Background(), db, []schemaMigration{{
		version: 1,
		name:    "intentional failure",
		apply: func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `CREATE TABLE migration_partial (id INTEGER PRIMARY KEY)`); err != nil {
				return err
			}
			return boom
		},
	}})
	if !errors.Is(err, boom) {
		t.Fatalf("runSQLiteMigrations() error = %v, want %v", err, boom)
	}

	var recorded int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&recorded); err != nil {
		t.Fatalf("count schema migrations: %v", err)
	}
	if recorded != 0 {
		t.Fatalf("recorded migrations = %d, want 0", recorded)
	}
	var tableCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'migration_partial'`).Scan(&tableCount); err != nil {
		t.Fatalf("inspect partial table: %v", err)
	}
	if tableCount != 0 {
		t.Fatalf("partial migration table count = %d, want 0", tableCount)
	}
}

func TestSQLiteMigrationsPreserveExistingUnversionedDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "existing.sqlite")
	db, err := openSQLiteDB(dbPath)
	if err != nil {
		t.Fatalf("openSQLiteDB() error = %v", err)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	if err := createInitialSchema(context.Background(), tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("createInitialSchema() error = %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO authors (id, current_name, photos_json) VALUES (41, 'Existing', '[]')`); err != nil {
		_ = tx.Rollback()
		t.Fatalf("insert existing author: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO store_metadata (key, value) VALUES ('next_author_id', 42)`); err != nil {
		_ = tx.Rollback()
		t.Fatalf("insert existing counter: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit existing database: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close existing database: %v", err)
	}

	store, err := newTrackStore(dbPath)
	if err != nil {
		t.Fatalf("newTrackStore(existing) error = %v", err)
	}
	t.Cleanup(func() { _ = store.db.Close() })
	item, ok := store.getAuthor(41)
	if !ok || item.CurrentName != "Existing" {
		t.Fatalf("existing author = %#v, found=%v", item, ok)
	}
	next, err := store.createAuthor(upsertAuthorRequest{CurrentName: "Next"})
	if err != nil {
		t.Fatalf("create author after migration: %v", err)
	}
	if next.ID != 42 {
		t.Fatalf("new author ID = %d, want 42", next.ID)
	}
}

func TestTargetedWritesDoNotTouchUnrelatedTables(t *testing.T) {
	store, err := newTrackStore(filepath.Join(t.TempDir(), "targeted.sqlite"))
	if err != nil {
		t.Fatalf("newTrackStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.db.Close() })

	userItem, err := store.createUser("listener@example.com", "password-hash")
	if err != nil {
		t.Fatalf("createUser() error = %v", err)
	}
	session, _, err := store.createRefreshSession(userItem.ID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("createRefreshSession() error = %v", err)
	}
	authorItem, err := store.createAuthor(upsertAuthorRequest{CurrentName: "Author"})
	if err != nil {
		t.Fatalf("createAuthor() error = %v", err)
	}
	albumItem, err := store.createAlbum(upsertAlbumRequest{
		Title:       "Album",
		ReleaseDate: time.Now().UTC(),
		IsPublished: true,
	})
	if err != nil {
		t.Fatalf("createAlbum() error = %v", err)
	}
	trackItem, err := store.create(upsertTrackRequest{
		Name:          "Track",
		AuthorIDs:     []int64{authorItem.ID},
		AlbumID:       albumItem.ID,
		AudioFilePath: "track.mp3",
	})
	if err != nil {
		t.Fatalf("create track error = %v", err)
	}

	installRejectTriggers(t, store.db, "users", "refresh_sessions")
	if _, err := store.db.Exec(`CREATE TRIGGER reject_existing_track_update_during_create
		BEFORE UPDATE ON tracks BEGIN SELECT RAISE(FAIL, 'unexpected track update'); END`); err != nil {
		t.Fatalf("create track-update guard: %v", err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER reject_existing_track_delete_during_create
		BEFORE DELETE ON tracks BEGIN SELECT RAISE(FAIL, 'unexpected track delete'); END`); err != nil {
		t.Fatalf("create track-delete guard: %v", err)
	}
	if _, err := store.create(upsertTrackRequest{
		Name:          "Second Track",
		AuthorIDs:     []int64{authorItem.ID},
		AlbumID:       albumItem.ID,
		AlbumOrder:    1,
		AudioFilePath: "second-track.mp3",
	}); err != nil {
		t.Fatalf("create track with user/session write guards: %v", err)
	}
	if _, err := store.db.Exec(`DROP TRIGGER reject_existing_track_update_during_create`); err != nil {
		t.Fatalf("drop track-update guard: %v", err)
	}
	if _, err := store.db.Exec(`DROP TRIGGER reject_existing_track_delete_during_create`); err != nil {
		t.Fatalf("drop track-delete guard: %v", err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER reject_track_insert_during_update
		BEFORE INSERT ON tracks BEGIN SELECT RAISE(FAIL, 'unexpected track insert'); END`); err != nil {
		t.Fatalf("create track-insert guard: %v", err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER reject_track_delete_during_update
		BEFORE DELETE ON tracks BEGIN SELECT RAISE(FAIL, 'unexpected track delete'); END`); err != nil {
		t.Fatalf("create track-delete guard: %v", err)
	}
	trackItem.Name = "Updated Track"
	if _, found, err := store.update(trackItem.ID, upsertTrackRequest{
		Name:          trackItem.Name,
		AuthorIDs:     trackItem.AuthorIDs,
		AlbumID:       trackItem.AlbumID,
		AudioFilePath: trackItem.AudioFilePath,
	}); err != nil || !found {
		t.Fatalf("update track found=%v error=%v", found, err)
	}
	if _, err := store.db.Exec(`DROP TRIGGER reject_track_insert_during_update`); err != nil {
		t.Fatalf("drop track-insert guard: %v", err)
	}
	if _, err := store.db.Exec(`DROP TRIGGER reject_track_delete_during_update`); err != nil {
		t.Fatalf("drop track-delete guard: %v", err)
	}
	removeRejectTriggers(t, store.db, "users", "refresh_sessions")

	var storedHash string
	if err := store.db.QueryRow(`SELECT token_hash FROM refresh_sessions WHERE id = ?`, session.ID).Scan(&storedHash); err != nil {
		t.Fatalf("load unchanged refresh session: %v", err)
	}
	if storedHash != session.TokenHash {
		t.Fatalf("refresh session hash changed during track update")
	}

	installRejectTriggers(t, store.db, "store_metadata", "authors", "albums", "tracks")
	if _, _, err := store.createRefreshSession(userItem.ID, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatalf("create refresh session with catalog write guards: %v", err)
	}
}

func TestUserAndSystemPlaylistsRollbackTogether(t *testing.T) {
	store, err := newTrackStore(filepath.Join(t.TempDir(), "rollback.sqlite"))
	if err != nil {
		t.Fatalf("newTrackStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.db.Close() })
	if _, err := store.db.Exec(`CREATE TRIGGER reject_system_playlist_insert
		BEFORE INSERT ON playlists BEGIN SELECT RAISE(FAIL, 'injected playlist failure'); END`); err != nil {
		t.Fatalf("create reject trigger: %v", err)
	}

	if _, err := store.createUser("rollback@example.com", "password-hash"); err == nil {
		t.Fatal("createUser() error = nil, want injected failure")
	}
	for _, table := range []string{"users", "playlists"} {
		var count int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s count = %d, want 0 after rollback", table, count)
		}
	}
	var nextUserID int64
	if err := store.db.QueryRow(`SELECT value FROM store_metadata WHERE key = 'next_user_id'`).Scan(&nextUserID); err != nil {
		t.Fatalf("load next_user_id: %v", err)
	}
	if nextUserID != 1 {
		t.Fatalf("next_user_id = %d, want 1 after rollback", nextUserID)
	}
}

func TestRefreshRotationIsAtomic(t *testing.T) {
	store, err := newTrackStore(filepath.Join(t.TempDir(), "refresh-rotation.sqlite"))
	if err != nil {
		t.Fatalf("newTrackStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.db.Close() })
	userItem, err := store.createUser("rotation@example.com", "password-hash")
	if err != nil {
		t.Fatalf("createUser() error = %v", err)
	}
	original, rawToken, err := store.createRefreshSession(userItem.ID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("createRefreshSession() error = %v", err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER reject_rotated_session
		BEFORE INSERT ON refresh_sessions BEGIN SELECT RAISE(FAIL, 'injected rotation failure'); END`); err != nil {
		t.Fatalf("create rotation trigger: %v", err)
	}
	if _, _, _, err := store.rotateRefreshSession(rawToken, time.Now().Add(2*time.Hour)); err == nil {
		t.Fatal("rotateRefreshSession() error = nil, want injected failure")
	}
	var originalCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM refresh_sessions WHERE id = ?`, original.ID).Scan(&originalCount); err != nil {
		t.Fatalf("count original session: %v", err)
	}
	if originalCount != 1 {
		t.Fatalf("original session count = %d, want 1 after rollback", originalCount)
	}
	if _, err := store.db.Exec(`DROP TRIGGER reject_rotated_session`); err != nil {
		t.Fatalf("drop rotation trigger: %v", err)
	}
	_, replacement, replacementToken, err := store.rotateRefreshSession(rawToken, time.Now().Add(2*time.Hour))
	if err != nil {
		t.Fatalf("rotateRefreshSession() error = %v", err)
	}
	var total, oldCount, replacementCount int
	if err := store.db.QueryRow(`SELECT COUNT(*),
		SUM(CASE WHEN id = ? THEN 1 ELSE 0 END),
		SUM(CASE WHEN id = ? THEN 1 ELSE 0 END)
		FROM refresh_sessions`, original.ID, replacement.ID).Scan(&total, &oldCount, &replacementCount); err != nil {
		t.Fatalf("inspect rotated sessions: %v", err)
	}
	if total != 1 || oldCount != 0 || replacementCount != 1 {
		t.Fatalf("sessions after rotation total=%d old=%d replacement=%d", total, oldCount, replacementCount)
	}
	if deleted, err := store.deleteRefreshSession(rawToken); err != nil || deleted {
		t.Fatalf("delete old refresh token deleted=%v error=%v", deleted, err)
	}
	if deleted, err := store.deleteRefreshSession(replacementToken); err != nil || !deleted {
		t.Fatalf("delete replacement refresh token deleted=%v error=%v", deleted, err)
	}
}

func TestDeletedHighestIDIsNotReused(t *testing.T) {
	store, err := newTrackStore(filepath.Join(t.TempDir(), "ids.sqlite"))
	if err != nil {
		t.Fatalf("newTrackStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.db.Close() })

	first, err := store.createAuthor(upsertAuthorRequest{CurrentName: "First"})
	if err != nil {
		t.Fatalf("create first author: %v", err)
	}
	if deleted, err := store.deleteAuthor(first.ID); err != nil || !deleted {
		t.Fatalf("delete first author deleted=%v error=%v", deleted, err)
	}
	second, err := store.createAuthor(upsertAuthorRequest{CurrentName: "Second"})
	if err != nil {
		t.Fatalf("create second author: %v", err)
	}
	if second.ID <= first.ID {
		t.Fatalf("second author ID = %d, want greater than deleted ID %d", second.ID, first.ID)
	}
}

func TestConcurrentCreatesAllocateDistinctIDs(t *testing.T) {
	store, err := newTrackStore(filepath.Join(t.TempDir(), "concurrent.sqlite"))
	if err != nil {
		t.Fatalf("newTrackStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.db.Close() })

	const count = 12
	ids := make(chan int64, count)
	errorsCh := make(chan error, count)
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			item, err := store.createAuthor(upsertAuthorRequest{CurrentName: "Concurrent " + string(rune('A'+index))})
			if err != nil {
				errorsCh <- err
				return
			}
			ids <- item.ID
		}()
	}
	wait.Wait()
	close(ids)
	close(errorsCh)
	for err := range errorsCh {
		t.Errorf("concurrent create error = %v", err)
	}
	seen := make(map[int64]struct{}, count)
	for id := range ids {
		if _, exists := seen[id]; exists {
			t.Errorf("duplicate allocated ID %d", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != count {
		t.Fatalf("allocated IDs = %d, want %d", len(seen), count)
	}
}

func installRejectTriggers(t *testing.T, db *sql.DB, tables ...string) {
	t.Helper()
	for _, table := range tables {
		for _, operation := range []string{"INSERT", "UPDATE", "DELETE"} {
			name := "reject_" + table + "_" + operation
			statement := `CREATE TRIGGER ` + name + ` BEFORE ` + operation + ` ON ` + table +
				` BEGIN SELECT RAISE(FAIL, 'unexpected unrelated write'); END`
			if _, err := db.Exec(statement); err != nil {
				t.Fatalf("create %s trigger: %v", name, err)
			}
		}
	}
}

func removeRejectTriggers(t *testing.T, db *sql.DB, tables ...string) {
	t.Helper()
	for _, table := range tables {
		for _, operation := range []string{"INSERT", "UPDATE", "DELETE"} {
			if _, err := db.Exec(`DROP TRIGGER reject_` + table + `_` + operation); err != nil {
				t.Fatalf("drop reject trigger for %s %s: %v", table, operation, err)
			}
		}
	}
}
