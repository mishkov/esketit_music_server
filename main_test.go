package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAnalyticsEventsHandlerStoresAnonymousEvents(t *testing.T) {
	store := newTestTrackStore(t)
	handler := analyticsEventsHandler(store, newAuthManager([]byte("test-secret"), time.Hour, time.Hour))

	body := `{
		"clientId": "install-anonymous-1",
		"sessionId": "session-1",
		"platform": "android",
		"appVersion": "1.4.0",
		"events": [
			{
				"eventId": "event-play-1",
				"type": "play",
				"trackId": 123,
				"positionMs": 0,
				"durationMs": 180000,
				"clientTime": "2026-07-02T10:00:00Z",
				"metadata": {
					"sourceType": "playlist",
					"sourceId": 55
				}
			},
			{
				"eventId": "event-search-1",
				"type": "search",
				"searchQuery": "ambient",
				"clientTime": "2026-07-02T10:00:05Z",
				"metadata": {
					"resultCount": 4
				}
			}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/analytics/events", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var response analyticsEventsResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if response.Accepted != 2 || response.Duplicates != 0 {
		t.Fatalf("response = %#v, want accepted 2 duplicates 0", response)
	}

	rows, err := store.db.Query(`SELECT event_id, user_id, client_id, session_id, event_type, search_query, metadata_json, platform, app_version FROM analytics_events ORDER BY id`)
	if err != nil {
		t.Fatalf("query analytics_events error = %v", err)
	}
	defer rows.Close()

	type storedEvent struct {
		eventID      string
		userID       sql.NullInt64
		clientID     string
		sessionID    string
		eventType    string
		searchQuery  sql.NullString
		metadataJSON string
		platform     string
		appVersion   string
	}
	var got []storedEvent
	for rows.Next() {
		var item storedEvent
		if err := rows.Scan(&item.eventID, &item.userID, &item.clientID, &item.sessionID, &item.eventType, &item.searchQuery, &item.metadataJSON, &item.platform, &item.appVersion); err != nil {
			t.Fatalf("Scan() error = %v", err)
		}
		got = append(got, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("stored events = %d, want 2", len(got))
	}
	if got[0].userID.Valid {
		t.Fatalf("anonymous user_id valid = true, want false")
	}
	if got[0].clientID != "install-anonymous-1" || got[0].sessionID != "session-1" || got[0].eventType != "play" {
		t.Fatalf("first event = %#v", got[0])
	}
	if got[1].eventType != "search" || !got[1].searchQuery.Valid || got[1].searchQuery.String != "ambient" {
		t.Fatalf("second event = %#v", got[1])
	}
	if got[0].platform != "android" || got[0].appVersion != "1.4.0" {
		t.Fatalf("platform/appVersion = %q/%q", got[0].platform, got[0].appVersion)
	}
}

func TestAnalyticsEventsHandlerAttachesUserAndDeduplicatesRetries(t *testing.T) {
	store := newTestTrackStore(t)
	user, err := store.createUser("listener@example.com", "hash")
	if err != nil {
		t.Fatalf("createUser() error = %v", err)
	}
	auth := newAuthManager([]byte("test-secret"), time.Hour, time.Hour)
	token, _, err := auth.createAccessToken(user.ID)
	if err != nil {
		t.Fatalf("createAccessToken() error = %v", err)
	}
	handler := analyticsEventsHandler(store, auth)

	body := `{
		"clientId": "install-user-1",
		"sessionId": "session-user-1",
		"events": [
			{
				"eventId": "event-skip-1",
				"type": "track_skip",
				"trackId": 321,
				"positionMs": 12000,
				"durationMs": 200000,
				"clientTime": "2026-07-02T10:01:00Z"
			}
		]
	}`

	req := httptest.NewRequest(http.MethodPost, "/api/analytics/events", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want %d; body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/analytics/events", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("retry status = %d, want %d; body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var retryResponse analyticsEventsResponse
	if err := json.NewDecoder(rec.Body).Decode(&retryResponse); err != nil {
		t.Fatalf("Decode() retry error = %v", err)
	}
	if retryResponse.Accepted != 0 || retryResponse.Duplicates != 1 {
		t.Fatalf("retry response = %#v, want accepted 0 duplicates 1", retryResponse)
	}

	var storedUserID sql.NullInt64
	if err := store.db.QueryRow(`SELECT user_id FROM analytics_events WHERE event_id = ?`, "event-skip-1").Scan(&storedUserID); err != nil {
		t.Fatalf("QueryRow() error = %v", err)
	}
	if !storedUserID.Valid || storedUserID.Int64 != user.ID {
		t.Fatalf("stored user_id = %#v, want %d", storedUserID, user.ID)
	}
}

func TestAnalyticsEventsHandlerRejectsInvalidRequests(t *testing.T) {
	store := newTestTrackStore(t)
	auth := newAuthManager([]byte("test-secret"), time.Hour, time.Hour)
	handler := analyticsEventsHandler(store, auth)

	tests := []struct {
		name string
		body string
	}{
		{
			name: "play missing track id",
			body: `{
				"clientId": "install-1",
				"sessionId": "session-1",
				"events": [
					{
						"eventId": "event-invalid-1",
						"type": "play",
						"clientTime": "2026-07-02T10:00:00Z"
					}
				]
			}`,
		},
		{
			name: "search missing query",
			body: `{
				"clientId": "install-1",
				"sessionId": "session-1",
				"events": [
					{
						"eventId": "event-invalid-2",
						"type": "search",
						"clientTime": "2026-07-02T10:00:00Z"
					}
				]
			}`,
		},
		{
			name: "missing client id",
			body: `{
				"sessionId": "session-1",
				"events": [
					{
						"eventId": "event-invalid-3",
						"type": "search",
						"searchQuery": "ambient",
						"clientTime": "2026-07-02T10:00:00Z"
					}
				]
			}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/analytics/events", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
		})
	}

	req := httptest.NewRequest(http.MethodPost, "/api/analytics/events", strings.NewReader(tests[0].body))
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token status = %d, want %d; body=%s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}

func TestCreateUserCreatesFavoritesPlaylist(t *testing.T) {
	store := newTestTrackStore(t)

	user, err := store.createUser("user@example.com", "hash")
	if err != nil {
		t.Fatalf("createUser() error = %v", err)
	}

	playlists := store.listPlaylists(user.ID, playlistListFilter{})
	if len(playlists.Items) != 1 {
		t.Fatalf("playlist count = %d, want 1", len(playlists.Items))
	}
	if !playlists.Items[0].IsFavorites || !playlists.Items[0].System {
		t.Fatalf("favorites playlist = %#v", playlists.Items[0])
	}
}

