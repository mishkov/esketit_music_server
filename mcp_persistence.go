package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MCPRepository keeps configuration, durable retry receipts and staged media at
// the same storage boundary as the catalog submission service.
type MCPRepository interface {
	SearchMCPCatalog(context.Context, int64, string, string, int, int) ([]searchReference, int, error)
	MCPAuthorReferenced(context.Context, int64) (bool, error)
	GetMCPSettings(context.Context) (mcpSettings, error)
	PatchMCPSettings(context.Context, mcpSettings, int64) error
	FindMCPRequest(context.Context, int64, string) (mcpRequestReceipt, bool, error)
	InsertMCPRequest(context.Context, mcpRequestReceipt) error
	FindMCPUpload(context.Context, int64, string) (mcpUpload, bool, error)
	InsertMCPUpload(context.Context, mcpUpload) error
	ClaimMCPUpload(context.Context, int64, string, int64) error
	ListMCPSubmissions(context.Context, int64, mcpListInput) ([]catalogSubmission, int, error)
}

type mcpSettings struct {
	ActingUserID     *int64 `json:"actingUserId"`
	WorkflowGuidance string `json:"workflowGuidance"`
	UploadGuidance   string `json:"uploadGuidance"`
	Version          int64  `json:"version"`
}
type mcpRequestReceipt struct {
	UserID                            int64
	RequestID, Operation, PayloadHash string
	Result                            json.RawMessage
}
type mcpUpload struct {
	Token               string    `json:"uploadToken"`
	UserID              int64     `json:"-"`
	Kind                string    `json:"kind"`
	OriginalName        string    `json:"originalName"`
	MediaType           string    `json:"mediaType"`
	SizeBytes           int64     `json:"sizeBytes"`
	SHA256              string    `json:"sha256"`
	StoredName          string    `json:"-"`
	MediaPath           string    `json:"-"`
	ClaimedSubmissionID *int64    `json:"claimedSubmissionId"`
	CreatedAt           time.Time `json:"createdAt"`
}

