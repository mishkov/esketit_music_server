package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// These interfaces are deliberately domain-specific. They form the storage
// boundary used by trackStore; HTTP and import handlers remain unaware of SQL.
type UserRepository interface {
	List(context.Context) ([]user, error)
	FindByID(context.Context, int64) (user, bool, error)
	FindByEmail(context.Context, string) (user, bool, error)
	Count(context.Context) (int, error)
	Insert(context.Context, user) error
	Update(context.Context, user) error
	Delete(context.Context, int64) error
}

type RefreshSessionRepository interface {
	FindByToken(context.Context, string, time.Time) (refreshSession, bool, error)
	Insert(context.Context, refreshSession) error
	Delete(context.Context, string) error
	DeleteExpired(context.Context, time.Time) error
}

type AuthorRepository interface {
	List(context.Context) ([]author, error)
	ListPopularityRankedIDs(context.Context) ([]int64, error)
	FindByID(context.Context, int64) (author, bool, error)
	Insert(context.Context, author) error
	Update(context.Context, author) error
	Delete(context.Context, int64) error
}

type CatalogRepository interface {
	ListAlbums(context.Context) ([]album, error)
	ListTracks(context.Context) ([]track, error)
	FindAlbumByID(context.Context, int64) (album, bool, error)
	FindTrackByID(context.Context, int64) (track, bool, error)
	InsertAlbum(context.Context, album) error
	UpdateAlbum(context.Context, album) error
	DeleteAlbum(context.Context, int64) error
	InsertTrack(context.Context, track) error
	UpdateTrack(context.Context, track) error
	DeleteTrack(context.Context, int64) error
}

type PlaylistRepository interface {
	List(context.Context) ([]playlist, error)
	FindByID(context.Context, int64) (playlist, bool, error)
	Insert(context.Context, playlist) error
	Update(context.Context, playlist) error
	Delete(context.Context, int64) error
}

type LyricsRepository interface {
	List(context.Context) ([]lyrics, error)
	FindByTrackID(context.Context, int64) (lyrics, bool, error)
	Insert(context.Context, lyrics) error
	Update(context.Context, lyrics) error
	DeleteByTrackID(context.Context, int64) error
}

// ReadRepository contains query-shaped operations that do not belong to a
// single mutable aggregate. Keeping them behind the storage boundary avoids
// rebuilding the whole catalog in memory for ordinary API reads.
type ReadRepository interface {
	ListAlbumsPage(context.Context, albumListFilter) ([]album, int, error)
	ListTracksPage(context.Context, trackListFilter) ([]track, int, error)
	ListPlaylistsPage(context.Context, int64, playlistListFilter) ([]playlist, int, error)
	ListTracksByIDs(context.Context, []int64) ([]track, error)
	ListAlbumsByIDs(context.Context, []int64) ([]album, error)
	ListAuthorsByIDs(context.Context, []int64) ([]author, error)
	ListPlaylistsByIDs(context.Context, []int64) ([]playlist, error)
	ListPreferencePlaylists(context.Context, int64) ([]playlist, error)
	FindPlaylistByShareToken(context.Context, string) (playlist, bool, error)
	SearchPage(context.Context, int64, searchListFilter) ([]searchReference, int, error)
	ListTrackAudioReferences(context.Context) ([]trackAudioReference, error)
	ListTracksBySourceProvider(context.Context, string) ([]track, error)
}

type metadataRepository interface {
	AllocateID(context.Context, string) (int64, error)
}

type domainRepositories struct {
	users     UserRepository
	sessions  RefreshSessionRepository
	authors   AuthorRepository
	catalog   CatalogRepository
	playlists PlaylistRepository
	lyrics    LyricsRepository
	metadata  metadataRepository
	reads     ReadRepository
}

// unitOfWork keeps *sql.Tx inside the SQLite adapter. Callers receive only
// domain repositories bound to the same transaction.
type unitOfWork interface {
	WithinTransaction(context.Context, func(domainRepositories) error) error
	WithinReadTransaction(context.Context, func(domainRepositories) error) error
}

type sqlExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type sqliteRepositories struct {
	q sqlExecutor
}

type sqliteUnitOfWork struct {
	db *sql.DB
}

var (
	errRepositoryConflict    = errors.New("repository conflict")
	errRepositoryNotFound    = errors.New("repository record not found")
	errRepositoryPersistence = errors.New("repository persistence failure")
)

type repositoryPersistenceError struct{ cause error }

