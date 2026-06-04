package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetTrackLyricsHandlerReturnsPlainLyrics(t *testing.T) {
	store := newTestTrackStore(t)
	trackItem := seedLyricsTrack(t, store)
	text := "Full lyrics here"

	if _, _, err := store.upsertLyrics(trackItem.ID, upsertLyricsRequest{
		Type:         lyricsTypePlain,
		LanguageCode: stringPtr("en"),
		Source:       stringPtr("artist"),
		IsVerified:   true,
		PlainText:    &text,
	}); err != nil {
		t.Fatalf("upsertLyrics() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/tracks/1/lyrics", nil)
	rec := httptest.NewRecorder()

	getTrackLyricsHandler(store).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got lyricsResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	if got.TrackID != trackItem.ID || got.Type != lyricsTypePlain {
		t.Fatalf("got = %#v", got)
	}
	if got.PlainText == nil || *got.PlainText != text {
		t.Fatalf("plainText = %#v, want %q", got.PlainText, text)
	}
	if len(got.Lines) != 0 {
		t.Fatalf("len(got.Lines) = %d, want 0", len(got.Lines))
	}
}

func TestGetTrackLyricsHandlerReturnsSyncedLyrics(t *testing.T) {
	store := newTestTrackStore(t)
	trackItem := seedLyricsTrack(t, store)

	if _, _, err := store.upsertLyrics(trackItem.ID, upsertLyricsRequest{
		Type:         lyricsTypeSynced,
		LanguageCode: stringPtr("en"),
		Source:       stringPtr("artist"),
		IsVerified:   true,
		Lines: []upsertSyncedLyricsLine{
			{StartMs: 0, EndMs: intPtr(4200), Text: "First line"},
			{StartMs: 4200, EndMs: intPtr(7600), Text: "Second line"},
		},
	}); err != nil {
		t.Fatalf("upsertLyrics() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/tracks/1/lyrics", nil)
	rec := httptest.NewRecorder()

	getTrackLyricsHandler(store).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got lyricsResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	if got.Type != lyricsTypeSynced {
		t.Fatalf("got.Type = %q, want %q", got.Type, lyricsTypeSynced)
	}
	if got.PlainText != nil {
		t.Fatalf("plainText = %#v, want nil", got.PlainText)
	}
	if len(got.Lines) != 2 || got.Lines[1].Text != "Second line" {
		t.Fatalf("got.Lines = %#v", got.Lines)
	}
}

func TestPutTrackLyricsHandlerCreatesAndReplacesPlainLyrics(t *testing.T) {
	store := newTestTrackStore(t)
	trackItem := seedLyricsTrack(t, store)
	handler := putTrackLyricsHandler(store)

	first := `{"type":"plain","languageCode":"en","isVerified":true,"source":"artist","plainText":"First"}`
	firstReq := httptest.NewRequest(http.MethodPut, "/api/tracks/1/lyrics", bytes.NewBufferString(first))
	firstRec := httptest.NewRecorder()

	handler.ServeHTTP(firstRec, firstReq)

	if firstRec.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want %d; body=%s", firstRec.Code, http.StatusCreated, firstRec.Body.String())
	}

	second := `{"type":"plain","languageCode":"uk","isVerified":false,"source":"moderator","plainText":"Second"}`
	secondReq := httptest.NewRequest(http.MethodPut, "/api/tracks/1/lyrics", bytes.NewBufferString(second))
	secondRec := httptest.NewRecorder()

	handler.ServeHTTP(secondRec, secondReq)

	if secondRec.Code != http.StatusOK {
		t.Fatalf("second status = %d, want %d; body=%s", secondRec.Code, http.StatusOK, secondRec.Body.String())
	}

	item, err := store.getLyrics(trackItem.ID)
	if err != nil {
		t.Fatalf("getLyrics() error = %v", err)
	}
	if item.PlainText == nil || *item.PlainText != "Second" {
		t.Fatalf("stored plainText = %#v, want %q", item.PlainText, "Second")
	}
	if item.LanguageCode == nil || *item.LanguageCode != "uk" {
		t.Fatalf("stored languageCode = %#v, want %q", item.LanguageCode, "uk")
	}
}

func TestPutTrackLyricsHandlerCreatesAndReplacesSyncedLyrics(t *testing.T) {
	store := newTestTrackStore(t)
	trackItem := seedLyricsTrack(t, store)
	handler := putTrackLyricsHandler(store)

	first := `{"type":"synced","languageCode":"en","isVerified":true,"source":"artist","lines":[{"startMs":0,"endMs":1000,"text":"One"}]}`
	firstReq := httptest.NewRequest(http.MethodPut, "/api/tracks/1/lyrics", bytes.NewBufferString(first))
	firstRec := httptest.NewRecorder()

	handler.ServeHTTP(firstRec, firstReq)

	if firstRec.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want %d; body=%s", firstRec.Code, http.StatusCreated, firstRec.Body.String())
	}

	second := `{"type":"synced","languageCode":"en","isVerified":false,"source":"moderator","lines":[{"startMs":0,"endMs":1200,"text":"First line"},{"startMs":1200,"endMs":2000,"text":"Second line"}]}`
	secondReq := httptest.NewRequest(http.MethodPut, "/api/tracks/1/lyrics", bytes.NewBufferString(second))
	secondRec := httptest.NewRecorder()

	handler.ServeHTTP(secondRec, secondReq)

	if secondRec.Code != http.StatusOK {
		t.Fatalf("second status = %d, want %d; body=%s", secondRec.Code, http.StatusOK, secondRec.Body.String())
	}

	item, err := store.getLyrics(trackItem.ID)
	if err != nil {
		t.Fatalf("getLyrics() error = %v", err)
	}
	if len(item.Lines) != 2 || item.Lines[1].Text != "Second line" {
		t.Fatalf("stored lines = %#v", item.Lines)
	}
}