func TestCreateAndUpdatePlaylistAllowsEmptyDescription(t *testing.T) {
	store := newTestTrackStore(t)

	user, err := store.createUser("listener@example.com", "hash")
	if err != nil {
		t.Fatalf("createUser() error = %v", err)
	}

	created, err := store.createPlaylist(user.ID, upsertPlaylistRequest{
		Name:       "No Description",
		Visibility: playlistVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("createPlaylist() error = %v", err)
	}
	if created.Description != "" {
		t.Fatalf("created.Description = %q, want empty", created.Description)
	}

	updated, exists, err := store.updatePlaylist(user.ID, created.ID, upsertPlaylistRequest{
		Name:        "Still No Description",
		Description: "   ",
		Visibility:  playlistVisibilityPrivate,
	})
	if err != nil || !exists {
		t.Fatalf("updatePlaylist() exists=%v err=%v, want exists true and nil err", exists, err)
	}
	if updated.Description != "" {
		t.Fatalf("updated.Description = %q, want empty", updated.Description)
	}
}

func TestDeleteTrackKeepsUnavailablePlaylistEntry(t *testing.T) {
	store := newTestTrackStore(t)

	user, err := store.createUser("listener@example.com", "hash")
	if err != nil {
		t.Fatalf("createUser() error = %v", err)
	}
	artist, album := seedPlaylistTrackDependencies(t, store)
	customPlaylist, err := store.createPlaylist(user.ID, upsertPlaylistRequest{
		Name:        "Mix",
		Description: "Test mix",
		Visibility:  playlistVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("createPlaylist() error = %v", err)
	}

	createdTrack, err := store.create(upsertTrackRequest{
		Name:          "Track",
		AuthorIDs:     []int64{artist.ID},
		AlbumID:       album.ID,
		AlbumOrder:    0,
		AudioFilePath: "/api/songs/track.mp3",
	})
	if err != nil {
		t.Fatalf("create() error = %v", err)
	}

	if err := store.addTrackToPlaylists(user.ID, createdTrack.ID, []int64{customPlaylist.ID}); err != nil {
		t.Fatalf("addTrackToPlaylists() error = %v", err)
	}

	deleted, err := store.delete(createdTrack.ID)
	if err != nil {
		t.Fatalf("delete() error = %v", err)
	}
	if !deleted {
		t.Fatalf("delete() deleted = false, want true")
	}

	tracksPage, ok := store.getPlaylistTracks(user.ID, customPlaylist.ID, 1, 20)
	if !ok {
		t.Fatalf("getPlaylistTracks() ok = false, want true")
	}
	if len(tracksPage.Items) != 1 {
		t.Fatalf("playlist tracks len = %d, want 1", len(tracksPage.Items))
	}
	if tracksPage.Items[0].IsAvailable {
		t.Fatalf("playlist track availability = true, want false")
	}
	if tracksPage.Items[0].Name != "Track" {
		t.Fatalf("playlist track name = %q, want Track", tracksPage.Items[0].Name)
	}
}

func TestSharedPlaylistTokenLifecycle(t *testing.T) {
	store := newTestTrackStore(t)

	user, err := store.createUser("owner@example.com", "hash")
	if err != nil {
		t.Fatalf("createUser() error = %v", err)
	}

	shared, err := store.createPlaylist(user.ID, upsertPlaylistRequest{
		Name:        "Shared Mix",
		Description: "Generated link mix",
		Visibility:  playlistVisibilityShared,
	})
	if err != nil {
		t.Fatalf("createPlaylist() error = %v", err)
	}
	if shared.ShareToken == "" {
		t.Fatal("shared.ShareToken = empty, want generated token")
	}
	if _, ok := store.getSharedPlaylist(shared.ShareToken); !ok {
		t.Fatal("getSharedPlaylist() ok = false, want true")
	}

	public, exists, err := store.updatePlaylist(user.ID, shared.ID, upsertPlaylistRequest{
		Name:        "Public Mix",
		Description: "Direct link mix",
		Visibility:  playlistVisibilityPublic,
	})
	if err != nil || !exists {
		t.Fatalf("updatePlaylist() public exists=%v err=%v, want exists true and nil err", exists, err)
	}
	if public.ShareToken != "" {
		t.Fatalf("public.ShareToken = %q, want empty", public.ShareToken)
	}
	if _, ok := store.getSharedPlaylist(shared.ShareToken); ok {
		t.Fatal("old shared token still resolves after playlist became public")
	}
	if _, ok := store.getPublicPlaylist(shared.ID); !ok {
		t.Fatal("getPublicPlaylist() ok = false, want true")
	}

	sharedAgain, exists, err := store.updatePlaylist(user.ID, shared.ID, upsertPlaylistRequest{
		Name:        "Shared Again",
		Description: "Generated link mix",
		Visibility:  playlistVisibilityShared,
	})
	if err != nil || !exists {
		t.Fatalf("updatePlaylist() shared exists=%v err=%v, want exists true and nil err", exists, err)
	}
	if sharedAgain.ShareToken == "" {
		t.Fatal("sharedAgain.ShareToken = empty, want generated token")
	}
	if _, ok := store.getSharedPlaylist(sharedAgain.ShareToken); !ok {
		t.Fatal("new shared token does not resolve")
	}
}

func TestPublicAndSharedPlaylistHandlersAllowAnonymousReads(t *testing.T) {
	store := newTestTrackStore(t)

	user, err := store.createUser("owner@example.com", "hash")
	if err != nil {
		t.Fatalf("createUser() error = %v", err)
	}
	artist, albumItem := seedPlaylistTrackDependencies(t, store)
	trackItem, err := store.create(upsertTrackRequest{
		Name:          "Open Track",
		AuthorIDs:     []int64{artist.ID},
		AlbumID:       albumItem.ID,
		AudioFilePath: "/api/songs/open-track.mp3",
	})
	if err != nil {
		t.Fatalf("create() error = %v", err)
	}

	publicPlaylist, err := store.createPlaylist(user.ID, upsertPlaylistRequest{
		Name:        "Public Mix",
		Description: "Direct link mix",
		Visibility:  playlistVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("createPlaylist() public error = %v", err)
	}
	sharedPlaylist, err := store.createPlaylist(user.ID, upsertPlaylistRequest{
		Name:        "Shared Mix",
		Description: "Generated link mix",
		Visibility:  playlistVisibilityShared,
	})
	if err != nil {
		t.Fatalf("createPlaylist() shared error = %v", err)
	}
	privatePlaylist, err := store.createPlaylist(user.ID, upsertPlaylistRequest{
		Name:        "Private Mix",
		Description: "Owner only mix",
		Visibility:  playlistVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("createPlaylist() private error = %v", err)
	}
	if err := store.addTrackToPlaylists(user.ID, trackItem.ID, []int64{publicPlaylist.ID, sharedPlaylist.ID}); err != nil {
		t.Fatalf("addTrackToPlaylists() error = %v", err)
	}
	if err := store.setFavoriteTrack(user.ID, trackItem.ID, true); err != nil {
		t.Fatalf("setFavoriteTrack() error = %v", err)
	}

	publicHandler := getPublicPlaylistByRouteHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/api/public/playlists/"+strconv.FormatInt(publicPlaylist.ID, 10), nil)
	rec := httptest.NewRecorder()
	publicHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("public playlist status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var publicGot playlistResponse
	if err := json.NewDecoder(rec.Body).Decode(&publicGot); err != nil {
		t.Fatalf("Decode() public playlist error = %v", err)
	}
	if publicGot.ID != publicPlaylist.ID || publicGot.ShareToken != "" {
		t.Fatalf("public playlist response = %#v, want id %d and no share token", publicGot, publicPlaylist.ID)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/public/playlists/"+strconv.FormatInt(privatePlaylist.ID, 10), nil)
	rec = httptest.NewRecorder()
	publicHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("private via public status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/public/playlists/"+strconv.FormatInt(sharedPlaylist.ID, 10), nil)
	rec = httptest.NewRecorder()
	publicHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("shared via public status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/public/playlists/"+strconv.FormatInt(publicPlaylist.ID, 10)+"/tracks", nil)
	rec = httptest.NewRecorder()
	publicHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("public tracks status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var tracksGot paginatedTracks
	if err := json.NewDecoder(rec.Body).Decode(&tracksGot); err != nil {
		t.Fatalf("Decode() public tracks error = %v", err)
	}
	if len(tracksGot.Items) != 1 || tracksGot.Items[0].ID != trackItem.ID || tracksGot.Items[0].IsFavorite {
		t.Fatalf("public tracks = %#v, want anonymous non-favorite track", tracksGot.Items)
	}

	sharedHandler := getSharedPlaylistByRouteHandler(store)
	req = httptest.NewRequest(http.MethodGet, "/api/shared/playlists/"+sharedPlaylist.ShareToken, nil)
	rec = httptest.NewRecorder()
	sharedHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("shared playlist status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var sharedGot playlistResponse
	if err := json.NewDecoder(rec.Body).Decode(&sharedGot); err != nil {
		t.Fatalf("Decode() shared playlist error = %v", err)
	}
	if sharedGot.ID != sharedPlaylist.ID || sharedGot.ShareToken != "" {
		t.Fatalf("shared playlist response = %#v, want id %d and no share token", sharedGot, sharedPlaylist.ID)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/shared/playlists/not-a-real-token", nil)
	rec = httptest.NewRecorder()
	sharedHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("wrong shared token status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestListTrackResponsesAppliesPaginationFiltersAndSearch(t *testing.T) {
	store := newTestTrackStore(t)

	artistOne, err := store.createAuthor(upsertAuthorRequest{CurrentName: "Artist One"})
	if err != nil {
		t.Fatalf("createAuthor() artist one error = %v", err)
	}
	artistTwo, err := store.createAuthor(upsertAuthorRequest{CurrentName: "Artist Two"})
	if err != nil {
		t.Fatalf("createAuthor() artist two error = %v", err)
	}
	albumOne, err := store.createAlbum(upsertAlbumRequest{
		Title:       "Album One",
		ReleaseDate: time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("createAlbum() album one error = %v", err)
	}
	albumTwo, err := store.createAlbum(upsertAlbumRequest{
		Title:       "Album Two",
		ReleaseDate: time.Date(2024, time.February, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("createAlbum() album two error = %v", err)
	}

	_, err = store.create(upsertTrackRequest{
		Name:          "Skyline",
		AuthorIDs:     []int64{artistOne.ID},
		AlbumID:       albumOne.ID,
		AudioFilePath: "/api/songs/skyline.mp3",
	})
	if err != nil {
		t.Fatalf("create() skyline error = %v", err)
	}
	_, err = store.create(upsertTrackRequest{
		Name:          "Night Drive",
		AuthorIDs:     []int64{artistOne.ID, artistTwo.ID},
		AlbumID:       albumTwo.ID,
		AudioFilePath: "/api/songs/night-drive.mp3",
	})
	if err != nil {
		t.Fatalf("create() night drive error = %v", err)
	}
	_, err = store.create(upsertTrackRequest{
		Name:          "Morning Light",
		AuthorIDs:     []int64{artistTwo.ID},
		AlbumID:       albumOne.ID,
		AudioFilePath: "/api/songs/morning-light.mp3",
	})
	if err != nil {
		t.Fatalf("create() morning light error = %v", err)
	}

	page := store.listTrackResponses(0, trackListFilter{
		Page:     2,
		PageSize: 1,
		AuthorID: artistOne.ID,
		Query:    "dr",
	})
	if page.Page != 2 {
		t.Fatalf("page.Page = %d, want 2", page.Page)
	}
	if page.PageSize != 1 {
		t.Fatalf("page.PageSize = %d, want 1", page.PageSize)
	}
	if page.TotalItems != 1 {
		t.Fatalf("page.TotalItems = %d, want 1", page.TotalItems)
	}
	if page.TotalPages != 1 {
		t.Fatalf("page.TotalPages = %d, want 1", page.TotalPages)
	}
	if len(page.Items) != 0 {
		t.Fatalf("len(page.Items) = %d, want 0", len(page.Items))
	}

	albumPage := store.listTrackResponses(0, trackListFilter{
		AlbumID: albumOne.ID,
		Query:   "light",
	})
	if albumPage.TotalItems != 1 {
		t.Fatalf("albumPage.TotalItems = %d, want 1", albumPage.TotalItems)
	}
	if len(albumPage.Items) != 1 {
		t.Fatalf("len(albumPage.Items) = %d, want 1", len(albumPage.Items))
	}
	if albumPage.Items[0].Name != "Morning Light" {
		t.Fatalf("albumPage.Items[0].Name = %q, want Morning Light", albumPage.Items[0].Name)
	}
}

func TestListTracksHandlerRejectsInvalidTrackFilters(t *testing.T) {
	store := newTestTrackStore(t)
	handler := listTracksHandler(store, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/tracks?authorId=0", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestCreateTrackAcceptsExternalLinkAndSourceMetadata(t *testing.T) {
	store := newTestTrackStore(t)
	artist, album := seedTrackDependencies(t, store)

	createdTrack, err := store.create(upsertTrackRequest{
		Name:          "Imported Track",
		AuthorIDs:     []int64{artist.ID},
		AlbumID:       album.ID,
		AlbumOrder:    0,
		AudioFilePath: "/api/songs/imported-track.mp3",
		AdditionalInfo: []additionalInfo{
			{
				"type":     "external_link",
				"provider": "youtube_music",
				"url":      " https://music.youtube.com/watch?v=abc123 ",
				"title":    " YouTube Music ",
			},
		},
		SourceMetadata: []sourceMetadata{
			{
				"provider": " youtube_music ",
				"kind":     " track ",
				"identity": map[string]any{
					"videoId": " abc123 ",
				},
				"url": " https://music.youtube.com/watch?v=abc123 ",
			},
		},
	})
	if err != nil {
		t.Fatalf("create() error = %v", err)
	}

	if got := createdTrack.AdditionalInfo[0]["url"]; got != "https://music.youtube.com/watch?v=abc123" {
		t.Fatalf("additionalInfo url = %#v, want trimmed URL", got)
	}
	if got := createdTrack.SourceMetadata[0]["provider"]; got != "youtube_music" {
		t.Fatalf("sourceMetadata provider = %#v, want youtube_music", got)
	}
	identity, ok := createdTrack.SourceMetadata[0]["identity"].(map[string]any)
	if !ok {
		t.Fatalf("sourceMetadata identity = %#v, want object", createdTrack.SourceMetadata[0]["identity"])
	}
	if got := identity["videoId"]; got != "abc123" {
		t.Fatalf("sourceMetadata identity.videoId = %#v, want abc123", got)
	}
}

func TestCreateTrackRejectsDuplicateSourceMetadataProviderIdentity(t *testing.T) {
	store := newTestTrackStore(t)
	artist, album := seedTrackDependencies(t, store)

	_, err := store.create(upsertTrackRequest{
		Name:          "Duplicate Source",
		AuthorIDs:     []int64{artist.ID},
		AlbumID:       album.ID,
		AlbumOrder:    0,
		AudioFilePath: "/api/songs/duplicate-source.mp3",
		SourceMetadata: []sourceMetadata{
			{"provider": "telegram", "identity": map[string]any{"chatId": "chan", "messageId": "42"}},
			{"provider": " Telegram ", "identity": map[string]any{"messageId": "42", "chatId": "chan"}},
		},
	})
	if err == nil {
		t.Fatal("create() error = nil, want validation error")
	}
	if !strings.Contains(err.Error(), "duplicates provider/identity pair") {
		t.Fatalf("create() error = %v, want duplicate sourceMetadata error", err)
	}
}

func TestSearchHandlerReturnsCombinedPaginatedResults(t *testing.T) {
	store := newTestTrackStore(t)
	handler := searchHandler(store, nil)

	artist, err := store.createAuthor(upsertAuthorRequest{CurrentName: "Dream Runner"})
	if err != nil {
		t.Fatalf("createAuthor() error = %v", err)
	}
	_, err = store.createAuthor(upsertAuthorRequest{CurrentName: "Other Artist"})
	if err != nil {
		t.Fatalf("createAuthor() other error = %v", err)
	}
	albumItem, err := store.createAlbum(upsertAlbumRequest{
		Title:       "Dreamscape",
		ReleaseDate: time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC),
		IsPublished: true,
	})
	if err != nil {
		t.Fatalf("createAlbum() error = %v", err)
	}
	_, err = store.create(upsertTrackRequest{
		Name:          "Dream State",
		AuthorIDs:     []int64{artist.ID},
		AlbumID:       albumItem.ID,
		AlbumOrder:    0,
		AudioFilePath: "/api/songs/dream-state.mp3",
	})
	if err != nil {
		t.Fatalf("create() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/search?query=dream&page=2&pageSize=2", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got paginatedSearchResults
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	if got.Page != 2 {
		t.Fatalf("got.Page = %d, want 2", got.Page)
	}
	if got.PageSize != 2 {
		t.Fatalf("got.PageSize = %d, want 2", got.PageSize)
	}
	if got.TotalItems != 3 {
		t.Fatalf("got.TotalItems = %d, want 3", got.TotalItems)
	}
	if got.TotalPages != 2 {
		t.Fatalf("got.TotalPages = %d, want 2", got.TotalPages)
	}
	if len(got.Items) != 1 {
		t.Fatalf("len(got.Items) = %d, want 1", len(got.Items))
	}
	if got.Items[0].Type != "album" {
		t.Fatalf("got.Items[0].Type = %q, want album", got.Items[0].Type)
	}
	if got.Items[0].Album == nil || got.Items[0].Album.Title != "Dreamscape" {
		t.Fatalf("got.Items[0].Album = %#v, want Dreamscape", got.Items[0].Album)
	}
}

func TestSearchHandlerIncludesDiscoverablePlaylists(t *testing.T) {
	store := newTestTrackStore(t)
	auth := newAuthManager([]byte("test-secret"), time.Hour, 24*time.Hour)
	handler := searchHandler(store, auth)

	owner, err := store.createUser("owner@example.com", "hash")
	if err != nil {
		t.Fatalf("createUser() owner error = %v", err)
	}
	other, err := store.createUser("other@example.com", "hash")
	if err != nil {
		t.Fatalf("createUser() other error = %v", err)
	}

	ownerPublic, err := store.createPlaylist(owner.ID, upsertPlaylistRequest{
		Name:        "Roadtrip Owner Public",
		Description: "Searchable",
		Visibility:  playlistVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("createPlaylist() owner public error = %v", err)
	}
	ownerPrivate, err := store.createPlaylist(owner.ID, upsertPlaylistRequest{
		Name:        "Roadtrip Owner Private",
		Description: "Owner searchable",
		Visibility:  playlistVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("createPlaylist() owner private error = %v", err)
	}
	ownerShared, err := store.createPlaylist(owner.ID, upsertPlaylistRequest{
		Name:        "Roadtrip Owner Shared",
		Description: "Owner searchable",
		Visibility:  playlistVisibilityShared,
	})
	if err != nil {
		t.Fatalf("createPlaylist() owner shared error = %v", err)
	}
	otherPublic, err := store.createPlaylist(other.ID, upsertPlaylistRequest{
		Name:        "Roadtrip Other Public",
		Description: "Searchable",
		Visibility:  playlistVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("createPlaylist() other public error = %v", err)
	}
	otherShared, err := store.createPlaylist(other.ID, upsertPlaylistRequest{
		Name:        "Roadtrip Other Shared",
		Description: "Link only",
		Visibility:  playlistVisibilityShared,
	})
	if err != nil {
		t.Fatalf("createPlaylist() other shared error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/search?query=roadtrip&pageSize=100", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assertSearchPlaylistIDs(t, rec, []int64{ownerPublic.ID, otherPublic.ID})

	token, _, err := auth.createAccessToken(owner.ID)
	if err != nil {
		t.Fatalf("createAccessToken() error = %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/search?query=roadtrip&pageSize=100", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assertSearchPlaylistIDs(t, rec, []int64{ownerPrivate.ID, ownerPublic.ID, ownerShared.ID, otherPublic.ID})

	if otherShared.ShareToken == "" {
		t.Fatal("otherShared.ShareToken = empty, want generated token")
	}
}

func TestSearchHandlerReturnsDisplayReadyTrackResults(t *testing.T) {
	store := newTestTrackStore(t)
	handler := searchHandler(store, nil)

	artistOne, err := store.createAuthor(upsertAuthorRequest{
		CurrentName: "Artist One",
		Photos:      []string{"/api/author-photos/artist-one.jpg"},
	})
	if err != nil {
		t.Fatalf("createAuthor() artist one error = %v", err)
	}
	artistTwo, err := store.createAuthor(upsertAuthorRequest{
		CurrentName: "Artist Two",
		Photos:      []string{"/api/author-photos/artist-two.jpg"},
	})
	if err != nil {
		t.Fatalf("createAuthor() artist two error = %v", err)
	}
	albumItem, err := store.createAlbum(upsertAlbumRequest{
		Title:          "Display Album",
		CoverImagePath: "/api/album-covers/display-album.jpg",
		ReleaseDate:    time.Date(2024, time.March, 1, 0, 0, 0, 0, time.UTC),
		IsPublished:    true,
	})
	if err != nil {
		t.Fatalf("createAlbum() error = %v", err)
	}
	_, err = store.create(upsertTrackRequest{
		Name:           "Display Ready Track",
		AuthorIDs:      []int64{artistOne.ID, artistTwo.ID},
		AlbumID:        albumItem.ID,
		AlbumOrder:     0,
		AudioFilePath:  "/api/songs/display-ready-track.mp3",
		AdditionalInfo: []additionalInfo{{"type": "external_link", "provider": "site", "url": "https://example.com/track"}},
	})
	if err != nil {
		t.Fatalf("create() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/search?query=ready", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got paginatedSearchResults
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if got.TotalItems != 1 {
		t.Fatalf("got.TotalItems = %d, want 1", got.TotalItems)
	}
	if len(got.Items) != 1 {
		t.Fatalf("len(got.Items) = %d, want 1", len(got.Items))
	}
	item := got.Items[0]
	if item.Type != "track" {
		t.Fatalf("item.Type = %q, want track", item.Type)
	}
	if item.Track == nil {
		t.Fatal("item.Track = nil, want track response")
	}
	track := item.Track
	if track.ID == 0 || track.Name != "Display Ready Track" || track.AudioFilePath != "/api/songs/display-ready-track.mp3" {
		t.Fatalf("track core fields = %#v, want existing track fields preserved", track)
	}
	if len(track.AuthorIDs) != 2 || track.AuthorIDs[0] != artistOne.ID || track.AuthorIDs[1] != artistTwo.ID {
		t.Fatalf("track.AuthorIDs = %#v, want both author IDs", track.AuthorIDs)
	}
	if len(track.Authors) != 2 {
		t.Fatalf("len(track.Authors) = %d, want 2", len(track.Authors))
	}
	if track.Authors[0].ID != artistOne.ID || track.Authors[0].CurrentName != "Artist One" || len(track.Authors[0].Photos) != 1 || track.Authors[0].Photos[0] != "/api/author-photos/artist-one.jpg" {
		t.Fatalf("track.Authors[0] = %#v, want first full author", track.Authors[0])
	}
	if track.Authors[1].ID != artistTwo.ID || track.Authors[1].CurrentName != "Artist Two" || len(track.Authors[1].Photos) != 1 || track.Authors[1].Photos[0] != "/api/author-photos/artist-two.jpg" {
		t.Fatalf("track.Authors[1] = %#v, want second full author", track.Authors[1])
	}
	if track.CoverImagePath != "/api/album-covers/display-album.jpg" {
		t.Fatalf("track.CoverImagePath = %q, want album cover", track.CoverImagePath)
	}
	if len(track.AdditionalInfo) != 1 {
		t.Fatalf("len(track.AdditionalInfo) = %d, want existing additional info", len(track.AdditionalInfo))
	}
	if track.IsFavorite {
		t.Fatal("track.IsFavorite = true, want false")
	}
	if !track.IsAvailable {
		t.Fatal("track.IsAvailable = false, want true")
	}
}

func TestSearchHandlerUsesOptionalAuthForFavoritesAndAdminAlbumVisibility(t *testing.T) {
	store := newTestTrackStore(t)
	auth := newAuthManager([]byte("test-secret"), time.Hour, 24*time.Hour)
	handler := searchHandler(store, auth)

	artist, err := store.createAuthor(upsertAuthorRequest{CurrentName: "Echo Artist"})
	if err != nil {
		t.Fatalf("createAuthor() error = %v", err)
	}
	fullAlbum, err := store.createAlbum(upsertAlbumRequest{
		Title:       "Echo Full",
		ReleaseDate: time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC),
		IsPublished: true,
	})
	if err != nil {
		t.Fatalf("createAlbum() full error = %v", err)
	}
	emptyAlbum, err := store.createAlbum(upsertAlbumRequest{
		Title:       "Echo Empty",
		ReleaseDate: time.Date(2024, time.January, 2, 0, 0, 0, 0, time.UTC),
		IsPublished: true,
	})
	if err != nil {
		t.Fatalf("createAlbum() empty error = %v", err)
	}
	trackItem, err := store.create(upsertTrackRequest{
		Name:          "Echo Song",
		AuthorIDs:     []int64{artist.ID},
		AlbumID:       fullAlbum.ID,
		AlbumOrder:    0,
		AudioFilePath: "/api/songs/echo-song.mp3",
	})
	if err != nil {
		t.Fatalf("create() error = %v", err)
	}

	listener, err := store.createUser("listener@example.com", "hash")
	if err != nil {
		t.Fatalf("createUser() listener error = %v", err)
	}
	admin, err := store.createUser("admin@example.com", "hash")
	if err != nil {
		t.Fatalf("createUser() admin error = %v", err)
	}

	store.mu.Lock()
	listener.Role = roleListener
	store.users[listener.ID] = listener
	admin.Role = roleAdmin
	store.users[admin.ID] = admin
	store.mu.Unlock()

	if err := store.setFavoriteTrack(listener.ID, trackItem.ID, true); err != nil {
		t.Fatalf("setFavoriteTrack() error = %v", err)
	}

	t.Run("anonymous", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/search?query=echo", nil)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assertSearchResponse(t, rec, 3, false, false, emptyAlbum.ID)
	})

	t.Run("listener", func(t *testing.T) {
		token, _, err := auth.createAccessToken(listener.ID)
		if err != nil {
			t.Fatalf("createAccessToken() error = %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/api/search?query=echo", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assertSearchResponse(t, rec, 3, true, false, emptyAlbum.ID)
	})

	t.Run("admin", func(t *testing.T) {
		token, _, err := auth.createAccessToken(admin.ID)
		if err != nil {
			t.Fatalf("createAccessToken() error = %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/api/search?query=echo", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assertSearchResponse(t, rec, 4, false, true, emptyAlbum.ID)
	})
}

func TestCreateAlbumAllowsPublishedAlbumWithoutTracks(t *testing.T) {
	store := newTestTrackStore(t)

	albumItem, err := store.createAlbum(upsertAlbumRequest{
		Title:       "Empty Release",
		ReleaseDate: time.Date(2024, time.March, 1, 0, 0, 0, 0, time.UTC),
		IsPublished: true,
	})
	if err != nil {
		t.Fatalf("createAlbum() error = %v", err)
	}

	if !albumItem.IsPublished {
		t.Fatalf("albumItem.IsPublished = false, want true")
	}
	if len(albumItem.TrackIDs) != 0 {
		t.Fatalf("len(albumItem.TrackIDs) = %d, want 0", len(albumItem.TrackIDs))
	}
}

func TestListAlbumsHandlerHidesEmptyAlbumsForNonAdmin(t *testing.T) {
	store := newTestTrackStore(t)
	auth := newAuthManager([]byte("test-secret"), time.Hour, 24*time.Hour)
	handler := listAlbumsHandler(store, auth)

	artist, err := store.createAuthor(upsertAuthorRequest{CurrentName: "Artist"})
	if err != nil {
		t.Fatalf("createAuthor() error = %v", err)
	}
	emptyAlbum, err := store.createAlbum(upsertAlbumRequest{
		Title:       "Empty",
		ReleaseDate: time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC),
		IsPublished: true,
	})
	if err != nil {
		t.Fatalf("createAlbum() empty error = %v", err)
	}
	fullAlbum, err := store.createAlbum(upsertAlbumRequest{
		Title:       "Full",
		ReleaseDate: time.Date(2024, time.January, 2, 0, 0, 0, 0, time.UTC),
		IsPublished: true,
	})
	if err != nil {
		t.Fatalf("createAlbum() full error = %v", err)
	}
	if _, err := store.create(upsertTrackRequest{
		Name:          "Track",
		AuthorIDs:     []int64{artist.ID},
		AlbumID:       fullAlbum.ID,
		AlbumOrder:    0,
		AudioFilePath: "/api/songs/full.mp3",
	}); err != nil {
		t.Fatalf("create() error = %v", err)
	}

	listener, err := store.createUser("listener@example.com", "hash")
	if err != nil {
		t.Fatalf("createUser() listener error = %v", err)
	}
	admin, err := store.createUser("admin@example.com", "hash")
	if err != nil {
		t.Fatalf("createUser() admin error = %v", err)
	}

	store.mu.Lock()
	listener.Role = roleListener
	store.users[listener.ID] = listener
	admin.Role = roleAdmin
	store.users[admin.ID] = admin
	store.mu.Unlock()

	t.Run("anonymous", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/albums", nil)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assertAlbumListResponse(t, rec, []int64{fullAlbum.ID})
	})

	t.Run("listener", func(t *testing.T) {
		token, _, err := auth.createAccessToken(listener.ID)
		if err != nil {
			t.Fatalf("createAccessToken() error = %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/api/albums", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assertAlbumListResponse(t, rec, []int64{fullAlbum.ID})
	})

	t.Run("admin", func(t *testing.T) {
		token, _, err := auth.createAccessToken(admin.ID)
		if err != nil {
			t.Fatalf("createAccessToken() error = %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/api/albums", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assertAlbumListResponse(t, rec, []int64{emptyAlbum.ID, fullAlbum.ID})
	})
}

func TestDeleteSongHandlerRejectsReferencedSong(t *testing.T) {
	store := newTestTrackStore(t)
	songsDir := t.TempDir()
	handler := deleteSongHandler(store, songsDir)

	artist, album := seedPlaylistTrackDependencies(t, store)
	if err := os.WriteFile(filepath.Join(songsDir, "track.mp3"), []byte("mp3"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, err := store.create(upsertTrackRequest{
		Name:          "Track",
		AuthorIDs:     []int64{artist.ID},
		AlbumID:       album.ID,
		AlbumOrder:    0,
		AudioFilePath: "/api/songs/track.mp3",
	})
	if err != nil {
		t.Fatalf("create() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/songs/track.mp3", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
	if _, err := os.Stat(filepath.Join(songsDir, "track.mp3")); err != nil {
		t.Fatalf("song file should remain on disk, Stat() error = %v", err)
	}
}

func TestDeleteSongHandlerDeletesUnreferencedSong(t *testing.T) {
	store := newTestTrackStore(t)
	songsDir := t.TempDir()
	handler := deleteSongHandler(store, songsDir)

	if err := os.WriteFile(filepath.Join(songsDir, "free.mp3"), []byte("mp3"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/songs/free.mp3", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if _, err := os.Stat(filepath.Join(songsDir, "free.mp3")); !os.IsNotExist(err) {
		t.Fatalf("song file should be deleted, Stat() error = %v", err)
	}
}

func TestListUnusedSongsHandlerReturnsOnlyUnreferencedSongs(t *testing.T) {
	store := newTestTrackStore(t)
	songsDir := t.TempDir()
	handler := listUnusedSongsHandler(store, songsDir)

	artist, album := seedPlaylistTrackDependencies(t, store)
	for _, name := range []string{"used.mp3", "free.mp3", "Another Free.mp3"} {
		if err := os.WriteFile(filepath.Join(songsDir, name), []byte("mp3"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", name, err)
		}
	}

	_, err := store.create(upsertTrackRequest{
		Name:          "Track",
		AuthorIDs:     []int64{artist.ID},
		AlbumID:       album.ID,
		AlbumOrder:    0,
		AudioFilePath: "https://cdn.example.com/songs/used.mp3?token=abc",
	})
	if err != nil {
		t.Fatalf("create() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/songs/unused", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var got []songInfo
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].Name != "Another Free.mp3" {
		t.Fatalf("got[0].Name = %q, want %q", got[0].Name, "Another Free.mp3")
	}
	if got[0].Path != "/api/songs/Another%20Free.mp3" {
		t.Fatalf("got[0].Path = %q, want %q", got[0].Path, "/api/songs/Another%20Free.mp3")
	}
	if got[0].URL != got[0].Path {
		t.Fatalf("got[0].URL = %q, want same as path %q", got[0].URL, got[0].Path)
	}
	if got[1].Name != "free.mp3" {
		t.Fatalf("got[1].Name = %q, want %q", got[1].Name, "free.mp3")
	}
	if got[1].Path != "/api/songs/free.mp3" {
		t.Fatalf("got[1].Path = %q, want %q", got[1].Path, "/api/songs/free.mp3")
	}
}

func TestUploadSongHandlerRandomizesStoredFileName(t *testing.T) {
	songsDir := t.TempDir()
	handler := uploadSongHandler(songsDir)

	rec := httptest.NewRecorder()
	req := newMultipartUploadRequest(t, http.MethodPost, "/api/songs", "demo track.mp3", []byte("mp3-data"))

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var got albumCoverInfo
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	if !strings.HasPrefix(got.Name, "demo track-") {
		t.Fatalf("got.Name = %q, want prefix %q", got.Name, "demo track-")
	}
	if !strings.HasSuffix(got.Name, ".mp3") {
		t.Fatalf("got.Name = %q, want .mp3 suffix", got.Name)
	}
	if got.URL != "/api/songs/"+url.PathEscape(got.Name) {
		t.Fatalf("got.URL = %q, want %q", got.URL, "/api/songs/"+url.PathEscape(got.Name))
	}
	if _, err := os.Stat(filepath.Join(songsDir, got.Name)); err != nil {
		t.Fatalf("stored file Stat() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(songsDir, "demo track.mp3")); !os.IsNotExist(err) {
		t.Fatalf("original filename should not be used directly, Stat() error = %v", err)
	}
}

func TestUploadAlbumCoverHandlerAllowsSameOriginalFileNameTwice(t *testing.T) {
	albumCoversDir := t.TempDir()
	handler := uploadAlbumCoverHandler(albumCoversDir)

	firstRec := httptest.NewRecorder()
	firstReq := newMultipartUploadRequest(t, http.MethodPost, "/api/album-covers", "cover.jpg", []byte("first"))
	handler.ServeHTTP(firstRec, firstReq)
	if firstRec.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want %d; body=%s", firstRec.Code, http.StatusCreated, firstRec.Body.String())
	}

	secondRec := httptest.NewRecorder()
	secondReq := newMultipartUploadRequest(t, http.MethodPost, "/api/album-covers", "cover.jpg", []byte("second"))
	handler.ServeHTTP(secondRec, secondReq)
	if secondRec.Code != http.StatusCreated {
		t.Fatalf("second status = %d, want %d; body=%s", secondRec.Code, http.StatusCreated, secondRec.Body.String())
	}

	var firstInfo albumCoverInfo
	if err := json.NewDecoder(firstRec.Body).Decode(&firstInfo); err != nil {
		t.Fatalf("Decode(first) error = %v", err)
	}
	var secondInfo albumCoverInfo
	if err := json.NewDecoder(secondRec.Body).Decode(&secondInfo); err != nil {
		t.Fatalf("Decode(second) error = %v", err)
	}

	if firstInfo.Name == secondInfo.Name {
		t.Fatalf("stored names should differ, both were %q", firstInfo.Name)
	}
	for _, info := range []albumCoverInfo{firstInfo, secondInfo} {
		if !strings.HasPrefix(info.Name, "cover-") {
			t.Fatalf("info.Name = %q, want prefix %q", info.Name, "cover-")
		}
		if !strings.HasSuffix(info.Name, ".jpg") {
			t.Fatalf("info.Name = %q, want .jpg suffix", info.Name)
		}
		if _, err := os.Stat(filepath.Join(albumCoversDir, info.Name)); err != nil {
			t.Fatalf("stored file %q Stat() error = %v", info.Name, err)
		}
	}
}

func TestUploadAuthorPhotoHandlerStoresUnderAuthorPhotosPath(t *testing.T) {
	authorPhotosDir := t.TempDir()
	handler := uploadAuthorPhotoHandler(authorPhotosDir)

	rec := httptest.NewRecorder()
	req := newMultipartUploadRequest(t, http.MethodPost, "/api/author-photos", "artist portrait.jpg", []byte("photo-data"))

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var got albumCoverInfo
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	if !strings.HasPrefix(got.Name, "artist portrait-") {
		t.Fatalf("got.Name = %q, want prefix %q", got.Name, "artist portrait-")
	}
	if !strings.HasSuffix(got.Name, ".jpg") {
		t.Fatalf("got.Name = %q, want .jpg suffix", got.Name)
	}
	if got.URL != "/api/author-photos/"+url.PathEscape(got.Name) {
		t.Fatalf("got.URL = %q, want %q", got.URL, "/api/author-photos/"+url.PathEscape(got.Name))
	}
	if got.Path != got.URL {
		t.Fatalf("got.Path = %q, want same as URL %q", got.Path, got.URL)
	}
	if _, err := os.Stat(filepath.Join(authorPhotosDir, got.Name)); err != nil {
		t.Fatalf("stored author photo Stat() error = %v", err)
	}
}

func TestUploadPlaylistCoverHandlerUpdatesPlaylistCover(t *testing.T) {
	store := newTestTrackStore(t)
	auth := newAuthManager([]byte("test-secret"), time.Hour, 24*time.Hour)
	albumCoversDir := t.TempDir()
	handler := requireAuth(auth, store, uploadPlaylistCoverByRouteHandler(store, albumCoversDir))

	user, err := store.createUser("listener@example.com", "hash")
	if err != nil {
		t.Fatalf("createUser() error = %v", err)
	}
	playlistItem, err := store.createPlaylist(user.ID, upsertPlaylistRequest{
		Name:        "Mix",
		Description: "Playlist with cover",
		Visibility:  playlistVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("createPlaylist() error = %v", err)
	}
	token, _, err := auth.createAccessToken(user.ID)
	if err != nil {
		t.Fatalf("createAccessToken() error = %v", err)
	}

	target := "/api/playlists/" + strconv.FormatInt(playlistItem.ID, 10) + "/cover"
	req := newMultipartUploadRequest(t, http.MethodPost, target, "cover.jpg", []byte("cover-data"))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got playlistResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if got.ID != playlistItem.ID {
		t.Fatalf("got.ID = %d, want %d", got.ID, playlistItem.ID)
	}
	if !strings.HasPrefix(got.CoverImagePath, "/api/album-covers/cover-") {
		t.Fatalf("got.CoverImagePath = %q, want uploaded album cover path", got.CoverImagePath)
	}
	if !strings.HasSuffix(got.CoverImagePath, ".jpg") {
		t.Fatalf("got.CoverImagePath = %q, want .jpg suffix", got.CoverImagePath)
	}

	storedName, err := url.PathUnescape(strings.TrimPrefix(got.CoverImagePath, "/api/album-covers/"))
	if err != nil {
		t.Fatalf("PathUnescape() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(albumCoversDir, storedName)); err != nil {
		t.Fatalf("stored playlist cover Stat() error = %v", err)
	}
	storedPlaylist, ok := store.getPlaylist(user.ID, playlistItem.ID)
	if !ok {
		t.Fatalf("getPlaylist() ok = false, want true")
	}
	if storedPlaylist.CoverImagePath != got.CoverImagePath {
		t.Fatalf("storedPlaylist.CoverImagePath = %q, want %q", storedPlaylist.CoverImagePath, got.CoverImagePath)
	}
}

func TestUploadPlaylistCoverHandlerRejectsNonOwnerBeforeWritingFile(t *testing.T) {
	store := newTestTrackStore(t)
	auth := newAuthManager([]byte("test-secret"), time.Hour, 24*time.Hour)
	albumCoversDir := t.TempDir()
	handler := requireAuth(auth, store, uploadPlaylistCoverByRouteHandler(store, albumCoversDir))

	owner, err := store.createUser("owner@example.com", "hash")
	if err != nil {
		t.Fatalf("createUser() owner error = %v", err)
	}
	other, err := store.createUser("other@example.com", "hash")
	if err != nil {
		t.Fatalf("createUser() other error = %v", err)
	}
	playlistItem, err := store.createPlaylist(owner.ID, upsertPlaylistRequest{
		Name:        "Owner Mix",
		Description: "Owned playlist",
		Visibility:  playlistVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("createPlaylist() error = %v", err)
	}
	token, _, err := auth.createAccessToken(other.ID)
	if err != nil {
		t.Fatalf("createAccessToken() error = %v", err)
	}

	target := "/api/playlists/" + strconv.FormatInt(playlistItem.ID, 10) + "/cover"
	req := newMultipartUploadRequest(t, http.MethodPost, target, "cover.jpg", []byte("cover-data"))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	entries, err := os.ReadDir(albumCoversDir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("album covers dir entries = %d, want 0", len(entries))
	}
}

func TestAutoplayNextHandlerSupportsAllSourceTypes(t *testing.T) {
	store := newTestTrackStore(t)
	auth := newAuthManager([]byte("test-secret"), time.Hour, 24*time.Hour)
	handler := requireAuth(auth, store, autoplayNextHandler(store))

	user, err := store.createUser("listener@example.com", "hash")
	if err != nil {
		t.Fatalf("createUser() error = %v", err)
	}

	artist, albumItem := seedPlaylistTrackDependencies(t, store)
	trackOne, err := store.create(upsertTrackRequest{
		Name:          "Track One",
		AuthorIDs:     []int64{artist.ID},
		AlbumID:       albumItem.ID,
		AlbumOrder:    0,
		AudioFilePath: "/api/songs/track-one.mp3",
	})
	if err != nil {
		t.Fatalf("create(trackOne) error = %v", err)
	}
	trackTwo, err := store.create(upsertTrackRequest{
		Name:          "Track Two",
		AuthorIDs:     []int64{artist.ID},
		AlbumID:       albumItem.ID,
		AlbumOrder:    1,
		AudioFilePath: "/api/songs/track-two.mp3",
	})
	if err != nil {
		t.Fatalf("create(trackTwo) error = %v", err)
	}
	playlistItem, err := store.createPlaylist(user.ID, upsertPlaylistRequest{
		Name:        "Queue",
		Description: "Queue description",
		Visibility:  playlistVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("createPlaylist() error = %v", err)
	}
	if err := store.addTrackToPlaylists(user.ID, trackOne.ID, []int64{playlistItem.ID}); err != nil {
		t.Fatalf("addTrackToPlaylists() error = %v", err)
	}

	token, _, err := auth.createAccessToken(user.ID)
	if err != nil {
		t.Fatalf("createAccessToken() error = %v", err)
	}

	tests := []struct {
		name       string
		sourceType string
		sourceID   *int64
	}{
		{name: "my vibe", sourceType: autoplaySourceMyVibe},
		{name: "playlist", sourceType: autoplaySourcePlaylist, sourceID: &playlistItem.ID},
		{name: "album", sourceType: autoplaySourceAlbum, sourceID: &albumItem.ID},
		{name: "track", sourceType: autoplaySourceTrack, sourceID: &trackOne.ID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := autoplayNextRequest{
				SourceType:       tt.sourceType,
				SourceID:         tt.sourceID,
				Profile:          "work",
				Count:            1,
				RecentTrackIDs:   []int64{trackOne.ID},
				ExcludedTrackIDs: []int64{trackOne.ID},
			}
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}

			req := httptest.NewRequest(http.MethodPost, "/api/autoplay/next", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
			}

			var response autoplayNextResponse
			if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
				t.Fatalf("Decode() error = %v", err)
			}

			if response.SourceType != tt.sourceType {
				t.Fatalf("response.SourceType = %q, want %q", response.SourceType, tt.sourceType)
			}
			if response.Profile != "work" {
				t.Fatalf("response.Profile = %q, want work", response.Profile)
			}
			if response.Strategy != "random_stub_v1" {
				t.Fatalf("response.Strategy = %q, want random_stub_v1", response.Strategy)
			}
			if len(response.Tracks) != 1 {
				t.Fatalf("len(response.Tracks) = %d, want 1", len(response.Tracks))
			}
			if response.Tracks[0].ID != trackTwo.ID {
				t.Fatalf("response.Tracks[0].ID = %d, want %d", response.Tracks[0].ID, trackTwo.ID)
			}
		})
	}
}

func TestAutoplayNextHandlerRejectsMissingSourceIDForPlaylist(t *testing.T) {
	store := newTestTrackStore(t)
	auth := newAuthManager([]byte("test-secret"), time.Hour, 24*time.Hour)
	handler := requireAuth(auth, store, autoplayNextHandler(store))

	user, err := store.createUser("listener@example.com", "hash")
	if err != nil {
		t.Fatalf("createUser() error = %v", err)
	}
	token, _, err := auth.createAccessToken(user.ID)
	if err != nil {
		t.Fatalf("createAccessToken() error = %v", err)
	}

	body := strings.NewReader(`{"sourceType":"playlist","count":1,"recentTrackIds":[],"excludedTrackIds":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/autoplay/next", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestAutoplayNextHandlerReturnsNotFoundForMissingTrackContext(t *testing.T) {
	store := newTestTrackStore(t)
	auth := newAuthManager([]byte("test-secret"), time.Hour, 24*time.Hour)
	handler := requireAuth(auth, store, autoplayNextHandler(store))

	user, err := store.createUser("listener@example.com", "hash")
	if err != nil {
		t.Fatalf("createUser() error = %v", err)
	}
	token, _, err := auth.createAccessToken(user.ID)
	if err != nil {
		t.Fatalf("createAccessToken() error = %v", err)
	}

	body := strings.NewReader(`{"sourceType":"track","sourceId":999,"count":1,"recentTrackIds":[],"excludedTrackIds":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/autoplay/next", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestNewTrackStoreNormalizesLegacyBareAudioFilePath(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "tracks_db.json")
	file := diskDBFile{
		NextTrackID:  2,
		NextAlbumID:  2,
		NextAuthorID: 2,
		Tracks: []json.RawMessage{
			json.RawMessage(`{"id":1,"name":"Legacy Track","authorIds":[1],"albumId":1,"audioFilePath":"Kino_-_Kamchatka_(SkySound.cc)-0HGJwR05.mp3","additionalInfo":[],"sourceMetadata":[]}`),
		},
		Albums: []album{
			{
				ID:             1,
				Title:          "Album",
				AuthorIDs:      []int64{1},
				ReleaseDate:    time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC),
				TrackIDs:       []int64{1},
				AdditionalInfo: []additionalInfo{},
			},
		},
		Authors: []author{
			{ID: 1, CurrentName: "Author", Photos: []string{}},
		},
	}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := os.WriteFile(dbPath, data, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	store, err := newTrackStore(dbPath)
	if err != nil {
		t.Fatalf("newTrackStore() error = %v", err)
	}

	got, ok := store.getTrackResponse(1, 0)
	if !ok {
		t.Fatalf("getTrackResponse() ok = false, want true")
	}
	if got.AudioFilePath != "/api/songs/Kino_-_Kamchatka_%28SkySound.cc%29-0HGJwR05.mp3" {
		t.Fatalf("got.AudioFilePath = %q", got.AudioFilePath)
	}
}

func TestNewTrackStorePersistsAndReloadsSQLiteData(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "tracks.db")
	store, err := newTrackStore(dbPath)
	if err != nil {
		t.Fatalf("newTrackStore() error = %v", err)
	}

	user, err := store.createUser("listener@example.com", "hash")
	if err != nil {
		t.Fatalf("createUser() error = %v", err)
	}
	artist, albumItem := seedPlaylistTrackDependencies(t, store)
	trackItem, err := store.create(upsertTrackRequest{
		Name:          "Reloaded Track",
		AuthorIDs:     []int64{artist.ID},
		AlbumID:       albumItem.ID,
		AudioFilePath: "/api/songs/reloaded-track.mp3",
	})
	if err != nil {
		t.Fatalf("create() error = %v", err)
	}
	playlistItem, err := store.createPlaylist(user.ID, upsertPlaylistRequest{
		Name:        "Reloaded Playlist",
		Description: "Reloaded playlist description",
		Visibility:  playlistVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("createPlaylist() error = %v", err)
	}
	if err := store.addTrackToPlaylists(user.ID, trackItem.ID, []int64{playlistItem.ID}); err != nil {
		t.Fatalf("addTrackToPlaylists() error = %v", err)
	}
	if err := store.setFavoriteTrack(user.ID, trackItem.ID, true); err != nil {
		t.Fatalf("setFavoriteTrack() error = %v", err)
	}
	plainText := "Reloaded lyrics"
	if _, _, err := store.upsertLyrics(trackItem.ID, upsertLyricsRequest{
		Type:      lyricsTypePlain,
		PlainText: &plainText,
	}); err != nil {
		t.Fatalf("upsertLyrics() error = %v", err)
	}
	_, rawRefreshToken, err := store.createRefreshSession(user.ID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("createRefreshSession() error = %v", err)
	}
	if err := store.db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reloaded, err := newTrackStore(dbPath)
	if err != nil {
		t.Fatalf("newTrackStore() reload error = %v", err)
	}
	defer reloaded.db.Close()

	reloadedTrack, ok := reloaded.getTrackResponse(trackItem.ID, user.ID)
	if !ok {
		t.Fatalf("getTrackResponse() ok = false, want true")
	}
	if reloadedTrack.Name != "Reloaded Track" || !reloadedTrack.IsFavorite {
		t.Fatalf("reloaded track = %#v", reloadedTrack)
	}
	reloadedUser, ok := reloaded.getUserByEmail("listener@example.com")
	if !ok || reloadedUser.ID != user.ID {
		t.Fatalf("reloaded user = %#v, ok=%v", reloadedUser, ok)
	}
	tracksPage, ok := reloaded.getPlaylistTracks(user.ID, playlistItem.ID, 1, 20)
	if !ok || len(tracksPage.Items) != 1 || tracksPage.Items[0].ID != trackItem.ID {
		t.Fatalf("reloaded playlist tracks = %#v, ok=%v", tracksPage, ok)
	}
	reloadedLyrics, err := reloaded.getLyrics(trackItem.ID)
	if err != nil {
		t.Fatalf("getLyrics() error = %v", err)
	}
	if reloadedLyrics.PlainText == nil || *reloadedLyrics.PlainText != plainText {
		t.Fatalf("reloaded lyrics = %#v", reloadedLyrics)
	}
	deleted, err := reloaded.deleteRefreshSession(rawRefreshToken)
	if err != nil {
		t.Fatalf("deleteRefreshSession() error = %v", err)
	}
	if !deleted {
		t.Fatal("deleteRefreshSession() deleted = false, want true")
	}
}

func newTestTrackStore(t *testing.T) *trackStore {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "tracks_db.json")
	store, err := newTrackStore(dbPath)
	if err != nil {
		t.Fatalf("newTrackStore() error = %v", err)
	}
	return store
}

func assertAlbumListResponse(t *testing.T, rec *httptest.ResponseRecorder, wantIDs []int64) {
	t.Helper()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got paginatedAlbums
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	if len(got.Items) != len(wantIDs) {
		t.Fatalf("len(got.Items) = %d, want %d", len(got.Items), len(wantIDs))
	}
	for i, wantID := range wantIDs {
		if got.Items[i].ID != wantID {
			t.Fatalf("got.Items[%d].ID = %d, want %d", i, got.Items[i].ID, wantID)
		}
	}
}

func assertSearchResponse(t *testing.T, rec *httptest.ResponseRecorder, wantTotal int, wantFavorite bool, wantEmptyAlbum bool, emptyAlbumID int64) {
	t.Helper()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got paginatedSearchResults
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	if got.TotalItems != wantTotal {
		t.Fatalf("got.TotalItems = %d, want %d", got.TotalItems, wantTotal)
	}

	foundFavorite := false
	foundEmptyAlbum := false
	for _, item := range got.Items {
		if item.Type == "track" && item.Track != nil && item.Track.Name == "Echo Song" {
			foundFavorite = item.Track.IsFavorite
		}
		if item.Type == "album" && item.Album != nil && item.Album.ID == emptyAlbumID {
			foundEmptyAlbum = true
		}
	}

	if foundFavorite != wantFavorite {
		t.Fatalf("foundFavorite = %v, want %v", foundFavorite, wantFavorite)
	}
	if foundEmptyAlbum != wantEmptyAlbum {
		t.Fatalf("foundEmptyAlbum = %v, want %v", foundEmptyAlbum, wantEmptyAlbum)
	}
}

func assertSearchPlaylistIDs(t *testing.T, rec *httptest.ResponseRecorder, wantIDs []int64) {
	t.Helper()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got paginatedSearchResults
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	gotIDs := make(map[int64]struct{})
	for _, item := range got.Items {
		if item.Type != "playlist" || item.Playlist == nil {
			continue
		}
		gotIDs[item.Playlist.ID] = struct{}{}
	}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("playlist result count = %d, want %d; results=%#v", len(gotIDs), len(wantIDs), got.Items)
	}
	for _, wantID := range wantIDs {
		if _, ok := gotIDs[wantID]; !ok {
			t.Fatalf("missing playlist id %d in results %#v", wantID, got.Items)
		}
	}
}

func seedPlaylistTrackDependencies(t *testing.T, store *trackStore) (author, album) {
	t.Helper()

	artist, err := store.createAuthor(upsertAuthorRequest{
		CurrentName: "Artist",
	})
	if err != nil {
		t.Fatalf("createAuthor() error = %v", err)
	}

	albumItem, err := store.createAlbum(upsertAlbumRequest{
		Title:       "Album",
		ReleaseDate: time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC),
		IsPublished: false,
	})
	if err != nil {
		t.Fatalf("createAlbum() error = %v", err)
	}

	return artist, albumItem
}

func newMultipartUploadRequest(t *testing.T, method, target, fileName string, content []byte) *http.Request {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	fileWriter, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		t.Fatalf("CreateFormFile() error = %v", err)
	}
	if _, err := fileWriter.Write(content); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	req := httptest.NewRequest(method, target, &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}
