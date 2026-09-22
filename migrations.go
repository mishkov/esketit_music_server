package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"
)

type schemaMigration struct {
	version int
	name    string
	apply   func(context.Context, *sql.Tx) error
}

func defaultSchemaMigrations() []schemaMigration {
	return []schemaMigration{
		{version: 1, name: "initial application schema", apply: createInitialSchema},
		{version: 2, name: "track creation timestamps", apply: migrateTrackCreatedAt},
		{version: 3, name: "unique system playlists", apply: migrateUniqueSystemPlaylists},
		{version: 4, name: "repository query indexes", apply: migrateRepositoryIndexes},
		{version: 5, name: "normalized system playlist keys", apply: migrateUniqueSystemPlaylists},
	}
}

func runSQLiteMigrations(ctx context.Context, db *sql.DB, migrations []schemaMigration) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema migrations table: %w", err)
	}

	ordered := append([]schemaMigration(nil), migrations...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].version < ordered[j].version })
	lastVersion := 0
	for _, migration := range ordered {
		if migration.version <= 0 || migration.version <= lastVersion {
			return fmt.Errorf("schema migrations must have unique, increasing positive versions: %d", migration.version)
		}
		lastVersion = migration.version

		var applied int
		err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, migration.version).Scan(&applied)
		if err != nil {
			return fmt.Errorf("check schema migration %d: %w", migration.version, err)
		}
		if applied != 0 {
			continue
		}

		log.Printf("applying SQLite schema migration %d (%s)", migration.version, migration.name)
		if err := applySQLiteMigration(ctx, db, migration); err != nil {
			return err
		}
		log.Printf("applied SQLite schema migration %d (%s)", migration.version, migration.name)
	}
	return nil
}

func applySQLiteMigration(ctx context.Context, db *sql.DB, migration schemaMigration) (returnErr error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema migration %d: %w", migration.version, err)
	}
	committed := false
	defer func() {
		if !committed {
			joinRollbackError(&returnErr, tx, fmt.Sprintf("schema migration %d", migration.version))
		}
	}()

	if err := migration.apply(ctx, tx); err != nil {
		return fmt.Errorf("apply schema migration %d (%s): %w", migration.version, migration.name, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		migration.version, migration.name, formatSQLiteTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record schema migration %d: %w", migration.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema migration %d: %w", migration.version, err)
	}
	committed = true
	return nil
}

