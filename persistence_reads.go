package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"

	"modernc.org/sqlite"
)

type searchReference struct {
	Type string
	ID   int64
}

type trackAudioReference struct {
	ID            int64
	AudioFilePath string
}

func init() {
	if err := sqlite.RegisterDeterministicScalarFunction("esketit_unicode_fold", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("unicode_fold expects one argument")
		}
		if args[0] == nil {
			return "", nil
		}
		value, ok := args[0].(string)
		if !ok {
			return nil, fmt.Errorf("unicode_fold expects text")
		}
		return strings.ToLower(value), nil
	}); err != nil {
		panic(fmt.Sprintf("register SQLite esketit_unicode_fold function: %v", err))
	}
	if err := sqlite.RegisterDeterministicScalarFunction("esketit_rfc3339_order_key", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("rfc3339_order_key expects one argument")
		}
		value, ok := args[0].(string)
		if !ok {
			return nil, fmt.Errorf("rfc3339_order_key expects text")
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return nil, err
		}
		return parsed.UTC().Format("2006-01-02T15:04:05.000000000Z"), nil
	}); err != nil {
		panic(fmt.Sprintf("register SQLite esketit_rfc3339_order_key function: %v", err))
	}
}

const (
	albumColumns    = `id, title, cover_image_path, author_ids_json, release_date, is_published, track_ids_json, additional_info_json`
	trackColumns    = `id, name, author_ids_json, album_id, audio_file_path, additional_info_json, source_metadata_json, created_at`
	authorColumns   = `id, current_name, photos_json`
	playlistColumns = `id, user_id, name, description, cover_image_path, visibility, share_token, track_items_json, system, kind`
)

