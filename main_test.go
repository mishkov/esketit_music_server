package main

import (
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
