package main

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func scanCatalogSubmission(row rowScanner) (catalogSubmission, error) {
	var item catalogSubmission
	var snapshotJSON, createdAt, submittedAt, updatedAt string
	var decidedAt sql.NullString
	var decidedBy sql.NullInt64
	if err := row.Scan(
		&item.ID, &item.EntityType, &item.EntityID, &item.RequesterUserID, &item.Status,
		&snapshotJSON, &createdAt, &submittedAt, &updatedAt, &decidedAt, &decidedBy,
	); err != nil {
		return catalogSubmission{}, translateSQLiteError(err)
	}
	if err := unmarshalJSONColumn(snapshotJSON, &item.Snapshot); err != nil {
		return catalogSubmission{}, err
	}
	var err error
	if item.CreatedAt, err = parseSQLiteTime(createdAt); err != nil {
		return catalogSubmission{}, err
	}
	if item.SubmittedAt, err = parseSQLiteTime(submittedAt); err != nil {
		return catalogSubmission{}, err
	}
	if item.UpdatedAt, err = parseSQLiteTime(updatedAt); err != nil {
		return catalogSubmission{}, err
	}
	if decidedAt.Valid {
		parsed, err := parseSQLiteTime(decidedAt.String)
		if err != nil {
			return catalogSubmission{}, err
		}
		item.DecidedAt = &parsed
	}
	item.DecidedByUserID = nullInt64Pointer(decidedBy)
	return item, nil
}

const catalogSubmissionColumns = `id, entity_type, entity_id, requester_user_id, status, snapshot_json, created_at, submitted_at, updated_at, decided_at, decided_by_user_id`

func (r *sqliteRepositories) InsertSubmission(ctx context.Context, item catalogSubmission) (catalogSubmission, error) {
	snapshotJSON, err := marshalJSONColumn(item.Snapshot)
	if err != nil {
		return catalogSubmission{}, err
	}
	result, err := r.q.ExecContext(ctx, `INSERT INTO catalog_submissions (
		entity_type, entity_id, requester_user_id, status, snapshot_json, created_at, submitted_at, updated_at, decided_at, decided_by_user_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.EntityType, item.EntityID, item.RequesterUserID, item.Status, snapshotJSON,
		formatSQLiteTime(item.CreatedAt), formatSQLiteTime(item.SubmittedAt), formatSQLiteTime(item.UpdatedAt),
		formatOptionalSQLiteTime(item.DecidedAt), item.DecidedByUserID)
	if err != nil {
		return catalogSubmission{}, translateSQLiteError(err)
	}
	item.ID, err = result.LastInsertId()
	if err != nil {
		return catalogSubmission{}, translateSQLiteError(err)
	}
	return item, nil
}

func (r *sqliteRepositories) FindSubmissionByID(ctx context.Context, id int64) (catalogSubmission, bool, error) {
	item, err := scanCatalogSubmission(r.q.QueryRowContext(ctx, `SELECT `+catalogSubmissionColumns+` FROM catalog_submissions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return catalogSubmission{}, false, nil
	}
	return item, err == nil, err
}

func (r *sqliteRepositories) FindByEntity(ctx context.Context, entityType string, entityID int64) (catalogSubmission, bool, error) {
	item, err := scanCatalogSubmission(r.q.QueryRowContext(ctx, `SELECT `+catalogSubmissionColumns+` FROM catalog_submissions WHERE entity_type = ? AND entity_id = ?`, entityType, entityID))
	if errors.Is(err, sql.ErrNoRows) {
		return catalogSubmission{}, false, nil
	}
	return item, err == nil, err
}

func (r *sqliteRepositories) UpdateSubmission(ctx context.Context, item catalogSubmission) error {
	snapshotJSON, err := marshalJSONColumn(item.Snapshot)
	if err != nil {
		return err
	}
	result, err := r.q.ExecContext(ctx, `UPDATE catalog_submissions SET status = ?, snapshot_json = ?, submitted_at = ?, updated_at = ?, decided_at = ?, decided_by_user_id = ? WHERE id = ?`,
		item.Status, snapshotJSON, formatSQLiteTime(item.SubmittedAt), formatSQLiteTime(item.UpdatedAt),
		formatOptionalSQLiteTime(item.DecidedAt), item.DecidedByUserID, item.ID)
	if err != nil {
		return translateSQLiteError(err)
	}
	return requireAffected(result)
}

