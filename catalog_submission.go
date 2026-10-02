package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	catalogPublicationPublished        = "published"
	catalogPublicationPendingReview    = "pending_review"
	catalogPublicationChangesRequested = "changes_requested"
	catalogPublicationRejected         = "rejected"

	catalogSubmissionEntityAuthor = "author"
	catalogSubmissionEntityAlbum  = "album"
	catalogSubmissionEntityTrack  = "track"

	catalogSubmissionStatusPendingReview    = "pending_review"
	catalogSubmissionStatusChangesRequested = "changes_requested"
	catalogSubmissionStatusApproved         = "approved"
	catalogSubmissionStatusRejected         = "rejected"
	catalogSubmissionStatusCancelled        = "cancelled"

	catalogFeedbackChangesRequested = "changes_requested"
	catalogFeedbackRejected         = "rejected"

	catalogReviewLeaseTTL = 10 * time.Minute
)

var (
	errCatalogSubmissionNotFound      = errors.New("catalog submission not found")
	errCatalogSubmissionForbidden     = errors.New("catalog submission is not owned by this user")
	errCatalogSubmissionState         = errors.New("catalog submission is not in the required state")
	errCatalogSubmissionDependency    = errors.New("catalog submission has unresolved dependencies")
	errCatalogTrackReviewPrerequisite = fmt.Errorf("%w: review album and authors first", errCatalogSubmissionDependency)
	errCatalogRejectedDependency      = fmt.Errorf("%w: an author or album was rejected", errCatalogSubmissionDependency)
	errCatalogReviewLeaseConflict     = errors.New("requester is already being reviewed")
	errCatalogReviewerAlreadyLeasing  = errors.New("reviewer already has an active review")
	errCatalogReviewLeaseInvalid      = errors.New("catalog review lease is missing, expired, or belongs to another reviewer")
	errCatalogSelfReview              = errors.New("users cannot review their own submissions")
	errCatalogUploadNotFound          = errors.New("staged audio upload not found")
	errCatalogUploadClaimed           = errors.New("staged audio upload is already assigned to a track")
)

type catalogSubmission struct {
	ID              int64          `json:"id"`
	EntityType      string         `json:"entityType"`
	EntityID        int64          `json:"entityId"`
	RequesterUserID int64          `json:"requesterUserId"`
	Status          string         `json:"status"`
	Snapshot        map[string]any `json:"snapshot"`
	CreatedAt       time.Time      `json:"createdAt"`
	SubmittedAt     time.Time      `json:"submittedAt"`
	UpdatedAt       time.Time      `json:"updatedAt"`
	DecidedAt       *time.Time     `json:"decidedAt,omitempty"`
	DecidedByUserID *int64         `json:"decidedByUserId,omitempty"`
}

type catalogSubmissionFeedback struct {
	ID             int64     `json:"id"`
	SubmissionID   int64     `json:"submissionId"`
	ReviewerUserID *int64    `json:"reviewerUserId,omitempty"`
	Kind           string    `json:"kind"`
	Message        string    `json:"message"`
	RatingPenalty  int       `json:"ratingPenalty"`
	CreatedAt      time.Time `json:"createdAt"`
}

type catalogSubmissionResponse struct {
	catalogSubmission
	Entity   any                         `json:"entity,omitempty"`
	Feedback []catalogSubmissionFeedback `json:"feedback"`
}

type catalogSubmissionListResponse struct {
	Items        []catalogSubmissionResponse `json:"items"`
	ImportRating int                         `json:"importRating"`
}

type catalogReviewLease struct {
	RequesterUserID int64     `json:"requesterUserId"`
	ReviewerUserID  int64     `json:"reviewerUserId"`
	LeaseToken      string    `json:"leaseToken"`
	AcquiredAt      time.Time `json:"acquiredAt"`
	HeartbeatAt     time.Time `json:"heartbeatAt"`
	ExpiresAt       time.Time `json:"expiresAt"`
}

type catalogReviewRequester struct {
	User          catalogReviewUser         `json:"user"`
	PendingCount  int                       `json:"pendingCount"`
	ImportRating  int                       `json:"importRating"`
	ActiveLease   *catalogReviewLeaseStatus `json:"activeLease,omitempty"`
	OldestPending time.Time                 `json:"oldestPendingAt"`
}

type catalogReviewLeaseStatus struct {
	ReviewerUserID int64     `json:"reviewerUserId"`
	AcquiredAt     time.Time `json:"acquiredAt"`
	HeartbeatAt    time.Time `json:"heartbeatAt"`
	ExpiresAt      time.Time `json:"expiresAt"`
}

type catalogReviewUser struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
}

type catalogReviewRequesterRecord struct {
	RequesterID   int64
	Email         string
	PendingCount  int
	ImportRating  int
	OldestPending time.Time
	Lease         *catalogReviewLease
}

type catalogReviewDecisionRequest struct {
	Message       string `json:"message"`
	RatingPenalty int    `json:"ratingPenalty"`
}

type createTrackSubmissionRequest struct {
	Name             string           `json:"name"`
	AuthorIDs        []int64          `json:"authorIds"`
	AlbumID          int64            `json:"albumId"`
	AlbumOrder       int              `json:"albumOrder"`
	AudioUploadToken string           `json:"audioUploadToken"`
	AdditionalInfo   []additionalInfo `json:"additionalInfo"`
	SourceMetadata   []sourceMetadata `json:"sourceMetadata"`
}

type updateTrackSubmissionRequest struct {
	Name             string           `json:"name"`
	AuthorIDs        []int64          `json:"authorIds"`
	AlbumID          int64            `json:"albumId"`
	AlbumOrder       int              `json:"albumOrder"`
	AudioUploadToken string           `json:"audioUploadToken,omitempty"`
	AdditionalInfo   []additionalInfo `json:"additionalInfo"`
	SourceMetadata   []sourceMetadata `json:"sourceMetadata"`
}

type catalogSubmissionUpload struct {
	Token           string    `json:"token"`
	RequesterUserID int64     `json:"requesterUserId"`
	StoredFileName  string    `json:"-"`
	OriginalName    string    `json:"originalName"`
	CreatedAt       time.Time `json:"createdAt"`
	ClaimedTrackID  *int64    `json:"claimedTrackId,omitempty"`
	SizeBytes       int64     `json:"sizeBytes,omitempty"`
}

func normalizePublicationStatus(value string) string {
	if strings.TrimSpace(value) == "" {
		return catalogPublicationPublished
	}
	return value
}

func catalogEntityVisible(status string, requestedBy *int64, viewerUserID int64) bool {
	switch normalizePublicationStatus(status) {
	case catalogPublicationPublished:
		return true
	case catalogPublicationPendingReview, catalogPublicationChangesRequested:
		return viewerUserID > 0 && requestedBy != nil && *requestedBy == viewerUserID
	}
	return false
}

func requestedByUserID(userID int64) *int64 {
	value := userID
	return &value
}

func normalizeCatalogDecision(request catalogReviewDecisionRequest, requireMessage bool) (catalogReviewDecisionRequest, error) {
	request.Message = strings.TrimSpace(request.Message)
	if requireMessage && request.Message == "" {
		return catalogReviewDecisionRequest{}, errors.New("feedback message is required")
	}
	if request.RatingPenalty < 0 {
		return catalogReviewDecisionRequest{}, errors.New("ratingPenalty must be greater than or equal to 0")
	}
	return request, nil
}

