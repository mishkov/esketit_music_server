package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/getsentry/sentry-go"
)

const (
	defaultDeezerAPIBaseURL         = "https://api.deezer.com"
	defaultDeezerUserAgent          = "EsketitMusicServer/1.0 (https://esketitmusic.online)"
	maxDeezerSearchResponseBytes    = 2 << 20
	deezerCoverExtraLargeDimension  = 1000
	deezerAlbumSearchOperation      = "album"
	deezerTrackSearchOperation      = "track"
	deezerBuildRequestStage         = "build request"
	deezerSendRequestStage          = "request"
	deezerReadResponseStage         = "read response"
	deezerDecodeResponseStage       = "decode response"
	deezerInvalidResponseShapeStage = "validate response"
)

// deezerAlbumCoverSearchError deliberately excludes request URLs, queries, and
// provider response bodies. Those values can contain user input and should not
// reach operational logs or error reporting.
type deezerAlbumCoverSearchError struct {
	Operation    string
	Stage        string
	StatusCode   int
	APIErrorCode int
	Err          error
}

func (e *deezerAlbumCoverSearchError) Error() string {
	prefix := "deezer " + deezerSearchOperationName(e.Operation) + " search"
	switch {
	case e.StatusCode != 0:
		return fmt.Sprintf("%s returned status %d", prefix, e.StatusCode)
	case e.APIErrorCode != 0:
		return fmt.Sprintf("%s returned API error code %d", prefix, e.APIErrorCode)
	case e.Stage != "":
		return fmt.Sprintf("%s failed to %s", prefix, e.Stage)
	default:
		return prefix + " failed"
	}
}

func (e *deezerAlbumCoverSearchError) Unwrap() error {
	return e.Err
}

func deezerSearchOperationName(operation string) string {
	if operation == deezerTrackSearchOperation {
		return deezerTrackSearchOperation
	}
	return deezerAlbumSearchOperation
}

type deezerAlbumCoverSearchProvider struct {
	apiBaseURL string
	client     *http.Client
	userAgent  string
}

func newDeezerAlbumCoverSearchProvider(apiBaseURL string, client *http.Client) *deezerAlbumCoverSearchProvider {
	apiBaseURL = strings.TrimRight(strings.TrimSpace(apiBaseURL), "/")
	if apiBaseURL == "" {
		apiBaseURL = defaultDeezerAPIBaseURL
	}
	if client == nil {
		client = &http.Client{Timeout: albumCoverSearchTimeout}
	}
	return &deezerAlbumCoverSearchProvider{
		apiBaseURL: apiBaseURL,
		client:     client,
		userAgent:  defaultDeezerUserAgent,
	}
}

type deezerSearchResponse struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code int `json:"code"`
	} `json:"error"`
}

type deezerAlbum struct {
	ID          int64  `json:"id"`
	Link        string `json:"link"`
	CoverMedium string `json:"cover_medium"`
	CoverXL     string `json:"cover_xl"`
}

type deezerTrack struct {
	Album deezerAlbum `json:"album"`
}

type deezerSearchResult struct {
	operation string
	albums    []deezerAlbum
	err       error
}

