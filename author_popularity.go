package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	authorPopularitySort            = "popularity"
	authorIDSort                    = "id"
	authorPopularityWindow          = 30 * 24 * time.Hour
	authorPopularityStateLookback   = 24 * time.Hour
	authorPopularityRefreshHour     = 3
	authorPopularityTimezoneEnvName = "AUTHOR_POPULARITY_TIMEZONE"
)

type authorListFilter struct {
	Sort string
}

type authorPopularityEvent struct {
	ID           int64
	ClientID     string
	SessionID    string
	EventType    string
	TrackID      int64
	PositionMs   *int64
	DurationMs   *int64
	MetadataJSON string
	ClientTime   time.Time
}

type authorPopularityEntry struct {
	AuthorID   int64
	ListenedMs int64
	Rank       int
}

type playbackSessionKey struct {
	ClientID  string
	SessionID string
}

type playbackPopularityState struct {
	TrackID    int64
	PositionMs int64
	DurationMs int64
	AnchorTime time.Time
	Playing    bool
}

type seekAnalyticsMetadata struct {
	FromPositionMs *int64 `json:"fromPositionMs"`
	ToPositionMs   *int64 `json:"toPositionMs"`
}

func loadAuthorPopularityLocationFromEnv() (*time.Location, error) {
	name := strings.TrimSpace(os.Getenv(authorPopularityTimezoneEnvName))
	if name == "" {
		return time.Local, nil
	}

	location, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("invalid %s %q: %w", authorPopularityTimezoneEnvName, name, err)
	}
	return location, nil
}

func (s *trackStore) refreshAuthorPopularity(ctx context.Context, snapshotTime time.Time) ([]authorPopularityEntry, error) {
	snapshotTime = snapshotTime.UTC()
	windowStart := snapshotTime.Add(-authorPopularityWindow)

	authorIDs, trackAuthorIDs := s.authorPopularityCatalogSnapshot()
	events, err := s.loadAuthorPopularityEvents(ctx, windowStart, snapshotTime)
	if err != nil {
		return nil, fmt.Errorf("load popularity analytics events: %w", err)
	}

	trackListeningMs := calculateTrackListeningMs(events, windowStart, snapshotTime)
	entries := rankAuthorsByListening(authorIDs, trackAuthorIDs, trackListeningMs)
	if err := s.replaceAuthorPopularitySnapshot(ctx, entries, windowStart, snapshotTime); err != nil {
		return nil, fmt.Errorf("replace author popularity snapshot: %w", err)
	}

	return entries, nil
}

func (s *trackStore) authorPopularityCatalogSnapshot() ([]int64, map[int64][]int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	authorIDs := make([]int64, 0, len(s.authors))
	for authorID := range s.authors {
		authorIDs = append(authorIDs, authorID)
	}

	trackAuthorIDs := make(map[int64][]int64, len(s.tracks))
	for trackID, item := range s.tracks {
		trackAuthorIDs[trackID] = append([]int64(nil), item.AuthorIDs...)
	}
	return authorIDs, trackAuthorIDs
}