func catalogSubmissionSnapshot(entity any) (map[string]any, error) {
	encoded, err := marshalJSONColumn(entity)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := unmarshalJSONColumn(encoded, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func validateSubmissionOwner(item catalogSubmission, requesterUserID int64) error {
	if item.RequesterUserID != requesterUserID {
		return errCatalogSubmissionForbidden
	}
	return nil
}

func validateSubmissionEditable(item catalogSubmission) error {
	if item.Status != catalogSubmissionStatusChangesRequested {
		return fmt.Errorf("%w: only submissions with requested changes can be edited", errCatalogSubmissionState)
	}
	return nil
}

func newCatalogSubmission(entityType string, entityID, requesterUserID int64, entity any, now time.Time) (catalogSubmission, error) {
	snapshot, err := catalogSubmissionSnapshot(entity)
	if err != nil {
		return catalogSubmission{}, err
	}
	return catalogSubmission{
		EntityType: entityType, EntityID: entityID, RequesterUserID: requesterUserID,
		Status: catalogSubmissionStatusPendingReview, Snapshot: snapshot,
		CreatedAt: now, SubmittedAt: now, UpdatedAt: now,
	}, nil
}

func (s *trackStore) createAuthorSubmission(requesterUserID int64, request upsertAuthorRequest) (catalogSubmissionResponse, error) {
	var result catalogSubmissionResponse
	err := s.withinStateTransaction(stateAuthors, func(state *domainState) error {
		item, err := state.createAuthor(request)
		if err != nil {
			return err
		}
		item.PublicationStatus = catalogPublicationPendingReview
		item.RequestedByUserID = requestedByUserID(requesterUserID)
		state.authors[item.ID] = item
		if err := state.repositories.authors.Update(state.ctx, item); err != nil {
			return err
		}
		submission, err := newCatalogSubmission(catalogSubmissionEntityAuthor, item.ID, requesterUserID, item, time.Now().UTC())
		if err != nil {
			return err
		}
		submission, err = state.repositories.submissions.InsertSubmission(state.ctx, submission)
		if err != nil {
			return err
		}
		result = catalogSubmissionResponse{catalogSubmission: submission, Entity: item, Feedback: []catalogSubmissionFeedback{}}
		return nil
	})
	return result, err
}

func (s *trackStore) createAlbumSubmission(requesterUserID int64, request upsertAlbumRequest) (catalogSubmissionResponse, error) {
	if len(request.TrackIDs) != 0 {
		return catalogSubmissionResponse{}, fmt.Errorf("%w: trackIds must be empty when submitting a new album", errInvalidAlbum)
	}
	request.AuthorIDs = normalizeAuthorIDs(request.AuthorIDs)
	var result catalogSubmissionResponse
	err := s.withinStateTransaction(stateCatalog, func(state *domainState) error {
		if err := validateSubmissionAuthorReferences(state, requesterUserID, request.AuthorIDs); err != nil {
			return err
		}
		item, err := state.createAlbum(request)
		if err != nil {
			return err
		}
		item.AuthorIDs = append([]int64(nil), request.AuthorIDs...)
		item.PublicationStatus = catalogPublicationPendingReview
		item.RequestedByUserID = requestedByUserID(requesterUserID)
		state.albums[item.ID] = item
		if err := state.repositories.catalog.UpdateAlbum(state.ctx, item); err != nil {
			return err
		}
		submission, err := newCatalogSubmission(catalogSubmissionEntityAlbum, item.ID, requesterUserID, item, time.Now().UTC())
		if err != nil {
			return err
		}
		submission, err = state.repositories.submissions.InsertSubmission(state.ctx, submission)
		if err != nil {
			return err
		}
		result = catalogSubmissionResponse{catalogSubmission: submission, Entity: item, Feedback: []catalogSubmissionFeedback{}}
		return nil
	})
	return result, err
}

func (s *trackStore) createTrackSubmission(requesterUserID int64, request createTrackSubmissionRequest) (catalogSubmissionResponse, error) {
	request.AudioUploadToken = strings.TrimSpace(request.AudioUploadToken)
	if request.AudioUploadToken == "" {
		return catalogSubmissionResponse{}, fmt.Errorf("%w: audioUploadToken is required", errInvalidTrack)
	}
	var result catalogSubmissionResponse
	err := s.withinStateTransaction(stateCatalog, func(state *domainState) error {
		upload, ok, err := state.repositories.submissions.FindSubmissionUpload(state.ctx, request.AudioUploadToken)
		if err != nil {
			return err
		}
		if !ok || upload.RequesterUserID != requesterUserID {
			return errCatalogUploadNotFound
		}
		if upload.ClaimedTrackID != nil {
			return errCatalogUploadClaimed
		}
		if err := validateSubmissionTrackReferences(state, requesterUserID, request.AuthorIDs, request.AlbumID); err != nil {
			return err
		}
		item, err := state.create(upsertTrackRequest{
			Name: request.Name, AuthorIDs: request.AuthorIDs, AlbumID: request.AlbumID, AlbumOrder: request.AlbumOrder,
			AudioFilePath: "/api/catalog-submissions/tracks/pending/audio", AdditionalInfo: request.AdditionalInfo, SourceMetadata: request.SourceMetadata,
		})
		if err != nil {
			return err
		}
		item.AudioFilePath = fmt.Sprintf("/api/catalog-submissions/tracks/%d/audio", item.ID)
		item.PublicationStatus = catalogPublicationPendingReview
		item.RequestedByUserID = requestedByUserID(requesterUserID)
		state.tracks[item.ID] = item
		if err := state.repositories.catalog.UpdateTrack(state.ctx, item); err != nil {
			return err
		}
		if err := state.repositories.submissions.ClaimSubmissionUpload(state.ctx, request.AudioUploadToken, requesterUserID, item.ID); err != nil {
			return err
		}
		submission, err := newCatalogSubmission(catalogSubmissionEntityTrack, item.ID, requesterUserID, item, time.Now().UTC())
		if err != nil {
			return err
		}
		submission, err = state.repositories.submissions.InsertSubmission(state.ctx, submission)
		if err != nil {
			return err
		}
		result = catalogSubmissionResponse{catalogSubmission: submission, Entity: item, Feedback: []catalogSubmissionFeedback{}}
		return nil
	})
	return result, err
}

func validateSubmissionAuthorReferences(state *domainState, requesterUserID int64, authorIDs []int64) error {
	for _, authorID := range normalizeAuthorIDs(authorIDs) {
		item, ok := state.authors[authorID]
		if !ok || !catalogEntityVisible(item.PublicationStatus, item.RequestedByUserID, requesterUserID) {
			return fmt.Errorf("%w: authorId %d is not available", errInvalidAlbum, authorID)
		}
	}
	return nil
}

func validateSubmissionTrackReferences(state *domainState, requesterUserID int64, authorIDs []int64, albumID int64) error {
	if err := validateSubmissionAuthorReferences(state, requesterUserID, authorIDs); err != nil {
		return fmt.Errorf("%w: %v", errInvalidTrack, err)
	}
	albumItem, ok := state.albums[albumID]
	if !ok || !catalogEntityVisible(albumItem.PublicationStatus, albumItem.RequestedByUserID, requesterUserID) {
		return fmt.Errorf("%w: albumId %d is not available", errInvalidTrack, albumID)
	}
	return nil
}

func (s *trackStore) listCatalogSubmissions(requesterUserID int64) (catalogSubmissionListResponse, error) {
	var result catalogSubmissionListResponse
	err := s.withinReadTransaction(func(ctx context.Context, repositories domainRepositories) error {
		items, err := repositories.submissions.ListSubmissionsByRequester(ctx, requesterUserID)
		if err != nil {
			return err
		}
		responses, err := buildCatalogSubmissionResponses(ctx, repositories, items)
		if err != nil {
			return err
		}
		rating, err := repositories.submissions.ImportRating(ctx, requesterUserID)
		if err != nil {
			return err
		}
		result = catalogSubmissionListResponse{Items: responses, ImportRating: rating}
		return nil
	})
	return result, err
}

func buildCatalogSubmissionResponses(ctx context.Context, repositories domainRepositories, items []catalogSubmission) ([]catalogSubmissionResponse, error) {
	ids := make([]int64, len(items))
	for index, item := range items {
		ids[index] = item.ID
	}
	feedback, err := repositories.submissions.ListFeedbackBySubmissionIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	feedbackBySubmission := make(map[int64][]catalogSubmissionFeedback, len(items))
	for _, item := range feedback {
		feedbackBySubmission[item.SubmissionID] = append(feedbackBySubmission[item.SubmissionID], item)
	}
	result := make([]catalogSubmissionResponse, 0, len(items))
	for _, item := range items {
		entity, err := catalogSubmissionEntity(ctx, repositories, item)
		if err != nil {
			return nil, err
		}
		itemFeedback := feedbackBySubmission[item.ID]
		if itemFeedback == nil {
			itemFeedback = []catalogSubmissionFeedback{}
		}
		result = append(result, catalogSubmissionResponse{catalogSubmission: item, Entity: entity, Feedback: itemFeedback})
	}
	return result, nil
}

func catalogSubmissionEntity(ctx context.Context, repositories domainRepositories, submission catalogSubmission) (any, error) {
	switch submission.EntityType {
	case catalogSubmissionEntityAuthor:
		item, ok, err := repositories.authors.FindByID(ctx, submission.EntityID)
		if err != nil {
			return nil, err
		}
		if ok {
			return item, nil
		}
	case catalogSubmissionEntityAlbum:
		item, ok, err := repositories.catalog.FindAlbumByID(ctx, submission.EntityID)
		if err != nil {
			return nil, err
		}
		if ok {
			return item, nil
		}
	case catalogSubmissionEntityTrack:
		item, ok, err := repositories.catalog.FindTrackByID(ctx, submission.EntityID)
		if err != nil {
			return nil, err
		}
		if ok {
			return item, nil
		}
	default:
		return nil, fmt.Errorf("unsupported catalog submission entity type %q", submission.EntityType)
	}
	return submission.Snapshot, nil
}

func (s *trackStore) updateAuthorSubmission(requesterUserID, authorID int64, request upsertAuthorRequest) (catalogSubmissionResponse, error) {
	var result catalogSubmissionResponse
	err := s.withinStateTransaction(stateAuthors, func(state *domainState) error {
		submission, ok, err := state.repositories.submissions.FindByEntity(state.ctx, catalogSubmissionEntityAuthor, authorID)
		if err != nil {
			return err
		}
		if !ok {
			return errCatalogSubmissionNotFound
		}
		if err := validateSubmissionOwner(submission, requesterUserID); err != nil {
			return err
		}
		if err := validateSubmissionEditable(submission); err != nil {
			return err
		}
		item, found, err := state.updateAuthor(authorID, request)
		if err != nil {
			return err
		}
		if !found {
			return errCatalogSubmissionNotFound
		}
		return updateSubmissionSnapshot(state.ctx, state.repositories, &submission, item, &result)
	})
	return result, err
}

func (s *trackStore) updateAlbumSubmission(requesterUserID, albumID int64, request upsertAlbumRequest) (catalogSubmissionResponse, error) {
	request.AuthorIDs = normalizeAuthorIDs(request.AuthorIDs)
	var result catalogSubmissionResponse
	err := s.withinStateTransaction(stateCatalog, func(state *domainState) error {
		submission, ok, err := state.repositories.submissions.FindByEntity(state.ctx, catalogSubmissionEntityAlbum, albumID)
		if err != nil {
			return err
		}
		if !ok {
			return errCatalogSubmissionNotFound
		}
		if err := validateSubmissionOwner(submission, requesterUserID); err != nil {
			return err
		}
		if err := validateSubmissionEditable(submission); err != nil {
			return err
		}
		if err := validateSubmissionAuthorReferences(state, requesterUserID, request.AuthorIDs); err != nil {
			return err
		}
		current, exists := state.albums[albumID]
		if !exists {
			return errCatalogSubmissionNotFound
		}
		request.TrackIDs = append([]int64(nil), current.TrackIDs...)
		item, found, err := state.updateAlbum(albumID, request)
		if err != nil {
			return err
		}
		if !found {
			return errCatalogSubmissionNotFound
		}
		if len(item.TrackIDs) == 0 {
			item.AuthorIDs = append([]int64(nil), request.AuthorIDs...)
			state.albums[item.ID] = item
			if err := state.repositories.catalog.UpdateAlbum(state.ctx, item); err != nil {
				return err
			}
		}
		return updateSubmissionSnapshot(state.ctx, state.repositories, &submission, item, &result)
	})
	return result, err
}

func (s *trackStore) updateTrackSubmission(requesterUserID, trackID int64, request updateTrackSubmissionRequest) (catalogSubmissionResponse, error) {
	var result catalogSubmissionResponse
	var oldStagedFile string
	err := s.withinStateTransaction(stateCatalog, func(state *domainState) error {
		submission, ok, err := state.repositories.submissions.FindByEntity(state.ctx, catalogSubmissionEntityTrack, trackID)
		if err != nil {
			return err
		}
		if !ok {
			return errCatalogSubmissionNotFound
		}
		if err := validateSubmissionOwner(submission, requesterUserID); err != nil {
			return err
		}
		if err := validateSubmissionEditable(submission); err != nil {
			return err
		}
		current, exists := state.tracks[trackID]
		if !exists {
			return errCatalogSubmissionNotFound
		}
		if err := validateSubmissionTrackReferences(state, requesterUserID, request.AuthorIDs, request.AlbumID); err != nil {
			return err
		}
		if token := strings.TrimSpace(request.AudioUploadToken); token != "" {
			newUpload, ok, err := state.repositories.submissions.FindSubmissionUpload(state.ctx, token)
			if err != nil {
				return err
			}
			if !ok || newUpload.RequesterUserID != requesterUserID {
				return errCatalogUploadNotFound
			}
			if newUpload.ClaimedTrackID != nil {
				return errCatalogUploadClaimed
			}
			oldUpload, ok, err := state.repositories.submissions.FindSubmissionUploadByTrack(state.ctx, trackID)
			if err != nil {
				return err
			}
			if ok {
				oldStagedFile = oldUpload.StoredFileName
				if err := state.repositories.submissions.DeleteSubmissionUpload(state.ctx, oldUpload.Token); err != nil {
					return err
				}
			}
			if err := state.repositories.submissions.ClaimSubmissionUpload(state.ctx, token, requesterUserID, trackID); err != nil {
				return err
			}
		}
		item, found, err := state.update(trackID, upsertTrackRequest{
			Name: request.Name, AuthorIDs: request.AuthorIDs, AlbumID: request.AlbumID, AlbumOrder: request.AlbumOrder,
			AudioFilePath: current.AudioFilePath, AdditionalInfo: request.AdditionalInfo, SourceMetadata: request.SourceMetadata,
		})
		if err != nil {
			return err
		}
		if !found {
			return errCatalogSubmissionNotFound
		}
		return updateSubmissionSnapshot(state.ctx, state.repositories, &submission, item, &result)
	})
	if err == nil && oldStagedFile != "" {
		if cleanupErr := removeFileForCleanup(s.catalogSubmissionAudioPath(oldStagedFile)); cleanupErr != nil {
			return catalogSubmissionResponse{}, fmt.Errorf("remove replaced staged audio: %w", cleanupErr)
		}
	}
	return result, err
}

func updateSubmissionSnapshot(ctx context.Context, repositories domainRepositories, submission *catalogSubmission, entity any, result *catalogSubmissionResponse) error {
	snapshot, err := catalogSubmissionSnapshot(entity)
	if err != nil {
		return err
	}
	submission.Snapshot = snapshot
	submission.UpdatedAt = time.Now().UTC()
	if err := repositories.submissions.UpdateSubmission(ctx, *submission); err != nil {
		return err
	}
	feedback, err := repositories.submissions.ListFeedbackBySubmissionIDs(ctx, []int64{submission.ID})
	if err != nil {
		return err
	}
	*result = catalogSubmissionResponse{catalogSubmission: *submission, Entity: entity, Feedback: feedback}
	return nil
}

func (s *trackStore) resubmitCatalogSubmission(requesterUserID, submissionID int64) (catalogSubmissionResponse, error) {
	var result catalogSubmissionResponse
	err := s.unitOfWork.WithinTransaction(context.Background(), func(repositories domainRepositories) error {
		item, ok, err := repositories.submissions.FindSubmissionByID(context.Background(), submissionID)
		if err != nil {
			return err
		}
		if !ok {
			return errCatalogSubmissionNotFound
		}
		if err := validateSubmissionOwner(item, requesterUserID); err != nil {
			return err
		}
		if err := validateSubmissionEditable(item); err != nil {
			return err
		}
		if err := ensureSubmissionDependenciesReady(context.Background(), repositories, item); err != nil {
			return err
		}
		entity, err := setCatalogEntityPublication(context.Background(), repositories, item.EntityType, item.EntityID, catalogPublicationPendingReview, requestedByUserID(requesterUserID))
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		item.Status = catalogSubmissionStatusPendingReview
		item.SubmittedAt = now
		item.UpdatedAt = now
		item.DecidedAt = nil
		item.DecidedByUserID = nil
		item.Snapshot, err = catalogSubmissionSnapshot(entity)
		if err != nil {
			return err
		}
		if err := repositories.submissions.UpdateSubmission(context.Background(), item); err != nil {
			return err
		}
		feedback, err := repositories.submissions.ListFeedbackBySubmissionIDs(context.Background(), []int64{item.ID})
		if err != nil {
			return err
		}
		result = catalogSubmissionResponse{catalogSubmission: item, Entity: entity, Feedback: feedback}
		return nil
	})
	return result, err
}

func ensureSubmissionDependenciesReady(ctx context.Context, repositories domainRepositories, submission catalogSubmission) error {
	if submission.EntityType != catalogSubmissionEntityTrack {
		return nil
	}
	item, ok, err := repositories.catalog.FindTrackByID(ctx, submission.EntityID)
	if err != nil {
		return err
	}
	if !ok {
		return errCatalogSubmissionNotFound
	}
	albumItem, ok, err := repositories.catalog.FindAlbumByID(ctx, item.AlbumID)
	if err != nil || !ok {
		return errCatalogSubmissionDependency
	}
	if normalizePublicationStatus(albumItem.PublicationStatus) == catalogPublicationChangesRequested ||
		normalizePublicationStatus(albumItem.PublicationStatus) == catalogPublicationRejected {
		return errCatalogSubmissionDependency
	}
	for _, authorID := range item.AuthorIDs {
		authorItem, ok, err := repositories.authors.FindByID(ctx, authorID)
		if err != nil || !ok {
			return errCatalogSubmissionDependency
		}
		if normalizePublicationStatus(authorItem.PublicationStatus) == catalogPublicationChangesRequested ||
			normalizePublicationStatus(authorItem.PublicationStatus) == catalogPublicationRejected {
			return errCatalogSubmissionDependency
		}
	}
	return nil
}

func setCatalogEntityPublication(ctx context.Context, repositories domainRepositories, entityType string, entityID int64, status string, requesterUserID *int64) (any, error) {
	switch entityType {
	case catalogSubmissionEntityAuthor:
		item, ok, err := repositories.authors.FindByID(ctx, entityID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errCatalogSubmissionNotFound
		}
		item.PublicationStatus, item.RequestedByUserID = status, requesterUserID
		return item, repositories.authors.Update(ctx, item)
	case catalogSubmissionEntityAlbum:
		item, ok, err := repositories.catalog.FindAlbumByID(ctx, entityID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errCatalogSubmissionNotFound
		}
		item.PublicationStatus, item.RequestedByUserID = status, requesterUserID
		return item, repositories.catalog.UpdateAlbum(ctx, item)
	case catalogSubmissionEntityTrack:
		item, ok, err := repositories.catalog.FindTrackByID(ctx, entityID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errCatalogSubmissionNotFound
		}
		item.PublicationStatus, item.RequestedByUserID = status, requesterUserID
		return item, repositories.catalog.UpdateTrack(ctx, item)
	default:
		return nil, fmt.Errorf("unsupported catalog entity type %q", entityType)
	}
}

func (s *trackStore) listCatalogReviewRequesters() ([]catalogReviewRequester, error) {
	var result []catalogReviewRequester
	err := s.withinReadTransaction(func(ctx context.Context, repositories domainRepositories) error {
		items, err := repositories.submissions.ListReviewRequesters(ctx, time.Now().UTC())
		if err != nil {
			return err
		}
		result = make([]catalogReviewRequester, 0, len(items))
		for _, item := range items {
			var activeLease *catalogReviewLeaseStatus
			if item.Lease != nil {
				activeLease = &catalogReviewLeaseStatus{
					ReviewerUserID: item.Lease.ReviewerUserID,
					AcquiredAt:     item.Lease.AcquiredAt, HeartbeatAt: item.Lease.HeartbeatAt, ExpiresAt: item.Lease.ExpiresAt,
				}
			}
			result = append(result, catalogReviewRequester{
				User:         catalogReviewUser{ID: item.RequesterID, Email: item.Email},
				PendingCount: item.PendingCount, ImportRating: item.ImportRating,
				ActiveLease: activeLease, OldestPending: item.OldestPending,
			})
		}
		return nil
	})
	return result, err
}

func (s *trackStore) acquireCatalogReviewLease(reviewerUserID, requesterUserID int64) (catalogReviewLease, error) {
	if reviewerUserID == requesterUserID {
		return catalogReviewLease{}, errCatalogSelfReview
	}
	var result catalogReviewLease
	err := s.unitOfWork.WithinTransaction(context.Background(), func(repositories domainRepositories) error {
		now := time.Now().UTC()
		if err := repositories.submissions.DeleteExpiredReviewLeases(context.Background(), now); err != nil {
			return err
		}
		pending, err := repositories.submissions.ListPendingSubmissionsByRequester(context.Background(), requesterUserID)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return errCatalogSubmissionNotFound
		}
		if existing, exists, err := repositories.submissions.FindReviewLeaseByRequester(context.Background(), requesterUserID); err != nil {
			return err
		} else if exists {
			if existing.ReviewerUserID != reviewerUserID {
				return errCatalogReviewLeaseConflict
			}
			token, err := randomToken(24)
			if err != nil {
				return err
			}
			oldToken := existing.LeaseToken
			existing.LeaseToken = token
			existing.HeartbeatAt = now
			existing.ExpiresAt = now.Add(catalogReviewLeaseTTL)
			if err := repositories.submissions.RotateReviewLease(context.Background(), existing, oldToken, now); err != nil {
				return err
			}
			result = existing
			return nil
		}
		if _, exists, err := repositories.submissions.FindReviewLeaseByReviewer(context.Background(), reviewerUserID); err != nil {
			return err
		} else if exists {
			return errCatalogReviewerAlreadyLeasing
		}
		token, err := randomToken(24)
		if err != nil {
			return err
		}
		result = catalogReviewLease{
			RequesterUserID: requesterUserID, ReviewerUserID: reviewerUserID, LeaseToken: token,
			AcquiredAt: now, HeartbeatAt: now, ExpiresAt: now.Add(catalogReviewLeaseTTL),
		}
		return repositories.submissions.InsertReviewLease(context.Background(), result)
	})
	return result, err
}

func (s *trackStore) renewCatalogReviewLease(reviewerUserID, requesterUserID int64, token string) (catalogReviewLease, error) {
	var result catalogReviewLease
	err := s.unitOfWork.WithinTransaction(context.Background(), func(repositories domainRepositories) error {
		now := time.Now().UTC()
		item, ok, err := repositories.submissions.FindReviewLeaseByRequester(context.Background(), requesterUserID)
		if err != nil {
			return err
		}
		if !ok || item.ReviewerUserID != reviewerUserID || item.LeaseToken != strings.TrimSpace(token) || !item.ExpiresAt.After(now) {
			return errCatalogReviewLeaseInvalid
		}
		item.HeartbeatAt = now
		item.ExpiresAt = now.Add(catalogReviewLeaseTTL)
		if err := repositories.submissions.UpdateReviewLease(context.Background(), item, now); err != nil {
			return err
		}
		result = item
		return nil
	})
	return result, err
}

func (s *trackStore) releaseCatalogReviewLease(reviewerUserID, requesterUserID int64, token string) error {
	return s.unitOfWork.WithinTransaction(context.Background(), func(repositories domainRepositories) error {
		return repositories.submissions.DeleteReviewLease(context.Background(), requesterUserID, reviewerUserID, strings.TrimSpace(token), time.Now().UTC())
	})
}

func validateCatalogReviewLease(ctx context.Context, repositories domainRepositories, reviewerUserID, requesterUserID int64, token string, now time.Time) (catalogReviewLease, error) {
	if reviewerUserID == requesterUserID {
		return catalogReviewLease{}, errCatalogSelfReview
	}
	item, ok, err := repositories.submissions.FindReviewLeaseByRequester(ctx, requesterUserID)
	if err != nil {
		return catalogReviewLease{}, err
	}
	if !ok || item.ReviewerUserID != reviewerUserID || item.LeaseToken != strings.TrimSpace(token) || !item.ExpiresAt.After(now) {
		return catalogReviewLease{}, errCatalogReviewLeaseInvalid
	}
	return item, nil
}

func (s *trackStore) listCatalogReviewSubmissions(reviewerUserID, requesterUserID int64, token string) ([]catalogSubmissionResponse, error) {
	var result []catalogSubmissionResponse
	err := s.withinReadTransaction(func(ctx context.Context, repositories domainRepositories) error {
		if _, err := validateCatalogReviewLease(ctx, repositories, reviewerUserID, requesterUserID, token, time.Now().UTC()); err != nil {
			return err
		}
		items, err := repositories.submissions.ListReviewSubmissionsByRequester(ctx, requesterUserID)
		if err != nil {
			return err
		}
		result, err = buildCatalogSubmissionResponses(ctx, repositories, items)
		return err
	})
	return result, err
}

func (s *trackStore) requestCatalogSubmissionChanges(reviewerUserID, submissionID int64, leaseToken string, request catalogReviewDecisionRequest) (catalogSubmissionResponse, error) {
	request, err := normalizeCatalogDecision(request, true)
	if err != nil {
		return catalogSubmissionResponse{}, err
	}
	var result catalogSubmissionResponse
	err = s.unitOfWork.WithinTransaction(context.Background(), func(repositories domainRepositories) error {
		ctx := context.Background()
		item, ok, err := repositories.submissions.FindSubmissionByID(ctx, submissionID)
		if err != nil {
			return err
		}
		if !ok {
			return errCatalogSubmissionNotFound
		}
		now := time.Now().UTC()
		lease, err := validateCatalogReviewLease(ctx, repositories, reviewerUserID, item.RequesterUserID, leaseToken, now)
		if err != nil {
			return err
		}
		if item.Status != catalogSubmissionStatusPendingReview {
			return errCatalogSubmissionState
		}
		if item.EntityType == catalogSubmissionEntityTrack {
			if err := checkCatalogDependencies(ctx, repositories, item.EntityID, false); err != nil {
				return err
			}
		}
		entity, err := setCatalogEntityPublication(ctx, repositories, item.EntityType, item.EntityID, catalogPublicationChangesRequested, requestedByUserID(item.RequesterUserID))
		if err != nil {
			return err
		}
		feedback, err := repositories.submissions.InsertFeedback(ctx, catalogSubmissionFeedback{
			SubmissionID: item.ID, ReviewerUserID: requestedByUserID(reviewerUserID), Kind: catalogFeedbackChangesRequested,
			Message: request.Message, RatingPenalty: request.RatingPenalty, CreatedAt: now,
		})
		if err != nil {
			return err
		}
		if err := repositories.submissions.InsertRatingEvent(ctx, item.RequesterUserID, item.ID, requestedByUserID(reviewerUserID),
			fmt.Sprintf("feedback:%d:penalty", feedback.ID), "review_penalty", -request.RatingPenalty, request.Message, now); err != nil {
			return err
		}
		item.Status = catalogSubmissionStatusChangesRequested
		item.UpdatedAt = now
		item.Snapshot, err = catalogSubmissionSnapshot(entity)
		if err != nil {
			return err
		}
		if err := repositories.submissions.UpdateSubmission(ctx, item); err != nil {
			return err
		}
		lease.HeartbeatAt, lease.ExpiresAt = now, now.Add(catalogReviewLeaseTTL)
		if err := repositories.submissions.UpdateReviewLease(ctx, lease, now); err != nil {
			return err
		}
		allFeedback, err := repositories.submissions.ListFeedbackBySubmissionIDs(ctx, []int64{item.ID})
		if err != nil {
			return err
		}
		result = catalogSubmissionResponse{catalogSubmission: item, Entity: entity, Feedback: allFeedback}
		return nil
	})
	return result, err
}

func (s *trackStore) approveCatalogSubmission(reviewerUserID, submissionID int64, leaseToken string) (catalogSubmissionResponse, error) {
	songsMutationMu.Lock()
	defer songsMutationMu.Unlock()
	var result catalogSubmissionResponse
	var movedFrom, movedTo string
	err := s.unitOfWork.WithinTransaction(context.Background(), func(repositories domainRepositories) error {
		ctx := context.Background()
		item, ok, err := repositories.submissions.FindSubmissionByID(ctx, submissionID)
		if err != nil {
			return err
		}
		if !ok {
			return errCatalogSubmissionNotFound
		}
		now := time.Now().UTC()
		lease, err := validateCatalogReviewLease(ctx, repositories, reviewerUserID, item.RequesterUserID, leaseToken, now)
		if err != nil {
			return err
		}
		if item.Status != catalogSubmissionStatusPendingReview {
			return errCatalogSubmissionState
		}

		if item.EntityType == catalogSubmissionEntityTrack {
			trackItem, ok, err := repositories.catalog.FindTrackByID(ctx, item.EntityID)
			if err != nil {
				return err
			}
			if !ok {
				return errCatalogSubmissionNotFound
			}
			if err := checkCatalogDependencies(ctx, repositories, item.EntityID, true); err != nil {
				return err
			}
			upload, ok, err := repositories.submissions.FindSubmissionUploadByTrack(ctx, trackItem.ID)
			if err != nil {
				return err
			}
			if !ok {
				return errCatalogUploadNotFound
			}
			publishedName, sourcePath, targetPath, err := s.publishStagedAudioToLibrary(upload)
			if err != nil {
				return err
			}
			movedFrom, movedTo = sourcePath, targetPath
			trackItem.AudioFilePath = "/api/songs/" + url.PathEscape(publishedName)
			trackItem.PublicationStatus = catalogPublicationPublished
			trackItem.RequestedByUserID = nil
			if err := repositories.catalog.UpdateTrack(ctx, trackItem); err != nil {
				return err
			}
			if err := repositories.submissions.DeleteSubmissionUpload(ctx, upload.Token); err != nil {
				return err
			}
			if err := repositories.submissions.InsertRatingEvent(ctx, item.RequesterUserID, item.ID, requestedByUserID(reviewerUserID),
				fmt.Sprintf("submission:%d:track-approved", item.ID), "track_approved", 10, "track approved", now); err != nil {
				return err
			}
			if _, hasLyrics, err := repositories.lyrics.FindByTrackID(ctx, trackItem.ID); err != nil {
				return err
			} else if hasLyrics {
				if err := repositories.submissions.InsertRatingEvent(ctx, item.RequesterUserID, item.ID, requestedByUserID(reviewerUserID),
					fmt.Sprintf("submission:%d:lyrics-bonus", item.ID), "lyrics_bonus", 5, "approved track includes lyrics", now); err != nil {
					return err
				}
			}
			result.Entity = trackItem
		} else {
			if item.EntityType == catalogSubmissionEntityAlbum {
				albumItem, ok, err := repositories.catalog.FindAlbumByID(ctx, item.EntityID)
				if err != nil {
					return err
				}
				if !ok {
					return errCatalogSubmissionNotFound
				}
				for _, authorID := range albumItem.AuthorIDs {
					if err := checkCatalogDependency(ctx, repositories, catalogSubmissionEntityAuthor, authorID, true); err != nil {
						return err
					}
				}
			}
			result.Entity, err = setCatalogEntityPublication(ctx, repositories, item.EntityType, item.EntityID, catalogPublicationPublished, nil)
			if err != nil {
				return err
			}
		}

		item.Status = catalogSubmissionStatusApproved
		item.UpdatedAt = now
		item.DecidedAt = &now
		item.DecidedByUserID = requestedByUserID(reviewerUserID)
		item.Snapshot, err = catalogSubmissionSnapshot(result.Entity)
		if err != nil {
			return err
		}
		if err := repositories.submissions.UpdateSubmission(ctx, item); err != nil {
			return err
		}
		lease.HeartbeatAt, lease.ExpiresAt = now, now.Add(catalogReviewLeaseTTL)
		if err := repositories.submissions.UpdateReviewLease(ctx, lease, now); err != nil {
			return err
		}
		feedback, err := repositories.submissions.ListFeedbackBySubmissionIDs(ctx, []int64{item.ID})
		if err != nil {
			return err
		}
		result.catalogSubmission, result.Feedback = item, feedback
		return nil
	})
	if err != nil && movedFrom != "" && movedTo != "" {
		rollbackErr := os.Remove(movedTo)
		if rollbackErr != nil {
			err = errors.Join(err, fmt.Errorf("remove unpublished audio after failed approval: %w", rollbackErr))
		}
	}
	if err == nil && movedFrom != "" {
		if cleanupErr := removeFileForCleanup(movedFrom); cleanupErr != nil {
			log.Printf("catalog submission approval left staged audio for startup cleanup: %s", safeOperationalError(cleanupErr))
		}
	}
	return result, err
}

func checkCatalogDependency(ctx context.Context, repositories domainRepositories, entityType string, entityID int64, requirePublished bool) error {
	entity, err := catalogEntityByType(ctx, repositories, entityType, entityID)
	if errors.Is(err, errCatalogSubmissionNotFound) {
		return errCatalogRejectedDependency
	}
	if err != nil {
		return err
	}
	status, _ := catalogEntityLifecycle(entity)
	if status == catalogPublicationRejected {
		return errCatalogRejectedDependency
	}
	if requirePublished && status != catalogPublicationPublished {
		return errCatalogTrackReviewPrerequisite
	}
	return nil
}

func checkCatalogDependencies(ctx context.Context, repositories domainRepositories, trackID int64, requirePublished bool) error {
	item, ok, err := repositories.catalog.FindTrackByID(ctx, trackID)
	if err != nil {
		return err
	}
	if !ok {
		return errCatalogSubmissionNotFound
	}
	var pendingError error
	for _, authorID := range item.AuthorIDs {
		if err := checkCatalogDependency(ctx, repositories, catalogSubmissionEntityAuthor, authorID, requirePublished); err != nil {
			if !errors.Is(err, errCatalogTrackReviewPrerequisite) {
				return err
			}
			pendingError = err
		}
	}
	if err := checkCatalogDependency(ctx, repositories, catalogSubmissionEntityAlbum, item.AlbumID, requirePublished); err != nil {
		return err
	}
	return pendingError
}

func catalogEntityByType(ctx context.Context, repositories domainRepositories, entityType string, entityID int64) (any, error) {
	switch entityType {
	case catalogSubmissionEntityAuthor:
		item, ok, err := repositories.authors.FindByID(ctx, entityID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errCatalogSubmissionNotFound
		}
		return item, nil
	case catalogSubmissionEntityAlbum:
		item, ok, err := repositories.catalog.FindAlbumByID(ctx, entityID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errCatalogSubmissionNotFound
		}
		return item, nil
	case catalogSubmissionEntityTrack:
		item, ok, err := repositories.catalog.FindTrackByID(ctx, entityID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errCatalogSubmissionNotFound
		}
		return item, nil
	default:
		return nil, errCatalogSubmissionNotFound
	}
}

func catalogEntityLifecycle(entity any) (string, *int64) {
	switch item := entity.(type) {
	case author:
		return normalizePublicationStatus(item.PublicationStatus), item.RequestedByUserID
	case album:
		return normalizePublicationStatus(item.PublicationStatus), item.RequestedByUserID
	case track:
		return normalizePublicationStatus(item.PublicationStatus), item.RequestedByUserID
	default:
		return "", nil
	}
}

func (s *trackStore) publishStagedAudioToLibrary(upload catalogSubmissionUpload) (string, string, string, error) {
	if strings.TrimSpace(s.songsDir) == "" {
		return "", "", "", errors.New("songs directory is not configured")
	}
	sourcePath := s.catalogSubmissionAudioPath(upload.StoredFileName)
	for attempt := 0; attempt < 10; attempt++ {
		name, err := randomizedStoredFileName(upload.OriginalName)
		if err != nil {
			return "", "", "", err
		}
		targetPath := filepath.Join(s.songsDir, name)
		if _, err := os.Lstat(targetPath); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", "", "", err
		}
		if err := os.Link(sourcePath, targetPath); err != nil {
			return "", "", "", fmt.Errorf("publish staged audio: %w", err)
		}
		return name, sourcePath, targetPath, nil
	}
	return "", "", "", errors.New("could not allocate a unique published audio filename")
}