func (p *deezerAlbumCoverSearchProvider) Search(ctx context.Context, query string, limit int) ([]albumCoverSuggestion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return []albumCoverSuggestion{}, nil
	}

	results := make(chan deezerSearchResult, 2)
	go func() {
		albums, err := p.searchAlbums(ctx, query, limit)
		results <- deezerSearchResult{operation: deezerAlbumSearchOperation, albums: albums, err: err}
	}()
	go func() {
		albums, err := p.searchTrackAlbums(ctx, query, limit)
		results <- deezerSearchResult{operation: deezerTrackSearchOperation, albums: albums, err: err}
	}()

	var albumResult, trackResult deezerSearchResult
	for range 2 {
		result := <-results
		if result.operation == deezerAlbumSearchOperation {
			albumResult = result
		} else {
			trackResult = result
		}
	}

	if albumResult.err != nil && trackResult.err != nil {
		return nil, errors.Join(albumResult.err, trackResult.err)
	}
	if albumResult.err != nil {
		log.Printf("Deezer album search failed; continuing with track results: %s", safeOperationalError(albumResult.err))
	}
	if trackResult.err != nil {
		log.Printf("Deezer track search failed; continuing with album results: %s", safeOperationalError(trackResult.err))
	}

	seenAlbumIDs := make(map[int64]struct{}, min(limit, len(albumResult.albums)+len(trackResult.albums)))
	droppedItems := 0
	mapAlbums := func(albums []deezerAlbum) []albumCoverSuggestion {
		items := make([]albumCoverSuggestion, 0, min(limit, len(albums)))
		for _, album := range albums {
			if len(items) >= limit {
				break
			}
			if album.ID <= 0 {
				droppedItems++
				continue
			}
			if _, seen := seenAlbumIDs[album.ID]; seen {
				continue
			}
			suggestion, ok := mapDeezerAlbumCoverSuggestion(album)
			if !ok {
				droppedItems++
				continue
			}
			seenAlbumIDs[album.ID] = struct{}{}
			items = append(items, suggestion)
		}
		return items
	}

	// Mapping direct album matches first gives them precedence when the same
	// album ID occurs in both responses. The balancing step then interleaves the
	// groups so an outer provider quota can take any prefix without losing all
	// track-derived matches.
	albumItems := mapAlbums(albumResult.albums)
	trackItems := mapAlbums(trackResult.albums)
	if droppedItems > 0 {
		log.Printf("Deezer album search omitted %d unusable result(s)", droppedItems)
	}
	return balanceDeezerSuggestions(albumItems, trackItems, limit), nil
}

func balanceDeezerSuggestions(albumItems, trackItems []albumCoverSuggestion, limit int) []albumCoverSuggestion {
	if limit <= 0 {
		return []albumCoverSuggestion{}
	}

	albumLimit := (limit + 1) / 2
	trackLimit := limit / 2
	albumCount := min(len(albumItems), albumLimit)
	trackLimit += albumLimit - albumCount
	trackCount := min(len(trackItems), trackLimit)
	albumCount = min(len(albumItems), albumCount+(trackLimit-trackCount))

	items := make([]albumCoverSuggestion, 0, albumCount+trackCount)
	for index := 0; index < max(albumCount, trackCount); index++ {
		if index < albumCount {
			items = append(items, albumItems[index])
		}
		if index < trackCount {
			items = append(items, trackItems[index])
		}
	}
	return items
}

func (p *deezerAlbumCoverSearchProvider) searchAlbums(ctx context.Context, query string, limit int) ([]deezerAlbum, error) {
	data, err := p.search(ctx, deezerAlbumSearchOperation, query, limit)
	if err != nil {
		return nil, err
	}
	var albums []deezerAlbum
	if err := json.Unmarshal(data, &albums); err != nil {
		return nil, &deezerAlbumCoverSearchError{
			Operation: deezerAlbumSearchOperation,
			Stage:     deezerDecodeResponseStage,
			Err:       err,
		}
	}
	if albums == nil {
		return nil, &deezerAlbumCoverSearchError{
			Operation: deezerAlbumSearchOperation,
			Stage:     deezerInvalidResponseShapeStage,
		}
	}
	return albums, nil
}

func (p *deezerAlbumCoverSearchProvider) searchTrackAlbums(ctx context.Context, query string, limit int) ([]deezerAlbum, error) {
	data, err := p.search(ctx, deezerTrackSearchOperation, query, limit)
	if err != nil {
		return nil, err
	}
	var tracks []deezerTrack
	if err := json.Unmarshal(data, &tracks); err != nil {
		return nil, &deezerAlbumCoverSearchError{
			Operation: deezerTrackSearchOperation,
			Stage:     deezerDecodeResponseStage,
			Err:       err,
		}
	}
	if tracks == nil {
		return nil, &deezerAlbumCoverSearchError{
			Operation: deezerTrackSearchOperation,
			Stage:     deezerInvalidResponseShapeStage,
		}
	}

	albums := make([]deezerAlbum, 0, len(tracks))
	for _, track := range tracks {
		albums = append(albums, track.Album)
	}
	return albums, nil
}

