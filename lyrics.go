package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	lyricsTypePlain  = "plain"
	lyricsTypeSynced = "synced"
)

type upsertLyricsRequest struct {
	Type         string                   `json:"type"`
	PlainText    *string                  `json:"plainText"`
	LanguageCode *string                  `json:"languageCode"`
	Source       *string                  `json:"source"`
	IsVerified   bool                     `json:"isVerified"`
	Lines        []upsertSyncedLyricsLine `json:"lines"`
}

type upsertSyncedLyricsLine struct {
	StartMs int    `json:"startMs"`
	EndMs   *int   `json:"endMs"`
	Text    string `json:"text"`
}

type lyricsResponse struct {
	TrackID      int64                `json:"trackId"`
	Type         string               `json:"type"`
	LanguageCode *string              `json:"languageCode"`
	IsVerified   bool                 `json:"isVerified"`
	Source       *string              `json:"source"`
	PlainText    *string              `json:"plainText"`
	Lines        []lyricsLineResponse `json:"lines"`
}

type lyricsLineResponse struct {
	StartMs int    `json:"startMs"`
	EndMs   *int   `json:"endMs"`
	Text    string `json:"text"`
}

func getTrackLyricsHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		trackID, err := parseTrackLyricsID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid track id", http.StatusBadRequest)
			return
		}

		item, err := store.getLyrics(trackID)
		if err != nil {
			switch {
			case errors.Is(err, errTrackNotFound), errors.Is(err, errLyricsNotFound):
				http.NotFound(w, r)
			default:
				writeSentryHTTPError(w, r, err, "failed to fetch lyrics", http.StatusInternalServerError, "lyrics", "fetch")
			}
			return
		}

		writeJSON(w, http.StatusOK, toLyricsResponse(item))
	}
}

func putTrackLyricsHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		trackID, err := parseTrackLyricsID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid track id", http.StatusBadRequest)
			return
		}

		req, err := decodeUpsertLyricsRequest(r)
		if err != nil {
			writeRequestDecodeError(w, err)
			return
		}

		item, created, err := store.upsertLyrics(trackID, req)
		if err != nil {
			switch {
			case errors.Is(err, errTrackNotFound), strings.Contains(err.Error(), "trackId"):
				http.NotFound(w, r)
			case errors.Is(err, errInvalidLyricsPayload):
				http.Error(w, err.Error(), http.StatusBadRequest)
			default:
				writeSentryHTTPError(w, r, err, "failed to save lyrics", http.StatusInternalServerError, "lyrics", "save")
			}
			return
		}

		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, toLyricsResponse(item))
	}
}

func deleteTrackLyricsHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		trackID, err := parseTrackLyricsID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid track id", http.StatusBadRequest)
			return
		}

		if err := store.deleteLyrics(trackID); err != nil {
			switch {
			case errors.Is(err, errTrackNotFound), errors.Is(err, errLyricsNotFound):
				http.NotFound(w, r)
			default:
				writeSentryHTTPError(w, r, err, "failed to delete lyrics", http.StatusInternalServerError, "lyrics", "delete")
			}
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func decodeUpsertLyricsRequest(r *http.Request) (upsertLyricsRequest, error) {
	var req upsertLyricsRequest
	if err := decodeJSON(r, &req); err != nil {
		return upsertLyricsRequest{}, err
	}
	return req, nil
}

func parseTrackLyricsID(path string) (int64, error) {
	return parseResourceID(strings.TrimSuffix(path, "/lyrics"), "/api/tracks/")
}

func (s *trackStore) getLyrics(trackID int64) (lyrics, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, ok := s.tracks[trackID]; !ok {
		return lyrics{}, errTrackNotFound
	}
	item, ok := s.lyricsByTrack[trackID]
	if !ok {
		return lyrics{}, errLyricsNotFound
	}
	return cloneLyrics(item), nil
}

func (s *trackStore) upsertLyrics(trackID int64, req upsertLyricsRequest) (lyrics, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tracks[trackID]; !ok {
		return lyrics{}, false, errTrackNotFound
	}

	previous, hadPrevious := s.lyricsByTrack[trackID]
	snapshot := cloneLyricsMap(s.lyricsByTrack)
	nextLyricsIDSnapshot := s.nextLyricsID
	nextLyricsLineIDSnapshot := s.nextLyricsLineID

	now := time.Now().UTC()
	item := lyrics{
		TrackID:      trackID,
		Type:         normalizeLyricsType(req.Type),
		PlainText:    normalizeOptionalString(req.PlainText),
		LanguageCode: normalizeOptionalString(req.LanguageCode),
		Source:       normalizeOptionalString(req.Source),
		IsVerified:   req.IsVerified,
		UpdatedAt:    now,
		CreatedAt:    now,
		Lines:        make([]syncedLyricLine, 0, len(req.Lines)),
	}
	if hadPrevious {
		item.ID = previous.ID
		item.CreatedAt = previous.CreatedAt
	} else {
		item.ID = s.nextLyricsID
		s.nextLyricsID++
	}
	for index, lineReq := range req.Lines {
		item.Lines = append(item.Lines, syncedLyricLine{
			ID:         s.nextLyricsLineID,
			LyricsID:   item.ID,
			StartMs:    lineReq.StartMs,
			EndMs:      cloneOptionalInt(lineReq.EndMs),
			Text:       strings.TrimSpace(lineReq.Text),
			OrderIndex: index,
		})
		s.nextLyricsLineID++
	}

	if err := validateLyrics(item); err != nil {
		s.nextLyricsID = nextLyricsIDSnapshot
		s.nextLyricsLineID = nextLyricsLineIDSnapshot
		return lyrics{}, false, err
	}

	s.lyricsByTrack[trackID] = item
	if err := s.persistLocked(); err != nil {
		s.lyricsByTrack = snapshot
		s.nextLyricsID = nextLyricsIDSnapshot
		s.nextLyricsLineID = nextLyricsLineIDSnapshot
		return lyrics{}, false, fmt.Errorf("persist lyrics: %w", err)
	}

	return cloneLyrics(item), !hadPrevious, nil
}

func (s *trackStore) deleteLyrics(trackID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tracks[trackID]; !ok {
		return errTrackNotFound
	}
	if _, ok := s.lyricsByTrack[trackID]; !ok {
		return errLyricsNotFound
	}

	snapshot := cloneLyricsMap(s.lyricsByTrack)
	delete(s.lyricsByTrack, trackID)
	if err := s.persistLocked(); err != nil {
		s.lyricsByTrack = snapshot
		return fmt.Errorf("persist lyrics deletion: %w", err)
	}

	return nil
}

func (s *trackStore) validateLyricsStateLocked() error {
	for trackID, item := range s.lyricsByTrack {
		if _, ok := s.tracks[trackID]; !ok {
			return fmt.Errorf("%w: trackId %d does not exist", errInvalidLyricsPayload, trackID)
		}
		if err := validateLyrics(item); err != nil {
			return err
		}
	}
	return nil
}

func validateLyrics(item lyrics) error {
	if item.TrackID <= 0 {
		return fmt.Errorf("%w: trackId is required", errInvalidLyricsPayload)
	}
	if item.ID <= 0 {
		return fmt.Errorf("%w: id is required", errInvalidLyricsPayload)
	}

	switch item.Type {
	case lyricsTypePlain:
		if item.PlainText == nil || strings.TrimSpace(*item.PlainText) == "" {
			return fmt.Errorf("%w: plainText is required for plain lyrics", errInvalidLyricsPayload)
		}
		if len(item.Lines) > 0 {
			return fmt.Errorf("%w: plain lyrics cannot include lines", errInvalidLyricsPayload)
		}
	case lyricsTypeSynced:
		if item.PlainText != nil {
			return fmt.Errorf("%w: synced lyrics cannot include plainText", errInvalidLyricsPayload)
		}
		if len(item.Lines) == 0 {
			return fmt.Errorf("%w: synced lyrics require at least one line", errInvalidLyricsPayload)
		}
	default:
		return fmt.Errorf("%w: type must be plain or synced", errInvalidLyricsPayload)
	}

	for index, line := range item.Lines {
		if line.ID <= 0 {
			return fmt.Errorf("%w: lines[%d].id is required", errInvalidLyricsPayload, index)
		}
		if line.LyricsID != item.ID {
			return fmt.Errorf("%w: lines[%d].lyricsId must match lyrics id", errInvalidLyricsPayload, index)
		}
		if line.OrderIndex != index {
			return fmt.Errorf("%w: lines[%d].orderIndex must be sequential", errInvalidLyricsPayload, index)
		}
		if strings.TrimSpace(line.Text) == "" {
			return fmt.Errorf("%w: lines[%d].text is required", errInvalidLyricsPayload, index)
		}
		if line.StartMs < 0 {
			return fmt.Errorf("%w: lines[%d].startMs must be greater than or equal to 0", errInvalidLyricsPayload, index)
		}
		if line.EndMs != nil && *line.EndMs < line.StartMs {
			return fmt.Errorf("%w: lines[%d].endMs must be greater than or equal to startMs", errInvalidLyricsPayload, index)
		}
		if index == 0 {
			continue
		}

		prev := item.Lines[index-1]
		if line.StartMs <= prev.StartMs {
			return fmt.Errorf("%w: lines must be sorted by startMs in ascending order", errInvalidLyricsPayload)
		}
		if prev.EndMs == nil {
			return fmt.Errorf("%w: lines[%d].endMs is required when another line follows", errInvalidLyricsPayload, index-1)
		}
		if line.StartMs < *prev.EndMs {
			return fmt.Errorf("%w: synced lyrics lines must not overlap", errInvalidLyricsPayload)
		}
	}

	return nil
}

func normalizeLyrics(item lyrics) (lyrics, bool) {
	item.Type = normalizeLyricsType(item.Type)
	item.PlainText = normalizeOptionalString(item.PlainText)
	item.LanguageCode = normalizeOptionalString(item.LanguageCode)
	item.Source = normalizeOptionalString(item.Source)
	item.Lines = normalizeSyncedLyricLines(item.Lines, item.ID)
	if item.ID <= 0 || item.TrackID <= 0 || item.Type == "" {
		return lyrics{}, false
	}
	if err := validateLyrics(item); err != nil {
		return lyrics{}, false
	}
	return item, true
}

func normalizeLyricsType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case lyricsTypePlain:
		return lyricsTypePlain
	case lyricsTypeSynced:
		return lyricsTypeSynced
	default:
		return ""
	}
}

