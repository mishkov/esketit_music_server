package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"
)

const catalogSubmissionEntityTrackUpdate = "track_update"

// The catalog identity and publication lifecycle are immutable; proposals carry
// every editable field and retain both versions for review and audit history.
type trackUpdateState struct {
	track
	Lyrics *upsertLyricsRequest `json:"lyrics"`
}
type trackUpdatePatch struct {
	Name             *string           `json:"name,omitempty"`
	AuthorIDs        *[]int64          `json:"authorIds,omitempty"`
	AlbumID          *int64            `json:"albumId,omitempty"`
	TrackNumber      *int              `json:"trackNumber,omitempty"`
	AudioUploadToken *string           `json:"audioUploadToken,omitempty"`
	AdditionalInfo   *[]additionalInfo `json:"additionalInfo,omitempty"`
	SourceMetadata   *[]sourceMetadata `json:"sourceMetadata,omitempty"`
	Lyrics           json.RawMessage   `json:"lyrics,omitempty"`
	Reason           *string           `json:"reason,omitempty"`
}
type createTrackUpdateSubmissionRequest struct {
	TrackID int64 `json:"trackId"`
	trackUpdatePatch
}
type updateTrackUpdateSubmissionRequest struct {
	ExpectedRevision int64 `json:"expectedRevision"`
	RefreshBaseline  bool  `json:"refreshBaseline,omitempty"`
	trackUpdatePatch
}
type trackUpdateProposal struct {
	TrackID          int64            `json:"trackId"`
	Before           trackUpdateState `json:"before"`
	After            trackUpdateState `json:"after"`
	ChangedFields    []string         `json:"changedFields"`
	AudioUploadToken string           `json:"audioUploadToken,omitempty"`
	Reason           string           `json:"reason"`
}
type trackUpdateSubmissionView struct {
	trackUpdateProposal
	BeforeAuthorNames []string          `json:"beforeAuthorNames"`
	AfterAuthorNames  []string          `json:"afterAuthorNames"`
	BeforeAlbumTitle  string            `json:"beforeAlbumTitle"`
	AfterAlbumTitle   string            `json:"afterAlbumTitle"`
	Current           *trackUpdateState `json:"current"`
	TargetAvailable   bool              `json:"targetAvailable"`
	IsOutdated        bool              `json:"isOutdated"`
}