func (s *trackStore) rejectCatalogSubmission(reviewerUserID, submissionID int64, leaseToken string, request catalogReviewDecisionRequest) (catalogSubmissionResponse, error) {
	request, err := normalizeCatalogDecision(request, true)
	if err != nil {
		return catalogSubmissionResponse{}, err
	}
	var result catalogSubmissionResponse
	var stagedFile string
	err = s.unitOfWork.WithinTransaction(context.Background(), func(repositories domainRepositories) error {
		ctx := context.Background()
		item, ok, err := repositories.submissions.FindSubmissionByID(ctx, submissionID)
		if err != nil {
			return err
		}
		if !ok {
			return errCatalogSubmissionNotFound
		}
		now := time.Now().UTC()
		lease, err := validateCatalogReviewLease(ctx, repositories, reviewerUserID, item.RequesterUserID, leaseToken, now)
		if err != nil {
			return err
		}
		if item.Status != catalogSubmissionStatusPendingReview {
			return errCatalogSubmissionState
		}
		if item.EntityType == catalogSubmissionEntityTrack {
			if err := checkCatalogDependencies(ctx, repositories, item.EntityID, false); err != nil {
				return err
			}
		}
		entity, err := catalogEntityByType(ctx, repositories, item.EntityType, item.EntityID)
		if err != nil {
			return err
		}
		feedback, err := repositories.submissions.InsertFeedback(ctx, catalogSubmissionFeedback{
			SubmissionID: item.ID, ReviewerUserID: requestedByUserID(reviewerUserID), Kind: catalogFeedbackRejected,
			Message: request.Message, RatingPenalty: request.RatingPenalty, CreatedAt: now,
		})
		if err != nil {
			return err
		}
		if err := repositories.submissions.InsertRatingEvent(ctx, item.RequesterUserID, item.ID, requestedByUserID(reviewerUserID),
			fmt.Sprintf("feedback:%d:penalty", feedback.ID), "review_penalty", -request.RatingPenalty, request.Message, now); err != nil {
			return err
		}
		stagedFile, err = deleteCatalogSubmissionEntity(ctx, repositories, item.EntityType, item.EntityID)
		if errors.Is(err, errCatalogSubmissionDependency) && item.EntityType != catalogSubmissionEntityTrack {
			// Keep referenced entities so pending tracks can be corrected or cancelled.
			_, err = setCatalogEntityPublication(ctx, repositories, item.EntityType, item.EntityID, catalogPublicationRejected, requestedByUserID(item.RequesterUserID))
		}
		if err != nil {
			return err
		}
		item.Status = catalogSubmissionStatusRejected
		item.UpdatedAt = now
		item.DecidedAt = &now
		item.DecidedByUserID = requestedByUserID(reviewerUserID)
		item.Snapshot, err = catalogSubmissionSnapshot(entity)
		if err != nil {
			return err
		}
		if err := repositories.submissions.UpdateSubmission(ctx, item); err != nil {
			return err
		}
		lease.HeartbeatAt, lease.ExpiresAt = now, now.Add(catalogReviewLeaseTTL)
		if err := repositories.submissions.UpdateReviewLease(ctx, lease, now); err != nil {
			return err
		}
		allFeedback, err := repositories.submissions.ListFeedbackBySubmissionIDs(ctx, []int64{item.ID})
		if err != nil {
			return err
		}
		result = catalogSubmissionResponse{catalogSubmission: item, Entity: item.Snapshot, Feedback: allFeedback}
		return nil
	})
	if err == nil && stagedFile != "" {
		if cleanupErr := removeFileForCleanup(s.catalogSubmissionAudioPath(stagedFile)); cleanupErr != nil {
			return catalogSubmissionResponse{}, fmt.Errorf("remove rejected staged audio: %w", cleanupErr)
		}
	}
	return result, err
}

