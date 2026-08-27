package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCalculateTrackListeningMsUsesPlaybackPositionDeltas(t *testing.T) {
	windowStart := time.Date(2026, time.July, 2, 0, 0, 0, 0, time.UTC)
	windowEnd := windowStart.Add(authorPopularityWindow)
	events := []authorPopularityEvent{
		popularityEvent(1, "play", 1, 0, 100_000, windowStart.Add(-10*time.Second), nil),
		popularityEvent(2, "pause", 1, 20_000, 100_000, windowStart.Add(10*time.Second), nil),
		popularityEvent(3, "resume", 1, 20_000, 100_000, windowStart.Add(20*time.Second), nil),
		popularityEvent(4, "seek", 1, 60_000, 100_000, windowStart.Add(30*time.Second), map[string]any{
			"fromPositionMs": 30_000,
			"toPositionMs":   60_000,
		}),
		popularityEvent(5, "pause", 1, 70_000, 100_000, windowStart.Add(40*time.Second), nil),
		popularityEvent(6, "play", 2, 0, 100_000, windowStart.Add(time.Minute), nil),
		popularityEvent(7, "track_complete", 2, 99_000, 100_000, windowStart.Add(100*time.Second), nil),
		popularityEvent(8, "play", 3, 0, 100_000, windowEnd.Add(time.Second), nil),
		popularityEvent(9, "pause", 3, 100_000, 100_000, windowEnd.Add(101*time.Second), nil),
	}

	got := calculateTrackListeningMs(events, windowStart, windowEnd)
	if got[1] != 30_000 {
		t.Fatalf("track 1 listened ms = %d, want 30000", got[1])
	}
	if got[2] != 99_000 {
		t.Fatalf("track 2 listened ms = %d, want 99000", got[2])
	}
	if got[3] != 0 {
		t.Fatalf("track 3 listened ms = %d, want 0 outside window", got[3])
	}
}

func TestCalculateTrackListeningMsDoesNotDoubleCountPausedPlayback(t *testing.T) {
	windowStart := time.Date(2026, time.July, 2, 0, 0, 0, 0, time.UTC)
	windowEnd := windowStart.Add(authorPopularityWindow)
	events := []authorPopularityEvent{
		popularityEvent(1, "play", 1, 0, 200_000, windowStart.Add(time.Minute), nil),
		popularityEvent(2, "pause", 1, 20_000, 200_000, windowStart.Add(80*time.Second), nil),
		popularityEvent(3, "resume", 1, 20_000, 200_000, windowStart.Add(10*time.Minute), nil),
		popularityEvent(4, "track_skip", 1, 35_000, 200_000, windowStart.Add(10*time.Minute+15*time.Second), nil),
	}

	got := calculateTrackListeningMs(events, windowStart, windowEnd)
	if got[1] != 35_000 {
		t.Fatalf("track 1 listened ms = %d, want 35000", got[1])
	}
}

func TestRankAuthorsByListeningCreditsEveryCollaborator(t *testing.T) {
	entries := rankAuthorsByListening(
		[]int64{1, 2, 3},
		map[int64][]int64{
			10: {1, 2},
			20: {2},
		},
		map[int64]int64{
			10: 30_000,
			20: 50_000,
		},
	)

	want := []authorPopularityEntry{
		{AuthorID: 2, ListenedMs: 80_000, Rank: 1},
		{AuthorID: 1, ListenedMs: 30_000, Rank: 2},
		{AuthorID: 3, ListenedMs: 0, Rank: 3},
	}
	if len(entries) != len(want) {
		t.Fatalf("entries = %#v, want %#v", entries, want)
	}
	for index := range want {
		if entries[index] != want[index] {
			t.Fatalf("entries[%d] = %#v, want %#v", index, entries[index], want[index])
		}
	}
}