func (s *trackStore) loadAuthorPopularityEvents(
	ctx context.Context,
	windowStart time.Time,
	windowEnd time.Time,
) (events []authorPopularityEvent, returnErr error) {
	queryStart := windowStart.Add(-authorPopularityStateLookback).Add(-time.Second)
	queryEnd := windowEnd.Add(time.Second)
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT id, client_id, session_id, event_type, track_id, position_ms, duration_ms, metadata_json, client_time
		FROM analytics_events
		WHERE client_time >= ? AND client_time < ?
			AND event_type IN ('play', 'pause', 'resume', 'seek', 'track_change', 'track_complete', 'track_skip', 'playback_error')`,
		formatSQLiteTime(queryStart),
		formatSQLiteTime(queryEnd),
	)
	if err != nil {
		return nil, err
	}
	defer joinRowsCloseError(&returnErr, rows, "load author popularity analytics")

	for rows.Next() {
		var (
			event         authorPopularityEvent
			trackID       sql.NullInt64
			positionMs    sql.NullInt64
			durationMs    sql.NullInt64
			clientTimeRaw string
		)
		if err := rows.Scan(
			&event.ID,
			&event.ClientID,
			&event.SessionID,
			&event.EventType,
			&trackID,
			&positionMs,
			&durationMs,
			&event.MetadataJSON,
			&clientTimeRaw,
		); err != nil {
			return nil, err
		}
		clientTime, err := parseSQLiteTime(clientTimeRaw)
		if err != nil {
			return nil, fmt.Errorf("parse analytics event id %d client_time: %w", event.ID, err)
		}
		if clientTime.Before(queryStart.Add(time.Second)) || !clientTime.Before(windowEnd) {
			continue
		}
		if trackID.Valid {
			event.TrackID = trackID.Int64
		}
		if positionMs.Valid {
			value := positionMs.Int64
			event.PositionMs = &value
		}
		if durationMs.Valid {
			value := durationMs.Int64
			event.DurationMs = &value
		}
		event.ClientTime = clientTime
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.SliceStable(events, func(i, j int) bool {
		if events[i].ClientID != events[j].ClientID {
			return events[i].ClientID < events[j].ClientID
		}
		if events[i].SessionID != events[j].SessionID {
			return events[i].SessionID < events[j].SessionID
		}
		if !events[i].ClientTime.Equal(events[j].ClientTime) {
			return events[i].ClientTime.Before(events[j].ClientTime)
		}
		return events[i].ID < events[j].ID
	})
	return events, nil
}

func calculateTrackListeningMs(events []authorPopularityEvent, windowStart, windowEnd time.Time) map[int64]int64 {
	totals := make(map[int64]int64)
	states := make(map[playbackSessionKey]playbackPopularityState)

	for _, event := range events {
		if event.TrackID <= 0 || event.ClientTime.Before(windowStart.Add(-authorPopularityStateLookback)) || !event.ClientTime.Before(windowEnd) {
			continue
		}
		key := playbackSessionKey{ClientID: event.ClientID, SessionID: event.SessionID}
		state, hasState := states[key]

		switch event.EventType {
		case analyticsEventPlay:
			states[key] = newPlaybackPopularityState(event, true, true)

		case analyticsEventResume:
			if !hasState || state.TrackID != event.TrackID {
				states[key] = newPlaybackPopularityState(event, true, true)
				continue
			}
			state.PositionMs = analyticsPositionOrDefault(event.PositionMs, state.PositionMs)
			state.DurationMs = analyticsDurationOrDefault(event.DurationMs, state.DurationMs)
			state.AnchorTime = event.ClientTime
			state.Playing = true
			states[key] = state

		case analyticsEventPause:
			if !hasState || state.TrackID != event.TrackID {
				states[key] = newPlaybackPopularityState(event, false, true)
				continue
			}
			state.DurationMs = analyticsDurationOrDefault(event.DurationMs, state.DurationMs)
			if state.Playing && event.PositionMs != nil {
				addPlaybackPositionDelta(totals, state, *event.PositionMs, event.ClientTime, windowStart, windowEnd)
				state.PositionMs = *event.PositionMs
			}
			state.AnchorTime = event.ClientTime
			state.Playing = false
			states[key] = state

		case analyticsEventSeek:
			if !hasState || state.TrackID != event.TrackID {
				states[key] = newPlaybackPopularityState(event, false, true)
				continue
			}
			state.DurationMs = analyticsDurationOrDefault(event.DurationMs, state.DurationMs)
			metadata := decodeSeekAnalyticsMetadata(event.MetadataJSON)
			if state.Playing && metadata.FromPositionMs != nil {
				addPlaybackPositionDelta(totals, state, *metadata.FromPositionMs, event.ClientTime, windowStart, windowEnd)
			}
			switch {
			case metadata.ToPositionMs != nil:
				state.PositionMs = *metadata.ToPositionMs
			case event.PositionMs != nil:
				state.PositionMs = *event.PositionMs
			}
			state.AnchorTime = event.ClientTime
			states[key] = state

		case analyticsEventTrackSkip, analyticsEventTrackComplete, analyticsEventPlaybackError:
			if hasState && state.TrackID == event.TrackID {
				state.DurationMs = analyticsDurationOrDefault(event.DurationMs, state.DurationMs)
				if state.Playing && event.PositionMs != nil {
					addPlaybackPositionDelta(totals, state, *event.PositionMs, event.ClientTime, windowStart, windowEnd)
				}
			}
			delete(states, key)

		case analyticsEventTrackChange:
			// The client currently emits durationMs before its duration state has
			// switched to the new track, so do not use it as the new track's cap.
			states[key] = newPlaybackPopularityState(event, true, false)
		}
	}

	return totals
}

func newPlaybackPopularityState(event authorPopularityEvent, playing, includeDuration bool) playbackPopularityState {
	state := playbackPopularityState{
		TrackID:    event.TrackID,
		PositionMs: analyticsPositionOrDefault(event.PositionMs, 0),
		AnchorTime: event.ClientTime,
		Playing:    playing,
	}
	if includeDuration {
		state.DurationMs = analyticsDurationOrDefault(event.DurationMs, 0)
	}
	return state
}

func analyticsPositionOrDefault(value *int64, fallback int64) int64 {
	if value == nil || *value < 0 {
		return fallback
	}
	return *value
}

func analyticsDurationOrDefault(value *int64, fallback int64) int64 {
	if value == nil || *value <= 0 {
		return fallback
	}
	return *value
}

func decodeSeekAnalyticsMetadata(value string) seekAnalyticsMetadata {
	var metadata seekAnalyticsMetadata
	if err := json.Unmarshal([]byte(value), &metadata); err != nil {
		return seekAnalyticsMetadata{}
	}
	return metadata
}

func addPlaybackPositionDelta(
	totals map[int64]int64,
	state playbackPopularityState,
	targetPositionMs int64,
	eventTime time.Time,
	windowStart time.Time,
	windowEnd time.Time,
) {
	startPosition := clampPlaybackPosition(state.PositionMs, state.DurationMs)
	endPosition := clampPlaybackPosition(targetPositionMs, state.DurationMs)
	delta := endPosition - startPosition
	if delta <= 0 {
		return
	}

	delta = listeningDeltaWithinWindow(delta, state.AnchorTime, eventTime, windowStart, windowEnd)
	if delta <= 0 {
		return
	}
	totals[state.TrackID] += delta
}

func clampPlaybackPosition(value, durationMs int64) int64 {
	if value < 0 {
		return 0
	}
	if durationMs > 0 && value > durationMs {
		return durationMs
	}
	return value
}

func listeningDeltaWithinWindow(delta int64, segmentStart, segmentEnd, windowStart, windowEnd time.Time) int64 {
	if delta <= 0 || segmentEnd.Before(segmentStart) || !segmentEnd.After(windowStart) || !segmentStart.Before(windowEnd) {
		return 0
	}
	overlapStart := segmentStart
	if overlapStart.Before(windowStart) {
		overlapStart = windowStart
	}
	overlapEnd := segmentEnd
	if overlapEnd.After(windowEnd) {
		overlapEnd = windowEnd
	}
	if !overlapEnd.After(overlapStart) {
		return 0
	}

	segmentDuration := segmentEnd.Sub(segmentStart)
	if segmentDuration <= 0 || (overlapStart.Equal(segmentStart) && overlapEnd.Equal(segmentEnd)) {
		return delta
	}
	overlapDuration := overlapEnd.Sub(overlapStart)
	return int64(float64(delta) * float64(overlapDuration) / float64(segmentDuration))
}

func rankAuthorsByListening(
	authorIDs []int64,
	trackAuthorIDs map[int64][]int64,
	trackListeningMs map[int64]int64,
) []authorPopularityEntry {
	totals := make(map[int64]int64, len(authorIDs))
	for _, authorID := range authorIDs {
		totals[authorID] = 0
	}
	for trackID, listenedMs := range trackListeningMs {
		if listenedMs <= 0 {
			continue
		}
		for _, authorID := range trackAuthorIDs[trackID] {
			if _, exists := totals[authorID]; exists {
				totals[authorID] += listenedMs
			}
		}
	}

	entries := make([]authorPopularityEntry, 0, len(totals))
	for authorID, listenedMs := range totals {
		entries = append(entries, authorPopularityEntry{AuthorID: authorID, ListenedMs: listenedMs})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].ListenedMs != entries[j].ListenedMs {
			return entries[i].ListenedMs > entries[j].ListenedMs
		}
		return entries[i].AuthorID < entries[j].AuthorID
	})
	for index := range entries {
		entries[index].Rank = index + 1
	}
	return entries
}

func (s *trackStore) replaceAuthorPopularitySnapshot(
	ctx context.Context,
	entries []authorPopularityEntry,
	windowStart time.Time,
	windowEnd time.Time,
) (returnErr error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			joinRollbackError(&returnErr, tx, "replace author popularity snapshot")
		}
	}()

	if _, err := tx.ExecContext(ctx, `DELETE FROM author_popularity_snapshot`); err != nil {
		return err
	}
	calculatedAt := formatSQLiteTime(time.Now().UTC())
	windowStartValue := formatSQLiteTime(windowStart)
	windowEndValue := formatSQLiteTime(windowEnd)
	for _, entry := range entries {
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO author_popularity_snapshot (
				author_id, ranking_position, listened_ms, calculated_at, window_started_at, window_ended_at
			) VALUES (?, ?, ?, ?, ?, ?)`,
			entry.AuthorID,
			entry.Rank,
			entry.ListenedMs,
			calculatedAt,
			windowStartValue,
			windowEndValue,
		); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

func runAuthorPopularityScheduler(ctx context.Context, store *trackStore, location *time.Location) {
	for {
		now := time.Now()
		nextRefresh := nextAuthorPopularityRefresh(now, location)
		timer := time.NewTimer(time.Until(nextRefresh))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		entries, err := store.refreshAuthorPopularity(ctx, time.Now())
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return
			}
			log.Printf("author popularity refresh failed: %s", safeOperationalError(err))
			captureSentryError(ctx, err, "database", "author_popularity.refresh")
			continue
		}
		log.Printf("author popularity refreshed authors=%d window_days=%d", len(entries), int(authorPopularityWindow/(24*time.Hour)))
	}
}

func nextAuthorPopularityRefresh(now time.Time, location *time.Location) time.Time {
	if location == nil {
		location = time.Local
	}
	localNow := now.In(location)
	next := time.Date(
		localNow.Year(),
		localNow.Month(),
		localNow.Day(),
		authorPopularityRefreshHour,
		0,
		0,
		0,
		location,
	)
	if !next.After(localNow) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}