func createInitialSchema(ctx context.Context, tx *sql.Tx) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS store_metadata (key TEXT PRIMARY KEY, value INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS authors (id INTEGER PRIMARY KEY, current_name TEXT NOT NULL, photos_json TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS albums (
			id INTEGER PRIMARY KEY,
			title TEXT NOT NULL,
			cover_image_path TEXT NOT NULL,
			author_ids_json TEXT NOT NULL,
			release_date TEXT NOT NULL,
			is_published INTEGER NOT NULL,
			track_ids_json TEXT NOT NULL,
			additional_info_json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tracks (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			author_ids_json TEXT NOT NULL,
			album_id INTEGER NOT NULL,
			audio_file_path TEXT NOT NULL,
			additional_info_json TEXT NOT NULL,
			source_metadata_json TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY,
			email TEXT NOT NULL UNIQUE,
			role TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS refresh_sessions (
			id TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL,
			token_hash TEXT NOT NULL,
			created_at TEXT NOT NULL,
			expires_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS playlists (
			id INTEGER PRIMARY KEY,
			user_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			description TEXT NOT NULL,
			cover_image_path TEXT NOT NULL,
			visibility TEXT NOT NULL,
			share_token TEXT NOT NULL,
			track_items_json TEXT NOT NULL,
			system INTEGER NOT NULL,
			kind TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS lyrics (
			id INTEGER PRIMARY KEY,
			track_id INTEGER NOT NULL UNIQUE,
			type TEXT NOT NULL,
			plain_text TEXT,
			language_code TEXT,
			source TEXT,
			is_verified INTEGER NOT NULL,
			updated_at TEXT NOT NULL,
			created_at TEXT NOT NULL,
			lines_json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS analytics_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			event_id TEXT NOT NULL UNIQUE,
			user_id INTEGER,
			client_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			event_type TEXT NOT NULL,
			track_id INTEGER,
			playlist_id INTEGER,
			album_id INTEGER,
			position_ms INTEGER,
			duration_ms INTEGER,
			search_query TEXT,
			metadata_json TEXT NOT NULL,
			client_time TEXT NOT NULL,
			received_at TEXT NOT NULL,
			platform TEXT NOT NULL,
			app_version TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS author_popularity_snapshot (
			author_id INTEGER PRIMARY KEY,
			ranking_position INTEGER NOT NULL UNIQUE,
			listened_ms INTEGER NOT NULL,
			calculated_at TEXT NOT NULL,
			window_started_at TEXT NOT NULL,
			window_ended_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_analytics_events_user_received ON analytics_events (user_id, received_at)`,
		`CREATE INDEX IF NOT EXISTS idx_analytics_events_client_received ON analytics_events (client_id, received_at)`,
		`CREATE INDEX IF NOT EXISTS idx_analytics_events_type_received ON analytics_events (event_type, received_at)`,
		`CREATE INDEX IF NOT EXISTS idx_analytics_events_track_received ON analytics_events (track_id, received_at)`,
		`CREATE INDEX IF NOT EXISTS idx_analytics_events_client_session_time ON analytics_events (client_id, session_id, client_time, id)`,
		`CREATE INDEX IF NOT EXISTS idx_analytics_events_client_time ON analytics_events (client_time)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func migrateTrackCreatedAt(ctx context.Context, tx *sql.Tx) (returnErr error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(tracks)`)
	if err != nil {
		return err
	}
	hasCreatedAt := false
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			_ = rows.Close()
			return err
		}
		if name == "created_at" {
			hasCreatedAt = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if hasCreatedAt {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE tracks ADD COLUMN created_at TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}

	idRows, err := tx.QueryContext(ctx, `SELECT id FROM tracks ORDER BY id DESC`)
	if err != nil {
		return err
	}
	var ids []int64
	for idRows.Next() {
		var id int64
		if err := idRows.Scan(&id); err != nil {
			_ = idRows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := idRows.Err(); err != nil {
		_ = idRows.Close()
		return err
	}
	if err := idRows.Close(); err != nil {
		return err
	}
	baseTime := time.Now().UTC().Truncate(time.Second)
	for index, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE tracks SET created_at = ? WHERE id = ?`,
			formatSQLiteTime(baseTime.Add(-time.Duration(index)*time.Second)), id); err != nil {
			return err
		}
	}
	return nil
}

func migrateUniqueSystemPlaylists(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, user_id, track_items_json,
		CASE WHEN system = 1 AND TRIM(kind) = '' THEN 'favorites' ELSE LOWER(TRIM(kind)) END
		FROM playlists
		WHERE LOWER(TRIM(kind)) IN ('favorites', 'dislikes') OR (system = 1 AND TRIM(kind) = '')
		ORDER BY user_id, kind, id`)
	if err != nil {
		return err
	}
	type systemPlaylistRow struct {
		id, userID int64
		kind       string
		items      []playlistTrack
	}
	var items []systemPlaylistRow
	for rows.Next() {
		var item systemPlaylistRow
		var trackItemsJSON string
		if err := rows.Scan(&item.id, &item.userID, &trackItemsJSON, &item.kind); err != nil {
			_ = rows.Close()
			return err
		}
		if err := unmarshalJSONColumn(trackItemsJSON, &item.items); err != nil {
			_ = rows.Close()
			return err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	canonical := make(map[string]systemPlaylistRow)
	for _, item := range items {
		key := fmt.Sprintf("%d\x00%s", item.userID, item.kind)
		first, exists := canonical[key]
		if !exists {
			canonical[key] = item
			continue
		}
		for _, trackItem := range item.items {
			first.items = appendPlaylistTrack(first.items, trackItem)
		}
		encoded, err := marshalJSONColumn(first.items)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE playlists SET track_items_json = ? WHERE id = ?`, encoded, first.id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM playlists WHERE id = ?`, item.id); err != nil {
			return err
		}
		canonical[key] = first
	}
	for _, item := range canonical {
		if _, err := tx.ExecContext(ctx, `UPDATE playlists SET system = 1, kind = ? WHERE id = ?`, item.kind, item.id); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_playlists_user_system_kind
		ON playlists (user_id, kind) WHERE kind IN ('favorites', 'dislikes')`)
	return err
}

func migrateRepositoryIndexes(ctx context.Context, tx *sql.Tx) error {
	statements := []string{
		`CREATE INDEX IF NOT EXISTS idx_refresh_sessions_token_hash ON refresh_sessions (token_hash)`,
		`CREATE INDEX IF NOT EXISTS idx_refresh_sessions_expires_at ON refresh_sessions (expires_at)`,
		`CREATE INDEX IF NOT EXISTS idx_tracks_album_id ON tracks (album_id)`,
		`CREATE INDEX IF NOT EXISTS idx_playlists_user_id ON playlists (user_id, id)`,
		`CREATE INDEX IF NOT EXISTS idx_lyrics_track_id ON lyrics (track_id)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func runSQLiteStartupRepairs(ctx context.Context, db *sql.DB) (returnErr error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin SQLite startup repairs: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			joinRollbackError(&returnErr, tx, "SQLite startup repairs")
		}
	}()

	if err := validateSQLiteRelationships(ctx, tx); err != nil {
		return err
	}

	counters := []struct {
		key   string
		query string
	}{
		{"next_track_id", `SELECT COALESCE(MAX(id), 0) + 1 FROM tracks`},
		{"next_album_id", `SELECT COALESCE(MAX(id), 0) + 1 FROM albums`},
		{"next_author_id", `SELECT COALESCE(MAX(id), 0) + 1 FROM authors`},
		{"next_user_id", `SELECT COALESCE(MAX(id), 0) + 1 FROM users`},
		{"next_playlist_id", `SELECT COALESCE(MAX(id), 0) + 1 FROM playlists`},
		{"next_lyrics_id", `SELECT COALESCE(MAX(id), 0) + 1 FROM lyrics`},
		{"next_lyrics_line_id", `SELECT COALESCE(MAX(CAST(json_extract(lines.value, '$.id') AS INTEGER)), 0) + 1 FROM lyrics LEFT JOIN json_each(lyrics.lines_json) AS lines`},
	}
	for _, counter := range counters {
		var minimum int64
		if err := tx.QueryRowContext(ctx, counter.query).Scan(&minimum); err != nil {
			return fmt.Errorf("calculate %s: %w", counter.key, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO store_metadata (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = MAX(store_metadata.value, excluded.value)`, counter.key, minimum); err != nil {
			return fmt.Errorf("repair %s: %w", counter.key, err)
		}
	}

	repositories := newDomainRepositories(tx)
	rows, err := tx.QueryContext(ctx, `SELECT users.id, kinds.kind
		FROM users CROSS JOIN (SELECT 'favorites' AS kind UNION ALL SELECT 'dislikes') AS kinds
		WHERE NOT EXISTS (
			SELECT 1 FROM playlists
			WHERE playlists.user_id = users.id AND playlists.kind = kinds.kind
		)
		ORDER BY users.id, kinds.kind`)
	if err != nil {
		return fmt.Errorf("find missing system playlists: %w", err)
	}
	type missingSystemPlaylist struct {
		userID int64
		kind   string
	}
	var missing []missingSystemPlaylist
	for rows.Next() {
		var item missingSystemPlaylist
		if err := rows.Scan(&item.userID, &item.kind); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan missing system playlist: %w", err)
		}
		missing = append(missing, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate missing system playlists: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close missing system playlists: %w", err)
	}
	for _, item := range missing {
		id, err := repositories.metadata.AllocateID(ctx, "next_playlist_id")
		if err != nil {
			return err
		}
		name := "Favorites"
		if item.kind == playlistKindDislikes {
			name = "Disliked"
		}
		if err := repositories.playlists.Insert(ctx, playlist{
			ID: id, UserID: item.userID, Name: name, Visibility: playlistVisibilityPrivate,
			TrackItems: []playlistTrack{}, System: true, Kind: item.kind,
		}); err != nil {
			return fmt.Errorf("create missing %s playlist for user %d: %w", item.kind, item.userID, err)
		}
	}
	if err := repositories.sessions.DeleteExpired(ctx, time.Now().UTC()); err != nil {
		return fmt.Errorf("remove expired refresh sessions: %w", err)
	}
	var foreignKeyViolation string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT "table" || ':' || rowid FROM pragma_foreign_key_check LIMIT 1), '')`).Scan(&foreignKeyViolation); err != nil {
		return fmt.Errorf("check SQLite relationships: %w", err)
	}
	if foreignKeyViolation != "" {
		return fmt.Errorf("invalid SQLite relationship at %s", foreignKeyViolation)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit SQLite startup repairs: %w", err)
	}
	committed = true
	return nil
}

func validateSQLiteRelationships(ctx context.Context, tx *sql.Tx) error {
	checks := []struct {
		domain string
		query  string
	}{
		{"authors", `SELECT id FROM authors WHERE id <= 0 OR TRIM(current_name) = '' OR json_valid(photos_json) = 0 ORDER BY id LIMIT 1`},
		{"albums", `SELECT id FROM albums WHERE id <= 0 OR TRIM(title) = '' OR TRIM(release_date) = '' OR json_valid(author_ids_json) = 0 OR json_valid(track_ids_json) = 0 OR json_valid(additional_info_json) = 0 ORDER BY id LIMIT 1`},
		{"tracks", `SELECT id FROM tracks WHERE id <= 0 OR TRIM(name) = '' OR TRIM(audio_file_path) = '' OR TRIM(created_at) = '' OR json_valid(author_ids_json) = 0 OR json_valid(additional_info_json) = 0 OR json_valid(source_metadata_json) = 0 ORDER BY id LIMIT 1`},
		{"users", `SELECT id FROM users WHERE id <= 0 OR TRIM(email) = '' OR TRIM(role) = '' OR TRIM(password_hash) = '' OR TRIM(created_at) = '' ORDER BY id LIMIT 1`},
		{"refresh_sessions", `SELECT rowid FROM refresh_sessions WHERE TRIM(id) = '' OR TRIM(token_hash) = '' OR TRIM(created_at) = '' OR TRIM(expires_at) = '' ORDER BY rowid LIMIT 1`},
		{"playlists", `SELECT id FROM playlists WHERE id <= 0 OR TRIM(name) = '' OR json_valid(track_items_json) = 0 ORDER BY id LIMIT 1`},
		{"lyrics", `SELECT id FROM lyrics WHERE id <= 0 OR TRIM(type) = '' OR TRIM(updated_at) = '' OR TRIM(created_at) = '' OR json_valid(lines_json) = 0 ORDER BY id LIMIT 1`},
		{"tracks", `SELECT tracks.id FROM tracks LEFT JOIN albums ON albums.id = tracks.album_id WHERE albums.id IS NULL ORDER BY tracks.id LIMIT 1`},
		{"tracks", `SELECT tracks.id FROM tracks WHERE json_array_length(tracks.author_ids_json) = 0 OR EXISTS (SELECT 1 FROM json_each(tracks.author_ids_json) AS item LEFT JOIN authors ON authors.id = CAST(item.value AS INTEGER) WHERE authors.id IS NULL) ORDER BY tracks.id LIMIT 1`},
		{"albums", `SELECT albums.id FROM albums WHERE EXISTS (SELECT 1 FROM json_each(albums.track_ids_json) AS item LEFT JOIN tracks ON tracks.id = CAST(item.value AS INTEGER) WHERE tracks.id IS NULL OR tracks.album_id != albums.id) ORDER BY albums.id LIMIT 1`},
		{"refresh_sessions", `SELECT refresh_sessions.rowid FROM refresh_sessions LEFT JOIN users ON users.id = refresh_sessions.user_id WHERE users.id IS NULL ORDER BY refresh_sessions.rowid LIMIT 1`},
		{"playlists", `SELECT playlists.id FROM playlists LEFT JOIN users ON users.id = playlists.user_id WHERE users.id IS NULL ORDER BY playlists.id LIMIT 1`},
		{"lyrics", `SELECT lyrics.id FROM lyrics LEFT JOIN tracks ON tracks.id = lyrics.track_id WHERE tracks.id IS NULL ORDER BY lyrics.id LIMIT 1`},
	}
	for _, check := range checks {
		var id int64
		err := tx.QueryRowContext(ctx, check.query).Scan(&id)
		if err == nil {
			return fmt.Errorf("invalid SQLite %s row id %d", check.domain, id)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("validate SQLite %s: %w", check.domain, err)
		}
	}
	return nil
}

func currentSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var version int
	err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	return version, nil
}