func migrateTrackUpdateSubmissions(ctx context.Context, tx *sql.Tx) error {
	return rebuildProposalSubmissions(ctx, tx, true, true)
}
func decodeTrackUpdateProposal(sub catalogSubmission) (p trackUpdateProposal, err error) {
	raw, err := json.Marshal(sub.Snapshot)
	if err == nil {
		err = json.Unmarshal(raw, &p)
	}
	return
}
func publishedTrackUpdateState(ctx context.Context, repos domainRepositories, id int64) (trackUpdateState, error) {
	t, err := publishedLyricsTarget(ctx, repos, id)
	if err != nil {
		return trackUpdateState{}, err
	}
	state := trackUpdateState{track: t}
	current, err := currentLyricsResponse(ctx, repos, id)
	if err != nil {
		return state, err
	}
	if current != nil {
		raw, _ := json.Marshal(current)
		state.Lyrics = &upsertLyricsRequest{}
		err = json.Unmarshal(raw, state.Lyrics)
	}
	return state, err
}
func trackUpdateChangedFields(before, after trackUpdateState) []string {
	a, _ := catalogSubmissionSnapshot(before)
	b, _ := catalogSubmissionSnapshot(after)
	fields := []string{}
	for _, key := range []string{"name", "authorIds", "albumId", "trackNumber", "audioFilePath", "additionalInfo", "sourceMetadata", "lyrics"} {
		if !reflect.DeepEqual(a[key], b[key]) {
			fields = append(fields, key)
		}
	}
	return fields
}
func rebaseTrackUpdate(p *trackUpdateProposal, current trackUpdateState) error {
	original, err := catalogSubmissionSnapshot(p.After)
	if err != nil {
		return err
	}
	rebased, err := catalogSubmissionSnapshot(current)
	if err != nil {
		return err
	}
	for _, key := range p.ChangedFields {
		rebased[key] = original[key]
	}
	raw, err := json.Marshal(rebased)
	if err != nil {
		return err
	}
	p.Before = current
	return json.Unmarshal(raw, &p.After)
}
func applyTrackUpdatePatch(p *trackUpdateProposal, patch trackUpdatePatch, subID int64) error {
	if patch.Name != nil {
		p.After.Name = strings.TrimSpace(*patch.Name)
	}
	if patch.AuthorIDs != nil {
		p.After.AuthorIDs = normalizeAuthorIDs(*patch.AuthorIDs)
	}
	if patch.AlbumID != nil {
		if *patch.AlbumID != p.After.AlbumID && patch.TrackNumber == nil {
			return fmt.Errorf("%w: specify trackNumber when moving albums", errInvalidTrack)
		}
		p.After.AlbumID = *patch.AlbumID
	}
	if patch.TrackNumber != nil {
		p.After.TrackNumber = *patch.TrackNumber
	}
	if patch.AdditionalInfo != nil {
		p.After.AdditionalInfo = normalizeAdditionalInfo(*patch.AdditionalInfo)
	}
	if patch.SourceMetadata != nil {
		p.After.SourceMetadata = normalizeSourceMetadata(*patch.SourceMetadata)
	}
	if patch.Reason != nil {
		p.Reason = strings.TrimSpace(*patch.Reason)
	}
	if utf8.RuneCountInString(p.Reason) > 1000 {
		return fmt.Errorf("%w: reason must be at most 1000 characters", errInvalidTrack)
	}
	if patch.Lyrics != nil {
		p.After.Lyrics = nil
		if strings.TrimSpace(string(patch.Lyrics)) != "null" {
			p.After.Lyrics = &upsertLyricsRequest{}
			if err := decodeMCPArguments(patch.Lyrics, p.After.Lyrics); err != nil {
				return fmt.Errorf("%w: %v", errInvalidLyricsPayload, err)
			}
		}
	}
	if patch.AudioUploadToken != nil {
		p.AudioUploadToken = strings.TrimSpace(*patch.AudioUploadToken)
		p.After.AudioFilePath = p.Before.AudioFilePath
		if p.AudioUploadToken != "" {
			p.After.AudioFilePath = fmt.Sprintf("/api/catalog-submissions/track-updates/%d/audio", subID)
		}
	}
	p.ChangedFields = trackUpdateChangedFields(p.Before, p.After)
	if len(p.ChangedFields) == 0 {
		return fmt.Errorf("%w: proposal must change at least one editable track field", errInvalidTrack)
	}
	return nil
}
func validateTrackUpdate(ctx context.Context, repos domainRepositories, userID int64, p trackUpdateProposal, published bool) error {
	t := p.After.track
	authors := map[int64]author{}
	for _, id := range t.AuthorIDs {
		a, found, err := repos.authors.FindByID(ctx, id)
		if err != nil {
			return err
		}
		if !found || !catalogEntityVisible(a.PublicationStatus, a.RequestedByUserID, userID) {
			return fmt.Errorf("%w: author unavailable", errInvalidTrack)
		}
		authors[id] = a
		if err := checkCatalogDependency(ctx, repos, "author", id, published); err != nil {
			return err
		}
	}
	a, found, err := repos.catalog.FindAlbumByID(ctx, t.AlbumID)
	if err != nil {
		return err
	}
	if !found || !catalogEntityVisible(a.PublicationStatus, a.RequestedByUserID, userID) {
		return fmt.Errorf("%w: album unavailable", errInvalidTrack)
	}
	if err := checkCatalogDependency(ctx, repos, "album", t.AlbumID, published); err != nil {
		return err
	}
	if t.TrackNumber < 1 {
		return fmt.Errorf("%w: trackNumber must be positive", errInvalidTrack)
	}
	if err := validateTrack(t, authors); err != nil {
		return err
	}
	if err := validateAdditionalInfo(t.AdditionalInfo); err != nil {
		return fmt.Errorf("%w: %v", errInvalidTrack, err)
	}
	if err := validateSourceMetadata(t.SourceMetadata); err != nil {
		return fmt.Errorf("%w: %v", errInvalidTrack, err)
	}
	tracks, err := repos.catalog.ListAlbumTracks(ctx, t.AlbumID)
	if err != nil {
		return err
	}
	for _, other := range tracks {
		if other.ID != t.ID && other.TrackNumber == t.TrackNumber {
			return errTrackNumberConflict
		}
	}
	if p.After.Lyrics != nil {
		return validateProposedLyrics(t.ID, p.After.Lyrics)
	}
	return nil
}
func trackUpdateUpload(ctx context.Context, repos domainRepositories, userID int64, token string, submissionID int64) (catalogSubmissionUpload, error) {
	upload, found, err := repos.mcp.FindMCPUpload(ctx, userID, token)
	if err != nil {
		return catalogSubmissionUpload{}, err
	}
	if !found || upload.Kind != "audio" {
		return catalogSubmissionUpload{}, errCatalogUploadNotFound
	}
	if upload.ClaimedSubmissionID != nil && *upload.ClaimedSubmissionID != submissionID {
		return catalogSubmissionUpload{}, errCatalogUploadClaimed
	}
	staged, found, err := repos.submissions.FindSubmissionUpload(ctx, token)
	if err != nil {
		return staged, err
	}
	if !found || staged.RequesterUserID != userID {
		return staged, errCatalogUploadNotFound
	}
	if staged.ClaimedTrackID != nil {
		return staged, errCatalogUploadClaimed
	}
	return staged, nil
}
func (s *trackStore) createTrackUpdateSubmission(userID int64, req createTrackUpdateSubmissionRequest) (catalogSubmissionResponse, error) {
	var result catalogSubmissionResponse
	err := s.unitOfWork.WithinTransaction(context.Background(), func(repos domainRepositories) error {
		ctx := context.Background()
		allowed, err := repos.access.UserHasPermission(ctx, userID, permissionTracksSubmit)
		if err != nil {
			return err
		}
		if !allowed {
			return errCatalogSubmissionForbidden
		}
		before, err := publishedTrackUpdateState(ctx, repos, req.TrackID)
		if err != nil {
			return err
		}
		owned, err := repos.submissions.ListSubmissionsByRequester(ctx, userID)
		if err != nil {
			return err
		}
		for _, sub := range owned {
			if sub.EntityType == catalogSubmissionEntityTrackUpdate && sub.EntityID == req.TrackID && (sub.Status == catalogSubmissionStatusPendingReview || sub.Status == catalogSubmissionStatusChangesRequested) {
				return fmt.Errorf("%w: an active track update proposal already exists", errCatalogSubmissionState)
			}
		}
		p := trackUpdateProposal{TrackID: req.TrackID, Before: before, After: before}
		if err := applyTrackUpdatePatch(&p, req.trackUpdatePatch, 0); err != nil {
			return err
		}
		if err := validateTrackUpdate(ctx, repos, userID, p, false); err != nil {
			return err
		}
		if p.AudioUploadToken != "" {
			if _, err := trackUpdateUpload(ctx, repos, userID, p.AudioUploadToken, 0); err != nil {
				return err
			}
		}
		sub, err := newCatalogSubmission(catalogSubmissionEntityTrackUpdate, req.TrackID, userID, p, time.Now().UTC())
		if err != nil {
			return err
		}
		sub, err = repos.submissions.InsertSubmission(ctx, sub)
		if err != nil {
			return err
		}
		if p.AudioUploadToken != "" {
			if err := repos.mcp.ClaimMCPUpload(ctx, userID, p.AudioUploadToken, sub.ID); err != nil {
				return err
			}
			p.After.AudioFilePath = fmt.Sprintf("/api/catalog-submissions/track-updates/%d/audio", sub.ID)
			sub.Snapshot, err = catalogSubmissionSnapshot(p)
			if err != nil {
				return err
			}
			if err := repos.submissions.UpdateSubmission(ctx, sub); err != nil {
				return err
			}
			sub.Revision++
		}
		responses, err := buildCatalogSubmissionResponses(ctx, repos, []catalogSubmission{sub})
		if err == nil {
			result = responses[0]
		}
		return err
	})
	return result, err
}
func (s *trackStore) updateTrackUpdateSubmission(userID, id int64, req updateTrackUpdateSubmissionRequest) (catalogSubmissionResponse, error) {
	var result catalogSubmissionResponse
	var cleanup string
	err := s.unitOfWork.WithinTransaction(context.Background(), func(repos domainRepositories) error {
		ctx := context.Background()
		sub, found, err := repos.submissions.FindSubmissionByID(ctx, id)
		if err != nil {
			return err
		}
		if !found || sub.EntityType != catalogSubmissionEntityTrackUpdate {
			return errCatalogSubmissionNotFound
		}
		if err := validateSubmissionOwner(sub, userID); err != nil {
			return err
		}
		if sub.Revision != req.ExpectedRevision {
			return errMCPStaleRevision
		}
		if err := validateSubmissionEditable(sub); err != nil {
			return err
		}
		p, err := decodeTrackUpdateProposal(sub)
		if err != nil {
			return err
		}
		current, err := publishedTrackUpdateState(ctx, repos, p.TrackID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current, p.Before) && !req.RefreshBaseline {
			return fmt.Errorf("%w: published track changed; inspect current and set refreshBaseline=true", errCatalogSubmissionDependency)
		}
		if req.RefreshBaseline {
			if err := rebaseTrackUpdate(&p, current); err != nil {
				return err
			}
		}
		oldToken := p.AudioUploadToken
		if err := applyTrackUpdatePatch(&p, req.trackUpdatePatch, id); err != nil {
			return err
		}
		if err := validateTrackUpdate(ctx, repos, userID, p, false); err != nil {
			return err
		}
		if p.AudioUploadToken != "" {
			if _, err := trackUpdateUpload(ctx, repos, userID, p.AudioUploadToken, id); err != nil {
				return err
			}
		}
		if oldToken != p.AudioUploadToken {
			if p.AudioUploadToken != "" {
				if err := repos.mcp.ClaimMCPUpload(ctx, userID, p.AudioUploadToken, id); err != nil {
					return err
				}
			}
			if oldToken != "" {
				cleanup, err = releaseTrackUpdateUpload(ctx, repos, userID, oldToken, id)
				if err != nil {
					return err
				}
			}
		}
		return updateSubmissionSnapshot(ctx, repos, &sub, p, &result)
	})
	if err == nil && cleanup != "" {
		err = s.cleanupSubmissionFile(s.catalogSubmissionAudioPath(cleanup))
	}
	return result, err
}
func trackUpdateProposalView(ctx context.Context, repos domainRepositories, sub catalogSubmission) (trackUpdateSubmissionView, error) {
	p, err := decodeTrackUpdateProposal(sub)
	v := trackUpdateSubmissionView{trackUpdateProposal: p}
	if err != nil {
		return v, err
	}
	current, err := publishedTrackUpdateState(ctx, repos, p.TrackID)
	if errors.Is(err, errCatalogSubmissionNotFound) || errors.Is(err, errCatalogSubmissionState) {
		v.IsOutdated = true
		return v, nil
	}
	if err != nil {
		return v, err
	}
	for _, pair := range []struct {
		state trackUpdateState
		names *[]string
		title *string
	}{
		{p.Before, &v.BeforeAuthorNames, &v.BeforeAlbumTitle}, {p.After, &v.AfterAuthorNames, &v.AfterAlbumTitle},
	} {
		for _, id := range pair.state.AuthorIDs {
			a, found, err := repos.authors.FindByID(ctx, id)
			if err != nil {
				return v, err
			}
			name := fmt.Sprintf("Artist #%d", id)
			if found {
				name = a.CurrentName
			}
			*pair.names = append(*pair.names, name)
		}
		a, found, err := repos.catalog.FindAlbumByID(ctx, pair.state.AlbumID)
		if err != nil {
			return v, err
		}
		if found {
			*pair.title = a.Title
		}
	}
	v.Current = &current
	v.TargetAvailable = true
	v.IsOutdated = !reflect.DeepEqual(current, p.Before)
	return v, nil
}
func trackUpdateDependenciesReady(ctx context.Context, repos domainRepositories, sub catalogSubmission) error {
	v, err := trackUpdateProposalView(ctx, repos, sub)
	if err != nil {
		return err
	}
	if !v.TargetAvailable || v.IsOutdated {
		return fmt.Errorf("%w: published track changed; refresh the track update proposal", errCatalogSubmissionDependency)
	}
	if err := validateTrackUpdate(ctx, repos, sub.RequesterUserID, v.trackUpdateProposal, true); err != nil {
		return fmt.Errorf("%w: %v", errCatalogSubmissionDependency, err)
	}
	if v.AudioUploadToken != "" {
		_, err = trackUpdateUpload(ctx, repos, sub.RequesterUserID, v.AudioUploadToken, sub.ID)
	}
	return err
}
func releaseTrackUpdateUpload(ctx context.Context, repos domainRepositories, userID int64, token string, id int64) (string, error) {
	u, err := trackUpdateUpload(ctx, repos, userID, token, id)
	if err != nil {
		return "", err
	}
	if err := repos.submissions.DeleteSubmissionUpload(ctx, token); err != nil {
		return "", err
	}
	return u.StoredFileName, nil
}
func (s *trackStore) applyTrackUpdateProposal(ctx context.Context, repos domainRepositories, sub catalogSubmission) (p trackUpdateProposal, from, to string, err error) {
	p, err = decodeTrackUpdateProposal(sub)
	if err != nil {
		return
	}
	if err = trackUpdateDependenciesReady(ctx, repos, sub); err != nil {
		return
	}
	if p.AudioUploadToken != "" {
		var upload catalogSubmissionUpload
		upload, err = trackUpdateUpload(ctx, repos, sub.RequesterUserID, p.AudioUploadToken, sub.ID)
		if err != nil {
			return
		}
		var name string
		name, from, to, err = s.publishStagedAudioToLibrary(upload)
		if err != nil {
			return
		}
		p.After.AudioFilePath = "/api/songs/" + url.PathEscape(name)
		if err = repos.submissions.DeleteSubmissionUpload(ctx, upload.Token); err != nil {
			return
		}
	}
	state, loadErr := loadDomainState(ctx, s, repos, stateCatalog)
	if loadErr != nil {
		err = loadErr
		return
	}
	t := p.After.track
	p.After.track, _, err = state.update(t.ID, upsertTrackRequest{Name: t.Name, AuthorIDs: t.AuthorIDs, AlbumID: t.AlbumID, TrackNumber: t.TrackNumber, AudioFilePath: t.AudioFilePath, AdditionalInfo: t.AdditionalInfo, SourceMetadata: t.SourceMetadata})
	if err != nil {
		return
	}
	if !reflect.DeepEqual(p.Before.Lyrics, p.After.Lyrics) {
		if p.After.Lyrics == nil {
			err = repos.lyrics.DeleteByTrackID(ctx, p.TrackID)
		} else {
			_, _, err = saveLyrics(ctx, repos, p.TrackID, *p.After.Lyrics)
		}
	}
	return
}
func trackUpdateSubmissionHandler(store *trackStore, update bool) http.HandlerFunc {
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
			id, err = parseResourceID(r.URL.Path, "/api/catalog-submissions/track-updates/")
			if err != nil {
				http.Error(w, "invalid submission id", http.StatusBadRequest)
				return
			}
			var req updateTrackUpdateSubmissionRequest
			if err = decodeTrackUpdateHTTP(r, &req); err == nil {
				result, err = store.updateTrackUpdateSubmission(userID, id, req)
			}
		} else {
			var req createTrackUpdateSubmissionRequest
			if err = decodeTrackUpdateHTTP(r, &req); err == nil {
				result, err = store.createTrackUpdateSubmission(userID, req)
			}
		}
		if err != nil {
			writeCatalogSubmissionError(w, r, err, "catalog_submissions.track_update")
			return
		}
		status := http.StatusCreated
		if update {
			status = http.StatusOK
		}
		writeJSON(w, status, result)
	}
}
func serveTrackUpdateAudioHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/audio") {
			http.NotFound(w, r)
			return
		}
		id, err := parseResourceID(strings.TrimSuffix(r.URL.Path, "/audio"), "/api/catalog-submissions/track-updates/")
		if err != nil {
			http.Error(w, "invalid submission id", http.StatusBadRequest)
			return
		}
		var upload catalogSubmissionUpload
		err = store.withinReadTransaction(func(ctx context.Context, repos domainRepositories) error {
			sub, found, err := repos.submissions.FindSubmissionByID(ctx, id)
			if err != nil {
				return err
			}
			if !found || sub.EntityType != catalogSubmissionEntityTrackUpdate {
				return errCatalogSubmissionNotFound
			}
			if userID != sub.RequesterUserID {
				if _, err := validateCatalogReviewLease(ctx, repos, userID, sub.RequesterUserID, r.Header.Get(catalogReviewLeaseHeader), time.Now().UTC()); err != nil {
					return err
				}
			}
			if sub.Status != catalogSubmissionStatusPendingReview && sub.Status != catalogSubmissionStatusChangesRequested {
				return errCatalogSubmissionState
			}
			p, err := decodeTrackUpdateProposal(sub)
			if err != nil {
				return err
			}
			upload, err = trackUpdateUpload(ctx, repos, sub.RequesterUserID, p.AudioUploadToken, id)
			return err
		})
		if err != nil {
			writeCatalogSubmissionError(w, r, err, "catalog_submissions.track_update.audio")
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		serveCatalogSubmissionAudioFile(w, r, store.catalogSubmissionAudioPath(upload.StoredFileName))
	}
}

func decodeTrackUpdateArguments(raw json.RawMessage, target any) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return mcpInvalid("arguments", "expected an object")
	}
	for _, key := range []string{"trackId", "name", "authorIds", "albumId", "trackNumber", "audioUploadToken", "additionalInfo", "sourceMetadata", "reason", "expectedRevision", "refreshBaseline"} {
		if value, ok := fields[key]; ok && strings.TrimSpace(string(value)) == "null" {
			return mcpInvalid(key, "must not be null; omit to preserve or use [] to clear metadata")
		}
	}
	if err := decodeMCPArguments(raw, target); err != nil {
		return mcpInvalid("arguments", err.Error())
	}
	return nil
}
func decodeTrackUpdateHTTP(r *http.Request, target any) error {
	var raw json.RawMessage
	if err := decodeJSON(r, &raw); err != nil {
		return err
	}
	return decodeTrackUpdateArguments(raw, target)
}
