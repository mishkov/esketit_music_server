package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const catalogReviewLeaseHeader = "X-Review-Lease"

func listOwnCatalogSubmissionsHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		result, err := store.listCatalogSubmissions(userID)
		if err != nil {
			writeSentryInternalError(w, r, err, "failed to list catalog submissions", "database", "catalog_submissions.list")
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func createAuthorSubmissionHandler(store *trackStore, authorPhotosDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		var request upsertAuthorRequest
		paths, cleanup, err := decodeCatalogImageSubmission(w, r, &request, authorPhotosDir, "photos", "/api/author-photos/", maxSubmissionAuthorPhotos)
		committed := false
		defer func() { cleanup(committed) }()
		if err != nil {
			writeCatalogSubmissionImageError(w, r, err)
			return
		}
		request.Photos = paths
		result, err := store.createAuthorSubmission(userID, request)
		if err != nil {
			writeCatalogSubmissionError(w, r, err, "catalog_submissions.authors.create")
			return
		}
		committed = true
		writeJSON(w, http.StatusCreated, result)
	}
}

func createAlbumSubmissionHandler(store *trackStore, albumCoversDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		var request upsertAlbumRequest
		paths, cleanup, err := decodeCatalogImageSubmission(w, r, &request, albumCoversDir, "cover", "/api/album-covers/", 1)
		committed := false
		defer func() { cleanup(committed) }()
		if err != nil {
			writeCatalogSubmissionImageError(w, r, err)
			return
		}
		if len(paths) != 0 {
			request.CoverImagePath = paths[0]
		}
		result, err := store.createAlbumSubmission(userID, request)
		if err != nil {
			writeCatalogSubmissionError(w, r, err, "catalog_submissions.albums.create")
			return
		}
		committed = true
		writeJSON(w, http.StatusCreated, result)
	}
}

func createTrackSubmissionHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		var request createTrackSubmissionRequest
		if err := decodeJSON(r, &request); err != nil {
			writeRequestDecodeError(w, err)
			return
		}
		result, err := store.createTrackSubmission(userID, request)
		if err != nil {
			writeCatalogSubmissionError(w, r, err, "catalog_submissions.tracks.create")
			return
		}
		writeJSON(w, http.StatusCreated, result)
	}
}

func updateAuthorSubmissionHandler(store *trackStore, authorPhotosDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		entityID, err := parseResourceID(r.URL.Path, "/api/catalog-submissions/authors/")
		if err != nil {
			http.Error(w, "invalid author id", http.StatusBadRequest)
			return
		}
		var request upsertAuthorRequest
		paths, cleanup, err := decodeCatalogImageSubmission(w, r, &request, authorPhotosDir, "photos", "/api/author-photos/", maxSubmissionAuthorPhotos)
		committed := false
		defer func() { cleanup(committed) }()
		if err != nil {
			writeCatalogSubmissionImageError(w, r, err)
			return
		}
		request.Photos = paths
		result, err := store.updateAuthorSubmission(userID, entityID, request)
		if err != nil {
			writeCatalogSubmissionError(w, r, err, "catalog_submissions.authors.update")
			return
		}
		committed = true
		writeJSON(w, http.StatusOK, result)
	}
}

func updateAlbumSubmissionHandler(store *trackStore, albumCoversDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		entityID, err := parseResourceID(r.URL.Path, "/api/catalog-submissions/albums/")
		if err != nil {
			http.Error(w, "invalid album id", http.StatusBadRequest)
			return
		}
		var request upsertAlbumRequest
		paths, cleanup, err := decodeCatalogImageSubmission(w, r, &request, albumCoversDir, "cover", "/api/album-covers/", 1)
		committed := false
		defer func() { cleanup(committed) }()
		if err != nil {
			writeCatalogSubmissionImageError(w, r, err)
			return
		}
		if len(paths) != 0 {
			request.CoverImagePath = paths[0]
		}
		result, err := store.updateAlbumSubmission(userID, entityID, request)
		if err != nil {
			writeCatalogSubmissionError(w, r, err, "catalog_submissions.albums.update")
			return
		}
		committed = true
		writeJSON(w, http.StatusOK, result)
	}
}

func updateTrackSubmissionByRouteHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/lyrics") {
			upsertTrackSubmissionLyricsHandler(store).ServeHTTP(w, r)
			return
		}
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		entityID, err := parseResourceID(r.URL.Path, "/api/catalog-submissions/tracks/")
		if err != nil {
			http.Error(w, "invalid track id", http.StatusBadRequest)
			return
		}
		var request updateTrackSubmissionRequest
		if err := decodeJSON(r, &request); err != nil {
			writeRequestDecodeError(w, err)
			return
		}
		result, err := store.updateTrackSubmission(userID, entityID, request)
		if err != nil {
			writeCatalogSubmissionError(w, r, err, "catalog_submissions.tracks.update")
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func upsertTrackSubmissionLyricsHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		trackID, err := parseResourceID(strings.TrimSuffix(r.URL.Path, "/lyrics"), "/api/catalog-submissions/tracks/")
		if err != nil {
			http.Error(w, "invalid track id", http.StatusBadRequest)
			return
		}
		request, err := decodeUpsertLyricsRequest(r)
		if err != nil {
			writeRequestDecodeError(w, err)
			return
		}
		item, created, err := store.upsertCatalogSubmissionLyrics(userID, trackID, request)
		if err != nil {
			writeCatalogSubmissionError(w, r, err, "catalog_submissions.tracks.lyrics")
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, toLyricsResponse(item))
	}
}

func resubmitCatalogSubmissionHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		submissionID, err := parseResourceID(strings.TrimSuffix(r.URL.Path, "/resubmit"), "/api/catalog-submissions/")
		if err != nil {
			http.Error(w, "invalid submission id", http.StatusBadRequest)
			return
		}
		result, err := store.resubmitCatalogSubmission(userID, submissionID)
		if err != nil {
			writeCatalogSubmissionError(w, r, err, "catalog_submissions.resubmit")
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func cancelCatalogSubmissionHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		submissionID, err := parseResourceID(r.URL.Path, "/api/catalog-submissions/")
		if err != nil {
			http.Error(w, "invalid submission id", http.StatusBadRequest)
			return
		}
		result, err := store.cancelCatalogSubmission(userID, submissionID)
		if err != nil {
			writeCatalogSubmissionError(w, r, err, "catalog_submissions.cancel")
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func uploadCatalogSubmissionAudioHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if strings.TrimSpace(store.songsDir) == "" {
			writeSentryInternalError(w, r, errors.New("songs directory is not configured"), "failed to stage audio", "storage", "catalog_submissions.audio.configure")
			return
		}
		if err := os.MkdirAll(store.catalogSubmissionAudioDir(), 0o755); err != nil {
			writeSentryInternalError(w, r, err, "failed to stage audio", "storage", "catalog_submissions.audio.mkdir")
			return
		}
		info, err := uploadMediaFile(w, r, store.catalogSubmissionAudioDir(), "failed to create staged audio file", "/api/catalog-submissions/audio/")
		if err != nil {
			writeUploadError(w, r, err, "catalog_submissions.audio.upload")
			return
		}
		upload, err := store.registerCatalogSubmissionUpload(userID, info.Name, info.OriginalName, info.SizeBytes)
		if err != nil {
			_ = removeFileForCleanup(store.catalogSubmissionAudioPath(info.Name))
			writeSentryInternalError(w, r, err, "failed to register staged audio", "database", "catalog_submissions.audio.register")
			return
		}
		writeJSON(w, http.StatusCreated, upload)
	}
}

func serveCatalogSubmissionAudioHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		trackID, err := parseResourceID(strings.TrimSuffix(r.URL.Path, "/audio"), "/api/catalog-submissions/tracks/")
		if err != nil {
			http.Error(w, "invalid track id", http.StatusBadRequest)
			return
		}
		upload, err := store.catalogSubmissionAudioAccess(userID, trackID, r.Header.Get(catalogReviewLeaseHeader))
		if err != nil {
			writeCatalogSubmissionError(w, r, err, "catalog_submissions.audio.read")
			return
		}
		serveCatalogSubmissionAudioFile(w, r, store.catalogSubmissionAudioPath(upload.StoredFileName), upload.OriginalName)
	}
}

func serveCatalogSubmissionAudioFile(w http.ResponseWriter, r *http.Request, path, displayName string) {
	songsMutationMu.RLock()
	file, err := os.Open(path)
	if err != nil {
		songsMutationMu.RUnlock()
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		writeSentryInternalError(w, r, err, "failed to read staged audio", "storage", "catalog_submissions.audio.open")
		return
	}
	info, statErr := file.Stat()
	songsMutationMu.RUnlock()
	if statErr != nil {
		_ = file.Close()
		writeSentryInternalError(w, r, statErr, "failed to read staged audio", "storage", "catalog_submissions.audio.stat")
		return
	}
	defer file.Close()
	if !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, displayName, info.ModTime(), file)
}

func listCatalogReviewRequestersHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := store.listCatalogReviewRequesters()
		if err != nil {
			writeSentryInternalError(w, r, err, "failed to list review requesters", "database", "catalog_reviews.requesters")
			return
		}
		writeJSON(w, http.StatusOK, items)
	}
}

func catalogReviewRequesterRouteHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reviewerID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		suffix := "/lease"
		if r.Method == http.MethodGet {
			suffix = "/submissions"
		}
		requesterID, err := parseCatalogReviewRequesterID(r.URL.Path, suffix)
		if err != nil {
			http.Error(w, "invalid requester id", http.StatusBadRequest)
			return
		}
		switch r.Method {
		case http.MethodPost:
			lease, err := store.acquireCatalogReviewLease(reviewerID, requesterID)
			if err != nil {
				writeCatalogSubmissionError(w, r, err, "catalog_reviews.lease.acquire")
				return
			}
			writeJSON(w, http.StatusCreated, lease)
		case http.MethodPut:
			lease, err := store.renewCatalogReviewLease(reviewerID, requesterID, r.Header.Get(catalogReviewLeaseHeader))
			if err != nil {
				writeCatalogSubmissionError(w, r, err, "catalog_reviews.lease.renew")
				return
			}
			writeJSON(w, http.StatusOK, lease)
		case http.MethodDelete:
			if err := store.releaseCatalogReviewLease(reviewerID, requesterID, r.Header.Get(catalogReviewLeaseHeader)); err != nil {
				writeCatalogSubmissionError(w, r, err, "catalog_reviews.lease.release")
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			items, err := store.listCatalogReviewSubmissions(reviewerID, requesterID, r.Header.Get(catalogReviewLeaseHeader))
			if err != nil {
				writeCatalogSubmissionError(w, r, err, "catalog_reviews.submissions")
				return
			}
			writeJSON(w, http.StatusOK, items)
		default:
			http.NotFound(w, r)
		}
	}
}

func catalogReviewDecisionRouteHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reviewerID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		action := filepath.Base(r.URL.Path)
		submissionID, err := parseCatalogReviewSubmissionID(r.URL.Path, action)
		if err != nil {
			http.Error(w, "invalid submission id", http.StatusBadRequest)
			return
		}
		var result catalogSubmissionResponse
		switch action {
		case "approve":
			result, err = store.approveCatalogSubmission(reviewerID, submissionID, r.Header.Get(catalogReviewLeaseHeader))
		case "request-changes", "reject":
			var request catalogReviewDecisionRequest
			if decodeErr := decodeJSON(r, &request); decodeErr != nil {
				writeRequestDecodeError(w, decodeErr)
				return
			}
			if action == "request-changes" {
				result, err = store.requestCatalogSubmissionChanges(reviewerID, submissionID, r.Header.Get(catalogReviewLeaseHeader), request)
			} else {
				result, err = store.rejectCatalogSubmission(reviewerID, submissionID, r.Header.Get(catalogReviewLeaseHeader), request)
			}
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			writeCatalogSubmissionError(w, r, err, "catalog_reviews.decide")
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func parseCatalogReviewRequesterID(path, suffix string) (int64, error) {
	return parseResourceID(strings.TrimSuffix(path, suffix), "/api/catalog-reviews/requesters/")
}

func parseCatalogReviewSubmissionID(path, action string) (int64, error) {
	return parseResourceID(strings.TrimSuffix(path, "/"+action), "/api/catalog-reviews/submissions/")
}

func writeCatalogSubmissionError(w http.ResponseWriter, r *http.Request, err error, operation string) {
	switch {
	case errors.Is(err, errInvalidAuthor), errors.Is(err, errInvalidAlbum), errors.Is(err, errInvalidTrack),
		errors.Is(err, errInvalidLyricsPayload), strings.Contains(err.Error(), "required"), strings.Contains(err.Error(), "must be"):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, errCatalogSubmissionNotFound), errors.Is(err, errCatalogUploadNotFound), errors.Is(err, errRepositoryNotFound):
		http.NotFound(w, r)
	case errors.Is(err, errCatalogSubmissionForbidden), errors.Is(err, errCatalogSelfReview):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, errCatalogSubmissionState), errors.Is(err, errCatalogSubmissionDependency),
		errors.Is(err, errCatalogReviewLeaseConflict), errors.Is(err, errCatalogReviewerAlreadyLeasing),
		errors.Is(err, errCatalogReviewLeaseInvalid), errors.Is(err, errCatalogUploadClaimed), errors.Is(err, errRepositoryConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		writeSentryInternalError(w, r, err, "catalog submission operation failed", "database", operation)
	}
}
