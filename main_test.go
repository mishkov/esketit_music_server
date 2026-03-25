package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

func newTestTrackStore(t *testing.T) *trackStore {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "tracks_db.json")
	store, err := newTrackStore(dbPath)
	if err != nil {
		t.Fatalf("newTrackStore() error = %v", err)
	}
	return store
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
