package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const catalogSubmissionEntityAlbumTrackOrder = "album_track_order"

var errTrackNumberProposalExists = fmt.Errorf("%w: an active order proposal already exists for this requester and album", errCatalogSubmissionState)

type createAlbumTrackOrderSubmissionRequest struct {
	AlbumID int64 `json:"albumId"`
	reorderAlbumTracksRequest
	Reason string `json:"reason"`
}

type updateAlbumTrackOrderSubmissionRequest struct {
	ExpectedRevision      int64                    `json:"expectedRevision"`
	Tracks                *[]trackNumberAssignment `json:"tracks"`
	ExpectedOrderRevision *int64                   `json:"expectedOrderRevision"`
	Reason                *string                  `json:"reason"`
}

func (r *updateAlbumTrackOrderSubmissionRequest) UnmarshalJSON(raw []byte) error {
	type patch updateAlbumTrackOrderSubmissionRequest
	var value patch
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, key := range []string{"tracks", "expectedOrderRevision", "reason"} {
		if field, supplied := fields[key]; supplied && strings.TrimSpace(string(field)) == "null" {
			return fmt.Errorf("%s must not be null; omit it to preserve the current value", key)
		}
	}
	*r = updateAlbumTrackOrderSubmissionRequest(value)
	return nil
}

// The snapshot describes a proposal, never the mutable catalog album itself.
type albumTrackOrderProposal struct {
	AlbumID               int64                   `json:"albumId"`
	BaseTracks            []trackNumberAssignment `json:"baseTracks"`
	Tracks                []trackNumberAssignment `json:"tracks"`
	ExpectedOrderRevision int64                   `json:"expectedOrderRevision"`
	Reason                string                  `json:"reason"`
	AppliedOrderRevision  int64                   `json:"appliedOrderRevision,omitempty"`
}

type trackNumberTrackSummary struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type albumTrackOrderSubmissionView struct {
	albumTrackOrderProposal
	AlbumTitle           string                    `json:"albumTitle"`
	CoverImagePath       string                    `json:"coverImagePath"`
	Outdated             bool                      `json:"outdated"`
	TargetAvailable      bool                      `json:"targetAvailable"`
	CurrentTracks        []trackNumberAssignment   `json:"currentTracks"`
	CurrentOrderRevision int64                     `json:"currentOrderRevision,omitempty"`
	TrackDetails         []trackNumberTrackSummary `json:"trackDetails"`
}