func (r *sqliteRepositories) ListSubmissionsByRequester(ctx context.Context, requesterUserID int64) ([]catalogSubmission, error) {
	return r.listCatalogSubmissions(ctx, `WHERE requester_user_id = ? ORDER BY updated_at DESC, id DESC`, requesterUserID)
}

func (r *sqliteRepositories) ListPendingSubmissionsByRequester(ctx context.Context, requesterUserID int64) ([]catalogSubmission, error) {
	return r.listCatalogSubmissions(ctx, `WHERE requester_user_id = ? AND status = ?
		ORDER BY CASE entity_type WHEN 'track' THEN 0 WHEN 'album' THEN 1 ELSE 2 END, submitted_at, id`, requesterUserID, catalogSubmissionStatusPendingReview)
}

func (r *sqliteRepositories) ListReviewSubmissionsByRequester(ctx context.Context, requesterUserID int64) ([]catalogSubmission, error) {
	return r.listCatalogSubmissions(ctx, `WHERE requester_user_id = ? AND status IN (?, ?)
		ORDER BY CASE status WHEN 'pending_review' THEN 0 ELSE 1 END,
			CASE entity_type WHEN 'track' THEN 0 WHEN 'album' THEN 1 ELSE 2 END, submitted_at, id`,
		requesterUserID, catalogSubmissionStatusPendingReview, catalogSubmissionStatusChangesRequested)
}

