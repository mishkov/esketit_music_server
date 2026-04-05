package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
		AudioFilePath: "/songs/track.mp3",
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
		AudioFilePath: "/songs/skyline.mp3",
	})
	if err != nil {
		t.Fatalf("create() skyline error = %v", err)
	}
	_, err = store.create(upsertTrackRequest{
		Name:          "Night Drive",
		AuthorIDs:     []int64{artistOne.ID, artistTwo.ID},
		AlbumID:       albumTwo.ID,
		AudioFilePath: "/songs/night-drive.mp3",
	})
	if err != nil {
		t.Fatalf("create() night drive error = %v", err)
	}
	_, err = store.create(upsertTrackRequest{
		Name:          "Morning Light",
		AuthorIDs:     []int64{artistTwo.ID},
		AlbumID:       albumOne.ID,
		AudioFilePath: "/songs/morning-light.mp3",
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

	req := httptest.NewRequest(http.MethodGet, "/tracks?authorId=0", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
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
		AudioFilePath: "/songs/dream-state.mp3",
	})
	if err != nil {
		t.Fatalf("create() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/search?query=dream&page=2&pageSize=2", nil)
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
		AudioFilePath: "/songs/echo-song.mp3",
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
		req := httptest.NewRequest(http.MethodGet, "/search?query=echo", nil)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assertSearchResponse(t, rec, 3, false, false, emptyAlbum.ID)
	})

	t.Run("listener", func(t *testing.T) {
		token, _, err := auth.createAccessToken(listener.ID)
		if err != nil {
			t.Fatalf("createAccessToken() error = %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/search?query=echo", nil)
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

		req := httptest.NewRequest(http.MethodGet, "/search?query=echo", nil)
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
		AudioFilePath: "/songs/full.mp3",
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
		req := httptest.NewRequest(http.MethodGet, "/albums", nil)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assertAlbumListResponse(t, rec, []int64{fullAlbum.ID})
	})

	t.Run("listener", func(t *testing.T) {
		token, _, err := auth.createAccessToken(listener.ID)
		if err != nil {
			t.Fatalf("createAccessToken() error = %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/albums", nil)
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

		req := httptest.NewRequest(http.MethodGet, "/albums", nil)
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
		AudioFilePath: "/songs/track.mp3",
	})
	if err != nil {
		t.Fatalf("create() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/songs/track.mp3", nil)
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

	req := httptest.NewRequest(http.MethodDelete, "/songs/free.mp3", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/songs/unused", nil)
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
	if got[0].Path != "/songs/Another%20Free.mp3" {
		t.Fatalf("got[0].Path = %q, want %q", got[0].Path, "/songs/Another%20Free.mp3")
	}
	if got[0].URL != got[0].Path {
		t.Fatalf("got[0].URL = %q, want same as path %q", got[0].URL, got[0].Path)
	}
	if got[1].Name != "free.mp3" {
		t.Fatalf("got[1].Name = %q, want %q", got[1].Name, "free.mp3")
	}
	if got[1].Path != "/songs/free.mp3" {
		t.Fatalf("got[1].Path = %q, want %q", got[1].Path, "/songs/free.mp3")
	}
}

func TestUploadSongHandlerRandomizesStoredFileName(t *testing.T) {
	songsDir := t.TempDir()
	handler := uploadSongHandler(songsDir)

	rec := httptest.NewRecorder()
	req := newMultipartUploadRequest(t, http.MethodPost, "/songs", "demo track.mp3", []byte("mp3-data"))

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
	if got.URL != "/songs/"+url.PathEscape(got.Name) {
		t.Fatalf("got.URL = %q, want %q", got.URL, "/songs/"+url.PathEscape(got.Name))
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
	firstReq := newMultipartUploadRequest(t, http.MethodPost, "/album-covers", "cover.jpg", []byte("first"))
	handler.ServeHTTP(firstRec, firstReq)
	if firstRec.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want %d; body=%s", firstRec.Code, http.StatusCreated, firstRec.Body.String())
	}

	secondRec := httptest.NewRecorder()
	secondReq := newMultipartUploadRequest(t, http.MethodPost, "/album-covers", "cover.jpg", []byte("second"))
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