// Rebuild the parent table without losing feedback, rating links or claimed MCP
// uploads. Foreign keys remain enabled throughout this transactional migration.
func migrateAlbumTrackOrderSubmissions(ctx context.Context, tx *sql.Tx) error {
	statements := []string{
		`CREATE TABLE catalog_submissions_new (
		 id INTEGER PRIMARY KEY AUTOINCREMENT,
		 entity_type TEXT NOT NULL CHECK(entity_type IN ('author','album','track','album_track_order')),
		 entity_id INTEGER NOT NULL,
		 requester_user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
		 status TEXT NOT NULL CHECK(status IN ('pending_review','changes_requested','approved','rejected','cancelled')),
		 snapshot_json TEXT NOT NULL, created_at TEXT NOT NULL, submitted_at TEXT NOT NULL, updated_at TEXT NOT NULL,
		 decided_at TEXT, decided_by_user_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
		 revision INTEGER NOT NULL DEFAULT 1 CHECK(revision > 0))`,
		`INSERT INTO catalog_submissions_new SELECT id,entity_type,entity_id,requester_user_id,status,snapshot_json,created_at,submitted_at,updated_at,decided_at,decided_by_user_id,revision FROM catalog_submissions`,
		`CREATE TEMP TABLE order_migration_feedback AS SELECT * FROM catalog_submission_feedback`,
		`CREATE TEMP TABLE order_migration_ratings(id INTEGER PRIMARY KEY, submission_id INTEGER)`,
		`INSERT INTO order_migration_ratings SELECT id,submission_id FROM import_rating_events WHERE submission_id IS NOT NULL`,
		`CREATE TEMP TABLE order_migration_uploads(token TEXT PRIMARY KEY, claimed_submission_id INTEGER)`,
		`INSERT INTO order_migration_uploads SELECT token,claimed_submission_id FROM mcp_uploads WHERE claimed_submission_id IS NOT NULL`,
		`CREATE TEMP TABLE order_migration_sequence AS SELECT seq FROM sqlite_sequence WHERE name = 'catalog_submissions'`,
		`UPDATE mcp_uploads SET claimed_submission_id = NULL`,
		`DROP TABLE catalog_submissions`,
		`ALTER TABLE catalog_submissions_new RENAME TO catalog_submissions`,
		`UPDATE sqlite_sequence SET seq = MAX(seq, COALESCE((SELECT seq FROM order_migration_sequence), seq)) WHERE name = 'catalog_submissions'`,
		`INSERT INTO catalog_submission_feedback SELECT * FROM order_migration_feedback`,
		`UPDATE import_rating_events SET submission_id = (SELECT submission_id FROM order_migration_ratings WHERE order_migration_ratings.id = import_rating_events.id) WHERE id IN (SELECT id FROM order_migration_ratings)`,
		`UPDATE mcp_uploads SET claimed_submission_id = (SELECT claimed_submission_id FROM order_migration_uploads WHERE order_migration_uploads.token = mcp_uploads.token) WHERE token IN (SELECT token FROM order_migration_uploads)`,
		`DROP TABLE order_migration_feedback`, `DROP TABLE order_migration_ratings`, `DROP TABLE order_migration_uploads`, `DROP TABLE order_migration_sequence`,
		`CREATE UNIQUE INDEX idx_catalog_submissions_entity ON catalog_submissions(entity_type,entity_id) WHERE entity_type != 'album_track_order'`,
		`CREATE UNIQUE INDEX idx_album_track_order_active ON catalog_submissions(requester_user_id,entity_id) WHERE entity_type = 'album_track_order' AND status IN ('pending_review','changes_requested')`,
		`CREATE INDEX idx_catalog_submissions_requester_status ON catalog_submissions(requester_user_id,status,submitted_at,id)`,
		`CREATE INDEX idx_catalog_submissions_review_queue ON catalog_submissions(status,requester_user_id,submitted_at,id)`,
		`CREATE INDEX idx_mcp_submissions_requester_updated ON catalog_submissions(requester_user_id,updated_at DESC,id DESC)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO permissions(code,description,created_at) VALUES(?,?,?)`, permissionAlbumsSubmitTrackOrder, "Propose album track order changes for review", formatSQLiteTime(time.Now().UTC())); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO role_permissions(role_id,permission_id) SELECT roles.id,permissions.id FROM roles CROSS JOIN permissions WHERE roles.name IN (?,?) AND permissions.code = ?`, roleAdmin, roleListener, permissionAlbumsSubmitTrackOrder)
	return err
}

func decodeAlbumTrackOrderProposal(item catalogSubmission) (albumTrackOrderProposal, error) {
	var proposal albumTrackOrderProposal
	raw, err := json.Marshal(item.Snapshot)
	if err == nil {
		err = json.Unmarshal(raw, &proposal)
	}
	return proposal, err
}

func validateAlbumTrackOrderProposal(ctx context.Context, repos domainRepositories, userID int64, req createAlbumTrackOrderSubmissionRequest) (album, error) {
	if req.AlbumID < 1 || req.Tracks == nil || req.ExpectedOrderRevision < 1 {
		return album{}, fmt.Errorf("%w: albumId, tracks and positive expectedOrderRevision are required", errInvalidAlbum)
	}
	if utf8.RuneCountInString(strings.TrimSpace(req.Reason)) > 1000 {
		return album{}, fmt.Errorf("%w: reason must be at most 1000 characters", errInvalidAlbum)
	}
	item, found, err := repos.catalog.FindAlbumByID(ctx, req.AlbumID)
	if err != nil {
		return album{}, err
	}
	if !found {
		return album{}, errRepositoryNotFound
	}
	if normalizePublicationStatus(item.PublicationStatus) != catalogPublicationPublished {
		return album{}, errTrackNumberState
	}
	if item.OrderRevision != req.ExpectedOrderRevision {
		return album{}, errTrackNumberStale
	}
	tracks, err := repos.catalog.ListAlbumTracks(ctx, item.ID)
	if err != nil {
		return album{}, err
	}
	memberIDs := make([]int64, 0, len(tracks))
	for _, track := range tracks {
		if !catalogEntityVisible(track.PublicationStatus, track.RequestedByUserID, userID) {
			return album{}, errTrackNumberHidden
		}
		memberIDs = append(memberIDs, track.ID)
	}
	if err := validateExactTrackOrder(item.TrackIDs, memberIDs); err != nil {
		return album{}, fmt.Errorf("%w: current album membership is inconsistent", errInvalidAlbum)
	}
	if err := validateTrackNumbers(req.Tracks, item.TrackIDs); err != nil {
		return album{}, fmt.Errorf("%w: %v", errInvalidAlbum, err)
	}
	return item, nil
}

func proposalRequest(p albumTrackOrderProposal) createAlbumTrackOrderSubmissionRequest {
	return createAlbumTrackOrderSubmissionRequest{AlbumID: p.AlbumID, reorderAlbumTracksRequest: reorderAlbumTracksRequest{Tracks: p.Tracks, ExpectedOrderRevision: p.ExpectedOrderRevision}, Reason: p.Reason}
}

func (s *trackStore) createAlbumTrackOrderSubmission(userID int64, req createAlbumTrackOrderSubmissionRequest) (catalogSubmissionResponse, error) {
	var result catalogSubmissionResponse
	err := s.unitOfWork.WithinTransaction(context.Background(), func(repos domainRepositories) error {
		ctx := context.Background()
		allowed, err := repos.access.UserHasPermission(ctx, userID, permissionAlbumsSubmitTrackOrder)
		if err != nil {
			return err
		}
		if !allowed {
			return errCatalogSubmissionForbidden
		}
		item, err := validateAlbumTrackOrderProposal(ctx, repos, userID, req)
		if err != nil {
			return err
		}
		owned, err := repos.submissions.ListSubmissionsByRequester(ctx, userID)
		if err != nil {
			return err
		}
		for _, sub := range owned {
			if sub.EntityType == catalogSubmissionEntityAlbumTrackOrder && sub.EntityID == item.ID && (sub.Status == catalogSubmissionStatusPendingReview || sub.Status == catalogSubmissionStatusChangesRequested) {
				return errTrackNumberProposalExists
			}
		}
		base, err := repos.catalog.ListAlbumTracks(ctx, item.ID)
		if err != nil {
			return err
		}
		proposal := albumTrackOrderProposal{AlbumID: item.ID, BaseTracks: assignmentsForTracks(base), Tracks: append([]trackNumberAssignment{}, req.Tracks...), ExpectedOrderRevision: item.OrderRevision, Reason: strings.TrimSpace(req.Reason)}
		sub, err := newCatalogSubmission(catalogSubmissionEntityAlbumTrackOrder, item.ID, userID, proposal, time.Now().UTC())
		if err != nil {
			return err
		}
		sub, err = repos.submissions.InsertSubmission(ctx, sub)
		if err != nil {
			return err
		}
		responses, err := buildCatalogSubmissionResponses(ctx, repos, []catalogSubmission{sub})
		if err == nil {
			result = responses[0]
		}
		return err
	})
	return result, err
}

func (s *trackStore) updateAlbumTrackOrderSubmission(userID, submissionID int64, req updateAlbumTrackOrderSubmissionRequest) (catalogSubmissionResponse, error) {
	var result catalogSubmissionResponse
	err := s.unitOfWork.WithinTransaction(context.Background(), func(repos domainRepositories) error {
		ctx := context.Background()
		allowed, err := repos.access.UserHasPermission(ctx, userID, permissionCatalogSubmissionsUpdate)
		if err != nil {
			return err
		}
		if !allowed {
			return errCatalogSubmissionForbidden
		}
		sub, found, err := repos.submissions.FindSubmissionByID(ctx, submissionID)
		if err != nil {
			return err
		}
		if !found {
			return errCatalogSubmissionNotFound
		}
		if err := validateSubmissionOwner(sub, userID); err != nil {
			return err
		}
		if sub.EntityType != catalogSubmissionEntityAlbumTrackOrder {
			return errCatalogSubmissionNotFound
		}
		if req.ExpectedRevision < 1 {
			return fmt.Errorf("%w: expectedRevision must be positive", errInvalidAlbum)
		}
		if req.ExpectedRevision != sub.Revision {
			return errMCPStaleRevision
		}
		if err := validateSubmissionEditable(sub); err != nil {
			return err
		}
		if req.Tracks == nil && req.ExpectedOrderRevision == nil && req.Reason == nil {
			return fmt.Errorf("%w: at least one proposal field is required", errInvalidAlbum)
		}
		proposal, err := decodeAlbumTrackOrderProposal(sub)
		if err != nil {
			return err
		}
		if req.Tracks != nil {
			proposal.Tracks = *req.Tracks
		}
		if req.ExpectedOrderRevision != nil {
			proposal.ExpectedOrderRevision = *req.ExpectedOrderRevision
		}
		if req.Reason != nil {
			proposal.Reason = strings.TrimSpace(*req.Reason)
		}
		item, err := validateAlbumTrackOrderProposal(ctx, repos, userID, proposalRequest(proposal))
		if err != nil {
			return err
		}
		base, err := repos.catalog.ListAlbumTracks(ctx, item.ID)
		if err != nil {
			return err
		}
		proposal.BaseTracks = assignmentsForTracks(base)
		if err := updateSubmissionSnapshot(ctx, repos, &sub, proposal, &result); err != nil {
			return err
		}
		result.Entity, err = albumTrackOrderProposalView(ctx, repos, sub)
		return err
	})
	return result, err
}

func albumTrackOrderProposalView(ctx context.Context, repos domainRepositories, sub catalogSubmission) (albumTrackOrderSubmissionView, error) {
	proposal, err := decodeAlbumTrackOrderProposal(sub)
	view := albumTrackOrderSubmissionView{albumTrackOrderProposal: proposal, CurrentTracks: []trackNumberAssignment{}, TrackDetails: []trackNumberTrackSummary{}}
	active := sub.Status == catalogSubmissionStatusPendingReview || sub.Status == catalogSubmissionStatusChangesRequested
	view.Outdated = active // An unavailable target cannot be approved.
	if err != nil {
		return view, err
	}
	item, found, err := repos.catalog.FindAlbumByID(ctx, proposal.AlbumID)
	if err != nil || !found {
		return view, err
	}
	if !catalogEntityVisible(item.PublicationStatus, item.RequestedByUserID, sub.RequesterUserID) {
		return view, nil
	}
	view.TargetAvailable = true
	view.AlbumTitle = item.Title
	view.CoverImagePath = item.CoverImagePath
	view.CurrentOrderRevision = item.OrderRevision
	tracks, err := repos.reads.ListTracksByIDs(ctx, item.TrackIDs)
	if err != nil {
		return view, err
	}
	visible := map[int64]track{}
	for _, t := range tracks {
		if catalogEntityVisible(t.PublicationStatus, t.RequestedByUserID, sub.RequesterUserID) {
			visible[t.ID] = t
		}
	}
	for _, id := range item.TrackIDs {
		if t, ok := visible[id]; ok {
			view.CurrentTracks = append(view.CurrentTracks, trackNumberAssignment{TrackID: id, TrackNumber: t.TrackNumber})
			view.TrackDetails = append(view.TrackDetails, trackNumberTrackSummary{ID: id, Name: t.Name})
		}
	}
	view.Outdated = active && (proposal.ExpectedOrderRevision != item.OrderRevision || !sameTrackNumberAssignments(proposal.BaseTracks, view.CurrentTracks))
	return view, nil
}

// Track numbers define the order; JSON array order is irrelevant.
func sameTrackNumberAssignments(before, current []trackNumberAssignment) bool {
	if len(before) != len(current) {
		return false
	}
	numbers := make(map[int64]int, len(before))
	for _, item := range before {
		if _, duplicate := numbers[item.TrackID]; duplicate {
			return false
		}
		numbers[item.TrackID] = item.TrackNumber
	}
	for _, item := range current {
		if number, found := numbers[item.TrackID]; !found || number != item.TrackNumber {
			return false
		}
		delete(numbers, item.TrackID)
	}
	return len(numbers) == 0
}

func validateAlbumTrackOrderBase(ctx context.Context, repos domainRepositories, proposal albumTrackOrderProposal) error {
	tracks, err := repos.catalog.ListAlbumTracks(ctx, proposal.AlbumID)
	if err != nil {
		return err
	}
	if !sameTrackNumberAssignments(proposal.BaseTracks, assignmentsForTracks(tracks)) {
		return errTrackNumberStale
	}
	return nil
}

func applyAlbumTrackOrderProposal(ctx context.Context, repos domainRepositories, sub catalogSubmission) (albumTrackOrderProposal, error) {
	proposal, err := decodeAlbumTrackOrderProposal(sub)
	if err != nil {
		return proposal, err
	}
	item, err := validateAlbumTrackOrderProposal(ctx, repos, sub.RequesterUserID, proposalRequest(proposal))
	if err != nil {
		return proposal, err
	}
	if err := validateAlbumTrackOrderBase(ctx, repos, proposal); err != nil {
		return proposal, err
	}
	if err := repos.catalog.SetAlbumTrackNumbers(ctx, item.ID, proposal.Tracks); err != nil {
		return proposal, err
	}
	item, _, err = repos.catalog.FindAlbumByID(ctx, item.ID)
	if err != nil {
		return proposal, err
	}
	proposal.AppliedOrderRevision = item.OrderRevision
	return proposal, nil
}

func albumTrackOrderSubmissionHandler(store *trackStore, update bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		var result catalogSubmissionResponse
		var err error
		if update {
			var id int64
			id, err = parseResourceID(r.URL.Path, "/api/catalog-submissions/album-track-orders/")
			if err != nil {
				http.Error(w, "invalid submission id", http.StatusBadRequest)
				return
			}
			var req updateAlbumTrackOrderSubmissionRequest
			if err := decodeJSON(r, &req); err != nil {
				writeRequestDecodeError(w, err)
				return
			}
			result, err = store.updateAlbumTrackOrderSubmission(userID, id, req)
		} else {
			var req createAlbumTrackOrderSubmissionRequest
			if err := decodeJSON(r, &req); err != nil {
				writeRequestDecodeError(w, err)
				return
			}
			result, err = store.createAlbumTrackOrderSubmission(userID, req)
		}
		if err != nil {
			writeCatalogSubmissionError(w, r, err, "catalog_submissions.album_track_orders")
			return
		}
		status := http.StatusCreated
		if update {
			status = http.StatusOK
		}
		writeJSON(w, status, result)
	}
}

func albumTrackOrderDependenciesReady(ctx context.Context, repos domainRepositories, sub catalogSubmission) error {
	proposal, err := decodeAlbumTrackOrderProposal(sub)
	if err != nil {
		return err
	}
	_, err = validateAlbumTrackOrderProposal(ctx, repos, sub.RequesterUserID, proposalRequest(proposal))
	if err == nil {
		err = validateAlbumTrackOrderBase(ctx, repos, proposal)
	}
	if err != nil {
		if errors.Is(err, errTrackNumberStale) || errors.Is(err, errTrackNumberState) || errors.Is(err, errTrackNumberHidden) || errors.Is(err, errInvalidAlbum) || errors.Is(err, errRepositoryNotFound) {
			return fmt.Errorf("%w: refresh the album order proposal: %w", errCatalogSubmissionDependency, err)
		}
	}
	return err
}