func (s *trackStore) cancelCatalogSubmission(requesterUserID, submissionID int64) (catalogSubmissionResponse, error) {
	var result catalogSubmissionResponse
	var stagedFile string
	err := s.unitOfWork.WithinTransaction(context.Background(), func(repositories domainRepositories) error {
		ctx := context.Background()
		item, ok, err := repositories.submissions.FindSubmissionByID(ctx, submissionID)
		if err != nil {
			return err
		}
		if !ok {
			return errCatalogSubmissionNotFound
		}
		if err := validateSubmissionOwner(item, requesterUserID); err != nil {
			return err
		}
		if item.Status != catalogSubmissionStatusPendingReview && item.Status != catalogSubmissionStatusChangesRequested {
			return errCatalogSubmissionState
		}
		entity, err := catalogEntityByType(ctx, repositories, item.EntityType, item.EntityID)
		if err != nil {
			return err
		}
		stagedFile, err = deleteCatalogSubmissionEntity(ctx, repositories, item.EntityType, item.EntityID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		item.Status = catalogSubmissionStatusCancelled
		item.UpdatedAt = now
		item.DecidedAt = &now
		item.Snapshot, err = catalogSubmissionSnapshot(entity)
		if err != nil {
			return err
		}
		if err := repositories.submissions.UpdateSubmission(ctx, item); err != nil {
			return err
		}
		feedback, err := repositories.submissions.ListFeedbackBySubmissionIDs(ctx, []int64{item.ID})
		if err != nil {
			return err
		}
		result = catalogSubmissionResponse{catalogSubmission: item, Entity: item.Snapshot, Feedback: feedback}
		return nil
	})
	if err == nil && stagedFile != "" {
		if cleanupErr := removeFileForCleanup(s.catalogSubmissionAudioPath(stagedFile)); cleanupErr != nil {
			return catalogSubmissionResponse{}, fmt.Errorf("remove cancelled staged audio: %w", cleanupErr)
		}
	}
	return result, err
}

func deleteCatalogSubmissionEntity(ctx context.Context, repositories domainRepositories, entityType string, entityID int64) (string, error) {
	switch entityType {
	case catalogSubmissionEntityTrack:
		item, ok, err := repositories.catalog.FindTrackByID(ctx, entityID)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", errCatalogSubmissionNotFound
		}
		upload, _, err := repositories.submissions.FindSubmissionUploadByTrack(ctx, entityID)
		if err != nil {
			return "", err
		}
		albumItem, albumFound, err := repositories.catalog.FindAlbumByID(ctx, item.AlbumID)
		if err != nil {
			return "", err
		}
		if albumFound {
			albumItem.TrackIDs = removeInt64(albumItem.TrackIDs, entityID)
			remaining, err := repositories.reads.ListTracksByIDs(ctx, albumItem.TrackIDs)
			if err != nil {
				return "", err
			}
			authorSet := make(map[int64]struct{})
			for _, trackItem := range remaining {
				for _, authorID := range trackItem.AuthorIDs {
					authorSet[authorID] = struct{}{}
				}
			}
			albumItem.AuthorIDs = setToSortedIDs(authorSet)
			if err := repositories.catalog.UpdateAlbum(ctx, albumItem); err != nil {
				return "", err
			}
		}
		playlists, err := repositories.playlists.List(ctx)
		if err != nil {
			return "", err
		}
		for _, playlistItem := range playlists {
			updated := removePlaylistTrack(playlistItem.TrackItems, entityID)
			if len(updated) == len(playlistItem.TrackItems) {
				continue
			}
			playlistItem.TrackItems = updated
			if err := repositories.playlists.Update(ctx, playlistItem); err != nil {
				return "", err
			}
		}
		if _, found, err := repositories.lyrics.FindByTrackID(ctx, entityID); err != nil {
			return "", err
		} else if found {
			if err := repositories.lyrics.DeleteByTrackID(ctx, entityID); err != nil {
				return "", err
			}
		}
		if err := repositories.catalog.DeleteTrack(ctx, entityID); err != nil {
			return "", err
		}
		return upload.StoredFileName, nil
	case catalogSubmissionEntityAlbum:
		item, ok, err := repositories.catalog.FindAlbumByID(ctx, entityID)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", errCatalogSubmissionNotFound
		}
		if len(item.TrackIDs) != 0 {
			return "", errCatalogSubmissionDependency
		}
		return "", repositories.catalog.DeleteAlbum(ctx, entityID)
	case catalogSubmissionEntityAuthor:
		tracks, err := repositories.catalog.ListTracks(ctx)
		if err != nil {
			return "", err
		}
		for _, item := range tracks {
			if containsInt64(item.AuthorIDs, entityID) {
				return "", errCatalogSubmissionDependency
			}
		}
		albums, err := repositories.catalog.ListAlbums(ctx)
		if err != nil {
			return "", err
		}
		for _, item := range albums {
			if containsInt64(item.AuthorIDs, entityID) {
				return "", errCatalogSubmissionDependency
			}
		}
		return "", repositories.authors.Delete(ctx, entityID)
	default:
		return "", errCatalogSubmissionNotFound
	}
}