func (r *sqliteRepositories) ListAlbumsPage(ctx context.Context, filter albumListFilter) ([]album, int, error) {
	where, args := albumReadFilter(filter)
	total, err := queryCount(ctx, r.q, `SELECT COUNT(*) FROM albums`+where, args)
	if err != nil {
		return nil, 0, err
	}
	page, pageSize := normalizePage(filter.Page), normalizePageSize(filter.PageSize)
	rows, err := r.q.QueryContext(ctx, `SELECT `+albumColumns+` FROM albums`+where+` ORDER BY id LIMIT ? OFFSET ?`, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return nil, 0, translateSQLiteError(err)
	}
	defer rows.Close()
	items := make([]album, 0, pageSize)
	for rows.Next() {
		item, err := scanAlbum(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, translateSQLiteError(rows.Err())
}

func albumReadFilter(filter albumListFilter) (string, []any) {
	predicates := make([]string, 0, 4)
	args := make([]any, 0, 4)
	if filter.AuthorID > 0 {
		predicates = append(predicates, `EXISTS (SELECT 1 FROM json_each(albums.author_ids_json) WHERE CAST(value AS INTEGER) = ?)`)
		args = append(args, filter.AuthorID)
	}
	if filter.IsPublished != nil {
		predicates = append(predicates, `is_published = ?`)
		args = append(args, boolToSQLiteInt(*filter.IsPublished))
	}
	if !filter.IncludeEmpty {
		predicates = append(predicates, `json_array_length(track_ids_json) > 0`)
	}
	if query := strings.ToLower(strings.TrimSpace(filter.Query)); query != "" {
		predicates = append(predicates, `instr(esketit_unicode_fold(title), ?) > 0`)
		args = append(args, query)
	}
	return sqlWhere(predicates), args
}

func (r *sqliteRepositories) ListTracksPage(ctx context.Context, filter trackListFilter) ([]track, int, error) {
	where, args := trackReadFilter(filter)
	total, err := queryCount(ctx, r.q, `SELECT COUNT(*) FROM tracks`+where, args)
	if err != nil {
		return nil, 0, err
	}
	orderBy := "id ASC"
	if filter.Order == sortOrderDesc {
		orderBy = "id DESC"
	}
	if filter.Sort == trackListSortCreatedAt {
		if filter.Order == sortOrderDesc {
			orderBy = "esketit_rfc3339_order_key(created_at) DESC, id DESC"
		} else {
			orderBy = "esketit_rfc3339_order_key(created_at) ASC, id ASC"
		}
	}
	page, pageSize := normalizePage(filter.Page), normalizePageSize(filter.PageSize)
	rows, err := r.q.QueryContext(ctx, `SELECT `+trackColumns+` FROM tracks`+where+` ORDER BY `+orderBy+` LIMIT ? OFFSET ?`, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return nil, 0, translateSQLiteError(err)
	}
	defer rows.Close()
	items := make([]track, 0, pageSize)
	for rows.Next() {
		item, err := scanTrack(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, translateSQLiteError(rows.Err())
}

func trackReadFilter(filter trackListFilter) (string, []any) {
	predicates := make([]string, 0, 3)
	args := make([]any, 0, 3)
	if filter.AuthorID > 0 {
		predicates = append(predicates, `EXISTS (SELECT 1 FROM json_each(tracks.author_ids_json) WHERE CAST(value AS INTEGER) = ?)`)
		args = append(args, filter.AuthorID)
	}
	if filter.AlbumID > 0 {
		predicates = append(predicates, `album_id = ?`)
		args = append(args, filter.AlbumID)
	}
	if query := strings.ToLower(strings.TrimSpace(filter.Query)); query != "" {
		predicates = append(predicates, `instr(esketit_unicode_fold(name), ?) > 0`)
		args = append(args, query)
	}
	return sqlWhere(predicates), args
}

func (r *sqliteRepositories) ListPlaylistsPage(ctx context.Context, userID int64, filter playlistListFilter) ([]playlist, int, error) {
	predicates := []string{`user_id = ?`}
	args := []any{userID}
	if visibility := normalizePlaylistVisibility(filter.Visibility); visibility != "" {
		predicates = append(predicates, `visibility = ?`)
		args = append(args, visibility)
	}
	if query := strings.ToLower(strings.TrimSpace(filter.Query)); query != "" {
		predicates = append(predicates, `(instr(esketit_unicode_fold(name), ?) > 0 OR instr(esketit_unicode_fold(description), ?) > 0)`)
		args = append(args, query, query)
	}
	where := sqlWhere(predicates)
	total, err := queryCount(ctx, r.q, `SELECT COUNT(*) FROM playlists`+where, args)
	if err != nil {
		return nil, 0, err
	}
	page, pageSize := normalizePage(filter.Page), normalizePageSize(filter.PageSize)
	orderBy := `CASE kind WHEN 'favorites' THEN 0 WHEN 'dislikes' THEN 1 ELSE 2 END, esketit_unicode_fold(name), id`
	rows, err := r.q.QueryContext(ctx, `SELECT `+playlistColumns+` FROM playlists`+where+` ORDER BY `+orderBy+` LIMIT ? OFFSET ?`, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return nil, 0, translateSQLiteError(err)
	}
	defer rows.Close()
	items := make([]playlist, 0, pageSize)
	for rows.Next() {
		item, err := scanPlaylist(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, translateSQLiteError(rows.Err())
}

func (r *sqliteRepositories) ListTracksByIDs(ctx context.Context, ids []int64) ([]track, error) {
	query, args, ok := idQuery(`SELECT `+trackColumns+` FROM tracks WHERE id IN (%s) ORDER BY id`, ids)
	if !ok {
		return []track{}, nil
	}
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer rows.Close()
	items := make([]track, 0, len(ids))
	for rows.Next() {
		item, err := scanTrack(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func (r *sqliteRepositories) ListAlbumsByIDs(ctx context.Context, ids []int64) ([]album, error) {
	query, args, ok := idQuery(`SELECT `+albumColumns+` FROM albums WHERE id IN (%s) ORDER BY id`, ids)
	if !ok {
		return []album{}, nil
	}
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer rows.Close()
	items := make([]album, 0, len(ids))
	for rows.Next() {
		item, err := scanAlbum(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func (r *sqliteRepositories) ListAuthorsByIDs(ctx context.Context, ids []int64) ([]author, error) {
	query, args, ok := idQuery(`SELECT `+authorColumns+` FROM authors WHERE id IN (%s) ORDER BY id`, ids)
	if !ok {
		return []author{}, nil
	}
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer rows.Close()
	items := make([]author, 0, len(ids))
	for rows.Next() {
		item, err := scanAuthor(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func (r *sqliteRepositories) ListPlaylistsByIDs(ctx context.Context, ids []int64) ([]playlist, error) {
	query, args, ok := idQuery(`SELECT `+playlistColumns+` FROM playlists WHERE id IN (%s) ORDER BY id`, ids)
	if !ok {
		return []playlist{}, nil
	}
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer rows.Close()
	items := make([]playlist, 0, len(ids))
	for rows.Next() {
		item, err := scanPlaylist(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func (r *sqliteRepositories) ListPreferencePlaylists(ctx context.Context, userID int64) ([]playlist, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT `+playlistColumns+` FROM playlists WHERE user_id = ? AND kind IN (?, ?) ORDER BY id`, userID, playlistKindFavorites, playlistKindDislikes)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer rows.Close()
	items := make([]playlist, 0, 2)
	for rows.Next() {
		item, err := scanPlaylist(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func (r *sqliteRepositories) FindPlaylistByShareToken(ctx context.Context, token string) (playlist, bool, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return playlist{}, false, nil
	}
	item, err := scanPlaylist(r.q.QueryRowContext(ctx, `SELECT `+playlistColumns+` FROM playlists WHERE visibility = ? AND share_token = ? LIMIT 1`, playlistVisibilityShared, token))
	if isNoRows(err) {
		return playlist{}, false, nil
	}
	return item, err == nil, err
}

func (r *sqliteRepositories) SearchPage(ctx context.Context, userID int64, filter searchListFilter) ([]searchReference, int, error) {
	query := strings.ToLower(strings.TrimSpace(filter.Query))
	parts := make([]string, 0, 4)
	args := make([]any, 0, 12)
	parts = append(parts, `SELECT 'author' AS item_type, id, current_name AS item_name FROM authors WHERE (? = '' OR instr(esketit_unicode_fold(current_name), ?) > 0)`)
	args = append(args, query, query)
	albumEmptyPredicate := ""
	if !filter.IncludeEmpty {
		albumEmptyPredicate = ` AND json_array_length(track_ids_json) > 0`
	}
	parts = append(parts, `SELECT 'album' AS item_type, id, title AS item_name FROM albums WHERE (? = '' OR instr(esketit_unicode_fold(title), ?) > 0)`+albumEmptyPredicate)
	args = append(args, query, query)
	parts = append(parts, `SELECT 'track' AS item_type, id, name AS item_name FROM tracks WHERE (? = '' OR instr(esketit_unicode_fold(name), ?) > 0)`)
	args = append(args, query, query)
	parts = append(parts, `SELECT 'playlist' AS item_type, id, name AS item_name FROM playlists WHERE (visibility = ? OR (? > 0 AND user_id = ?)) AND (? = '' OR instr(esketit_unicode_fold(name), ?) > 0 OR instr(esketit_unicode_fold(description), ?) > 0)`)
	args = append(args, playlistVisibilityPublic, userID, userID, query, query, query)
	union := strings.Join(parts, ` UNION ALL `)
	total, err := queryCount(ctx, r.q, `SELECT COUNT(*) FROM (`+union+`)`, args)
	if err != nil {
		return nil, 0, err
	}
	page, pageSize := normalizePage(filter.Page), normalizePageSize(filter.PageSize)
	rows, err := r.q.QueryContext(ctx, `SELECT item_type, id FROM (`+union+`) ORDER BY esketit_unicode_fold(item_name), item_type, id LIMIT ? OFFSET ?`, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return nil, 0, translateSQLiteError(err)
	}
	defer rows.Close()
	items := make([]searchReference, 0, pageSize)
	for rows.Next() {
		var item searchReference
		if err := rows.Scan(&item.Type, &item.ID); err != nil {
			return nil, 0, translateSQLiteError(err)
		}
		items = append(items, item)
	}
	return items, total, translateSQLiteError(rows.Err())
}

func (r *sqliteRepositories) ListTrackAudioReferences(ctx context.Context) ([]trackAudioReference, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT id, audio_file_path FROM tracks ORDER BY id`)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer rows.Close()
	items := make([]trackAudioReference, 0)
	for rows.Next() {
		var item trackAudioReference
		if err := rows.Scan(&item.ID, &item.AudioFilePath); err != nil {
			return nil, translateSQLiteError(err)
		}
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func (r *sqliteRepositories) ListTracksBySourceProvider(ctx context.Context, provider string) ([]track, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return []track{}, nil
	}
	rows, err := r.q.QueryContext(ctx, `SELECT `+trackColumns+` FROM tracks WHERE EXISTS (
		SELECT 1 FROM json_each(tracks.source_metadata_json) AS metadata
		WHERE json_type(metadata.value, '$.provider') = 'text'
			AND esketit_unicode_fold(trim(CAST(json_extract(metadata.value, '$.provider') AS TEXT))) = ?
	) ORDER BY id`, provider)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer rows.Close()
	items := make([]track, 0)
	for rows.Next() {
		item, err := scanTrack(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func queryCount(ctx context.Context, q sqlExecutor, statement string, args []any) (int, error) {
	var count int
	if err := q.QueryRowContext(ctx, statement, args...).Scan(&count); err != nil {
		return 0, translateSQLiteError(err)
	}
	return count, nil
}

func sqlWhere(predicates []string) string {
	if len(predicates) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(predicates, " AND ")
}

func idQuery(format string, ids []int64) (string, []any, bool) {
	ids = normalizeTrackIDs(ids)
	if len(ids) == 0 {
		return "", nil, false
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for index, id := range ids {
		placeholders[index] = "?"
		args[index] = id
	}
	return fmt.Sprintf(format, strings.Join(placeholders, ",")), args, true
}

func isNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}
