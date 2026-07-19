package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakeLyricsSearchProvider struct {
	records []lyricsProviderRecord
	err     error
	request lyricsSearchRequest
	calls   int
}

func (p *fakeLyricsSearchProvider) Search(_ context.Context, req lyricsSearchRequest) ([]lyricsProviderRecord, error) {
	p.request = req
	p.calls++
	return p.records, p.err
}

func TestLyricsSearchRouteRequiresAdmin(t *testing.T) {
	store := newTestTrackStore(t)
	trackItem := seedLyricsTrack(t, store)
	auth := newAuthManager([]byte("test-secret"), time.Hour, time.Hour)
	provider := &fakeLyricsSearchProvider{}
	handler := postTrackByRouteHandler(store, auth, newLyricsSearchService(provider))
	body := `{"trackName":"Track","artistNames":["Artist"],"albumName":"Album"}`
	path := "/api/tracks/" + int64String(trackItem.ID) + "/lyrics/search"

	listener := createTestUserWithRole(t, store, roleListener, "listener-lyrics@example.com")
	admin := createTestUserWithRole(t, store, roleAdmin, "admin-lyrics@example.com")

	t.Run("anonymous", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("listener", func(t *testing.T) {
		token, _, err := auth.createAccessToken(listener.ID)
		if err != nil {
			t.Fatalf("createAccessToken() error = %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
		}
	})

	t.Run("admin", func(t *testing.T) {
		token, _, err := auth.createAccessToken(admin.ID)
		if err != nil {
			t.Fatalf("createAccessToken() error = %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
		}
	})
}

func TestLyricsSearchHandlerRequiresExistingTrack(t *testing.T) {
	store := newTestTrackStore(t)
	provider := &fakeLyricsSearchProvider{}
	req := httptest.NewRequest(http.MethodPost, "/api/tracks/999/lyrics/search", strings.NewReader(`{"trackName":"Track","artistNames":["Artist"]}`))
	rec := httptest.NewRecorder()

	lyricsSearchHandler(store, newLyricsSearchService(provider)).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if provider.calls != 0 {
		t.Fatalf("provider calls = %d, want 0", provider.calls)
	}
}

func TestLyricsSearchHandlerValidatesAndNormalizesRequest(t *testing.T) {
	store := newTestTrackStore(t)
	_ = seedLyricsTrack(t, store)

	tests := []struct {
		name string
		body string
	}{
		{name: "missing track", body: `{"trackName":"  ","artistNames":["Artist"]}`},
		{name: "missing artists", body: `{"trackName":"Track","artistNames":[" ",""]}`},
		{name: "zero duration", body: `{"trackName":"Track","artistNames":["Artist"],"durationMs":0}`},
		{name: "negative duration", body: `{"trackName":"Track","artistNames":["Artist"],"durationMs":-1}`},
		{name: "unknown field", body: `{"trackName":"Track","artistNames":["Artist"],"unexpected":true}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &fakeLyricsSearchProvider{}
			req := httptest.NewRequest(http.MethodPost, "/api/tracks/1/lyrics/search", strings.NewReader(test.body))
			rec := httptest.NewRecorder()
			lyricsSearchHandler(store, newLyricsSearchService(provider)).ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if provider.calls != 0 {
				t.Fatalf("provider calls = %d, want 0", provider.calls)
			}
		})
	}

	provider := &fakeLyricsSearchProvider{}
	body := `{"trackName":"  Track title  ","artistNames":[" First artist ","first ARTIST"," ","Featured artist"],"albumName":" Album ","durationMs":213000}`
	req := httptest.NewRequest(http.MethodPost, "/api/tracks/1/lyrics/search", strings.NewReader(body))
	rec := httptest.NewRecorder()
	lyricsSearchHandler(store, newLyricsSearchService(provider)).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if provider.request.TrackName != "Track title" || provider.request.AlbumName != "Album" {
		t.Fatalf("normalized request = %#v", provider.request)
	}
	if !reflect.DeepEqual(provider.request.ArtistNames, []string{"First artist", "Featured artist"}) {
		t.Fatalf("artistNames = %#v", provider.request.ArtistNames)
	}
}

func TestLyricsSearchHandlerReturnsEmptyItemsArray(t *testing.T) {
	store := newTestTrackStore(t)
	_ = seedLyricsTrack(t, store)
	req := httptest.NewRequest(http.MethodPost, "/api/tracks/1/lyrics/search", strings.NewReader(`{"trackName":"Track","artistNames":["Artist"]}`))
	rec := httptest.NewRecorder()

	lyricsSearchHandler(store, newLyricsSearchService(&fakeLyricsSearchProvider{})).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"items":[]}` {
		t.Fatalf("body = %s, want items array", got)
	}
}

func TestLRCLIBProviderRequestAndMapping(t *testing.T) {
	var gotQuery map[string][]string
	var gotUserAgent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		gotUserAgent = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":3396226,"trackName":"I Want to Live","artistName":"Borislav Slavov","albumName":"Baldur's Gate 3","duration":233.125,"instrumental":false,"plainLyrics":"First","syncedLyrics":"[00:01]First"}]`))
	}))
	defer server.Close()

	provider := newLRCLIBProvider(server.URL, server.Client())
	records, err := provider.Search(context.Background(), lyricsSearchRequest{
		TrackName:   "I Want & Live",
		ArtistNames: []string{"First Artist", "Featured Artist"},
		AlbumName:   "Album + Deluxe",
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if !reflect.DeepEqual(gotQuery["track_name"], []string{"I Want & Live"}) ||
		!reflect.DeepEqual(gotQuery["artist_name"], []string{"First Artist, Featured Artist"}) ||
		!reflect.DeepEqual(gotQuery["album_name"], []string{"Album + Deluxe"}) {
		t.Fatalf("query = %#v", gotQuery)
	}
	if gotUserAgent != defaultLRCLIBUserAgent {
		t.Fatalf("User-Agent = %q, want %q", gotUserAgent, defaultLRCLIBUserAgent)
	}
	if len(records) != 1 || records[0].ID != 3396226 || records[0].DurationMs != 233125 {
		t.Fatalf("records = %#v", records)
	}
}

func TestLyricsSearchHandlerConvertsProviderFailures(t *testing.T) {
	tests := []struct {
		name       string
		handler    http.HandlerFunc
		wantStatus int
	}{
		{
			name: "non-2xx",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			},
			wantStatus: http.StatusBadGateway,
		},
		{
			name: "malformed JSON",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"not":"an array"}`))
			},
			wantStatus: http.StatusBadGateway,
		},
		{
			name: "invalid response shape",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`null`))
			},
			wantStatus: http.StatusBadGateway,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			store := newTestTrackStore(t)
			_ = seedLyricsTrack(t, store)
			provider := newLRCLIBProvider(server.URL, server.Client())
			req := httptest.NewRequest(http.MethodPost, "/api/tracks/1/lyrics/search", strings.NewReader(`{"trackName":"Track","artistNames":["Artist"]}`))
			rec := httptest.NewRecorder()
			lyricsSearchHandler(store, newLyricsSearchService(provider)).ServeHTTP(rec, req)
			if rec.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, test.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestLyricsSearchHandlerConvertsProviderTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	store := newTestTrackStore(t)
	_ = seedLyricsTrack(t, store)
	provider := newLRCLIBProvider(server.URL, &http.Client{Timeout: 20 * time.Millisecond})
	req := httptest.NewRequest(http.MethodPost, "/api/tracks/1/lyrics/search", strings.NewReader(`{"trackName":"Track","artistNames":["Artist"]}`))
	rec := httptest.NewRecorder()

	lyricsSearchHandler(store, newLyricsSearchService(provider)).ServeHTTP(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusGatewayTimeout, rec.Body.String())
	}
}

func TestLyricsSearchServiceMapsLyricsVariants(t *testing.T) {
	plain := "Plain lyrics"
	synced := "[00:01.25]Synced line"
	malformed := "this is not LRC"
	provider := &fakeLyricsSearchProvider{records: []lyricsProviderRecord{
		{ID: 1, TrackName: "Plain", PlainLyrics: &plain},
		{ID: 2, TrackName: "Synced", SyncedLyrics: &synced},
		{ID: 3, TrackName: "Both", PlainLyrics: &plain, SyncedLyrics: &synced},
		{ID: 4, TrackName: "Instrumental", Instrumental: true},
		{ID: 5, TrackName: "Malformed with plain", PlainLyrics: &plain, SyncedLyrics: &malformed},
		{ID: 6, TrackName: "Malformed only", SyncedLyrics: &malformed},
	}}
	items, err := newLyricsSearchService(provider).Search(context.Background(), lyricsSearchRequest{TrackName: "Track", ArtistNames: []string{"Artist"}})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	byID := make(map[int64]lyricsSearchCandidate, len(items))
	for _, item := range items {
		byID[item.ProviderID] = item
	}
	if len(byID) != 5 {
		t.Fatalf("candidate count = %d, want 5; items=%#v", len(byID), items)
	}
	if byID[3].Provider != "lrclib" || byID[3].Source != "LRCLIB #3" {
		t.Fatalf("provider identity = %#v", byID[3])
	}
	if byID[1].PlainText == nil || len(byID[1].SyncedLines) != 0 {
		t.Fatalf("plain candidate = %#v", byID[1])
	}
	if byID[2].PlainText != nil || len(byID[2].SyncedLines) != 1 {
		t.Fatalf("synced candidate = %#v", byID[2])
	}
	if byID[3].PlainText == nil || len(byID[3].SyncedLines) != 1 {
		t.Fatalf("both candidate = %#v", byID[3])
	}
	if !byID[4].Instrumental || byID[4].PlainText != nil || len(byID[4].SyncedLines) != 0 {
		t.Fatalf("instrumental candidate = %#v", byID[4])
	}
	if byID[5].PlainText == nil || len(byID[5].SyncedLines) != 0 {
		t.Fatalf("malformed-with-plain candidate = %#v", byID[5])
	}
}

func TestLyricsSearchServiceDeduplicatesAndMergesByProviderID(t *testing.T) {
	plain := "Plain"
	synced := "[00:01]Synced"
	provider := &fakeLyricsSearchProvider{records: []lyricsProviderRecord{
		{ID: 42, TrackName: "Track", ArtistName: "Artist", PlainLyrics: &plain},
		{ID: 42, TrackName: "Track", ArtistName: "Artist", SyncedLyrics: &synced},
	}}
	items, err := newLyricsSearchService(provider).Search(context.Background(), lyricsSearchRequest{TrackName: "Track", ArtistNames: []string{"Artist"}})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(items) != 1 || items[0].PlainText == nil || len(items[0].SyncedLines) != 1 {
		t.Fatalf("items = %#v", items)
	}
}

func TestLyricsSearchServiceLimitsResponseToTwentyCandidates(t *testing.T) {
	plain := "Lyrics"
	records := make([]lyricsProviderRecord, 0, 25)
	for id := int64(25); id >= 1; id-- {
		records = append(records, lyricsProviderRecord{ID: id, TrackName: "Track", ArtistName: "Artist", PlainLyrics: &plain})
	}
	items, err := newLyricsSearchService(&fakeLyricsSearchProvider{records: records}).Search(
		context.Background(),
		lyricsSearchRequest{TrackName: "Track", ArtistNames: []string{"Artist"}},
	)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(items) != 20 || items[0].ProviderID != 1 || items[19].ProviderID != 20 {
		t.Fatalf("items = %#v", items)
	}
}

func TestLyricsSearchServiceRanksBestMatchesDeterministically(t *testing.T) {
	plain := "Lyrics"
	provider := &fakeLyricsSearchProvider{records: []lyricsProviderRecord{
		{ID: 30, TrackName: "Wanted Song (Live)", ArtistName: "Right Artist", AlbumName: "Right Album", PlainLyrics: &plain},
		{ID: 20, TrackName: "Wanted Song", ArtistName: "Wrong Artist", AlbumName: "Other", PlainLyrics: &plain},
		{ID: 10, TrackName: "Wanted Song", ArtistName: "Right Artist", AlbumName: "Right Album", PlainLyrics: &plain},
		{ID: 9, TrackName: "Wanted Song", ArtistName: "Right Artist", AlbumName: "Right Album", PlainLyrics: &plain},
	}}
	items, err := newLyricsSearchService(provider).Search(context.Background(), lyricsSearchRequest{
		TrackName: "wanted song!", ArtistNames: []string{"RIGHT ARTIST"}, AlbumName: "right album",
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	want := []int64{9, 10, 20, 30}
	got := make([]int64, len(items))
	for index := range items {
		got[index] = items[index].ProviderID
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("provider IDs = %#v, want %#v", got, want)
	}
}

func TestLyricsSearchServiceRanksByDurationWhenProvided(t *testing.T) {
	plain := "Lyrics"
	provider := &fakeLyricsSearchProvider{records: []lyricsProviderRecord{
		{ID: 1, TrackName: "Track", ArtistName: "Artist", DurationMs: 240000, PlainLyrics: &plain},
		{ID: 2, TrackName: "Track", ArtistName: "Artist", DurationMs: 213900, PlainLyrics: &plain},
		{ID: 3, TrackName: "Track", ArtistName: "Artist", DurationMs: 213100, PlainLyrics: &plain},
	}}
	duration := 213000
	items, err := newLyricsSearchService(provider).Search(context.Background(), lyricsSearchRequest{
		TrackName: "Track", ArtistNames: []string{"Artist"}, DurationMs: &duration,
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got := []int64{items[0].ProviderID, items[1].ProviderID, items[2].ProviderID}; !reflect.DeepEqual(got, []int64{3, 2, 1}) {
		t.Fatalf("provider IDs = %#v", got)
	}
}

func TestParseSyncedLRCFractionalTimestampsAndEndTimes(t *testing.T) {
	lines, err := parseSyncedLRC("[01:23]Whole\n[01:23.4]Tenths\n[01:23.45]Hundredths\n[01:23.456]Milliseconds")
	if err != nil {
		t.Fatalf("parseSyncedLRC() error = %v", err)
	}
	wantStarts := []int{83000, 83400, 83450, 83456}
	for index, want := range wantStarts {
		if lines[index].StartMs != want {
			t.Fatalf("lines[%d].StartMs = %d, want %d", index, lines[index].StartMs, want)
		}
		if index+1 < len(lines) {
			if lines[index].EndMs == nil || *lines[index].EndMs != wantStarts[index+1] {
				t.Fatalf("lines[%d].EndMs = %#v, want %d", index, lines[index].EndMs, wantStarts[index+1])
			}
		}
	}
	if lines[len(lines)-1].EndMs != nil {
		t.Fatalf("last EndMs = %#v, want nil", lines[len(lines)-1].EndMs)
	}
}

func TestParseSyncedLRCMultipleTimestampsAndMetadata(t *testing.T) {
	lines, err := parseSyncedLRC("[ar:Artist]\n[ti:Title]\n[al:Album]\n[00:01][00:03.5]Repeated")
	if err != nil {
		t.Fatalf("parseSyncedLRC() error = %v", err)
	}
	if len(lines) != 2 || lines[0].StartMs != 1000 || lines[1].StartMs != 3500 || lines[0].Text != "Repeated" || lines[1].Text != "Repeated" {
		t.Fatalf("lines = %#v", lines)
	}
}

func TestParseSyncedLRCOffsetClampsAndMergesDuplicateTimestamps(t *testing.T) {
	lrc := "[offset:-1500]\n[00:00.5]First\n[00:01]First\n[00:01]Second\n[00:03]Third"
	lines, err := parseSyncedLRC(lrc)
	if err != nil {
		t.Fatalf("parseSyncedLRC() error = %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("len(lines) = %d, want 2; lines=%#v", len(lines), lines)
	}
	if lines[0].StartMs != 0 || lines[0].Text != "First\nSecond" {
		t.Fatalf("first line = %#v", lines[0])
	}
	if lines[0].EndMs == nil || *lines[0].EndMs != 1500 || lines[1].StartMs != 1500 || lines[1].EndMs != nil {
		t.Fatalf("timeline = %#v", lines)
	}
}

func TestLyricsSearchDoesNotPersistResults(t *testing.T) {
	store := newTestTrackStore(t)
	trackItem := seedLyricsTrack(t, store)
	storedText := "Existing lyrics"
	if _, _, err := store.upsertLyrics(trackItem.ID, upsertLyricsRequest{Type: lyricsTypePlain, PlainText: &storedText}); err != nil {
		t.Fatalf("upsertLyrics() error = %v", err)
	}
	resultText := "Search result"
	provider := &fakeLyricsSearchProvider{records: []lyricsProviderRecord{{ID: 1, TrackName: "Track", ArtistName: "Artist", PlainLyrics: &resultText}}}
	req := httptest.NewRequest(http.MethodPost, "/api/tracks/1/lyrics/search", bytes.NewBufferString(`{"trackName":"Track","artistNames":["Artist"]}`))
	rec := httptest.NewRecorder()
	lyricsSearchHandler(store, newLyricsSearchService(provider)).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	stored, err := store.getLyrics(trackItem.ID)
	if err != nil {
		t.Fatalf("getLyrics() error = %v", err)
	}
	if stored.PlainText == nil || *stored.PlainText != storedText {
		t.Fatalf("stored lyrics = %#v", stored.PlainText)
	}
}

func TestLyricsSearchServicePropagatesProviderError(t *testing.T) {
	wantErr := errors.New("provider failed")
	_, err := newLyricsSearchService(&fakeLyricsSearchProvider{err: wantErr}).Search(context.Background(), lyricsSearchRequest{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Search() error = %v, want %v", err, wantErr)
	}
}

func createTestUserWithRole(t *testing.T, store *trackStore, role, email string) user {
	t.Helper()
	u, err := store.createUser(email, "hash")
	if err != nil {
		t.Fatalf("createUser() error = %v", err)
	}
	store.mu.Lock()
	u.Role = role
	store.users[u.ID] = u
	store.mu.Unlock()
	return u
}

func int64String(value int64) string {
	return strconv.FormatInt(value, 10)
}
