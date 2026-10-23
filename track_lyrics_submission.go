package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"
)

const catalogSubmissionEntityTrackLyrics = "track_lyrics"

func isCatalogProposal(kind string) bool {
	return kind == catalogSubmissionEntityAlbumTrackOrder || kind == catalogSubmissionEntityTrackLyrics
}

func migrateTrackLyricsSubmissions(ctx context.Context, tx *sql.Tx) error {
	return rebuildProposalSubmissions(ctx, tx, true)
}

type createTrackLyricsSubmissionRequest struct {
	TrackID int64                `json:"trackId"`
	Lyrics  *upsertLyricsRequest `json:"lyrics"`
}

type updateTrackLyricsSubmissionRequest struct {
	ExpectedRevision int64                `json:"expectedRevision"`
	Lyrics           *upsertLyricsRequest `json:"lyrics"`
}

// BaseLyrics prevents an old proposal from overwriting a more recent correction.
type trackLyricsProposal struct {
	TrackID       int64               `json:"trackId"`
	Name          string              `json:"name"`
	AudioFilePath string              `json:"audioFilePath"`
	Lyrics        upsertLyricsRequest `json:"lyrics"`
	BaseLyrics    *lyricsResponse     `json:"baseLyrics"`
}

type trackLyricsSubmissionView struct {
	trackLyricsProposal
	AuthorIDs       []int64 `json:"authorIds"`
	AlbumID         int64   `json:"albumId"`
	TrackNumber     int     `json:"trackNumber"`
	TargetAvailable bool    `json:"targetAvailable"`
	IsOutdated      bool    `json:"isOutdated"`
}

func decodeTrackLyricsProposal(sub catalogSubmission) (trackLyricsProposal, error) {
	var proposal trackLyricsProposal
	raw, err := json.Marshal(sub.Snapshot)
	if err == nil {
		err = json.Unmarshal(raw, &proposal)
	}
	return proposal, err
}

func currentLyricsResponse(ctx context.Context, repos domainRepositories, id int64) (*lyricsResponse, error) {
	item, found, err := repos.lyrics.FindByTrackID(ctx, id)
	if err != nil || !found {
		return nil, err
	}
	value := toLyricsResponse(item)
	return &value, nil
}

func validateProposedLyrics(id int64, request *upsertLyricsRequest) error {
	if request == nil {
		return fmt.Errorf("%w: lyrics is required", errInvalidLyricsPayload)
	}
	item := lyrics{ID: 1, TrackID: id, Type: normalizeLyricsType(request.Type), PlainText: normalizeOptionalString(request.PlainText)}
	for index, line := range request.Lines {
		item.Lines = append(item.Lines, syncedLyricLine{ID: int64(index + 1), LyricsID: 1, OrderIndex: index, StartMs: line.StartMs, EndMs: line.EndMs, Text: line.Text})
	}
	return validateLyrics(item)
}

func publishedLyricsTarget(ctx context.Context, repos domainRepositories, id int64) (track, error) {
	item, found, err := repos.catalog.FindTrackByID(ctx, id)
	if err != nil {
		return item, err
	}
	if !found {
		return item, errCatalogSubmissionNotFound
	}
	if normalizePublicationStatus(item.PublicationStatus) != catalogPublicationPublished {
		return item, fmt.Errorf("%w: lyrics proposals require a published track", errCatalogSubmissionState)
	}
	return item, nil
}