func (e repositoryPersistenceError) Error() string { return errRepositoryPersistence.Error() }
func (e repositoryPersistenceError) Unwrap() error { return e.cause }
func (e repositoryPersistenceError) Is(target error) bool {
	return target == errRepositoryPersistence
}

func (u *sqliteUnitOfWork) WithinTransaction(ctx context.Context, operation func(domainRepositories) error) (returnErr error) {
	tx, err := u.db.BeginTx(ctx, nil)
	if err != nil {
		return translateSQLiteError(err)
	}
	committed := false
	defer func() {
		if !committed {
			joinRollbackError(&returnErr, tx, "domain write")
		}
	}()

	if err := operation(newDomainRepositories(tx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return translateSQLiteError(err)
	}
	committed = true
	return nil
}

func (u *sqliteUnitOfWork) WithinReadTransaction(ctx context.Context, operation func(domainRepositories) error) (returnErr error) {
	tx, err := u.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return translateSQLiteError(err)
	}
	committed := false
	defer func() {
		if !committed {
			joinRollbackError(&returnErr, tx, "domain read")
		}
	}()

	if err := operation(newDomainRepositories(tx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return translateSQLiteError(err)
	}
	committed = true
	return nil
}

// SQLite constraint errors use extended codes whose low byte is 19. All other
// driver failures retain their cause for diagnostics while exposing only a
// stable repository message upstream.
func translateSQLiteError(err error) error {
	if err == nil {
		return nil
	}
	type codedSQLiteError interface{ Code() int }
	var coded codedSQLiteError
	if errors.As(err, &coded) && coded.Code()&0xff == 19 {
		return errRepositoryConflict
	}
	return repositoryPersistenceError{cause: err}
}

func (r *sqliteRepositories) AllocateID(ctx context.Context, key string) (int64, error) {
	var next int64
	err := r.q.QueryRowContext(ctx, `UPDATE store_metadata
		SET value = value + 1
		WHERE key = ?
		RETURNING value - 1`, key).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: missing ID counter %q", errRepositoryConflict, key)
	}
	if err != nil {
		return 0, translateSQLiteError(err)
	}
	return next, nil
}