func migrateMCP(ctx context.Context, tx *sql.Tx) error {
	statements := []string{
		`ALTER TABLE catalog_submissions ADD COLUMN revision INTEGER NOT NULL DEFAULT 1 CHECK(revision > 0)`,
		`CREATE TABLE mcp_settings (
    id INTEGER PRIMARY KEY CHECK(id = 1), acting_user_id INTEGER REFERENCES users(id) ON DELETE RESTRICT,
    workflow_guidance TEXT NOT NULL, upload_guidance TEXT NOT NULL, version INTEGER NOT NULL CHECK(version > 0))`,
		`INSERT INTO mcp_settings VALUES (1, NULL,
    'Fetch context at session start. Search the catalog before creating entities. Reuse matching authors and albums. Submit missing authors, then albums, then tracks. Preserve source metadata. Check feedback, correct requested changes, and resubmit. Use a stable requestId for retries of the same operation; use a new requestId when changing the payload. Fetch fresh submission details before editing.',
    'Download media locally, then upload bytes using the authenticated HTTP endpoint. Do not send local paths, remote URLs, or base64 through MCP. Preserve the upload token for submission and retry with the same Idempotency-Key and file.', 1)`,
		`CREATE TABLE mcp_requests (user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    request_id TEXT NOT NULL, operation TEXT NOT NULL, payload_hash TEXT NOT NULL,
    result_json TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY(user_id, request_id))`,
		`CREATE TABLE mcp_uploads (token TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    kind TEXT NOT NULL CHECK(kind IN ('audio','author_photo','album_cover')),
    original_name TEXT NOT NULL, media_type TEXT NOT NULL, size_bytes INTEGER NOT NULL CHECK(size_bytes > 0),
    sha256 TEXT NOT NULL, stored_name TEXT NOT NULL, media_path TEXT NOT NULL,
    claimed_submission_id INTEGER REFERENCES catalog_submissions(id) ON DELETE RESTRICT, created_at TEXT NOT NULL)`,
		`CREATE INDEX idx_mcp_submissions_requester_updated ON catalog_submissions(requester_user_id, updated_at DESC, id DESC)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}
func (r *sqliteRepositories) GetMCPSettings(ctx context.Context) (mcpSettings, error) {
	var s mcpSettings
	var id sql.NullInt64
	err := r.q.QueryRowContext(ctx, `SELECT acting_user_id, workflow_guidance, upload_guidance, version FROM mcp_settings WHERE id=1`).Scan(&id, &s.WorkflowGuidance, &s.UploadGuidance, &s.Version)
	s.ActingUserID = nullInt64Pointer(id)
	return s, translateSQLiteError(err)
}
func (r *sqliteRepositories) PatchMCPSettings(ctx context.Context, s mcpSettings, expected int64) error {
	res, err := r.q.ExecContext(ctx, `UPDATE mcp_settings SET acting_user_id=?, workflow_guidance=?, upload_guidance=?, version=version+1 WHERE id=1 AND version=?`, s.ActingUserID, s.WorkflowGuidance, s.UploadGuidance, expected)
	if err != nil {
		return translateSQLiteError(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return translateSQLiteError(err)
	}
	if n != 1 {
		return errMCPStaleRevision
	}
	return nil
}
func (r *sqliteRepositories) FindMCPRequest(ctx context.Context, userID int64, requestID string) (mcpRequestReceipt, bool, error) {
	v := mcpRequestReceipt{UserID: userID, RequestID: requestID}
	var raw string
	err := r.q.QueryRowContext(ctx, `SELECT operation,payload_hash,result_json FROM mcp_requests WHERE user_id=? AND request_id=?`, userID, requestID).Scan(&v.Operation, &v.PayloadHash, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return v, false, nil
	}
	v.Result = json.RawMessage(raw)
	return v, err == nil, translateSQLiteError(err)
}
func (r *sqliteRepositories) InsertMCPRequest(ctx context.Context, v mcpRequestReceipt) error {
	_, err := r.q.ExecContext(ctx, `INSERT INTO mcp_requests VALUES(?,?,?,?,?,?)`, v.UserID, v.RequestID, v.Operation, v.PayloadHash, string(v.Result), formatSQLiteTime(time.Now().UTC()))
	return translateSQLiteError(err)
}
func (r *sqliteRepositories) FindMCPUpload(ctx context.Context, userID int64, token string) (mcpUpload, bool, error) {
	v := mcpUpload{Token: token, UserID: userID}
	var claimed sql.NullInt64
	var created string
	err := r.q.QueryRowContext(ctx, `SELECT kind,original_name,media_type,size_bytes,sha256,stored_name,media_path,claimed_submission_id,created_at FROM mcp_uploads WHERE token=? AND user_id=?`, token, userID).Scan(&v.Kind, &v.OriginalName, &v.MediaType, &v.SizeBytes, &v.SHA256, &v.StoredName, &v.MediaPath, &claimed, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return v, false, nil
	}
	if err != nil {
		return v, false, translateSQLiteError(err)
	}
	v.ClaimedSubmissionID = nullInt64Pointer(claimed)
	v.CreatedAt, err = parseSQLiteTime(created)
	return v, err == nil, err
}
func (r *sqliteRepositories) InsertMCPUpload(ctx context.Context, v mcpUpload) error {
	_, err := r.q.ExecContext(ctx, `INSERT INTO mcp_uploads VALUES(?,?,?,?,?,?,?,?,?,?,?)`, v.Token, v.UserID, v.Kind, v.OriginalName, v.MediaType, v.SizeBytes, v.SHA256, v.StoredName, v.MediaPath, v.ClaimedSubmissionID, formatSQLiteTime(v.CreatedAt))
	return translateSQLiteError(err)
}
func (r *sqliteRepositories) ClaimMCPUpload(ctx context.Context, userID int64, token string, submissionID int64) error {
	res, err := r.q.ExecContext(ctx, `UPDATE mcp_uploads SET claimed_submission_id=? WHERE token=? AND user_id=? AND claimed_submission_id IS NULL`, submissionID, token, userID)
	if err != nil {
		return translateSQLiteError(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return translateSQLiteError(err)
	}
	if n != 1 {
		return errCatalogUploadClaimed
	}
	return nil
}
func (r *sqliteRepositories) ListMCPSubmissions(ctx context.Context, userID int64, f mcpListInput) (items []catalogSubmission, total int, returnErr error) {
	where := ` WHERE requester_user_id=?`
	args := []any{userID}
	if f.Status != "" {
		where += ` AND status=?`
		args = append(args, f.Status)
	}
	if f.EntityType != "" {
		where += ` AND entity_type=?`
		args = append(args, f.EntityType)
	}
	if f.UpdatedSince != "" {
		t, err := time.Parse(time.RFC3339Nano, f.UpdatedSince)
		if err != nil {
			return nil, 0, mcpInvalid("updatedSince", "must be an RFC3339 timestamp")
		}
		where += ` AND julianday(updated_at)>=julianday(?)`
		args = append(args, formatSQLiteTime(t))
	}
	if err := r.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_submissions`+where, args...).Scan(&total); err != nil {
		return nil, 0, translateSQLiteError(err)
	}
	args = append(args, f.PageSize, (f.Page-1)*f.PageSize)
	rows, err := r.q.QueryContext(ctx, `SELECT `+catalogSubmissionColumns+` FROM catalog_submissions`+where+` ORDER BY updated_at DESC,id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, translateSQLiteError(err)
	}
	defer joinRowsCloseError(&returnErr, rows, "list MCP submissions")
	items = []catalogSubmission{}
	for rows.Next() {
		v, err := scanCatalogSubmission(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, v)
	}
	return items, total, translateSQLiteError(rows.Err())
}

// This request-local adapter joins existing domain workflows to an outer unit
// of work. It never starts nested SQL transactions or shares mutable state.
type joinedUnitOfWork struct{ repositories domainRepositories }

func (u joinedUnitOfWork) WithinTransaction(_ context.Context, f func(domainRepositories) error) error {
	return f(u.repositories)
}
func (u joinedUnitOfWork) WithinReadTransaction(_ context.Context, f func(domainRepositories) error) error {
	return f(u.repositories)
}

func (r *sqliteRepositories) SearchMCPCatalog(ctx context.Context, userID int64, query, kind string, page, pageSize int) (refs []searchReference, total int, returnErr error) {
	tables := []struct{ table, kind, name string }{{"authors", "author", "current_name"}, {"albums", "album", "title"}, {"tracks", "track", "name"}}
	parts := []string{}
	args := []any{}
	for _, t := range tables {
		if kind != "" && kind != t.kind {
			continue
		}
		visibility := catalogSQLVisibility(t.table, userID, &args)
		parts = append(parts, fmt.Sprintf("SELECT '%s' item_type,id,%s item_name FROM %s WHERE %s AND instr(esketit_unicode_fold(%s),?)>0", t.kind, t.name, t.table, visibility, t.name))
		args = append(args, strings.ToLower(strings.TrimSpace(query)))
	}
	union := strings.Join(parts, " UNION ALL ")
	if len(parts) == 0 {
		return nil, 0, mcpInvalid("entityType", "unknown entity type")
	}
	if err := r.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM (`+union+`)`, args...).Scan(&total); err != nil {
		return nil, 0, translateSQLiteError(err)
	}
	rows, err := r.q.QueryContext(ctx, `SELECT item_type,id FROM (`+union+`) ORDER BY esketit_unicode_fold(item_name),item_type,id LIMIT ? OFFSET ?`, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return nil, 0, translateSQLiteError(err)
	}
	defer joinRowsCloseError(&returnErr, rows, "search MCP catalog")
	refs = []searchReference{}
	for rows.Next() {
		var ref searchReference
		if err := rows.Scan(&ref.Type, &ref.ID); err != nil {
			return nil, 0, translateSQLiteError(err)
		}
		refs = append(refs, ref)
	}
	return refs, total, translateSQLiteError(rows.Err())
}
func (r *sqliteRepositories) MCPAuthorReferenced(ctx context.Context, id int64) (bool, error) {
	var found bool
	err := r.q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tracks,json_each(tracks.author_ids_json) WHERE CAST(json_each.value AS INTEGER)=? UNION ALL SELECT 1 FROM albums,json_each(albums.author_ids_json) WHERE CAST(json_each.value AS INTEGER)=?)`, id, id).Scan(&found)
	return found, translateSQLiteError(err)
}
