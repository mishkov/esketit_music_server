package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var errTrackNumberConflict = errors.New("track number is already occupied in this album")

type trackNumberAssignment struct {
	TrackID     int64 `json:"trackId"`
	TrackNumber int   `json:"trackNumber"`
}

func migratePersistentTrackNumbers(ctx context.Context, tx *sql.Tx) error {
	// Refuse ambiguous legacy membership instead of discarding or inventing order.
	var invalid int
	checks := []string{
		`SELECT COUNT(*) FROM albums WHERE NOT json_valid(track_ids_json) OR json_type(track_ids_json)!='array'`,
		`SELECT COUNT(*) FROM albums a,json_each(a.track_ids_json) j LEFT JOIN tracks t ON t.id=j.value AND t.album_id=a.id WHERE j.type!='integer' OR t.id IS NULL`,
		`SELECT COUNT(*) FROM tracks t WHERE (SELECT COUNT(*) FROM albums a,json_each(a.track_ids_json) j WHERE a.id=t.album_id AND j.value=t.id)!=1`,
	}
	for _, query := range checks {
		if err := tx.QueryRowContext(ctx, query).Scan(&invalid); err != nil {
			return err
		}
		if invalid != 0 {
			return fmt.Errorf("persistent track numbers migration: inconsistent legacy album membership (%d rows); no changes applied", invalid)
		}
	}
	statements := []string{
		`CREATE TABLE tracks_numbered (
 id INTEGER PRIMARY KEY, name TEXT NOT NULL, author_ids_json TEXT NOT NULL,
 album_id INTEGER NOT NULL REFERENCES albums(id) ON DELETE RESTRICT,
 track_number INTEGER NOT NULL CHECK(track_number > 0),
 audio_file_path TEXT NOT NULL, additional_info_json TEXT NOT NULL, source_metadata_json TEXT NOT NULL, created_at TEXT NOT NULL,
 publication_status TEXT NOT NULL DEFAULT 'published' CHECK(publication_status IN ('published','pending_review','changes_requested')),
 requested_by_user_id INTEGER REFERENCES users(id) ON DELETE RESTRICT, UNIQUE(album_id,track_number))`,
		`INSERT INTO tracks_numbered SELECT t.id,t.name,t.author_ids_json,t.album_id,
 (SELECT CAST(j.key AS INTEGER)+1 FROM albums a,json_each(a.track_ids_json) j WHERE a.id=t.album_id AND CAST(j.value AS INTEGER)=t.id),
 t.audio_file_path,t.additional_info_json,t.source_metadata_json,t.created_at,t.publication_status,t.requested_by_user_id FROM tracks t`,
		// Dropping tracks cascades only claimed uploads. Unclaimed rows remain
		// in place and must not be inserted again when restoring the claims.
		`CREATE TEMP TABLE numbered_uploads AS SELECT * FROM catalog_submission_uploads WHERE claimed_track_id IS NOT NULL`,
		`DROP TABLE tracks`,
		`ALTER TABLE tracks_numbered RENAME TO tracks`,
		`INSERT INTO catalog_submission_uploads SELECT * FROM numbered_uploads`,
		`DROP TABLE numbered_uploads`,
		`CREATE INDEX idx_tracks_album_id ON tracks(album_id)`,
		`CREATE INDEX idx_tracks_publication ON tracks(publication_status,requested_by_user_id,id)`,
		`ALTER TABLE albums DROP COLUMN track_ids_json`,
		`CREATE TRIGGER track_number_insert AFTER INSERT ON tracks BEGIN UPDATE albums SET order_revision=order_revision+1 WHERE id=NEW.album_id; END`,
		`CREATE TRIGGER track_number_delete AFTER DELETE ON tracks BEGIN UPDATE albums SET order_revision=order_revision+1 WHERE id=OLD.album_id; END`,
		`CREATE TRIGGER track_number_update AFTER UPDATE OF album_id,track_number ON tracks WHEN OLD.album_id!=NEW.album_id OR OLD.track_number!=NEW.track_number BEGIN
 UPDATE albums SET order_revision=order_revision+1 WHERE id=OLD.album_id;
 UPDATE albums SET order_revision=order_revision+1 WHERE id=NEW.album_id AND NEW.album_id!=OLD.album_id; END`,
	}
	for _, q := range statements {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	// Convert stored proposals, including terminal history, rather than retaining
	// two competing payload contracts. Historic deleted track numbers are unknown.
	rows, err := tx.QueryContext(ctx, `SELECT id,entity_type,entity_id,snapshot_json FROM catalog_submissions WHERE entity_type IN ('track','album_track_order')`)
	if err != nil {
		return err
	}
	type saved struct {
		id       int64
		kind     string
		entityID int64
		payload  map[string]any
	}
	items := []saved{}
	for rows.Next() {
		var v saved
		var raw string
		if err := rows.Scan(&v.id, &v.kind, &v.entityID, &raw); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(raw), &v.payload); err != nil {
			rows.Close()
			return err
		}
		items = append(items, v)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, v := range items {
		if v.kind == "album_track_order" {
			for old, newKey := range map[string]string{"baseTrackIds": "baseTracks", "trackIds": "tracks"} {
				raw, _ := json.Marshal(v.payload[old])
				var ids []int64
				if err := json.Unmarshal(raw, &ids); err != nil {
					return err
				}
				assignments := make([]trackNumberAssignment, 0, len(ids))
				for i, id := range ids {
					assignments = append(assignments, trackNumberAssignment{TrackID: id, TrackNumber: i + 1})
				}
				v.payload[newKey] = assignments
				delete(v.payload, old)
			}
		} else {
			var number int
			err := tx.QueryRowContext(ctx, `SELECT track_number FROM tracks WHERE id=?`, v.entityID).Scan(&number)
			if err == nil {
				v.payload["trackNumber"] = number
			} else if errors.Is(err, sql.ErrNoRows) {
				v.payload["trackNumber"] = nil
			} else {
				return err
			}
		}
		raw, err := json.Marshal(v.payload)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE catalog_submissions SET snapshot_json=? WHERE id=?`, string(raw), v.id); err != nil {
			return err
		}
	}
	return nil
}

func validateTrackNumbers(assignments []trackNumberAssignment, members []int64) error {
	if assignments == nil {
		return fmt.Errorf("%w: tracks assignments are required", errInvalidAlbum)
	}
	ids := make([]int64, 0, len(assignments))
	numbers := map[int]bool{}
	for _, a := range assignments {
		if a.TrackNumber < 1 {
			return fmt.Errorf("%w: trackNumber must be positive", errInvalidAlbum)
		}
		if numbers[a.TrackNumber] {
			return fmt.Errorf("%w: duplicate trackNumber %d", errInvalidAlbum, a.TrackNumber)
		}
		numbers[a.TrackNumber] = true
		ids = append(ids, a.TrackID)
	}
	if err := validateExactTrackOrder(ids, members); err != nil {
		return fmt.Errorf("%w: %v", errInvalidAlbum, err)
	}
	return nil
}

func assignmentsForTracks(tracks []track) []trackNumberAssignment {
	result := make([]trackNumberAssignment, 0, len(tracks))
	for _, t := range tracks {
		result = append(result, trackNumberAssignment{TrackID: t.ID, TrackNumber: t.TrackNumber})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].TrackNumber < result[j].TrackNumber })
	return result
}

func (r *sqliteRepositories) SetAlbumTrackNumbers(ctx context.Context, albumID int64, assignments []trackNumberAssignment) error {
	current, err := r.ListAlbumTracks(ctx, albumID)
	if err != nil {
		return err
	}
	if err := validateTrackNumbers(assignments, trackIDsForTracks(current)); err != nil {
		return err
	}
	original, found, err := r.FindAlbumByID(ctx, albumID)
	if err != nil {
		return err
	}
	if !found {
		return errRepositoryNotFound
	}
	numbers := map[int]bool{}
	currentNumbers := map[int64]int{}
	changed := false
	for _, t := range current {
		numbers[t.TrackNumber] = true
		currentNumbers[t.ID] = t.TrackNumber
	}
	for _, a := range assignments {
		numbers[a.TrackNumber] = true
		if currentNumbers[a.TrackID] != a.TrackNumber {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	// Positive unused staging slots permit swaps under the immediate UNIQUE and
	// CHECK constraints. No intermediate assignments escape this transaction.
	next := 1
	for _, a := range assignments {
		for numbers[next] {
			next++
		}
		numbers[next] = true
		if _, err := r.q.ExecContext(ctx, `UPDATE tracks SET track_number=? WHERE id=? AND album_id=?`, next, a.TrackID, albumID); err != nil {
			return translateSQLiteError(err)
		}
	}
	for _, a := range assignments {
		if _, err := r.q.ExecContext(ctx, `UPDATE tracks SET track_number=? WHERE id=? AND album_id=?`, a.TrackNumber, a.TrackID, albumID); err != nil {
			return translateSQLiteError(err)
		}
	}
	_, err = r.q.ExecContext(ctx, `UPDATE albums SET order_revision=? WHERE id=?`, original.OrderRevision+1, albumID)
	return translateSQLiteError(err)
}

func trackIDsForTracks(tracks []track) []int64 {
	ids := make([]int64, 0, len(tracks))
	for _, t := range tracks {
		ids = append(ids, t.ID)
	}
	return ids
}

func trackWriteError(err error) error {
	if err != nil && strings.Contains(err.Error(), "tracks.album_id, tracks.track_number") {
		return errTrackNumberConflict
	}
	return translateSQLiteError(err)
}

// Historic playlist tombstones may predate numbering and have no known number.
func knownTrackNumber(number int) *int {
	if number < 1 {
		return nil
	}
	return &number
}