func (r *sqliteRepositories) List(ctx context.Context) (items []user, returnErr error) {
	rows, err := r.q.QueryContext(ctx, `SELECT id, email, role, password_hash, created_at FROM users ORDER BY id`)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer joinRowsCloseError(&returnErr, rows, "list users")
	for rows.Next() {
		item, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

type rowScanner interface {
	Scan(...any) error
}

func scanUser(row rowScanner) (user, error) {
	var item user
	var createdAt string
	if err := row.Scan(&item.ID, &item.Email, &item.Role, &item.PasswordHash, &createdAt); err != nil {
		return user{}, translateSQLiteError(err)
	}
	parsed, err := parseSQLiteTime(createdAt)
	if err != nil {
		return user{}, fmt.Errorf("parse user creation time: %w", err)
	}
	item.CreatedAt = parsed
	return item, nil
}

func (r *sqliteRepositories) FindByID(ctx context.Context, id int64) (user, bool, error) {
	item, err := scanUser(r.q.QueryRowContext(ctx, `SELECT id, email, role, password_hash, created_at FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return user{}, false, nil
	}
	return item, err == nil, err
}

func (r *sqliteRepositories) FindByEmail(ctx context.Context, email string) (user, bool, error) {
	item, err := scanUser(r.q.QueryRowContext(ctx, `SELECT id, email, role, password_hash, created_at FROM users WHERE email = ?`, normalizeEmail(email)))
	if errors.Is(err, sql.ErrNoRows) {
		return user{}, false, nil
	}
	return item, err == nil, err
}

func (r *sqliteRepositories) Count(ctx context.Context) (int, error) {
	var count int
	err := r.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count)
	return count, translateSQLiteError(err)
}

func (r *sqliteRepositories) Insert(ctx context.Context, item user) error {
	createdAt := formatSQLiteTime(item.CreatedAt)
	_, err := r.q.ExecContext(ctx, `INSERT INTO users (id, email, role, password_hash, created_at) VALUES (?, ?, ?, ?, ?)`,
		item.ID, item.Email, item.Role, item.PasswordHash, createdAt)
	return translateSQLiteError(err)
}

func (r *sqliteRepositories) Update(ctx context.Context, item user) error {
	result, err := r.q.ExecContext(ctx, `UPDATE users SET email = ?, role = ?, password_hash = ?, created_at = ? WHERE id = ?`,
		item.Email, item.Role, item.PasswordHash, formatSQLiteTime(item.CreatedAt), item.ID)
	if err != nil {
		return translateSQLiteError(err)
	}
	return requireAffected(result)
}

func (r *sqliteRepositories) Delete(ctx context.Context, id int64) error {
	result, err := r.q.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return translateSQLiteError(err)
	}
	return requireAffected(result)
}

func (r *sqliteRepositories) FindByToken(ctx context.Context, rawToken string, now time.Time) (refreshSession, bool, error) {
	var item refreshSession
	var createdAt, expiresAt string
	err := r.q.QueryRowContext(ctx, `SELECT id, user_id, token_hash, created_at, expires_at
		FROM refresh_sessions WHERE token_hash = ? AND expires_at >= ?`, hashToken(rawToken), formatSQLiteTime(now)).
		Scan(&item.ID, &item.UserID, &item.TokenHash, &createdAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return refreshSession{}, false, nil
	}
	if err != nil {
		return refreshSession{}, false, translateSQLiteError(err)
	}
	item.CreatedAt, err = parseSQLiteTime(createdAt)
	if err == nil {
		item.ExpiresAt, err = parseSQLiteTime(expiresAt)
	}
	return item, err == nil, err
}

func (r *sqliteRepositories) InsertSession(ctx context.Context, item refreshSession) error {
	createdAt := formatSQLiteTime(item.CreatedAt)
	expiresAt := formatSQLiteTime(item.ExpiresAt)
	_, err := r.q.ExecContext(ctx, `INSERT INTO refresh_sessions (id, user_id, token_hash, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		item.ID, item.UserID, item.TokenHash, createdAt, expiresAt)
	return translateSQLiteError(err)
}

func (r *sqliteRepositories) DeleteSession(ctx context.Context, id string) error {
	result, err := r.q.ExecContext(ctx, `DELETE FROM refresh_sessions WHERE id = ?`, id)
	if err != nil {
		return translateSQLiteError(err)
	}
	return requireAffected(result)
}

func (r *sqliteRepositories) DeleteExpired(ctx context.Context, now time.Time) error {
	_, err := r.q.ExecContext(ctx, `DELETE FROM refresh_sessions WHERE expires_at < ?`, formatSQLiteTime(now))
	return translateSQLiteError(err)
}

func (r *sqliteRepositories) ListAuthors(ctx context.Context) (items []author, returnErr error) {
	rows, err := r.q.QueryContext(ctx, `SELECT id, current_name, photos_json FROM authors ORDER BY id`)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer joinRowsCloseError(&returnErr, rows, "list authors")
	for rows.Next() {
		var item author
		var photosJSON string
		if err := rows.Scan(&item.ID, &item.CurrentName, &photosJSON); err != nil {
			return nil, translateSQLiteError(err)
		}
		if err := unmarshalJSONColumn(photosJSON, &item.Photos); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func (r *sqliteRepositories) ListPopularityRankedAuthorIDs(ctx context.Context) (items []int64, returnErr error) {
	rows, err := r.q.QueryContext(ctx, `SELECT author_id FROM author_popularity_snapshot ORDER BY ranking_position`)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer joinRowsCloseError(&returnErr, rows, "list popularity-ranked authors")
	for rows.Next() {
		var authorID int64
		if err := rows.Scan(&authorID); err != nil {
			return nil, translateSQLiteError(err)
		}
		items = append(items, authorID)
	}
	return items, translateSQLiteError(rows.Err())
}

func scanAuthor(row rowScanner) (author, error) {
	var item author
	var photosJSON string
	if err := row.Scan(&item.ID, &item.CurrentName, &photosJSON); err != nil {
		return author{}, translateSQLiteError(err)
	}
	if err := unmarshalJSONColumn(photosJSON, &item.Photos); err != nil {
		return author{}, err
	}
	return item, nil
}

func (r *sqliteRepositories) FindAuthorByID(ctx context.Context, id int64) (author, bool, error) {
	item, err := scanAuthor(r.q.QueryRowContext(ctx, `SELECT id, current_name, photos_json FROM authors WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return author{}, false, nil
	}
	return item, err == nil, err
}

func (r *sqliteRepositories) InsertAuthor(ctx context.Context, item author) error {
	photosJSON, err := marshalJSONColumn(item.Photos)
	if err != nil {
		return err
	}
	_, err = r.q.ExecContext(ctx, `INSERT INTO authors (id, current_name, photos_json) VALUES (?, ?, ?)`, item.ID, item.CurrentName, photosJSON)
	return translateSQLiteError(err)
}

func (r *sqliteRepositories) UpdateAuthor(ctx context.Context, item author) error {
	photosJSON, err := marshalJSONColumn(item.Photos)
	if err != nil {
		return err
	}
	result, err := r.q.ExecContext(ctx, `UPDATE authors SET current_name = ?, photos_json = ? WHERE id = ?`, item.CurrentName, photosJSON, item.ID)
	if err != nil {
		return translateSQLiteError(err)
	}
	return requireAffected(result)
}

func (r *sqliteRepositories) DeleteAuthor(ctx context.Context, id int64) error {
	result, err := r.q.ExecContext(ctx, `DELETE FROM authors WHERE id = ?`, id)
	if err != nil {
		return translateSQLiteError(err)
	}
	return requireAffected(result)
}

func (r *sqliteRepositories) ListAlbums(ctx context.Context) (items []album, returnErr error) {
	rows, err := r.q.QueryContext(ctx, `SELECT id, title, cover_image_path, author_ids_json, release_date, is_published, track_ids_json, additional_info_json FROM albums ORDER BY id`)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer joinRowsCloseError(&returnErr, rows, "list albums")
	for rows.Next() {
		var item album
		var authorIDsJSON, releaseDate, trackIDsJSON, additionalInfoJSON string
		var isPublished int
		if err := rows.Scan(&item.ID, &item.Title, &item.CoverImagePath, &authorIDsJSON, &releaseDate, &isPublished, &trackIDsJSON, &additionalInfoJSON); err != nil {
			return nil, translateSQLiteError(err)
		}
		if err := unmarshalJSONColumn(authorIDsJSON, &item.AuthorIDs); err != nil {
			return nil, err
		}
		if err := unmarshalJSONColumn(trackIDsJSON, &item.TrackIDs); err != nil {
			return nil, err
		}
		if err := unmarshalJSONColumn(additionalInfoJSON, &item.AdditionalInfo); err != nil {
			return nil, err
		}
		item.ReleaseDate, err = parseSQLiteTime(releaseDate)
		if err != nil {
			return nil, err
		}
		item.IsPublished = isPublished != 0
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func scanAlbum(row rowScanner) (album, error) {
	var item album
	var authorIDsJSON, releaseDate, trackIDsJSON, additionalInfoJSON string
	var isPublished int
	if err := row.Scan(&item.ID, &item.Title, &item.CoverImagePath, &authorIDsJSON, &releaseDate, &isPublished, &trackIDsJSON, &additionalInfoJSON); err != nil {
		return album{}, translateSQLiteError(err)
	}
	if err := unmarshalJSONColumn(authorIDsJSON, &item.AuthorIDs); err != nil {
		return album{}, err
	}
	if err := unmarshalJSONColumn(trackIDsJSON, &item.TrackIDs); err != nil {
		return album{}, err
	}
	if err := unmarshalJSONColumn(additionalInfoJSON, &item.AdditionalInfo); err != nil {
		return album{}, err
	}
	parsed, err := parseSQLiteTime(releaseDate)
	if err != nil {
		return album{}, err
	}
	item.ReleaseDate = parsed
	item.IsPublished = isPublished != 0
	return item, nil
}

func (r *sqliteRepositories) FindAlbumByID(ctx context.Context, id int64) (album, bool, error) {
	item, err := scanAlbum(r.q.QueryRowContext(ctx, `SELECT id, title, cover_image_path, author_ids_json, release_date, is_published, track_ids_json, additional_info_json FROM albums WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return album{}, false, nil
	}
	return item, err == nil, err
}

func (r *sqliteRepositories) ListTracks(ctx context.Context) (items []track, returnErr error) {
	rows, err := r.q.QueryContext(ctx, `SELECT id, name, author_ids_json, album_id, audio_file_path, additional_info_json, source_metadata_json, created_at FROM tracks ORDER BY id`)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer joinRowsCloseError(&returnErr, rows, "list tracks")
	for rows.Next() {
		var item track
		var authorIDsJSON, additionalInfoJSON, sourceMetadataJSON, createdAt string
		if err := rows.Scan(&item.ID, &item.Name, &authorIDsJSON, &item.AlbumID, &item.AudioFilePath, &additionalInfoJSON, &sourceMetadataJSON, &createdAt); err != nil {
			return nil, translateSQLiteError(err)
		}
		if err := unmarshalJSONColumn(authorIDsJSON, &item.AuthorIDs); err != nil {
			return nil, err
		}
		if err := unmarshalJSONColumn(additionalInfoJSON, &item.AdditionalInfo); err != nil {
			return nil, err
		}
		if err := unmarshalJSONColumn(sourceMetadataJSON, &item.SourceMetadata); err != nil {
			return nil, err
		}
		item.CreatedAt, err = parseSQLiteTime(createdAt)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func scanTrack(row rowScanner) (track, error) {
	var item track
	var authorIDsJSON, additionalInfoJSON, sourceMetadataJSON, createdAt string
	if err := row.Scan(&item.ID, &item.Name, &authorIDsJSON, &item.AlbumID, &item.AudioFilePath, &additionalInfoJSON, &sourceMetadataJSON, &createdAt); err != nil {
		return track{}, translateSQLiteError(err)
	}
	if err := unmarshalJSONColumn(authorIDsJSON, &item.AuthorIDs); err != nil {
		return track{}, err
	}
	if err := unmarshalJSONColumn(additionalInfoJSON, &item.AdditionalInfo); err != nil {
		return track{}, err
	}
	if err := unmarshalJSONColumn(sourceMetadataJSON, &item.SourceMetadata); err != nil {
		return track{}, err
	}
	parsed, err := parseSQLiteTime(createdAt)
	if err != nil {
		return track{}, err
	}
	item.CreatedAt = parsed
	return item, nil
}

func (r *sqliteRepositories) FindTrackByID(ctx context.Context, id int64) (track, bool, error) {
	item, err := scanTrack(r.q.QueryRowContext(ctx, `SELECT id, name, author_ids_json, album_id, audio_file_path, additional_info_json, source_metadata_json, created_at FROM tracks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return track{}, false, nil
	}
	return item, err == nil, err
}

func encodeAlbum(item album) (string, string, string, error) {
	authorIDsJSON, err := marshalJSONColumn(item.AuthorIDs)
	if err != nil {
		return "", "", "", err
	}
	trackIDsJSON, err := marshalJSONColumn(item.TrackIDs)
	if err != nil {
		return "", "", "", err
	}
	additionalInfoJSON, err := marshalJSONColumn(item.AdditionalInfo)
	if err != nil {
		return "", "", "", err
	}
	return authorIDsJSON, trackIDsJSON, additionalInfoJSON, nil
}

func (r *sqliteRepositories) InsertAlbum(ctx context.Context, item album) error {
	authorIDsJSON, trackIDsJSON, additionalInfoJSON, err := encodeAlbum(item)
	if err != nil {
		return err
	}
	_, err = r.q.ExecContext(ctx, `INSERT INTO albums (id, title, cover_image_path, author_ids_json, release_date, is_published, track_ids_json, additional_info_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, item.ID, item.Title, item.CoverImagePath, authorIDsJSON, formatSQLiteTime(item.ReleaseDate), boolToSQLiteInt(item.IsPublished), trackIDsJSON, additionalInfoJSON)
	return translateSQLiteError(err)
}

func (r *sqliteRepositories) UpdateAlbum(ctx context.Context, item album) error {
	authorIDsJSON, trackIDsJSON, additionalInfoJSON, err := encodeAlbum(item)
	if err != nil {
		return err
	}
	result, err := r.q.ExecContext(ctx, `UPDATE albums SET title = ?, cover_image_path = ?, author_ids_json = ?, release_date = ?,
		is_published = ?, track_ids_json = ?, additional_info_json = ? WHERE id = ?`,
		item.Title, item.CoverImagePath, authorIDsJSON, formatSQLiteTime(item.ReleaseDate), boolToSQLiteInt(item.IsPublished), trackIDsJSON, additionalInfoJSON, item.ID)
	if err != nil {
		return translateSQLiteError(err)
	}
	return requireAffected(result)
}

func (r *sqliteRepositories) DeleteAlbum(ctx context.Context, id int64) error {
	result, err := r.q.ExecContext(ctx, `DELETE FROM albums WHERE id = ?`, id)
	if err != nil {
		return translateSQLiteError(err)
	}
	return requireAffected(result)
}

func encodeTrack(item track) (string, string, string, error) {
	authorIDsJSON, err := marshalJSONColumn(item.AuthorIDs)
	if err != nil {
		return "", "", "", err
	}
	additionalInfoJSON, err := marshalJSONColumn(item.AdditionalInfo)
	if err != nil {
		return "", "", "", err
	}
	sourceMetadataJSON, err := marshalJSONColumn(item.SourceMetadata)
	if err != nil {
		return "", "", "", err
	}
	return authorIDsJSON, additionalInfoJSON, sourceMetadataJSON, nil
}

func (r *sqliteRepositories) InsertTrack(ctx context.Context, item track) error {
	authorIDsJSON, additionalInfoJSON, sourceMetadataJSON, err := encodeTrack(item)
	if err != nil {
		return err
	}
	_, err = r.q.ExecContext(ctx, `INSERT INTO tracks (id, name, author_ids_json, album_id, audio_file_path, additional_info_json, source_metadata_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, item.ID, item.Name, authorIDsJSON, item.AlbumID, item.AudioFilePath, additionalInfoJSON, sourceMetadataJSON, formatSQLiteTime(item.CreatedAt))
	return translateSQLiteError(err)
}

func (r *sqliteRepositories) UpdateTrack(ctx context.Context, item track) error {
	authorIDsJSON, additionalInfoJSON, sourceMetadataJSON, err := encodeTrack(item)
	if err != nil {
		return err
	}
	result, err := r.q.ExecContext(ctx, `UPDATE tracks SET name = ?, author_ids_json = ?, album_id = ?, audio_file_path = ?,
		additional_info_json = ?, source_metadata_json = ?, created_at = ? WHERE id = ?`,
		item.Name, authorIDsJSON, item.AlbumID, item.AudioFilePath, additionalInfoJSON, sourceMetadataJSON, formatSQLiteTime(item.CreatedAt), item.ID)
	if err != nil {
		return translateSQLiteError(err)
	}
	return requireAffected(result)
}

func (r *sqliteRepositories) DeleteTrack(ctx context.Context, id int64) error {
	result, err := r.q.ExecContext(ctx, `DELETE FROM tracks WHERE id = ?`, id)
	if err != nil {
		return translateSQLiteError(err)
	}
	return requireAffected(result)
}

func (r *sqliteRepositories) ListPlaylists(ctx context.Context) (items []playlist, returnErr error) {
	rows, err := r.q.QueryContext(ctx, `SELECT id, user_id, name, description, cover_image_path, visibility, share_token, track_items_json, system, kind FROM playlists ORDER BY user_id, id`)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer joinRowsCloseError(&returnErr, rows, "list playlists")
	for rows.Next() {
		var item playlist
		var trackItemsJSON string
		var system int
		if err := rows.Scan(&item.ID, &item.UserID, &item.Name, &item.Description, &item.CoverImagePath, &item.Visibility, &item.ShareToken, &trackItemsJSON, &system, &item.Kind); err != nil {
			return nil, translateSQLiteError(err)
		}
		if err := unmarshalJSONColumn(trackItemsJSON, &item.TrackItems); err != nil {
			return nil, err
		}
		item.System = system != 0
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func scanPlaylist(row rowScanner) (playlist, error) {
	var item playlist
	var trackItemsJSON string
	var system int
	if err := row.Scan(&item.ID, &item.UserID, &item.Name, &item.Description, &item.CoverImagePath, &item.Visibility, &item.ShareToken, &trackItemsJSON, &system, &item.Kind); err != nil {
		return playlist{}, translateSQLiteError(err)
	}
	if err := unmarshalJSONColumn(trackItemsJSON, &item.TrackItems); err != nil {
		return playlist{}, err
	}
	item.System = system != 0
	return item, nil
}

func (r *sqliteRepositories) FindPlaylistByID(ctx context.Context, id int64) (playlist, bool, error) {
	item, err := scanPlaylist(r.q.QueryRowContext(ctx, `SELECT id, user_id, name, description, cover_image_path, visibility, share_token, track_items_json, system, kind FROM playlists WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return playlist{}, false, nil
	}
	return item, err == nil, err
}

func encodePlaylist(item playlist) (string, error) {
	trackItemsJSON, err := marshalJSONColumn(item.TrackItems)
	if err != nil {
		return "", err
	}
	return trackItemsJSON, nil
}

func (r *sqliteRepositories) InsertPlaylist(ctx context.Context, item playlist) error {
	trackItemsJSON, err := encodePlaylist(item)
	if err != nil {
		return err
	}
	_, err = r.q.ExecContext(ctx, `INSERT INTO playlists (id, user_id, name, description, cover_image_path, visibility, share_token, track_items_json, system, kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, item.ID, item.UserID, item.Name, item.Description, item.CoverImagePath, item.Visibility, item.ShareToken, trackItemsJSON, boolToSQLiteInt(item.System), item.Kind)
	return translateSQLiteError(err)
}

func (r *sqliteRepositories) UpdatePlaylist(ctx context.Context, item playlist) error {
	trackItemsJSON, err := encodePlaylist(item)
	if err != nil {
		return err
	}
	result, err := r.q.ExecContext(ctx, `UPDATE playlists SET user_id = ?, name = ?, description = ?, cover_image_path = ?,
		visibility = ?, share_token = ?, track_items_json = ?, system = ?, kind = ? WHERE id = ?`,
		item.UserID, item.Name, item.Description, item.CoverImagePath, item.Visibility, item.ShareToken, trackItemsJSON, boolToSQLiteInt(item.System), item.Kind, item.ID)
	if err != nil {
		return translateSQLiteError(err)
	}
	return requireAffected(result)
}

func (r *sqliteRepositories) DeletePlaylist(ctx context.Context, id int64) error {
	result, err := r.q.ExecContext(ctx, `DELETE FROM playlists WHERE id = ?`, id)
	if err != nil {
		return translateSQLiteError(err)
	}
	return requireAffected(result)
}

func (r *sqliteRepositories) ListLyrics(ctx context.Context) (items []lyrics, returnErr error) {
	rows, err := r.q.QueryContext(ctx, `SELECT id, track_id, type, plain_text, language_code, source, is_verified, updated_at, created_at, lines_json FROM lyrics ORDER BY track_id`)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer joinRowsCloseError(&returnErr, rows, "list lyrics")
	for rows.Next() {
		var item lyrics
		var plainText, languageCode, source sql.NullString
		var verified int
		var updatedAt, createdAt, linesJSON string
		if err := rows.Scan(&item.ID, &item.TrackID, &item.Type, &plainText, &languageCode, &source, &verified, &updatedAt, &createdAt, &linesJSON); err != nil {
			return nil, translateSQLiteError(err)
		}
		item.PlainText = nullStringPointer(plainText)
		item.LanguageCode = nullStringPointer(languageCode)
		item.Source = nullStringPointer(source)
		item.IsVerified = verified != 0
		item.UpdatedAt, err = parseSQLiteTime(updatedAt)
		if err != nil {
			return nil, err
		}
		item.CreatedAt, err = parseSQLiteTime(createdAt)
		if err != nil {
			return nil, err
		}
		if err := unmarshalJSONColumn(linesJSON, &item.Lines); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func scanLyrics(row rowScanner) (lyrics, error) {
	var item lyrics
	var plainText, languageCode, source sql.NullString
	var verified int
	var updatedAt, createdAt, linesJSON string
	if err := row.Scan(&item.ID, &item.TrackID, &item.Type, &plainText, &languageCode, &source, &verified, &updatedAt, &createdAt, &linesJSON); err != nil {
		return lyrics{}, translateSQLiteError(err)
	}
	item.PlainText = nullStringPointer(plainText)
	item.LanguageCode = nullStringPointer(languageCode)
	item.Source = nullStringPointer(source)
	item.IsVerified = verified != 0
	var err error
	item.UpdatedAt, err = parseSQLiteTime(updatedAt)
	if err == nil {
		item.CreatedAt, err = parseSQLiteTime(createdAt)
	}
	if err == nil {
		err = unmarshalJSONColumn(linesJSON, &item.Lines)
	}
	return item, err
}

func (r *sqliteRepositories) FindLyricsByTrackID(ctx context.Context, trackID int64) (lyrics, bool, error) {
	item, err := scanLyrics(r.q.QueryRowContext(ctx, `SELECT id, track_id, type, plain_text, language_code, source, is_verified, updated_at, created_at, lines_json FROM lyrics WHERE track_id = ?`, trackID))
	if errors.Is(err, sql.ErrNoRows) {
		return lyrics{}, false, nil
	}
	return item, err == nil, err
}

func encodeLyrics(item lyrics) (string, error) {
	linesJSON, err := marshalJSONColumn(item.Lines)
	if err != nil {
		return "", err
	}
	return linesJSON, nil
}

func (r *sqliteRepositories) InsertLyrics(ctx context.Context, item lyrics) error {
	linesJSON, err := encodeLyrics(item)
	if err != nil {
		return err
	}
	_, err = r.q.ExecContext(ctx, `INSERT INTO lyrics (id, track_id, type, plain_text, language_code, source, is_verified, updated_at, created_at, lines_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, item.ID, item.TrackID, item.Type, sqlNullString(item.PlainText), sqlNullString(item.LanguageCode), sqlNullString(item.Source), boolToSQLiteInt(item.IsVerified), formatSQLiteTime(item.UpdatedAt), formatSQLiteTime(item.CreatedAt), linesJSON)
	return translateSQLiteError(err)
}

func (r *sqliteRepositories) UpdateLyrics(ctx context.Context, item lyrics) error {
	linesJSON, err := encodeLyrics(item)
	if err != nil {
		return err
	}
	result, err := r.q.ExecContext(ctx, `UPDATE lyrics SET track_id = ?, type = ?, plain_text = ?, language_code = ?, source = ?,
		is_verified = ?, updated_at = ?, created_at = ?, lines_json = ? WHERE id = ?`, item.TrackID, item.Type, sqlNullString(item.PlainText), sqlNullString(item.LanguageCode), sqlNullString(item.Source), boolToSQLiteInt(item.IsVerified), formatSQLiteTime(item.UpdatedAt), formatSQLiteTime(item.CreatedAt), linesJSON, item.ID)
	if err != nil {
		return translateSQLiteError(err)
	}
	return requireAffected(result)
}

func (r *sqliteRepositories) DeleteByTrackID(ctx context.Context, trackID int64) error {
	result, err := r.q.ExecContext(ctx, `DELETE FROM lyrics WHERE track_id = ?`, trackID)
	if err != nil {
		return translateSQLiteError(err)
	}
	return requireAffected(result)
}

func requireAffected(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return translateSQLiteError(err)
	}
	if affected == 0 {
		return errRepositoryNotFound
	}
	return nil
}

// Go does not support overloading, so the concrete adapter methods below
// bridge the domain interfaces that intentionally share method names.
type sqliteUserRepository struct{ *sqliteRepositories }
type sqliteSessionRepository struct{ *sqliteRepositories }
type sqliteAuthorRepository struct{ *sqliteRepositories }
type sqlitePlaylistRepository struct{ *sqliteRepositories }
type sqliteLyricsRepository struct{ *sqliteRepositories }

func (r sqliteSessionRepository) Insert(ctx context.Context, item refreshSession) error {
	return r.InsertSession(ctx, item)
}
func (r sqliteSessionRepository) Delete(ctx context.Context, id string) error {
	return r.DeleteSession(ctx, id)
}
func (r sqliteAuthorRepository) List(ctx context.Context) ([]author, error) {
	return r.ListAuthors(ctx)
}
func (r sqliteAuthorRepository) ListPopularityRankedIDs(ctx context.Context) ([]int64, error) {
	return r.ListPopularityRankedAuthorIDs(ctx)
}
func (r sqliteAuthorRepository) FindByID(ctx context.Context, id int64) (author, bool, error) {
	return r.FindAuthorByID(ctx, id)
}
func (r sqliteAuthorRepository) Insert(ctx context.Context, item author) error {
	return r.InsertAuthor(ctx, item)
}
func (r sqliteAuthorRepository) Update(ctx context.Context, item author) error {
	return r.UpdateAuthor(ctx, item)
}
func (r sqliteAuthorRepository) Delete(ctx context.Context, id int64) error {
	return r.DeleteAuthor(ctx, id)
}
func (r sqlitePlaylistRepository) List(ctx context.Context) ([]playlist, error) {
	return r.ListPlaylists(ctx)
}
func (r sqlitePlaylistRepository) FindByID(ctx context.Context, id int64) (playlist, bool, error) {
	return r.FindPlaylistByID(ctx, id)
}
func (r sqlitePlaylistRepository) Insert(ctx context.Context, item playlist) error {
	return r.InsertPlaylist(ctx, item)
}
func (r sqlitePlaylistRepository) Update(ctx context.Context, item playlist) error {
	return r.UpdatePlaylist(ctx, item)
}
func (r sqlitePlaylistRepository) Delete(ctx context.Context, id int64) error {
	return r.DeletePlaylist(ctx, id)
}
func (r sqliteLyricsRepository) List(ctx context.Context) ([]lyrics, error) {
	return r.ListLyrics(ctx)
}
func (r sqliteLyricsRepository) FindByTrackID(ctx context.Context, trackID int64) (lyrics, bool, error) {
	return r.FindLyricsByTrackID(ctx, trackID)
}
func (r sqliteLyricsRepository) Insert(ctx context.Context, item lyrics) error {
	return r.InsertLyrics(ctx, item)
}
func (r sqliteLyricsRepository) Update(ctx context.Context, item lyrics) error {
	return r.UpdateLyrics(ctx, item)
}

func newDomainRepositories(q sqlExecutor) domainRepositories {
	base := &sqliteRepositories{q: q}
	return domainRepositories{
		users:     sqliteUserRepository{base},
		sessions:  sqliteSessionRepository{base},
		authors:   sqliteAuthorRepository{base},
		catalog:   base,
		playlists: sqlitePlaylistRepository{base},
		lyrics:    sqliteLyricsRepository{base},
		metadata:  base,
		reads:     base,
	}
}
