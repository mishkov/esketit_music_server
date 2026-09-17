package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"
)

// These interfaces are deliberately domain-specific. They form the storage
// boundary used by trackStore; HTTP and import handlers remain unaware of SQL.
type UserRepository interface {
	List(context.Context) ([]user, error)
	FindByID(context.Context, int64) (user, bool, error)
	FindByEmail(context.Context, string) (user, bool, error)
	Save(context.Context, user) error
	Delete(context.Context, int64) error
}

type RefreshSessionRepository interface {
	List(context.Context) ([]refreshSession, error)
	FindByToken(context.Context, string, time.Time) (refreshSession, bool, error)
	Save(context.Context, refreshSession) error
	Delete(context.Context, string) error
	DeleteExpired(context.Context, time.Time) error
}

type AuthorRepository interface {
	List(context.Context) ([]author, error)
	Save(context.Context, author) error
	Delete(context.Context, int64) error
}

type CatalogRepository interface {
	ListAlbums(context.Context) ([]album, error)
	ListTracks(context.Context) ([]track, error)
	SaveAlbum(context.Context, album) error
	DeleteAlbum(context.Context, int64) error
	SaveTrack(context.Context, track) error
	DeleteTrack(context.Context, int64) error
}

type PlaylistRepository interface {
	List(context.Context) ([]playlist, error)
	Save(context.Context, playlist) error
	Delete(context.Context, int64) error
}

type LyricsRepository interface {
	List(context.Context) ([]lyrics, error)
	Save(context.Context, lyrics) error
	DeleteByTrackID(context.Context, int64) error
}

type metadataRepository interface {
	LoadNextIDs(context.Context) (map[string]int64, error)
	SetNextID(context.Context, string, int64) error
	AdvanceNextID(context.Context, string, int64, int64) error
}

type domainRepositories struct {
	users     UserRepository
	sessions  RefreshSessionRepository
	authors   AuthorRepository
	catalog   CatalogRepository
	playlists PlaylistRepository
	lyrics    LyricsRepository
	metadata  metadataRepository
}

// unitOfWork keeps *sql.Tx inside the SQLite adapter. Callers receive only
// domain repositories bound to the same transaction.
type unitOfWork interface {
	WithinTransaction(context.Context, func(domainRepositories) error) error
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
	errRepositoryConflict = errors.New("repository conflict")
	errRepositoryNotFound = errors.New("repository record not found")
)

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

// SQLite constraint errors use extended codes whose low byte is 19. Keep the
// driver detail at this boundary instead of exposing SQLite messages upstream.
func translateSQLiteError(err error) error {
	if err == nil {
		return nil
	}
	type codedSQLiteError interface{ Code() int }
	var coded codedSQLiteError
	if errors.As(err, &coded) && coded.Code()&0xff == 19 {
		return fmt.Errorf("%w: %v", errRepositoryConflict, err)
	}
	return err
}

func (r *sqliteRepositories) SetNextID(ctx context.Context, key string, value int64) error {
	_, err := r.q.ExecContext(ctx, `INSERT INTO store_metadata (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
		WHERE store_metadata.value < excluded.value`, key, value)
	return translateSQLiteError(err)
}

func (r *sqliteRepositories) LoadNextIDs(ctx context.Context) (values map[string]int64, returnErr error) {
	rows, err := r.q.QueryContext(ctx, `SELECT key, value FROM store_metadata`)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer joinRowsCloseError(&returnErr, rows, "load ID counters")
	values = make(map[string]int64)
	for rows.Next() {
		var key string
		var value int64
		if err := rows.Scan(&key, &value); err != nil {
			return nil, translateSQLiteError(err)
		}
		values[key] = value
	}
	return values, translateSQLiteError(rows.Err())
}

