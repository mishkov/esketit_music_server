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
	_, err = tx.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_playlists_user_system_kind
		ON playlists (user_id, kind) WHERE kind IN ('favorites', 'dislikes')`)
	return err
}

func currentSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var version int
	err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	return version, nil
}