func TestPutTrackLyricsHandlerRejectsInvalidPayload(t *testing.T) {
	store := newTestTrackStore(t)
	_ = seedLyricsTrack(t, store)

	req := httptest.NewRequest(http.MethodPut, "/api/tracks/1/lyrics", bytes.NewBufferString(`{"type":"synced","plainText":"wrong","lines":[]}`))
	rec := httptest.NewRecorder()

	putTrackLyricsHandler(store).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestGetTrackLyricsHandlerReturnsNotFoundForMissingTrack(t *testing.T) {
	store := newTestTrackStore(t)

	req := httptest.NewRequest(http.MethodGet, "/api/tracks/999/lyrics", nil)
	rec := httptest.NewRecorder()

	getTrackLyricsHandler(store).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestGetTrackLyricsHandlerReturnsNotFoundForMissingLyrics(t *testing.T) {
	store := newTestTrackStore(t)
	_ = seedLyricsTrack(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/tracks/1/lyrics", nil)
	rec := httptest.NewRecorder()

	getTrackLyricsHandler(store).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestPutTrackLyricsHandlerRejectsInvalidSyncedTimelineOrdering(t *testing.T) {
	store := newTestTrackStore(t)
	_ = seedLyricsTrack(t, store)

	req := httptest.NewRequest(http.MethodPut, "/api/tracks/1/lyrics", bytes.NewBufferString(`{"type":"synced","lines":[{"startMs":1000,"endMs":2000,"text":"Second"},{"startMs":500,"endMs":900,"text":"First"}]}`))
	rec := httptest.NewRecorder()

	putTrackLyricsHandler(store).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func seedLyricsTrack(t *testing.T, store *trackStore) track {
	t.Helper()

	artist, album := seedPlaylistTrackDependencies(t, store)
	trackItem, err := store.create(upsertTrackRequest{
		Name:          "Lyrics Track",
		AuthorIDs:     []int64{artist.ID},
		AlbumID:       album.ID,
		AlbumOrder:    0,
		AudioFilePath: "/api/songs/lyrics-track.mp3",
	})
	if err != nil {
		t.Fatalf("create() error = %v", err)
	}
	return trackItem
}

func stringPtr(value string) *string {
	return &value
}

func intPtr(value int) *int {
	return &value
}