func normalizeSyncedLyricLines(lines []syncedLyricLine, lyricsID int64) []syncedLyricLine {
	if len(lines) == 0 {
		return []syncedLyricLine{}
	}

	normalized := make([]syncedLyricLine, 0, len(lines))
	for index, line := range lines {
		normalized = append(normalized, syncedLyricLine{
			ID:         line.ID,
			LyricsID:   lyricsID,
			StartMs:    line.StartMs,
			EndMs:      cloneOptionalInt(line.EndMs),
			Text:       strings.TrimSpace(line.Text),
			OrderIndex: index,
		})
	}
	return normalized
}

func normalizeOptionalString(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func cloneOptionalInt(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneLyrics(item lyrics) lyrics {
	item.PlainText = normalizeOptionalString(item.PlainText)
	item.LanguageCode = normalizeOptionalString(item.LanguageCode)
	item.Source = normalizeOptionalString(item.Source)
	item.Lines = normalizeSyncedLyricLines(item.Lines, item.ID)
	return item
}

func cloneLyricsMap(src map[int64]lyrics) map[int64]lyrics {
	cloned := make(map[int64]lyrics, len(src))
	for trackID, item := range src {
		cloned[trackID] = cloneLyrics(item)
	}
	return cloned
}

func toLyricsResponse(item lyrics) lyricsResponse {
	lines := make([]lyricsLineResponse, 0, len(item.Lines))
	for _, line := range item.Lines {
		lines = append(lines, lyricsLineResponse{
			StartMs: line.StartMs,
			EndMs:   cloneOptionalInt(line.EndMs),
			Text:    line.Text,
		})
	}
	return lyricsResponse{
		TrackID:      item.TrackID,
		Type:         item.Type,
		LanguageCode: normalizeOptionalString(item.LanguageCode),
		IsVerified:   item.IsVerified,
		Source:       normalizeOptionalString(item.Source),
		PlainText:    normalizeOptionalString(item.PlainText),
		Lines:        lines,
	}
}
