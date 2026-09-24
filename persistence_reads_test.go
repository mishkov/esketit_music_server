package main

import (
	"context"
	"testing"
	"time"
)

func TestScopedTrackReadFiltersAndPaginatesBeforeDecoding(t *testing.T) {
	store := newTestTrackStore(t)
	t.Cleanup(func() { _ = store.db.Close() })
	artist, albumItem := seedPlaylistTrackDependencies(t, store)

	for index, name := range []string{"Привет", "Приветствие"} {
		if _, err := store.create(upsertTrackRequest{
			Name: name, AuthorIDs: []int64{artist.ID}, AlbumID: albumItem.ID,
			AlbumOrder: index, AudioFilePath: name + ".mp3",
		}); err != nil {
			t.Fatalf("create track %q: %v", name, err)
		}
	}

	// This row contains valid JSON with the wrong shape. A whole-table decode
	// would fail, while a scoped query for the real album must never scan it.
	if _, err := store.db.Exec(`INSERT INTO tracks (
		id, name, author_ids_json, album_id, audio_file_path,
		additional_info_json, source_metadata_json, created_at
	) VALUES (999, 'unrelated', '[]', 999, '/api/songs/unrelated.mp3', '"wrong-shape"', '[]', ?)`, formatSQLiteTime(time.Now().UTC())); err != nil {
		t.Fatalf("insert unrelated malformed domain row: %v", err)
	}

	got, err := store.listTrackResponses(0, trackListFilter{
		Page: 1, PageSize: 1, AlbumID: albumItem.ID, Query: "ПРИВ",
		Sort: trackListSortID, Order: sortOrderAsc,
	})
	if err != nil {
		t.Fatalf("listTrackResponses() error = %v", err)
	}
	if got.TotalItems != 2 || got.TotalPages != 2 || len(got.Items) != 1 {
		t.Fatalf("pagination = %#v, want one of two matching tracks", got)
	}
	if got.Items[0].Name != "Привет" {
		t.Fatalf("first item = %q, want %q", got.Items[0].Name, "Привет")
	}
}

func TestScopedSearchPreservesUnicodeCaseFolding(t *testing.T) {
	store := newTestTrackStore(t)
	t.Cleanup(func() { _ = store.db.Close() })
	artist, albumItem := seedPlaylistTrackDependencies(t, store)
	created, err := store.create(upsertTrackRequest{
		Name: "Северный Ветер", AuthorIDs: []int64{artist.ID}, AlbumID: albumItem.ID,
		AudioFilePath: "north-wind.mp3",
	})
	if err != nil {
		t.Fatalf("create track: %v", err)
	}

	got, err := store.search(0, searchListFilter{Page: 1, PageSize: 10, Query: "СЕВЕРНЫЙ"})
	if err != nil {
		t.Fatalf("search() error = %v", err)
	}
	if got.TotalItems != 1 || len(got.Items) != 1 || got.Items[0].Track == nil {
		t.Fatalf("search result = %#v, want one track", got)
	}
	if got.Items[0].Track.ID != created.ID {
		t.Fatalf("track id = %d, want %d", got.Items[0].Track.ID, created.ID)
	}
}

func TestScopedTrackReadSortsRFC3339NanoChronologically(t *testing.T) {
	store := newTestTrackStore(t)
	t.Cleanup(func() { _ = store.db.Close() })
	artist, albumItem := seedPlaylistTrackDependencies(t, store)
	exactSecond := time.Date(2026, time.September, 23, 10, 0, 0, 0, time.UTC)
	items := []track{
		{ID: 50, Name: "fraction", AuthorIDs: []int64{artist.ID}, AlbumID: albumItem.ID, AudioFilePath: "/api/songs/fraction.mp3", CreatedAt: exactSecond.Add(100 * time.Millisecond)},
		{ID: 51, Name: "exact", AuthorIDs: []int64{artist.ID}, AlbumID: albumItem.ID, AudioFilePath: "/api/songs/exact.mp3", CreatedAt: exactSecond},
	}
	for _, item := range items {
		if err := store.catalogRepository.InsertTrack(context.Background(), item); err != nil {
			t.Fatalf("insert track %d: %v", item.ID, err)
		}
	}

	got, err := store.listTrackResponses(0, trackListFilter{
		Page: 1, PageSize: 10, AlbumID: albumItem.ID,
		Sort: trackListSortCreatedAt, Order: sortOrderAsc,
	})
	if err != nil {
		t.Fatalf("listTrackResponses() error = %v", err)
	}
	if len(got.Items) != 2 || got.Items[0].ID != 51 || got.Items[1].ID != 50 {
		t.Fatalf("createdAt order = %#v, want ids [51 50]", got.Items)
	}
}