func (r *sqliteRepositories) AdvanceNextID(ctx context.Context, key string, expected, next int64) error {
	if next <= expected {
		return fmt.Errorf("%w: invalid %s counter advance from %d to %d", errRepositoryConflict, key, expected, next)
	}
	result, err := r.q.ExecContext(ctx, `UPDATE store_metadata SET value = ? WHERE key = ? AND value = ?`, next, key, expected)
	if err != nil {
		return translateSQLiteError(err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return translateSQLiteError(err)
	}
	if affected != 1 {
		return fmt.Errorf("%w: stale %s counter", errRepositoryConflict, key)
	}
	return nil
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

func (r *sqliteRepositories) Save(ctx context.Context, item user) error {
	createdAt := formatSQLiteTime(item.CreatedAt)
	return updateOrInsert(ctx, r.q,
		`UPDATE users SET email = ?, role = ?, password_hash = ?, created_at = ? WHERE id = ?`,
		[]any{item.Email, item.Role, item.PasswordHash, createdAt, item.ID},
		`INSERT INTO users (id, email, role, password_hash, created_at) VALUES (?, ?, ?, ?, ?)`,
		[]any{item.ID, item.Email, item.Role, item.PasswordHash, createdAt},
	)
}

func (r *sqliteRepositories) Delete(ctx context.Context, id int64) error {
	result, err := r.q.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return translateSQLiteError(err)
	}
	return requireAffected(result)
}

func (r *sqliteRepositories) listSessions(ctx context.Context) (items []refreshSession, returnErr error) {
	rows, err := r.q.QueryContext(ctx, `SELECT id, user_id, token_hash, created_at, expires_at FROM refresh_sessions ORDER BY user_id, created_at`)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer joinRowsCloseError(&returnErr, rows, "list refresh sessions")
	for rows.Next() {
		var item refreshSession
		var createdAt, expiresAt string
		if err := rows.Scan(&item.ID, &item.UserID, &item.TokenHash, &createdAt, &expiresAt); err != nil {
			return nil, translateSQLiteError(err)
		}
		item.CreatedAt, err = parseSQLiteTime(createdAt)
		if err != nil {
			return nil, err
		}
		item.ExpiresAt, err = parseSQLiteTime(expiresAt)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func (r *sqliteRepositories) FindByToken(ctx context.Context, rawToken string, now time.Time) (refreshSession, bool, error) {
	items, err := r.listSessions(ctx)
	if err != nil {
		return refreshSession{}, false, err
	}
	hashed := hashToken(rawToken)
	for _, item := range items {
		if item.ExpiresAt.Before(now) {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(item.TokenHash), []byte(hashed)) == 1 {
			return item, true, nil
		}
	}
	return refreshSession{}, false, nil
}

func (r *sqliteRepositories) SaveSession(ctx context.Context, item refreshSession) error {
	createdAt := formatSQLiteTime(item.CreatedAt)
	expiresAt := formatSQLiteTime(item.ExpiresAt)
	return updateOrInsert(ctx, r.q,
		`UPDATE refresh_sessions SET user_id = ?, token_hash = ?, created_at = ?, expires_at = ? WHERE id = ?`,
		[]any{item.UserID, item.TokenHash, createdAt, expiresAt, item.ID},
		`INSERT INTO refresh_sessions (id, user_id, token_hash, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		[]any{item.ID, item.UserID, item.TokenHash, createdAt, expiresAt},
	)
}

func (r *sqliteRepositories) DeleteSession(ctx context.Context, id string) error {
	_, err := r.q.ExecContext(ctx, `DELETE FROM refresh_sessions WHERE id = ?`, id)
	return translateSQLiteError(err)
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

func (r *sqliteRepositories) SaveAuthor(ctx context.Context, item author) error {
	photosJSON, err := marshalJSONColumn(item.Photos)
	if err != nil {
		return err
	}
	return updateOrInsert(ctx, r.q,
		`UPDATE authors SET current_name = ?, photos_json = ? WHERE id = ?`,
		[]any{item.CurrentName, photosJSON, item.ID},
		`INSERT INTO authors (id, current_name, photos_json) VALUES (?, ?, ?)`,
		[]any{item.ID, item.CurrentName, photosJSON},
	)
}

func (r *sqliteRepositories) DeleteAuthor(ctx context.Context, id int64) error {
	_, err := r.q.ExecContext(ctx, `DELETE FROM authors WHERE id = ?`, id)
	return translateSQLiteError(err)
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

func (r *sqliteRepositories) SaveAlbum(ctx context.Context, item album) error {
	authorIDsJSON, err := marshalJSONColumn(item.AuthorIDs)
	if err != nil {
		return err
	}
	trackIDsJSON, err := marshalJSONColumn(item.TrackIDs)
	if err != nil {
		return err
	}
	additionalInfoJSON, err := marshalJSONColumn(item.AdditionalInfo)
	if err != nil {
		return err
	}
	releaseDate := formatSQLiteTime(item.ReleaseDate)
	isPublished := boolToSQLiteInt(item.IsPublished)
	return updateOrInsert(ctx, r.q,
		`UPDATE albums SET title = ?, cover_image_path = ?, author_ids_json = ?, release_date = ?,
		is_published = ?, track_ids_json = ?, additional_info_json = ? WHERE id = ?`,
		[]any{item.Title, item.CoverImagePath, authorIDsJSON, releaseDate, isPublished, trackIDsJSON, additionalInfoJSON, item.ID},
		`INSERT INTO albums (id, title, cover_image_path, author_ids_json, release_date, is_published, track_ids_json, additional_info_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		[]any{item.ID, item.Title, item.CoverImagePath, authorIDsJSON, releaseDate, isPublished, trackIDsJSON, additionalInfoJSON},
	)
}

func (r *sqliteRepositories) DeleteAlbum(ctx context.Context, id int64) error {
	_, err := r.q.ExecContext(ctx, `DELETE FROM albums WHERE id = ?`, id)
	return translateSQLiteError(err)
}

func (r *sqliteRepositories) SaveTrack(ctx context.Context, item track) error {
	authorIDsJSON, err := marshalJSONColumn(item.AuthorIDs)
	if err != nil {
		return err
	}
	additionalInfoJSON, err := marshalJSONColumn(item.AdditionalInfo)
	if err != nil {
		return err
	}
	sourceMetadataJSON, err := marshalJSONColumn(item.SourceMetadata)
	if err != nil {
		return err
	}
	createdAt := formatSQLiteTime(item.CreatedAt)
	return updateOrInsert(ctx, r.q,
		`UPDATE tracks SET name = ?, author_ids_json = ?, album_id = ?, audio_file_path = ?,
		additional_info_json = ?, source_metadata_json = ?, created_at = ? WHERE id = ?`,
		[]any{item.Name, authorIDsJSON, item.AlbumID, item.AudioFilePath, additionalInfoJSON, sourceMetadataJSON, createdAt, item.ID},
		`INSERT INTO tracks (id, name, author_ids_json, album_id, audio_file_path, additional_info_json, source_metadata_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		[]any{item.ID, item.Name, authorIDsJSON, item.AlbumID, item.AudioFilePath, additionalInfoJSON, sourceMetadataJSON, createdAt},
	)
}

func (r *sqliteRepositories) DeleteTrack(ctx context.Context, id int64) error {
	_, err := r.q.ExecContext(ctx, `DELETE FROM tracks WHERE id = ?`, id)
	return translateSQLiteError(err)
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

func (r *sqliteRepositories) SavePlaylist(ctx context.Context, item playlist) error {
	trackItemsJSON, err := marshalJSONColumn(item.TrackItems)
	if err != nil {
		return err
	}
	system := boolToSQLiteInt(item.System)
	return updateOrInsert(ctx, r.q,
		`UPDATE playlists SET user_id = ?, name = ?, description = ?, cover_image_path = ?,
		visibility = ?, share_token = ?, track_items_json = ?, system = ?, kind = ? WHERE id = ?`,
		[]any{item.UserID, item.Name, item.Description, item.CoverImagePath, item.Visibility, item.ShareToken, trackItemsJSON, system, item.Kind, item.ID},
		`INSERT INTO playlists (id, user_id, name, description, cover_image_path, visibility, share_token, track_items_json, system, kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		[]any{item.ID, item.UserID, item.Name, item.Description, item.CoverImagePath, item.Visibility, item.ShareToken, trackItemsJSON, system, item.Kind},
	)
}

func (r *sqliteRepositories) DeletePlaylist(ctx context.Context, id int64) error {
	_, err := r.q.ExecContext(ctx, `DELETE FROM playlists WHERE id = ?`, id)
	return translateSQLiteError(err)
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

func (r *sqliteRepositories) SaveLyrics(ctx context.Context, item lyrics) error {
	linesJSON, err := marshalJSONColumn(item.Lines)
	if err != nil {
		return err
	}
	plainText := sqlNullString(item.PlainText)
	languageCode := sqlNullString(item.LanguageCode)
	source := sqlNullString(item.Source)
	verified := boolToSQLiteInt(item.IsVerified)
	updatedAt := formatSQLiteTime(item.UpdatedAt)
	createdAt := formatSQLiteTime(item.CreatedAt)
	return updateOrInsert(ctx, r.q,
		`UPDATE lyrics SET track_id = ?, type = ?, plain_text = ?, language_code = ?, source = ?,
		is_verified = ?, updated_at = ?, created_at = ?, lines_json = ? WHERE id = ?`,
		[]any{item.TrackID, item.Type, plainText, languageCode, source, verified, updatedAt, createdAt, linesJSON, item.ID},
		`INSERT INTO lyrics (id, track_id, type, plain_text, language_code, source, is_verified, updated_at, created_at, lines_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		[]any{item.ID, item.TrackID, item.Type, plainText, languageCode, source, verified, updatedAt, createdAt, linesJSON},
	)
}

func (r *sqliteRepositories) DeleteByTrackID(ctx context.Context, trackID int64) error {
	_, err := r.q.ExecContext(ctx, `DELETE FROM lyrics WHERE track_id = ?`, trackID)
	return translateSQLiteError(err)
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

func updateOrInsert(
	ctx context.Context,
	q sqlExecutor,
	updateStatement string,
	updateArguments []any,
	insertStatement string,
	insertArguments []any,
) error {
	result, err := q.ExecContext(ctx, updateStatement, updateArguments...)
	if err != nil {
		return translateSQLiteError(err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return translateSQLiteError(err)
	}
	if affected > 0 {
		return nil
	}
	_, err = q.ExecContext(ctx, insertStatement, insertArguments...)
	return translateSQLiteError(err)
}

// Go does not support overloading, so the concrete adapter methods below
// bridge the domain interfaces that intentionally share names such as List,
// Save, and Delete.
type sqliteUserRepository struct{ *sqliteRepositories }
type sqliteSessionRepository struct{ *sqliteRepositories }
type sqliteAuthorRepository struct{ *sqliteRepositories }
type sqlitePlaylistRepository struct{ *sqliteRepositories }
type sqliteLyricsRepository struct{ *sqliteRepositories }

func (r sqliteSessionRepository) List(ctx context.Context) ([]refreshSession, error) {
	return r.listSessions(ctx)
}
func (r sqliteSessionRepository) Save(ctx context.Context, item refreshSession) error {
	return r.SaveSession(ctx, item)
}
func (r sqliteSessionRepository) Delete(ctx context.Context, id string) error {
	return r.DeleteSession(ctx, id)
}
func (r sqliteAuthorRepository) List(ctx context.Context) ([]author, error) {
	return r.ListAuthors(ctx)
}
func (r sqliteAuthorRepository) Save(ctx context.Context, item author) error {
	return r.SaveAuthor(ctx, item)
}
func (r sqliteAuthorRepository) Delete(ctx context.Context, id int64) error {
	return r.DeleteAuthor(ctx, id)
}
func (r sqlitePlaylistRepository) List(ctx context.Context) ([]playlist, error) {
	return r.ListPlaylists(ctx)
}
func (r sqlitePlaylistRepository) Save(ctx context.Context, item playlist) error {
	return r.SavePlaylist(ctx, item)
}
func (r sqlitePlaylistRepository) Delete(ctx context.Context, id int64) error {
	return r.DeletePlaylist(ctx, id)
}
func (r sqliteLyricsRepository) List(ctx context.Context) ([]lyrics, error) {
	return r.ListLyrics(ctx)
}
func (r sqliteLyricsRepository) Save(ctx context.Context, item lyrics) error {
	return r.SaveLyrics(ctx, item)
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
	}
}

type domainWriteScope struct {
	repairMetadata   bool
	metadataExpected map[string]int64
	users            bool
	sessions         bool
	authors          bool
	catalog          bool
	playlists        bool
	lyrics           bool
}

var allDomainWrites = domainWriteScope{
	repairMetadata: true,
	users:          true, sessions: true, authors: true,
	catalog: true, playlists: true, lyrics: true,
}

// commitDomainChangesLocked compares the locked cache with authoritative SQL
// state and applies only changed rows in the declared domains. It exists as a
// compatibility bridge while handlers continue using trackStore.
func (s *trackStore) commitDomainChangesLocked(ctx context.Context, scope domainWriteScope) error {
	if s.unitOfWork == nil {
		return errors.New("persistence unit of work is not initialized")
	}
	return s.unitOfWork.WithinTransaction(ctx, func(repositories domainRepositories) error {
		metadata := map[string]int64{
			"next_track_id":       s.nextTrackID,
			"next_album_id":       s.nextAlbumID,
			"next_author_id":      s.nextAuthorID,
			"next_user_id":        s.nextUserID,
			"next_playlist_id":    s.nextPlaylistID,
			"next_lyrics_id":      s.nextLyricsID,
			"next_lyrics_line_id": s.nextLyricsLineID,
		}
		if scope.repairMetadata {
			keys := make([]string, 0, len(metadata))
			for key := range metadata {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if err := repositories.metadata.SetNextID(ctx, key, metadata[key]); err != nil {
					return err
				}
			}
		} else if len(scope.metadataExpected) > 0 {
			keys := make([]string, 0, len(scope.metadataExpected))
			for key := range scope.metadataExpected {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				next, ok := metadata[key]
				if !ok {
					return fmt.Errorf("unknown metadata counter %q", key)
				}
				if err := repositories.metadata.AdvanceNextID(ctx, key, scope.metadataExpected[key], next); err != nil {
					return err
				}
			}
		}

		if scope.authors {
			items, err := repositories.authors.List(ctx)
			if err != nil {
				return err
			}
			current := make(map[int64]author, len(items))
			for _, item := range items {
				current[item.ID] = item
			}
			for id, item := range s.authors {
				if previous, ok := current[id]; !ok || !reflect.DeepEqual(previous, item) {
					if err := repositories.authors.Save(ctx, item); err != nil {
						return err
					}
				}
				delete(current, id)
			}
			for id := range current {
				if err := repositories.authors.Delete(ctx, id); err != nil {
					return err
				}
			}
		}

		if scope.catalog {
			albums, err := repositories.catalog.ListAlbums(ctx)
			if err != nil {
				return err
			}
			tracks, err := repositories.catalog.ListTracks(ctx)
			if err != nil {
				return err
			}
			currentAlbums := make(map[int64]album, len(albums))
			currentTracks := make(map[int64]track, len(tracks))
			for _, item := range albums {
				currentAlbums[item.ID] = item
			}
			for _, item := range tracks {
				currentTracks[item.ID] = item
			}
			// Deletes precede updates so track removal side effects remain atomic.
			for id := range currentTracks {
				if _, ok := s.tracks[id]; !ok {
					if err := repositories.catalog.DeleteTrack(ctx, id); err != nil {
						return err
					}
				}
			}
			for id := range currentAlbums {
				if _, ok := s.albums[id]; !ok {
					if err := repositories.catalog.DeleteAlbum(ctx, id); err != nil {
						return err
					}
				}
			}
			albumIDs := sortedInt64Keys(s.albums)
			for _, id := range albumIDs {
				item := s.albums[id]
				if previous, ok := currentAlbums[id]; !ok || !reflect.DeepEqual(previous, item) {
					if err := repositories.catalog.SaveAlbum(ctx, item); err != nil {
						return err
					}
				}
			}
			trackIDs := sortedInt64Keys(s.tracks)
			for _, id := range trackIDs {
				item := s.tracks[id]
				if previous, ok := currentTracks[id]; !ok || !reflect.DeepEqual(previous, item) {
					if err := repositories.catalog.SaveTrack(ctx, item); err != nil {
						return err
					}
				}
			}
		}

		if scope.users {
			items, err := repositories.users.List(ctx)
			if err != nil {
				return err
			}
			current := make(map[int64]user, len(items))
			for _, item := range items {
				current[item.ID] = item
			}
			for id, item := range s.users {
				if previous, ok := current[id]; !ok || !reflect.DeepEqual(previous, item) {
					if err := repositories.users.Save(ctx, item); err != nil {
						return err
					}
				}
				delete(current, id)
			}
			for id := range current {
				if err := repositories.users.Delete(ctx, id); err != nil {
					return err
				}
			}
		}

		if scope.sessions {
			items, err := repositories.sessions.List(ctx)
			if err != nil {
				return err
			}
			current := make(map[string]refreshSession, len(items))
			for _, item := range items {
				current[item.ID] = item
			}
			for id, item := range s.refreshSession {
				if previous, ok := current[id]; !ok || !reflect.DeepEqual(previous, item) {
					if err := repositories.sessions.Save(ctx, item); err != nil {
						return err
					}
				}
				delete(current, id)
			}
			for id := range current {
				if err := repositories.sessions.Delete(ctx, id); err != nil {
					return err
				}
			}
		}

		if scope.playlists {
			items, err := repositories.playlists.List(ctx)
			if err != nil {
				return err
			}
			current := make(map[int64]playlist, len(items))
			for _, item := range items {
				current[item.ID] = item
			}
			for id, item := range s.playlists {
				if previous, ok := current[id]; !ok || !reflect.DeepEqual(previous, item) {
					if err := repositories.playlists.Save(ctx, item); err != nil {
						return err
					}
				}
				delete(current, id)
			}
			for id := range current {
				if err := repositories.playlists.Delete(ctx, id); err != nil {
					return err
				}
			}
		}

		if scope.lyrics {
			items, err := repositories.lyrics.List(ctx)
			if err != nil {
				return err
			}
			current := make(map[int64]lyrics, len(items))
			for _, item := range items {
				current[item.TrackID] = item
			}
			for trackID, item := range s.lyricsByTrack {
				if previous, ok := current[trackID]; !ok || !reflect.DeepEqual(previous, item) {
					if err := repositories.lyrics.Save(ctx, item); err != nil {
						return err
					}
				}
				delete(current, trackID)
			}
			for trackID := range current {
				if err := repositories.lyrics.DeleteByTrackID(ctx, trackID); err != nil {
					return err
				}
			}
		}

		return nil
	})
}

func sortedInt64Keys[T any](items map[int64]T) []int64 {
	ids := make([]int64, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
