package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

var (
	errAlbumOrderForbidden = errors.New("albums.reorder_tracks permission is required")
	errAlbumOrderStale     = errors.New("album order revision has changed; fetch the current album before retrying")
	errAlbumOrderState     = errors.New("only approved albums (publicationStatus published) can be reordered")
	errAlbumOrderHidden    = errors.New("album contains tracks not visible to this user; the complete order cannot be changed")
)

type reorderAlbumTracksRequest struct {
	TrackIDs              []int64 `json:"trackIds"`
	ExpectedOrderRevision int64   `json:"expectedOrderRevision"`
}

type albumTrackOrder struct {
	AlbumID       int64   `json:"albumId"`
	TrackIDs      []int64 `json:"trackIds"`
	OrderRevision int64   `json:"orderRevision"`
}

func migrateAlbumOrder(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `ALTER TABLE albums ADD COLUMN order_revision INTEGER NOT NULL DEFAULT 1 CHECK(order_revision > 0)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO permissions (code, description, created_at) VALUES (?, ?, ?)`, permissionAlbumsReorderTracks, "Reorder approved album tracks", formatSQLiteTime(time.Now().UTC())); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO role_permissions (role_id, permission_id)
	 SELECT roles.id, permissions.id FROM roles CROSS JOIN permissions WHERE roles.name = ? AND permissions.code = ?`, roleAdmin, permissionAlbumsReorderTracks)
	return err
}

func (s *domainState) persistAlbumLocked(id int64) error {
	item := s.albums[id]
	if err := s.repositories.catalog.UpdateAlbum(s.ctx, &item); err != nil {
		return err
	}
	s.albums[id] = item
	return nil
}

func (s *trackStore) reorderAlbumTracks(ctx context.Context, userID, albumID int64, req reorderAlbumTracksRequest) (albumTrackOrder, error) {
	var result albumTrackOrder
	err := s.unitOfWork.WithinTransaction(ctx, func(repos domainRepositories) error {
		var err error
		result, err = reorderAlbumTracks(ctx, repos, userID, albumID, req)
		return err
	})
	return result, err
}

// HTTP and MCP call this operation with repositories bound to their write
// transaction. Validate the full persisted membership, never a read projection.
func reorderAlbumTracks(ctx context.Context, repos domainRepositories, userID, albumID int64, req reorderAlbumTracksRequest) (albumTrackOrder, error) {
	allowed, err := repos.access.UserHasPermission(ctx, userID, permissionAlbumsReorderTracks)
	if err != nil {
		return albumTrackOrder{}, err
	}
	if !allowed {
		return albumTrackOrder{}, errAlbumOrderForbidden
	}
	item, found, err := repos.catalog.FindAlbumByID(ctx, albumID)
	if err != nil {
		return albumTrackOrder{}, err
	}
	if !found {
		return albumTrackOrder{}, errRepositoryNotFound
	}
	if normalizePublicationStatus(item.PublicationStatus) != catalogPublicationPublished {
		return albumTrackOrder{}, errAlbumOrderState
	}
	if req.TrackIDs == nil || req.ExpectedOrderRevision < 1 {
		return albumTrackOrder{}, fmt.Errorf("%w: trackIds array and positive expectedOrderRevision are required", errInvalidAlbum)
	}
	if req.ExpectedOrderRevision != item.OrderRevision {
		return albumTrackOrder{}, errAlbumOrderStale
	}
	tracks, err := repos.catalog.ListAlbumTracks(ctx, albumID)
	if err != nil {
		return albumTrackOrder{}, err
	}
	memberIDs := make([]int64, 0, len(tracks))
	for _, track := range tracks {
		if !catalogEntityVisible(track.PublicationStatus, track.RequestedByUserID, userID) {
			return albumTrackOrder{}, errAlbumOrderHidden
		}
		memberIDs = append(memberIDs, track.ID)
	}
	if err := validateExactTrackOrder(item.TrackIDs, memberIDs); err != nil {
		return albumTrackOrder{}, fmt.Errorf("%w: current album membership is inconsistent", errInvalidAlbum)
	}
	// This existing validator rejects nonpositive, duplicate, missing and extra
	// IDs without normalizing them. It requires an exact permutation.
	if err := validateExactTrackOrder(req.TrackIDs, item.TrackIDs); err != nil {
		return albumTrackOrder{}, fmt.Errorf("%w: %v", errInvalidAlbum, err)
	}
	if !slices.Equal(item.TrackIDs, req.TrackIDs) {
		item.TrackIDs = append([]int64{}, req.TrackIDs...)
		if err := repos.catalog.UpdateAlbum(ctx, &item); err != nil {
			return albumTrackOrder{}, err
		}
	}
	return albumTrackOrder{AlbumID: item.ID, TrackIDs: append([]int64{}, item.TrackIDs...), OrderRevision: item.OrderRevision}, nil
}

func reorderAlbumTracksHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseResourceID(strings.TrimSuffix(r.URL.Path, "/track-order"), "/api/albums/")
		if err != nil {
			http.Error(w, "invalid album id", http.StatusBadRequest)
			return
		}
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		var req reorderAlbumTracksRequest
		if err := decodeJSON(r, &req); err != nil {
			writeRequestDecodeError(w, err)
			return
		}
		result, err := store.reorderAlbumTracks(r.Context(), userID, id, req)
		if err != nil {
			if errors.Is(err, errAlbumOrderForbidden) {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
			if errors.Is(err, errAlbumOrderStale) || errors.Is(err, errAlbumOrderState) || errors.Is(err, errAlbumOrderHidden) {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			writeCatalogSubmissionError(w, r, err, "albums.reorder_tracks")
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}