func TestRefreshAuthorPopularityPersistsRollingSnapshotAndOrdersAllAuthors(t *testing.T) {
	store := newTestTrackStore(t)
	authorOne, albumItem := seedPlaylistTrackDependencies(t, store)
	authorTwo, err := store.createAuthor(upsertAuthorRequest{CurrentName: "Second"})
	if err != nil {
		t.Fatal(err)
	}
	authorThree, err := store.createAuthor(upsertAuthorRequest{CurrentName: "Third"})
	if err != nil {
		t.Fatal(err)
	}
	trackOne, err := store.create(upsertTrackRequest{
		Name:          "First Track",
		AuthorIDs:     []int64{authorOne.ID},
		AlbumID:       albumItem.ID,
		AudioFilePath: "/api/songs/first.mp3",
	})
	if err != nil {
		t.Fatal(err)
	}
	trackTwo, err := store.create(upsertTrackRequest{
		Name:          "Second Track",
		AuthorIDs:     []int64{authorTwo.ID},
		AlbumID:       albumItem.ID,
		AudioFilePath: "/api/songs/second.mp3",
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshotTime := time.Date(2026, time.August, 27, 3, 0, 0, 0, time.UTC)
	events := []analyticsEventRecord{
		analyticsPopularityRecord("one-play", "client-1", "session-1", "play", trackOne.ID, 0, snapshotTime.Add(-time.Hour)),
		analyticsPopularityRecord("one-pause", "client-1", "session-1", "pause", trackOne.ID, 10_000, snapshotTime.Add(-time.Hour+10*time.Second)),
		analyticsPopularityRecord("two-play", "client-2", "session-2", "play", trackTwo.ID, 0, snapshotTime.Add(-time.Hour)),
		analyticsPopularityRecord("two-pause", "client-2", "session-2", "pause", trackTwo.ID, 20_000, snapshotTime.Add(-time.Hour+20*time.Second)),
		analyticsPopularityRecord("old-play", "client-old", "session-old", "play", trackOne.ID, 0, snapshotTime.Add(-31*24*time.Hour)),
		analyticsPopularityRecord("old-pause", "client-old", "session-old", "pause", trackOne.ID, 60_000, snapshotTime.Add(-31*24*time.Hour+time.Minute)),
	}
	if _, err := store.appendAnalyticsEvents(events); err != nil {
		t.Fatal(err)
	}

	entries, err := store.refreshAuthorPopularity(context.Background(), snapshotTime)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("entry count = %d, want 3", len(entries))
	}
	if entries[0].AuthorID != authorTwo.ID || entries[0].ListenedMs != 20_000 {
		t.Fatalf("first entry = %#v, want second author with 20000 ms", entries[0])
	}
	if entries[1].AuthorID != authorOne.ID || entries[1].ListenedMs != 10_000 {
		t.Fatalf("second entry = %#v, want first author with 10000 ms", entries[1])
	}
	if entries[2].AuthorID != authorThree.ID || entries[2].ListenedMs != 0 {
		t.Fatalf("third entry = %#v, want zero-listen third author", entries[2])
	}

	popularItems, err := store.listAuthors(authorListFilter{Sort: authorPopularitySort})
	if err != nil {
		t.Fatal(err)
	}
	assertAuthorIDs(t, popularItems, []int64{authorTwo.ID, authorOne.ID, authorThree.ID})
	idItems, err := store.listAuthors(authorListFilter{Sort: authorIDSort})
	if err != nil {
		t.Fatal(err)
	}
	assertAuthorIDs(t, idItems, []int64{authorOne.ID, authorTwo.ID, authorThree.ID})

	var windowStartRaw, windowEndRaw string
	if err := store.db.QueryRow(
		`SELECT window_started_at, window_ended_at FROM author_popularity_snapshot WHERE ranking_position = 1`,
	).Scan(&windowStartRaw, &windowEndRaw); err != nil {
		t.Fatal(err)
	}
	windowStart, err := parseSQLiteTime(windowStartRaw)
	if err != nil {
		t.Fatal(err)
	}
	windowEnd, err := parseSQLiteTime(windowEndRaw)
	if err != nil {
		t.Fatal(err)
	}
	if !windowStart.Equal(snapshotTime.Add(-authorPopularityWindow)) || !windowEnd.Equal(snapshotTime) {
		t.Fatalf("snapshot window = [%s, %s), want [%s, %s)", windowStart, windowEnd, snapshotTime.Add(-authorPopularityWindow), snapshotTime)
	}
}

func TestListAuthorsHandlerDefaultsToPopularityAndValidatesSort(t *testing.T) {
	store := newTestTrackStore(t)
	first, err := store.createAuthor(upsertAuthorRequest{CurrentName: "First"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.createAuthor(upsertAuthorRequest{CurrentName: "Second"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.replaceAuthorPopularitySnapshot(
		context.Background(),
		[]authorPopularityEntry{
			{AuthorID: second.ID, ListenedMs: 2, Rank: 1},
			{AuthorID: first.ID, ListenedMs: 1, Rank: 2},
		},
		time.Now().Add(-authorPopularityWindow),
		time.Now(),
	); err != nil {
		t.Fatal(err)
	}

	handler := listAuthorsHandler(store)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/authors", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("default status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	var got []author
	if err := json.NewDecoder(recorder.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	assertAuthorIDs(t, got, []int64{second.ID, first.ID})

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/authors?sort=id", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("id status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	got = nil
	if err := json.NewDecoder(recorder.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	assertAuthorIDs(t, got, []int64{first.ID, second.ID})

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/authors?sort=newest", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid sort status = %d, want 400", recorder.Code)
	}
}

func TestAuthorPopularitySchemaMigrationCreatesSnapshotAndIndexes(t *testing.T) {
	store := newTestTrackStore(t)
	wantObjects := []string{
		"author_popularity_snapshot",
		"idx_analytics_events_client_session_time",
		"idx_analytics_events_client_time",
	}
	for _, name := range wantObjects {
		var count int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("sqlite schema object %q count = %d, want 1", name, count)
		}
	}
}

func TestNextAuthorPopularityRefreshUsesConfiguredTimezone(t *testing.T) {
	location := time.FixedZone("test-zone", 3*60*60)
	beforeRefresh := time.Date(2026, time.August, 27, 2, 30, 0, 0, location)
	if got, want := nextAuthorPopularityRefresh(beforeRefresh, location), time.Date(2026, time.August, 27, 3, 0, 0, 0, location); !got.Equal(want) {
		t.Fatalf("next refresh before schedule = %s, want %s", got, want)
	}
	afterRefresh := time.Date(2026, time.August, 27, 3, 30, 0, 0, location)
	if got, want := nextAuthorPopularityRefresh(afterRefresh, location), time.Date(2026, time.August, 28, 3, 0, 0, 0, location); !got.Equal(want) {
		t.Fatalf("next refresh after schedule = %s, want %s", got, want)
	}
}

func popularityEvent(
	id int64,
	eventType string,
	trackID int64,
	positionMs int64,
	durationMs int64,
	clientTime time.Time,
	metadata map[string]any,
) authorPopularityEvent {
	metadataJSON, _ := json.Marshal(metadata)
	return authorPopularityEvent{
		ID:           id,
		ClientID:     "client",
		SessionID:    "session",
		EventType:    eventType,
		TrackID:      trackID,
		PositionMs:   popularityInt64Pointer(positionMs),
		DurationMs:   popularityInt64Pointer(durationMs),
		MetadataJSON: string(metadataJSON),
		ClientTime:   clientTime,
	}
}

func analyticsPopularityRecord(
	eventID string,
	clientID string,
	sessionID string,
	eventType string,
	trackID int64,
	positionMs int64,
	clientTime time.Time,
) analyticsEventRecord {
	return analyticsEventRecord{
		EventID:    eventID,
		ClientID:   clientID,
		SessionID:  sessionID,
		EventType:  eventType,
		TrackID:    popularityInt64Pointer(trackID),
		PositionMs: popularityIntPointer(int(positionMs)),
		DurationMs: popularityIntPointer(120_000),
		Metadata:   map[string]any{},
		ClientTime: clientTime,
		ReceivedAt: clientTime,
		Platform:   "test",
		AppVersion: "test",
	}
}

func popularityInt64Pointer(value int64) *int64 {
	return &value
}

func popularityIntPointer(value int) *int {
	return &value
}

func assertAuthorIDs(t *testing.T, authors []author, want []int64) {
	t.Helper()
	if len(authors) != len(want) {
		t.Fatalf("author count = %d, want %d; authors=%#v", len(authors), len(want), authors)
	}
	for index, authorID := range want {
		if authors[index].ID != authorID {
			t.Fatalf("authors[%d].ID = %d, want %d; authors=%#v", index, authors[index].ID, authorID, authors)
		}
	}
}
