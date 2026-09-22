package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTrackStoreRetainsNoPersistentDomainCollectionsOrCounters(t *testing.T) {
	storeType := reflect.TypeOf(trackStore{})
	for index := 0; index < storeType.NumField(); index++ {
		field := storeType.Field(index)
		if field.Type.Kind() == reflect.Map {
			t.Fatalf("trackStore field %s is a retained map", field.Name)
		}
		if strings.HasPrefix(strings.ToLower(field.Name), "next") {
			t.Fatalf("trackStore field %s is an in-memory ID counter", field.Name)
		}
	}
}

func TestRepositoryFailureUsesStableMessageAndRetainsDiagnosticCause(t *testing.T) {
	store, err := newTrackStore(filepath.Join(t.TempDir(), "repository-error.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = store.authorRepository.List(context.Background())
	if !errors.Is(err, errRepositoryPersistence) {
		t.Fatalf("repository error = %v, want stable persistence classification", err)
	}
	if err.Error() != errRepositoryPersistence.Error() {
		t.Fatalf("repository error message = %q, want %q", err.Error(), errRepositoryPersistence.Error())
	}
	if errors.Unwrap(err) == nil {
		t.Fatal("repository error did not retain its diagnostic cause")
	}
}

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

func TestSystemPlaylistNormalizationMigrationUpgradesAlreadyVersionedDatabase(t *testing.T) {
	db, err := openSQLiteDB(filepath.Join(t.TempDir(), "system-playlist-upgrade.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	migrations := defaultSchemaMigrations()
	if err := runSQLiteMigrations(context.Background(), db, migrations[:4]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users (id, email, role, password_hash, created_at) VALUES (1, 'user@example.com', 'listener', 'hash', ?)`, formatSQLiteTime(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	insertPlaylist := `INSERT INTO playlists (id, user_id, name, description, cover_image_path, visibility, share_token, track_items_json, system, kind)
		VALUES (?, 1, ?, '', '', 'private', '', ?, 1, ?)`
	if _, err := db.Exec(insertPlaylist, 10, "Favorites", `[{"trackId":1}]`, playlistKindFavorites); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(insertPlaylist, 11, "Legacy Favorites", `[{"trackId":2}]`, "Favorites"); err != nil {
		t.Fatal(err)
	}

	if err := runSQLiteMigrations(context.Background(), db, migrations); err != nil {
		t.Fatal(err)
	}
	var count, system int
	var kind, trackItemsJSON string
	if err := db.QueryRow(`SELECT COUNT(*), system, kind, track_items_json FROM playlists WHERE user_id = 1 AND kind = 'favorites'`).Scan(&count, &system, &kind, &trackItemsJSON); err != nil {
		t.Fatal(err)
	}
	if count != 1 || system != 1 || kind != playlistKindFavorites {
		t.Fatalf("normalized playlist count=%d system=%d kind=%q", count, system, kind)
	}
	var items []playlistTrack
	if err := unmarshalJSONColumn(trackItemsJSON, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || !containsPlaylistTrack(items, 1) || !containsPlaylistTrack(items, 2) {
		t.Fatalf("merged playlist items = %#v, want tracks 1 and 2", items)
	}
}

func TestStartupRepairsCountersUpwardAndIsRepeatable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "counter-repair.sqlite")
	store, err := newTrackStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.createAuthor(upsertAuthorRequest{CurrentName: "First"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE store_metadata SET value = 1 WHERE key = 'next_author_id'`); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		store, err = newTrackStore(dbPath)
		if err != nil {
			t.Fatalf("startup repair attempt %d: %v", attempt, err)
		}
		if attempt == 0 {
			if err := store.db.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(func() { _ = store.db.Close() })
	second, err := store.createAuthor(upsertAuthorRequest{CurrentName: "Second"})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID+1 {
		t.Fatalf("author ID after repeated counter repair = %d, want %d", second.ID, first.ID+1)
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

	installRejectTriggers(t, store.db, "users", "refresh_sessions", "authors", "playlists", "lyrics")
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
	removeRejectTriggers(t, store.db, "users", "refresh_sessions", "authors", "playlists", "lyrics")

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
	removeRejectTriggers(t, store.db, "store_metadata", "authors", "albums", "tracks")

	installRejectTriggers(t, store.db, "users", "refresh_sessions", "albums", "tracks", "playlists", "lyrics")
	if _, err := store.createAuthor(upsertAuthorRequest{CurrentName: "Independent Author"}); err != nil {
		t.Fatalf("create author with unrelated write guards: %v", err)
	}
	removeRejectTriggers(t, store.db, "users", "refresh_sessions", "albums", "tracks", "playlists", "lyrics")

	playlistItem, err := store.createPlaylist(userItem.ID, upsertPlaylistRequest{Name: "Before", Visibility: playlistVisibilityPrivate})
	if err != nil {
		t.Fatalf("create playlist: %v", err)
	}
	installRejectTriggers(t, store.db, "store_metadata", "users", "refresh_sessions", "authors", "albums", "tracks", "lyrics")
	if _, found, err := store.updatePlaylist(userItem.ID, playlistItem.ID, upsertPlaylistRequest{Name: "After", Visibility: playlistVisibilityPrivate}); err != nil || !found {
		t.Fatalf("update playlist with unrelated write guards found=%v error=%v", found, err)
	}
}

func TestTrackAndLyricsFailuresRollBackEntireTransaction(t *testing.T) {
	store, err := newTrackStore(filepath.Join(t.TempDir(), "rollback-targets.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.db.Close() })
	authorItem, err := store.createAuthor(upsertAuthorRequest{CurrentName: "Author"})
	if err != nil {
		t.Fatal(err)
	}
	albumItem, err := store.createAlbum(upsertAlbumRequest{Title: "Album", ReleaseDate: time.Now().UTC(), IsPublished: true})
	if err != nil {
		t.Fatal(err)
	}

	var counterBefore int64
	if err := store.db.QueryRow(`SELECT value FROM store_metadata WHERE key = 'next_track_id'`).Scan(&counterBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER reject_album_membership_update BEFORE UPDATE ON albums BEGIN SELECT RAISE(FAIL, 'injected album failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.create(upsertTrackRequest{Name: "Rollback", AuthorIDs: []int64{authorItem.ID}, AlbumID: albumItem.ID, AudioFilePath: "rollback.mp3"}); err == nil {
		t.Fatal("create track error = nil, want injected failure")
	}
	if _, err := store.db.Exec(`DROP TRIGGER reject_album_membership_update`); err != nil {
		t.Fatal(err)
	}
	var trackCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM tracks`).Scan(&trackCount); err != nil {
		t.Fatal(err)
	}
	var counterAfter int64
	if err := store.db.QueryRow(`SELECT value FROM store_metadata WHERE key = 'next_track_id'`).Scan(&counterAfter); err != nil {
		t.Fatal(err)
	}
	if trackCount != 0 || counterAfter != counterBefore {
		t.Fatalf("after rollback trackCount=%d counter=%d, want 0 and %d", trackCount, counterAfter, counterBefore)
	}

	trackItem, err := store.create(upsertTrackRequest{Name: "Lyrics", AuthorIDs: []int64{authorItem.ID}, AlbumID: albumItem.ID, AudioFilePath: "lyrics.mp3"})
	if err != nil {
		t.Fatal(err)
	}
	var lyricsCounterBefore, lineCounterBefore int64
	if err := store.db.QueryRow(`SELECT value FROM store_metadata WHERE key = 'next_lyrics_id'`).Scan(&lyricsCounterBefore); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT value FROM store_metadata WHERE key = 'next_lyrics_line_id'`).Scan(&lineCounterBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER reject_lyrics_insert BEFORE INSERT ON lyrics BEGIN SELECT RAISE(FAIL, 'injected lyrics failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.upsertLyrics(trackItem.ID, upsertLyricsRequest{Type: lyricsTypeSynced, Lines: []upsertSyncedLyricsLine{{StartMs: 0, Text: "line"}}}); err == nil {
		t.Fatal("upsert lyrics error = nil, want injected failure")
	}
	var lyricsCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM lyrics`).Scan(&lyricsCount); err != nil {
		t.Fatal(err)
	}
	var lyricsCounterAfter, lineCounterAfter int64
	if err := store.db.QueryRow(`SELECT value FROM store_metadata WHERE key = 'next_lyrics_id'`).Scan(&lyricsCounterAfter); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT value FROM store_metadata WHERE key = 'next_lyrics_line_id'`).Scan(&lineCounterAfter); err != nil {
		t.Fatal(err)
	}
	if lyricsCount != 0 || lyricsCounterAfter != lyricsCounterBefore || lineCounterAfter != lineCounterBefore {
		t.Fatalf("lyrics rollback count=%d counters=(%d,%d), want 0 and (%d,%d)", lyricsCount, lyricsCounterAfter, lineCounterAfter, lyricsCounterBefore, lineCounterBefore)
	}
	if _, err := store.db.Exec(`DROP TRIGGER reject_lyrics_insert`); err != nil {
		t.Fatal(err)
	}

	originalText := "original lyrics"
	if _, _, err := store.upsertLyrics(trackItem.ID, upsertLyricsRequest{Type: lyricsTypePlain, PlainText: &originalText}); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT value FROM store_metadata WHERE key = 'next_lyrics_line_id'`).Scan(&lineCounterBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER reject_lyrics_update BEFORE UPDATE ON lyrics BEGIN SELECT RAISE(FAIL, 'injected lyrics replacement failure'); END`); err != nil {
		t.Fatal(err)
	}
	replacementText := "replacement lyrics"
	if _, _, err := store.upsertLyrics(trackItem.ID, upsertLyricsRequest{
		Type:  lyricsTypeSynced,
		Lines: []upsertSyncedLyricsLine{{StartMs: 0, Text: replacementText}},
	}); err == nil {
		t.Fatal("replace lyrics error = nil, want injected failure")
	}
	storedLyrics, err := store.getLyrics(trackItem.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedLyrics.Type != lyricsTypePlain || storedLyrics.PlainText == nil || *storedLyrics.PlainText != originalText {
		t.Fatalf("lyrics after failed replacement = %#v, want original plain lyrics", storedLyrics)
	}
	if err := store.db.QueryRow(`SELECT value FROM store_metadata WHERE key = 'next_lyrics_line_id'`).Scan(&lineCounterAfter); err != nil {
		t.Fatal(err)
	}
	if lineCounterAfter != lineCounterBefore {
		t.Fatalf("line counter after failed replacement = %d, want %d", lineCounterAfter, lineCounterBefore)
	}
}

func TestTrackMovementAndDeletionFailuresRollBackAllRows(t *testing.T) {
	store, err := newTrackStore(filepath.Join(t.TempDir(), "rollback-track-mutations.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.db.Close() })
	authorItem, err := store.createAuthor(upsertAuthorRequest{CurrentName: "Author"})
	if err != nil {
		t.Fatal(err)
	}
	sourceAlbum, err := store.createAlbum(upsertAlbumRequest{Title: "Source", ReleaseDate: time.Now().UTC(), IsPublished: true})
	if err != nil {
		t.Fatal(err)
	}
	targetAlbum, err := store.createAlbum(upsertAlbumRequest{Title: "Target", ReleaseDate: time.Now().UTC(), IsPublished: true})
	if err != nil {
		t.Fatal(err)
	}
	trackItem, err := store.create(upsertTrackRequest{Name: "Movable", AuthorIDs: []int64{authorItem.ID}, AlbumID: sourceAlbum.ID, AudioFilePath: "movable.mp3"})
	if err != nil {
		t.Fatal(err)
	}

	rejectTargetUpdate := fmt.Sprintf(`CREATE TRIGGER reject_target_album_update BEFORE UPDATE ON albums WHEN NEW.id = %d BEGIN SELECT RAISE(FAIL, 'injected target album failure'); END`, targetAlbum.ID)
	if _, err := store.db.Exec(rejectTargetUpdate); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.update(trackItem.ID, upsertTrackRequest{
		Name: trackItem.Name, AuthorIDs: trackItem.AuthorIDs, AlbumID: targetAlbum.ID, AudioFilePath: trackItem.AudioFilePath,
	}); err == nil || !found {
		t.Fatalf("move track found=%v error=%v, want found with injected failure", found, err)
	}
	if _, err := store.db.Exec(`DROP TRIGGER reject_target_album_update`); err != nil {
		t.Fatal(err)
	}
	storedTrack, ok := store.getTrack(trackItem.ID)
	if !ok || storedTrack.AlbumID != sourceAlbum.ID {
		t.Fatalf("track after failed move = %#v found=%v, want source album %d", storedTrack, ok, sourceAlbum.ID)
	}
	storedSource, ok := store.getAlbum(sourceAlbum.ID)
	if !ok || !containsInt64(storedSource.TrackIDs, trackItem.ID) {
		t.Fatalf("source album after failed move = %#v found=%v", storedSource, ok)
	}
	storedTarget, ok := store.getAlbum(targetAlbum.ID)
	if !ok || containsInt64(storedTarget.TrackIDs, trackItem.ID) {
		t.Fatalf("target album after failed move = %#v found=%v", storedTarget, ok)
	}

	userItem, err := store.createUser("delete-rollback@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	playlistItem, err := store.createPlaylist(userItem.ID, upsertPlaylistRequest{Name: "Queue", Visibility: playlistVisibilityPrivate})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.addTrackToPlaylists(userItem.ID, trackItem.ID, []int64{playlistItem.ID}); err != nil {
		t.Fatal(err)
	}
	plainText := "must survive"
	if _, _, err := store.upsertLyrics(trackItem.ID, upsertLyricsRequest{Type: lyricsTypePlain, PlainText: &plainText}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER reject_track_delete BEFORE DELETE ON tracks BEGIN SELECT RAISE(FAIL, 'injected track delete failure'); END`); err != nil {
		t.Fatal(err)
	}
	if deleted, err := store.delete(trackItem.ID); err == nil || deleted {
		t.Fatalf("delete track deleted=%v error=%v, want injected failure", deleted, err)
	}
	if _, ok := store.getTrack(trackItem.ID); !ok {
		t.Fatal("track was removed despite delete rollback")
	}
	storedSource, ok = store.getAlbum(sourceAlbum.ID)
	if !ok || !containsInt64(storedSource.TrackIDs, trackItem.ID) {
		t.Fatalf("album after failed delete = %#v found=%v", storedSource, ok)
	}
	page, ok := store.getPlaylistTracks(userItem.ID, playlistItem.ID, 1, 20)
	if !ok || len(page.Items) != 1 || !page.Items[0].IsAvailable {
		t.Fatalf("playlist after failed delete = %#v found=%v", page, ok)
	}
	storedLyrics, err := store.getLyrics(trackItem.ID)
	if err != nil || storedLyrics.PlainText == nil || *storedLyrics.PlainText != plainText {
		t.Fatalf("lyrics after failed delete = %#v error=%v", storedLyrics, err)
	}
}

func TestMultiPlaylistAndPreferenceFailuresRollBack(t *testing.T) {
	store, err := newTrackStore(filepath.Join(t.TempDir(), "rollback-playlists.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.db.Close() })
	userItem, err := store.createUser("playlist-rollback@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	authorItem, albumItem := seedPlaylistTrackDependencies(t, store)
	trackItem, err := store.create(upsertTrackRequest{Name: "Track", AuthorIDs: []int64{authorItem.ID}, AlbumID: albumItem.ID, AudioFilePath: "track.mp3"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.createPlaylist(userItem.ID, upsertPlaylistRequest{Name: "First", Visibility: playlistVisibilityPrivate})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.createPlaylist(userItem.ID, upsertPlaylistRequest{Name: "Second", Visibility: playlistVisibilityPrivate})
	if err != nil {
		t.Fatal(err)
	}
	rejectSecond := fmt.Sprintf(`CREATE TRIGGER reject_second_playlist_update BEFORE UPDATE ON playlists WHEN NEW.id = %d BEGIN SELECT RAISE(FAIL, 'injected playlist failure'); END`, second.ID)
	if _, err := store.db.Exec(rejectSecond); err != nil {
		t.Fatal(err)
	}
	if err := store.addTrackToPlaylists(userItem.ID, trackItem.ID, []int64{first.ID, second.ID}); err == nil {
		t.Fatal("addTrackToPlaylists error = nil, want injected failure")
	}
	if _, err := store.db.Exec(`DROP TRIGGER reject_second_playlist_update`); err != nil {
		t.Fatal(err)
	}
	for _, playlistID := range []int64{first.ID, second.ID} {
		page, ok := store.getPlaylistTracks(userItem.ID, playlistID, 1, 20)
		if !ok || len(page.Items) != 0 {
			t.Fatalf("playlist %d after rollback = %#v found=%v, want empty", playlistID, page, ok)
		}
	}

	if err := store.setDislikedTrack(userItem.ID, trackItem.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER reject_dislike_update BEFORE UPDATE ON playlists WHEN NEW.kind = 'dislikes' BEGIN SELECT RAISE(FAIL, 'injected opposite preference failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.setFavoriteTrack(userItem.ID, trackItem.ID, true); err == nil {
		t.Fatal("setFavoriteTrack error = nil, want injected failure")
	}
	response, ok := store.getTrackResponse(trackItem.ID, userItem.ID)
	if !ok || response.IsFavorite || !response.IsDisliked {
		t.Fatalf("preference after rollback = %#v found=%v, want disliked only", response, ok)
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

func TestConcurrentTrackAndPlaylistMutationsDoNotLoseUpdates(t *testing.T) {
	store, err := newTrackStore(filepath.Join(t.TempDir(), "concurrent-domain.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.db.Close() })
	userItem, err := store.createUser("concurrent@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	authorItem, err := store.createAuthor(upsertAuthorRequest{CurrentName: "Concurrent Author"})
	if err != nil {
		t.Fatal(err)
	}
	albumItem, err := store.createAlbum(upsertAlbumRequest{Title: "Concurrent Album", ReleaseDate: time.Now().UTC(), IsPublished: true})
	if err != nil {
		t.Fatal(err)
	}
	playlistItem, err := store.createPlaylist(userItem.ID, upsertPlaylistRequest{Name: "Queue", Visibility: playlistVisibilityPrivate})
	if err != nil {
		t.Fatal(err)
	}

	const count = 10
	tracks := make(chan track, count)
	errorsCh := make(chan error, count)
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			item, err := store.create(upsertTrackRequest{
				Name: "Track " + string(rune('A'+index)), AuthorIDs: []int64{authorItem.ID},
				AlbumID: albumItem.ID, AlbumOrder: 0, AudioFilePath: "track.mp3",
			})
			if err != nil {
				errorsCh <- err
				return
			}
			tracks <- item
		}()
	}
	wait.Wait()
	close(tracks)
	close(errorsCh)
	for err := range errorsCh {
		t.Errorf("concurrent track creation: %v", err)
	}
	created := make([]track, 0, count)
	ids := make(map[int64]struct{}, count)
	for item := range tracks {
		created = append(created, item)
		ids[item.ID] = struct{}{}
	}
	if len(created) != count || len(ids) != count {
		t.Fatalf("created=%d distinct IDs=%d, want %d", len(created), len(ids), count)
	}
	storedAlbum, ok := store.getAlbum(albumItem.ID)
	if !ok || len(storedAlbum.TrackIDs) != count {
		t.Fatalf("album track IDs=%v found=%v, want %d tracks", storedAlbum.TrackIDs, ok, count)
	}

	errorsCh = make(chan error, count)
	wait = sync.WaitGroup{}
	for _, item := range created {
		item := item
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := store.addTrackToPlaylists(userItem.ID, item.ID, []int64{playlistItem.ID}); err != nil {
				errorsCh <- err
			}
		}()
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Errorf("concurrent playlist mutation: %v", err)
	}
	page, ok := store.getPlaylistTracks(userItem.ID, playlistItem.ID, 1, 100)
	if !ok || len(page.Items) != count {
		t.Fatalf("playlist items=%d found=%v, want %d", len(page.Items), ok, count)
	}
}

func TestConcurrentFavoriteAndDislikeChangesRemainMutuallyExclusive(t *testing.T) {
	store, err := newTrackStore(filepath.Join(t.TempDir(), "concurrent-preferences.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.db.Close() })
	userItem, err := store.createUser("preferences@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	authorItem, albumItem := seedPlaylistTrackDependencies(t, store)
	trackItem, err := store.create(upsertTrackRequest{Name: "Track", AuthorIDs: []int64{authorItem.ID}, AlbumID: albumItem.ID, AudioFilePath: "track.mp3"})
	if err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 12; attempt++ {
		if err := store.setFavoriteTrack(userItem.ID, trackItem.ID, false); err != nil {
			t.Fatal(err)
		}
		if err := store.setDislikedTrack(userItem.ID, trackItem.ID, false); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		results := make(chan error, 2)
		var wait sync.WaitGroup
		for _, operation := range []func() error{
			func() error { return store.setFavoriteTrack(userItem.ID, trackItem.ID, true) },
			func() error { return store.setDislikedTrack(userItem.ID, trackItem.ID, true) },
		} {
			operation := operation
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				results <- operation()
			}()
		}
		close(start)
		wait.Wait()
		close(results)
		for err := range results {
			if err != nil {
				t.Fatalf("attempt %d preference mutation: %v", attempt, err)
			}
		}
		response, ok := store.getTrackResponse(trackItem.ID, userItem.ID)
		if !ok || response.IsFavorite == response.IsDisliked {
			t.Fatalf("attempt %d preferences favorite=%v disliked=%v found=%v, want exactly one", attempt, response.IsFavorite, response.IsDisliked, ok)
		}
	}
}

func TestConcurrentRefreshRotationHasSingleWinner(t *testing.T) {
	store, err := newTrackStore(filepath.Join(t.TempDir(), "concurrent-refresh.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.db.Close() })
	userItem, err := store.createUser("rotation-race@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	_, rawToken, err := store.createRefreshSession(userItem.ID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	results := make(chan error, 2)
	var wait sync.WaitGroup
	for index := 0; index < 2; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, _, _, err := store.rotateRefreshSession(rawToken, time.Now().Add(2*time.Hour))
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	succeeded, rejected := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, errInvalidRefreshToken):
			rejected++
		default:
			t.Fatalf("unexpected rotation error: %v", err)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("rotation results succeeded=%d rejected=%d, want 1 and 1", succeeded, rejected)
	}
	var sessionCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM refresh_sessions`).Scan(&sessionCount); err != nil {
		t.Fatal(err)
	}
	if sessionCount != 1 {
		t.Fatalf("refresh session count=%d, want 1", sessionCount)
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

func containsPlaylistTrack(items []playlistTrack, trackID int64) bool {
	for _, item := range items {
		if item.TrackID == trackID {
			return true
		}
	}
	return false
}