func (p *deezerAlbumCoverSearchProvider) search(ctx context.Context, operation, query string, limit int) (json.RawMessage, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(p.apiBaseURL), "/")
	if baseURL == "" {
		baseURL = defaultDeezerAPIBaseURL
	}
	endpoint, err := url.Parse(baseURL + "/search/" + deezerSearchOperationName(operation))
	if err != nil {
		return nil, &deezerAlbumCoverSearchError{
			Operation: operation,
			Stage:     deezerBuildRequestStage,
			Err:       albumCoverURLCause(err),
		}
	}
	params := endpoint.Query()
	params.Set("q", query)
	params.Set("limit", strconv.Itoa(limit))
	endpoint.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, &deezerAlbumCoverSearchError{
			Operation: operation,
			Stage:     deezerBuildRequestStage,
			Err:       albumCoverURLCause(err),
		}
	}
	req.Header.Set("Accept", "application/json")
	userAgent := strings.TrimSpace(p.userAgent)
	if userAgent == "" {
		userAgent = defaultDeezerUserAgent
	}
	req.Header.Set("User-Agent", userAgent)

	req, span := withAlbumCoverProviderSpan(req, "deezer", "GET deezer."+deezerSearchOperationName(operation)+"_search")
	client := p.client
	if client == nil {
		client = &http.Client{Timeout: albumCoverSearchTimeout}
	}
	resp, err := client.Do(req)
	defer finishAlbumCoverProviderSpan(span, resp, err)
	if err != nil {
		return nil, &deezerAlbumCoverSearchError{
			Operation: operation,
			Stage:     deezerSendRequestStage,
			Err:       albumCoverURLCause(err),
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, &deezerAlbumCoverSearchError{
			Operation:  operation,
			StatusCode: resp.StatusCode,
		}
	}

	var payload deezerSearchResponse
	if err := decodeDeezerJSON(resp.Body, &payload); err != nil {
		if span != nil {
			if errors.Is(err, errAlbumCoverProviderResponseTooLarge) {
				span.Status = sentry.SpanStatusResourceExhausted
			} else {
				span.Status = sentry.SpanStatusDataLoss
			}
		}
		stage := deezerDecodeResponseStage
		if errors.Is(err, errAlbumCoverProviderResponseTooLarge) {
			stage = deezerReadResponseStage
		}
		return nil, &deezerAlbumCoverSearchError{
			Operation: operation,
			Stage:     stage,
			Err:       err,
		}
	}
	if payload.Error != nil {
		if span != nil {
			span.Status = sentry.SpanStatusInternalError
		}
		return nil, &deezerAlbumCoverSearchError{
			Operation:    operation,
			APIErrorCode: payload.Error.Code,
		}
	}
	if len(payload.Data) == 0 || string(payload.Data) == "null" {
		if span != nil {
			span.Status = sentry.SpanStatusDataLoss
		}
		return nil, &deezerAlbumCoverSearchError{
			Operation: operation,
			Stage:     deezerInvalidResponseShapeStage,
		}
	}
	return payload.Data, nil
}

func decodeDeezerJSON(body io.Reader, dst any) error {
	data, err := io.ReadAll(io.LimitReader(body, maxDeezerSearchResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxDeezerSearchResponseBytes {
		return errAlbumCoverProviderResponseTooLarge
	}
	return json.Unmarshal(data, dst)
}

func mapDeezerAlbumCoverSuggestion(album deezerAlbum) (albumCoverSuggestion, bool) {
	if album.ID <= 0 ||
		!isValidExternalHTTPURL(album.CoverMedium) ||
		!isValidExternalHTTPURL(album.CoverXL) {
		return albumCoverSuggestion{}, false
	}

	sourcePageURL := strings.TrimSpace(album.Link)
	if !isValidExternalHTTPURL(sourcePageURL) {
		sourcePageURL = "https://www.deezer.com/album/" + strconv.FormatInt(album.ID, 10)
	}
	suggestion := albumCoverSuggestion{
		ThumbnailURL:  album.CoverMedium,
		ImageURL:      album.CoverXL,
		Width:         deezerCoverExtraLargeDimension,
		Height:        deezerCoverExtraLargeDimension,
		SourcePageURL: sourcePageURL,
	}
	return suggestion, isValidAlbumCoverSuggestion(suggestion)
}