func removeInt64(items []int64, target int64) []int64 {
	result := make([]int64, 0, len(items))
	for _, item := range items {
		if item != target {
			result = append(result, item)
		}
	}
	return result
}

func (s *trackStore) catalogSubmissionAudioDir() string {
	return filepath.Join(s.songsDir, ".catalog-submissions")
}

func (s *trackStore) catalogSubmissionAudioPath(storedFileName string) string {
	return filepath.Join(s.catalogSubmissionAudioDir(), storedFileName)
}

func (s *trackStore) cleanupCatalogSubmissionAudio() error {
	dir := s.catalogSubmissionAudioDir()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var referencedNames []string
	if err := s.withinReadTransaction(func(ctx context.Context, repositories domainRepositories) error {
		var readErr error
		referencedNames, readErr = repositories.submissions.ListSubmissionUploadFileNames(ctx)
		return readErr
	}); err != nil {
		return err
	}
	referenced := make(map[string]struct{}, len(referencedNames))
	for _, name := range referencedNames {
		referenced[name] = struct{}{}
	}
	songsMutationMu.Lock()
	defer songsMutationMu.Unlock()
	activeStagedFiles := make([]os.FileInfo, 0, len(referencedNames))
	for _, name := range referencedNames {
		info, err := os.Stat(filepath.Join(dir, name))
		if err == nil && info.Mode().IsRegular() {
			activeStagedFiles = append(activeStagedFiles, info)
			continue
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	publicEntries, err := os.ReadDir(s.songsDir)
	if err != nil {
		return err
	}
	for _, entry := range publicEntries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		for _, stagedInfo := range activeStagedFiles {
			if !os.SameFile(info, stagedInfo) {
				continue
			}
			if err := removeFileForCleanup(filepath.Join(s.songsDir, entry.Name())); err != nil {
				return err
			}
			break
		}
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		if _, ok := referenced[entry.Name()]; ok {
			continue
		}
		if err := removeFileForCleanup(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (s *trackStore) registerCatalogSubmissionUpload(requesterUserID int64, storedFileName, originalName string, sizeBytes int64) (catalogSubmissionUpload, error) {
	token, err := randomToken(24)
	if err != nil {
		return catalogSubmissionUpload{}, err
	}
	item := catalogSubmissionUpload{
		Token: token, RequesterUserID: requesterUserID, StoredFileName: storedFileName,
		OriginalName: originalName, CreatedAt: time.Now().UTC(), SizeBytes: sizeBytes,
	}
	err = s.unitOfWork.WithinTransaction(context.Background(), func(repositories domainRepositories) error {
		return repositories.submissions.InsertSubmissionUpload(context.Background(), item)
	})
	return item, err
}

func (s *trackStore) catalogSubmissionAudioAccess(userID, trackID int64, leaseToken string) (catalogSubmissionUpload, error) {
	var result catalogSubmissionUpload
	err := s.withinReadTransaction(func(ctx context.Context, repositories domainRepositories) error {
		submission, ok, err := repositories.submissions.FindByEntity(ctx, catalogSubmissionEntityTrack, trackID)
		if err != nil {
			return err
		}
		if !ok || (submission.Status != catalogSubmissionStatusPendingReview && submission.Status != catalogSubmissionStatusChangesRequested) {
			return errCatalogSubmissionNotFound
		}
		if submission.RequesterUserID != userID {
			allowed, err := repositories.access.UserHasPermission(ctx, userID, permissionCatalogSubmissionsReview)
			if err != nil {
				return err
			}
			if !allowed {
				return errCatalogSubmissionForbidden
			}
			if _, err := validateCatalogReviewLease(ctx, repositories, userID, submission.RequesterUserID, leaseToken, time.Now().UTC()); err != nil {
				return err
			}
		}
		upload, ok, err := repositories.submissions.FindSubmissionUploadByTrack(ctx, trackID)
		if err != nil {
			return err
		}
		if !ok {
			return errCatalogUploadNotFound
		}
		result = upload
		return nil
	})
	return result, err
}

func (s *trackStore) upsertCatalogSubmissionLyrics(requesterUserID, trackID int64, request upsertLyricsRequest) (lyrics, bool, error) {
	return s.upsertSubmissionLyricsContext(context.Background(), requesterUserID, trackID, request)
}