func (s *trackStore) createTrackLyricsSubmission(userID int64, req createTrackLyricsSubmissionRequest) (catalogSubmissionResponse, error) {
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
		if err := validateProposedLyrics(req.TrackID, req.Lyrics); err != nil {
			return err
		}
		target, err := publishedLyricsTarget(ctx, repos, req.TrackID)
		if err != nil {
			return err
		}
		owned, err := repos.submissions.ListSubmissionsByRequester(ctx, userID)
		if err != nil {
			return err
		}
		for _, sub := range owned {
			if sub.EntityType == catalogSubmissionEntityTrackLyrics && sub.EntityID == target.ID && (sub.Status == catalogSubmissionStatusPendingReview || sub.Status == catalogSubmissionStatusChangesRequested) {
				return fmt.Errorf("%w: an active lyrics proposal already exists", errCatalogSubmissionState)
			}
		}
		base, err := currentLyricsResponse(ctx, repos, target.ID)
		if err != nil {
			return err
		}
		proposal := trackLyricsProposal{TrackID: target.ID, Name: target.Name, AudioFilePath: target.AudioFilePath, Lyrics: *req.Lyrics, BaseLyrics: base}
		sub, err := newCatalogSubmission(catalogSubmissionEntityTrackLyrics, target.ID, userID, proposal, time.Now().UTC())
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

func (s *trackStore) updateTrackLyricsSubmission(userID, id int64, req updateTrackLyricsSubmissionRequest) (catalogSubmissionResponse, error) {
	var result catalogSubmissionResponse
	err := s.unitOfWork.WithinTransaction(context.Background(), func(repos domainRepositories) error {
		ctx := context.Background()
		sub, found, err := repos.submissions.FindSubmissionByID(ctx, id)
		if err != nil {
			return err
		}
		if !found || sub.EntityType != catalogSubmissionEntityTrackLyrics {
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
		proposal, err := decodeTrackLyricsProposal(sub)
		if err != nil {
			return err
		}
		if req.Lyrics != nil {
			proposal.Lyrics = *req.Lyrics
		}
		if err := validateProposedLyrics(proposal.TrackID, &proposal.Lyrics); err != nil {
			return err
		}
		target, err := publishedLyricsTarget(ctx, repos, proposal.TrackID)
		if err != nil {
			return err
		}
		proposal.BaseLyrics, err = currentLyricsResponse(ctx, repos, proposal.TrackID)
		if err != nil {
			return err
		}
		proposal.Name, proposal.AudioFilePath = target.Name, target.AudioFilePath
		return updateSubmissionSnapshot(ctx, repos, &sub, proposal, &result)
	})
	return result, err
}

func trackLyricsProposalView(ctx context.Context, repos domainRepositories, sub catalogSubmission) (trackLyricsSubmissionView, error) {
	proposal, err := decodeTrackLyricsProposal(sub)
	view := trackLyricsSubmissionView{trackLyricsProposal: proposal}
	if err != nil {
		return view, err
	}
	target, err := publishedLyricsTarget(ctx, repos, proposal.TrackID)
	if errors.Is(err, errCatalogSubmissionNotFound) || errors.Is(err, errCatalogSubmissionState) {
		view.IsOutdated = true
		return view, nil
	}
	if err != nil {
		return view, err
	}
	view.TargetAvailable = true
	view.AuthorIDs, view.AlbumID, view.TrackNumber = target.AuthorIDs, target.AlbumID, target.TrackNumber
	view.Name, view.AudioFilePath = target.Name, target.AudioFilePath
	current, err := currentLyricsResponse(ctx, repos, proposal.TrackID)
	view.IsOutdated = !reflect.DeepEqual(current, proposal.BaseLyrics) || target.AudioFilePath != proposal.AudioFilePath
	return view, err
}

func trackLyricsDependenciesReady(ctx context.Context, repos domainRepositories, sub catalogSubmission) error {
	view, err := trackLyricsProposalView(ctx, repos, sub)
	if err != nil {
		return err
	}
	if !view.TargetAvailable || view.IsOutdated {
		return fmt.Errorf("%w: refresh the lyrics proposal because the published track or lyrics changed", errCatalogSubmissionDependency)
	}
	return validateProposedLyrics(view.TrackID, &view.Lyrics)
}

func applyTrackLyricsProposal(ctx context.Context, repos domainRepositories, sub catalogSubmission) (trackLyricsProposal, error) {
	proposal, err := decodeTrackLyricsProposal(sub)
	if err != nil {
		return proposal, err
	}
	if err := trackLyricsDependenciesReady(ctx, repos, sub); err != nil {
		return proposal, err
	}
	_, _, err = saveLyrics(ctx, repos, proposal.TrackID, proposal.Lyrics)
	return proposal, err
}

func trackLyricsSubmissionHandler(store *trackStore, update bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		var result catalogSubmissionResponse
		var err error
		if update {
			id, parseErr := parseResourceID(r.URL.Path, "/api/catalog-submissions/track-lyrics/")
			if parseErr != nil {
				http.Error(w, "invalid submission id", http.StatusBadRequest)
				return
			}
			var req updateTrackLyricsSubmissionRequest
			if err := decodeJSON(r, &req); err != nil {
				writeRequestDecodeError(w, err)
				return
			}
			result, err = store.updateTrackLyricsSubmission(userID, id, req)
		} else {
			var req createTrackLyricsSubmissionRequest
			if err := decodeJSON(r, &req); err != nil {
				writeRequestDecodeError(w, err)
				return
			}
			result, err = store.createTrackLyricsSubmission(userID, req)
		}
		if err != nil {
			writeCatalogSubmissionError(w, r, err, "catalog_submissions.track_lyrics")
			return
		}
		status := http.StatusCreated
		if update {
			status = http.StatusOK
		}
		writeJSON(w, status, result)
	}
}