func (r *sqliteRepositories) listCatalogSubmissions(ctx context.Context, suffix string, args ...any) (items []catalogSubmission, returnErr error) {
	rows, err := r.q.QueryContext(ctx, `SELECT `+catalogSubmissionColumns+` FROM catalog_submissions `+suffix, args...)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer joinRowsCloseError(&returnErr, rows, "list catalog submissions")
	for rows.Next() {
		item, err := scanCatalogSubmission(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func (r *sqliteRepositories) ListFeedbackBySubmissionIDs(ctx context.Context, ids []int64) (items []catalogSubmissionFeedback, returnErr error) {
	query, args, ok := idQuery(`SELECT id, submission_id, reviewer_user_id, kind, message, rating_penalty, created_at
		FROM catalog_submission_feedback WHERE submission_id IN (%s) ORDER BY created_at, id`, ids)
	if !ok {
		return []catalogSubmissionFeedback{}, nil
	}
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer joinRowsCloseError(&returnErr, rows, "list catalog submission feedback")
	for rows.Next() {
		var item catalogSubmissionFeedback
		var reviewer sql.NullInt64
		var createdAt string
		if err := rows.Scan(&item.ID, &item.SubmissionID, &reviewer, &item.Kind, &item.Message, &item.RatingPenalty, &createdAt); err != nil {
			return nil, translateSQLiteError(err)
		}
		item.ReviewerUserID = nullInt64Pointer(reviewer)
		item.CreatedAt, err = parseSQLiteTime(createdAt)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func (r *sqliteRepositories) InsertFeedback(ctx context.Context, item catalogSubmissionFeedback) (catalogSubmissionFeedback, error) {
	result, err := r.q.ExecContext(ctx, `INSERT INTO catalog_submission_feedback (submission_id, reviewer_user_id, kind, message, rating_penalty, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, item.SubmissionID, item.ReviewerUserID, item.Kind, item.Message, item.RatingPenalty, formatSQLiteTime(item.CreatedAt))
	if err != nil {
		return catalogSubmissionFeedback{}, translateSQLiteError(err)
	}
	item.ID, err = result.LastInsertId()
	if err != nil {
		return catalogSubmissionFeedback{}, translateSQLiteError(err)
	}
	return item, nil
}

func (r *sqliteRepositories) InsertRatingEvent(ctx context.Context, userID, submissionID int64, actorUserID *int64, eventKey, kind string, delta int, reason string, createdAt time.Time) error {
	if delta == 0 {
		return nil
	}
	_, err := r.q.ExecContext(ctx, `INSERT OR IGNORE INTO import_rating_events (user_id, submission_id, actor_user_id, event_key, kind, delta, reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, userID, submissionID, actorUserID, eventKey, kind, delta, reason, formatSQLiteTime(createdAt))
	return translateSQLiteError(err)
}

func (r *sqliteRepositories) ImportRating(ctx context.Context, userID int64) (int, error) {
	var result int
	if err := r.q.QueryRowContext(ctx, `SELECT COALESCE(SUM(delta), 0) FROM import_rating_events WHERE user_id = ?`, userID).Scan(&result); err != nil {
		return 0, translateSQLiteError(err)
	}
	return result, nil
}

func (r *sqliteRepositories) ListReviewRequesters(ctx context.Context, now time.Time) (items []catalogReviewRequesterRecord, returnErr error) {
	rows, err := r.q.QueryContext(ctx, `SELECT users.id, users.email,
		COUNT(catalog_submissions.id), MIN(catalog_submissions.submitted_at),
		COALESCE((SELECT SUM(delta) FROM import_rating_events WHERE user_id = users.id), 0),
		catalog_review_leases.reviewer_user_id, catalog_review_leases.lease_token,
		catalog_review_leases.acquired_at, catalog_review_leases.heartbeat_at, catalog_review_leases.expires_at
	FROM catalog_submissions
	JOIN users ON users.id = catalog_submissions.requester_user_id
	LEFT JOIN catalog_review_leases ON catalog_review_leases.requester_user_id = users.id AND catalog_review_leases.expires_at > ?
	WHERE catalog_submissions.status = ?
	GROUP BY users.id, users.email, catalog_review_leases.reviewer_user_id, catalog_review_leases.lease_token,
		catalog_review_leases.acquired_at, catalog_review_leases.heartbeat_at, catalog_review_leases.expires_at
	ORDER BY MIN(catalog_submissions.submitted_at), users.id`, formatSQLiteTime(now), catalogSubmissionStatusPendingReview)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer joinRowsCloseError(&returnErr, rows, "list catalog review requesters")
	for rows.Next() {
		var item catalogReviewRequesterRecord
		var oldestPending string
		var reviewer sql.NullInt64
		var token, acquiredAt, heartbeatAt, expiresAt sql.NullString
		if err := rows.Scan(&item.RequesterID, &item.Email, &item.PendingCount, &oldestPending, &item.ImportRating,
			&reviewer, &token, &acquiredAt, &heartbeatAt, &expiresAt); err != nil {
			return nil, translateSQLiteError(err)
		}
		parsed, err := parseSQLiteTime(oldestPending)
		if err != nil {
			return nil, err
		}
		item.OldestPending = parsed
		if reviewer.Valid {
			lease, err := catalogReviewLeaseFromSQL(item.RequesterID, reviewer.Int64, token.String, acquiredAt.String, heartbeatAt.String, expiresAt.String)
			if err != nil {
				return nil, err
			}
			item.Lease = &lease
		}
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func scanCatalogReviewLease(row rowScanner) (catalogReviewLease, error) {
	var requesterID, reviewerID int64
	var token, acquiredAt, heartbeatAt, expiresAt string
	if err := row.Scan(&requesterID, &reviewerID, &token, &acquiredAt, &heartbeatAt, &expiresAt); err != nil {
		return catalogReviewLease{}, translateSQLiteError(err)
	}
	return catalogReviewLeaseFromSQL(requesterID, reviewerID, token, acquiredAt, heartbeatAt, expiresAt)
}

func catalogReviewLeaseFromSQL(requesterID, reviewerID int64, token, acquiredAt, heartbeatAt, expiresAt string) (catalogReviewLease, error) {
	item := catalogReviewLease{RequesterUserID: requesterID, ReviewerUserID: reviewerID, LeaseToken: token}
	var err error
	if item.AcquiredAt, err = parseSQLiteTime(acquiredAt); err != nil {
		return catalogReviewLease{}, err
	}
	if item.HeartbeatAt, err = parseSQLiteTime(heartbeatAt); err != nil {
		return catalogReviewLease{}, err
	}
	if item.ExpiresAt, err = parseSQLiteTime(expiresAt); err != nil {
		return catalogReviewLease{}, err
	}
	return item, nil
}

func (r *sqliteRepositories) DeleteExpiredReviewLeases(ctx context.Context, now time.Time) error {
	_, err := r.q.ExecContext(ctx, `DELETE FROM catalog_review_leases WHERE expires_at <= ?`, formatSQLiteTime(now))
	return translateSQLiteError(err)
}

func (r *sqliteRepositories) FindReviewLeaseByRequester(ctx context.Context, requesterID int64) (catalogReviewLease, bool, error) {
	item, err := scanCatalogReviewLease(r.q.QueryRowContext(ctx, `SELECT requester_user_id, reviewer_user_id, lease_token, acquired_at, heartbeat_at, expires_at
		FROM catalog_review_leases WHERE requester_user_id = ?`, requesterID))
	if errors.Is(err, sql.ErrNoRows) {
		return catalogReviewLease{}, false, nil
	}
	return item, err == nil, err
}

func (r *sqliteRepositories) FindReviewLeaseByReviewer(ctx context.Context, reviewerID int64) (catalogReviewLease, bool, error) {
	item, err := scanCatalogReviewLease(r.q.QueryRowContext(ctx, `SELECT requester_user_id, reviewer_user_id, lease_token, acquired_at, heartbeat_at, expires_at
		FROM catalog_review_leases WHERE reviewer_user_id = ?`, reviewerID))
	if errors.Is(err, sql.ErrNoRows) {
		return catalogReviewLease{}, false, nil
	}
	return item, err == nil, err
}

func (r *sqliteRepositories) InsertReviewLease(ctx context.Context, item catalogReviewLease) error {
	_, err := r.q.ExecContext(ctx, `INSERT INTO catalog_review_leases (requester_user_id, reviewer_user_id, lease_token, acquired_at, heartbeat_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`, item.RequesterUserID, item.ReviewerUserID, item.LeaseToken,
		formatSQLiteTime(item.AcquiredAt), formatSQLiteTime(item.HeartbeatAt), formatSQLiteTime(item.ExpiresAt))
	return translateSQLiteError(err)
}

func (r *sqliteRepositories) RotateReviewLease(ctx context.Context, item catalogReviewLease, oldToken string, now time.Time) error {
	result, err := r.q.ExecContext(ctx, `UPDATE catalog_review_leases SET lease_token = ?, heartbeat_at = ?, expires_at = ?
		WHERE requester_user_id = ? AND reviewer_user_id = ? AND lease_token = ? AND expires_at > ?`,
		item.LeaseToken, formatSQLiteTime(item.HeartbeatAt), formatSQLiteTime(item.ExpiresAt),
		item.RequesterUserID, item.ReviewerUserID, oldToken, formatSQLiteTime(now))
	if err != nil {
		return translateSQLiteError(err)
	}
	if err := requireAffected(result); err != nil {
		return errCatalogReviewLeaseInvalid
	}
	return nil
}

func (r *sqliteRepositories) UpdateReviewLease(ctx context.Context, item catalogReviewLease, now time.Time) error {
	result, err := r.q.ExecContext(ctx, `UPDATE catalog_review_leases SET heartbeat_at = ?, expires_at = ?
		WHERE requester_user_id = ? AND reviewer_user_id = ? AND lease_token = ? AND expires_at > ?`,
		formatSQLiteTime(item.HeartbeatAt), formatSQLiteTime(item.ExpiresAt), item.RequesterUserID, item.ReviewerUserID, item.LeaseToken, formatSQLiteTime(now))
	if err != nil {
		return translateSQLiteError(err)
	}
	if err := requireAffected(result); err != nil {
		return errCatalogReviewLeaseInvalid
	}
	return nil
}

func (r *sqliteRepositories) DeleteReviewLease(ctx context.Context, requesterID, reviewerID int64, token string, now time.Time) error {
	result, err := r.q.ExecContext(ctx, `DELETE FROM catalog_review_leases WHERE requester_user_id = ? AND reviewer_user_id = ? AND lease_token = ? AND expires_at > ?`,
		requesterID, reviewerID, token, formatSQLiteTime(now))
	if err != nil {
		return translateSQLiteError(err)
	}
	if err := requireAffected(result); err != nil {
		return errCatalogReviewLeaseInvalid
	}
	return nil
}

func (r *sqliteRepositories) InsertSubmissionUpload(ctx context.Context, item catalogSubmissionUpload) error {
	_, err := r.q.ExecContext(ctx, `INSERT INTO catalog_submission_uploads (token, requester_user_id, stored_file_name, original_file_name, created_at, claimed_track_id)
		VALUES (?, ?, ?, ?, ?, ?)`, item.Token, item.RequesterUserID, item.StoredFileName, item.OriginalName, formatSQLiteTime(item.CreatedAt), item.ClaimedTrackID)
	return translateSQLiteError(err)
}

func (r *sqliteRepositories) FindSubmissionUpload(ctx context.Context, token string) (catalogSubmissionUpload, bool, error) {
	return r.findSubmissionUpload(ctx, `token = ?`, token)
}

func (r *sqliteRepositories) FindSubmissionUploadByTrack(ctx context.Context, trackID int64) (catalogSubmissionUpload, bool, error) {
	return r.findSubmissionUpload(ctx, `claimed_track_id = ?`, trackID)
}

func (r *sqliteRepositories) ListSubmissionUploadFileNames(ctx context.Context) (items []string, returnErr error) {
	rows, err := r.q.QueryContext(ctx, `SELECT stored_file_name FROM catalog_submission_uploads ORDER BY stored_file_name`)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer joinRowsCloseError(&returnErr, rows, "list catalog submission upload files")
	for rows.Next() {
		var item string
		if err := rows.Scan(&item); err != nil {
			return nil, translateSQLiteError(err)
		}
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func (r *sqliteRepositories) findSubmissionUpload(ctx context.Context, predicate string, value any) (catalogSubmissionUpload, bool, error) {
	var item catalogSubmissionUpload
	var createdAt string
	var claimed sql.NullInt64
	err := r.q.QueryRowContext(ctx, `SELECT token, requester_user_id, stored_file_name, original_file_name, created_at, claimed_track_id
		FROM catalog_submission_uploads WHERE `+predicate, value).Scan(&item.Token, &item.RequesterUserID, &item.StoredFileName, &item.OriginalName, &createdAt, &claimed)
	if errors.Is(err, sql.ErrNoRows) {
		return catalogSubmissionUpload{}, false, nil
	}
	if err != nil {
		return catalogSubmissionUpload{}, false, translateSQLiteError(err)
	}
	item.CreatedAt, err = parseSQLiteTime(createdAt)
	if err != nil {
		return catalogSubmissionUpload{}, false, err
	}
	item.ClaimedTrackID = nullInt64Pointer(claimed)
	return item, true, nil
}

func (r *sqliteRepositories) ClaimSubmissionUpload(ctx context.Context, token string, requesterID, trackID int64) error {
	result, err := r.q.ExecContext(ctx, `UPDATE catalog_submission_uploads SET claimed_track_id = ?
		WHERE token = ? AND requester_user_id = ? AND claimed_track_id IS NULL`, trackID, token, requesterID)
	if err != nil {
		return translateSQLiteError(err)
	}
	if err := requireAffected(result); err != nil {
		return errCatalogUploadClaimed
	}
	return nil
}

func (r *sqliteRepositories) DeleteSubmissionUpload(ctx context.Context, token string) error {
	result, err := r.q.ExecContext(ctx, `DELETE FROM catalog_submission_uploads WHERE token = ?`, token)
	if err != nil {
		return translateSQLiteError(err)
	}
	return requireAffected(result)
}

func formatOptionalSQLiteTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatSQLiteTime(*value)
}
