package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	defaultLRCLIBBaseURL      = "https://lrclib.net"
	defaultLRCLIBUserAgent    = "EsketitMusicServer/1.0 (https://esketitmusic.online)"
	defaultLRCLIBTimeout      = 12 * time.Second
	maxLRCLIBResponseSize     = 4 << 20
	maxLyricsSearchCandidates = 20
)

var (
	errLyricsProvider        = errors.New("lyrics provider error")
	errLyricsProviderTimeout = errors.New("lyrics provider timeout")
	lrcTimestampPattern      = regexp.MustCompile(`\[(\d+):(\d{2})(?:[\.:](\d{1,3}))?\]`)
	lrcOffsetPattern         = regexp.MustCompile(`(?i)^\[offset:([+-]?\d+)\]$`)
)

type lyricsSearchRequest struct {
	TrackName   string   `json:"trackName"`
	ArtistNames []string `json:"artistNames"`
	AlbumName   string   `json:"albumName"`
	DurationMs  *int     `json:"durationMs"`
}

type lyricsSearchResponse struct {
	Items []lyricsSearchCandidate `json:"items"`
}

type lyricsSearchCandidate struct {
	Provider     string             `json:"provider"`
	ProviderID   int64              `json:"providerId"`
	Source       string             `json:"source"`
	TrackName    string             `json:"trackName"`
	ArtistName   string             `json:"artistName"`
	AlbumName    string             `json:"albumName"`
	DurationMs   int                `json:"durationMs"`
	Instrumental bool               `json:"instrumental"`
	PlainText    *string            `json:"plainText"`
	SyncedLines  []lyricsSearchLine `json:"syncedLines"`
}

type lyricsSearchLine struct {
	StartMs int    `json:"startMs"`
	EndMs   *int   `json:"endMs"`
	Text    string `json:"text"`
}

// lyricsSearchProvider keeps provider transport details out of the service and handler.
type lyricsSearchProvider interface {
	Search(context.Context, lyricsSearchRequest) ([]lyricsProviderRecord, error)
}

type lyricsProviderRecord struct {
	ID           int64
	TrackName    string
	ArtistName   string
	AlbumName    string
	DurationMs   int
	Instrumental bool
	PlainLyrics  *string
	SyncedLyrics *string
}

type lyricsSearchService struct {
	provider lyricsSearchProvider
}

func newLyricsSearchService(provider lyricsSearchProvider) *lyricsSearchService {
	return &lyricsSearchService{provider: provider}
}

func newLyricsSearchServiceFromEnv() *lyricsSearchService {
	baseURL := strings.TrimSpace(os.Getenv("LRCLIB_BASE_URL"))
	if baseURL == "" {
		baseURL = defaultLRCLIBBaseURL
	}
	return newLyricsSearchService(newLRCLIBProvider(baseURL, &http.Client{Timeout: defaultLRCLIBTimeout}))
}

func (s *lyricsSearchService) Search(ctx context.Context, req lyricsSearchRequest) ([]lyricsSearchCandidate, error) {
	records, err := s.provider.Search(ctx, req)
	if err != nil {
		return nil, err
	}

	byID := make(map[int64]lyricsSearchCandidate, len(records))
	for _, record := range records {
		candidate, ok := mapLyricsProviderRecord(record)
		if !ok {
			continue
		}
		if existing, found := byID[candidate.ProviderID]; found {
			byID[candidate.ProviderID] = mergeLyricsSearchCandidates(existing, candidate)
			continue
		}
		byID[candidate.ProviderID] = candidate
	}

	type rankedCandidate struct {
		candidate lyricsSearchCandidate
		score     int
	}
	ranked := make([]rankedCandidate, 0, len(byID))
	for _, candidate := range byID {
		ranked = append(ranked, rankedCandidate{
			candidate: candidate,
			score:     lyricsCandidateMatchScore(req, candidate),
		})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].candidate.ProviderID < ranked[j].candidate.ProviderID
	})

	if len(ranked) > maxLyricsSearchCandidates {
		ranked = ranked[:maxLyricsSearchCandidates]
	}
	items := make([]lyricsSearchCandidate, 0, len(ranked))
	for _, item := range ranked {
		items = append(items, item.candidate)
	}
	return items, nil
}

type lrclibProvider struct {
	baseURL    string
	httpClient *http.Client
	userAgent  string
}

type lrclibSearchRecord struct {
	ID           int64   `json:"id"`
	TrackName    string  `json:"trackName"`
	ArtistName   string  `json:"artistName"`
	AlbumName    string  `json:"albumName"`
	Duration     float64 `json:"duration"`
	Instrumental bool    `json:"instrumental"`
	PlainLyrics  *string `json:"plainLyrics"`
	SyncedLyrics *string `json:"syncedLyrics"`
}

func newLRCLIBProvider(baseURL string, httpClient *http.Client) *lrclibProvider {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultLRCLIBTimeout}
	}
	return &lrclibProvider{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		httpClient: httpClient,
		userAgent:  defaultLRCLIBUserAgent,
	}
}

func (p *lrclibProvider) Search(ctx context.Context, req lyricsSearchRequest) ([]lyricsProviderRecord, error) {
	endpoint, err := url.Parse(p.baseURL + "/api/search")
	if err != nil {
		return nil, fmt.Errorf("%w: invalid LRCLIB base URL", errLyricsProvider)
	}
	query := endpoint.Query()
	query.Set("track_name", req.TrackName)
	query.Set("artist_name", strings.Join(req.ArtistNames, ", "))
	if req.AlbumName != "" {
		query.Set("album_name", req.AlbumName)
	}
	endpoint.RawQuery = query.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build LRCLIB request: %v", errLyricsProvider, err)
	}
	httpReq.Header.Set("User-Agent", p.userAgent)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isNetworkTimeout(err) {
			log.Printf("LRCLIB search timed out: %v", err)
			return nil, fmt.Errorf("%w: %v", errLyricsProviderTimeout, err)
		}
		log.Printf("LRCLIB search request failed: %v", err)
		return nil, fmt.Errorf("%w: %v", errLyricsProvider, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("LRCLIB search returned status %d", resp.StatusCode)
		return nil, fmt.Errorf("%w: LRCLIB returned status %d", errLyricsProvider, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLRCLIBResponseSize+1))
	if err != nil {
		log.Printf("failed to read LRCLIB search response: %v", err)
		return nil, fmt.Errorf("%w: read LRCLIB response", errLyricsProvider)
	}
	if len(body) > maxLRCLIBResponseSize {
		log.Printf("LRCLIB search response exceeded %d bytes", maxLRCLIBResponseSize)
		return nil, fmt.Errorf("%w: LRCLIB response too large", errLyricsProvider)
	}

	var transportRecords []lrclibSearchRecord
	if err := json.Unmarshal(body, &transportRecords); err != nil {
		log.Printf("failed to decode LRCLIB search response: %v", err)
		return nil, fmt.Errorf("%w: invalid LRCLIB JSON", errLyricsProvider)
	}
	if transportRecords == nil {
		log.Printf("failed to decode LRCLIB search response: expected a JSON array")
		return nil, fmt.Errorf("%w: invalid LRCLIB response shape", errLyricsProvider)
	}

	records := make([]lyricsProviderRecord, 0, len(transportRecords))
	for _, item := range transportRecords {
		durationMs := int(math.Round(item.Duration * 1000))
		if durationMs < 0 {
			durationMs = 0
		}
		records = append(records, lyricsProviderRecord{
			ID:           item.ID,
			TrackName:    item.TrackName,
			ArtistName:   item.ArtistName,
			AlbumName:    item.AlbumName,
			DurationMs:   durationMs,
			Instrumental: item.Instrumental,
			PlainLyrics:  item.PlainLyrics,
			SyncedLyrics: item.SyncedLyrics,
		})
	}
	return records, nil
}

func isNetworkTimeout(err error) bool {
	var networkErr net.Error
	return errors.As(err, &networkErr) && networkErr.Timeout()
}

func lyricsSearchHandler(store *trackStore, service *lyricsSearchService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		trackID, err := parseTrackLyricsSearchID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid track id", http.StatusBadRequest)
			return
		}
		if _, ok := store.get(trackID); !ok {
			http.NotFound(w, r)
			return
		}

		req, err := decodeLyricsSearchRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		items, err := service.Search(r.Context(), req)
		if err != nil {
			if errors.Is(err, errLyricsProviderTimeout) {
				http.Error(w, "lyrics provider timed out", http.StatusGatewayTimeout)
				return
			}
			http.Error(w, "lyrics provider request failed", http.StatusBadGateway)
			return
		}
		writeJSON(w, http.StatusOK, lyricsSearchResponse{Items: items})
	}
}

func decodeLyricsSearchRequest(r *http.Request) (lyricsSearchRequest, error) {
	var req lyricsSearchRequest
	if err := decodeJSON(r, &req); err != nil {
		return lyricsSearchRequest{}, err
	}

	req.TrackName = strings.TrimSpace(req.TrackName)
	if req.TrackName == "" {
		return lyricsSearchRequest{}, errors.New("trackName is required")
	}
	req.AlbumName = strings.TrimSpace(req.AlbumName)
	if req.DurationMs != nil && *req.DurationMs <= 0 {
		return lyricsSearchRequest{}, errors.New("durationMs must be greater than zero")
	}

	seenArtists := make(map[string]struct{}, len(req.ArtistNames))
	artistNames := make([]string, 0, len(req.ArtistNames))
	for _, name := range req.ArtistNames {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, seen := seenArtists[key]; seen {
			continue
		}
		seenArtists[key] = struct{}{}
		artistNames = append(artistNames, name)
	}
	if len(artistNames) == 0 {
		return lyricsSearchRequest{}, errors.New("artistNames must contain at least one non-empty name")
	}
	req.ArtistNames = artistNames
	return req, nil
}

func parseTrackLyricsSearchID(path string) (int64, error) {
	return parseResourceID(strings.TrimSuffix(path, "/lyrics/search"), "/api/tracks/")
}

func mapLyricsProviderRecord(record lyricsProviderRecord) (lyricsSearchCandidate, bool) {
	if record.ID <= 0 {
		return lyricsSearchCandidate{}, false
	}

	plainText := usableLyricsText(record.PlainLyrics)
	syncedLines := []lyricsSearchLine{}
	if record.SyncedLyrics != nil && strings.TrimSpace(*record.SyncedLyrics) != "" {
		parsed, err := parseSyncedLRC(*record.SyncedLyrics)
		if err == nil {
			syncedLines = parsed
		} else {
			log.Printf("LRCLIB record %d contains unusable synced lyrics: %v", record.ID, err)
		}
	}
	if plainText == nil && len(syncedLines) == 0 && !record.Instrumental {
		return lyricsSearchCandidate{}, false
	}

	return lyricsSearchCandidate{
		Provider:     "lrclib",
		ProviderID:   record.ID,
		Source:       "LRCLIB #" + strconv.FormatInt(record.ID, 10),
		TrackName:    record.TrackName,
		ArtistName:   record.ArtistName,
		AlbumName:    record.AlbumName,
		DurationMs:   record.DurationMs,
		Instrumental: record.Instrumental,
		PlainText:    plainText,
		SyncedLines:  syncedLines,
	}, true
}

func usableLyricsText(value *string) *string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	copy := *value
	return &copy
}

func mergeLyricsSearchCandidates(existing, candidate lyricsSearchCandidate) lyricsSearchCandidate {
	if existing.PlainText == nil {
		existing.PlainText = candidate.PlainText
	}
	if len(existing.SyncedLines) == 0 && len(candidate.SyncedLines) > 0 {
		existing.SyncedLines = candidate.SyncedLines
	}
	existing.Instrumental = existing.Instrumental || candidate.Instrumental
	return existing
}

func parseSyncedLRC(value string) ([]lyricsSearchLine, error) {
	offsetMs := 0
	type rawLine struct {
		startMs int
		text    string
		order   int
	}
	rawLines := make([]rawLine, 0)
	order := 0

	for _, sourceLine := range strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(strings.TrimSuffix(sourceLine, "\r"))
		if match := lrcOffsetPattern.FindStringSubmatch(trimmed); match != nil {
			if parsed, err := strconv.Atoi(match[1]); err == nil {
				offsetMs = parsed
			}
			continue
		}

		matches := lrcTimestampPattern.FindAllStringSubmatch(trimmed, -1)
		if len(matches) == 0 {
			continue
		}
		text := strings.TrimSpace(lrcTimestampPattern.ReplaceAllString(trimmed, ""))
		if text == "" {
			continue
		}
		for _, match := range matches {
			startMs, ok := lrcTimestampMilliseconds(match)
			if !ok {
				continue
			}
			rawLines = append(rawLines, rawLine{startMs: startMs, text: text, order: order})
			order++
		}
	}
	if len(rawLines) == 0 {
		return nil, errors.New("synced lyrics contain no usable lines")
	}

	for index := range rawLines {
		rawLines[index].startMs += offsetMs
		if rawLines[index].startMs < 0 {
			rawLines[index].startMs = 0
		}
	}
	sort.SliceStable(rawLines, func(i, j int) bool {
		if rawLines[i].startMs != rawLines[j].startMs {
			return rawLines[i].startMs < rawLines[j].startMs
		}
		return rawLines[i].order < rawLines[j].order
	})

	lines := make([]lyricsSearchLine, 0, len(rawLines))
	for _, raw := range rawLines {
		if len(lines) == 0 || lines[len(lines)-1].StartMs != raw.startMs {
			lines = append(lines, lyricsSearchLine{StartMs: raw.startMs, Text: raw.text})
			continue
		}
		last := &lines[len(lines)-1]
		if !containsMergedLyricText(last.Text, raw.text) {
			last.Text += "\n" + raw.text
		}
	}
	for index := 0; index+1 < len(lines); index++ {
		endMs := lines[index+1].StartMs
		lines[index].EndMs = &endMs
	}
	return lines, nil
}

func lrcTimestampMilliseconds(match []string) (int, bool) {
	minutes, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, false
	}
	seconds, err := strconv.Atoi(match[2])
	if err != nil || seconds >= 60 {
		return 0, false
	}
	fractionMs := 0
	if len(match) > 3 {
		switch len(match[3]) {
		case 1:
			fractionMs, _ = strconv.Atoi(match[3] + "00")
		case 2:
			fractionMs, _ = strconv.Atoi(match[3] + "0")
		case 3:
			fractionMs, _ = strconv.Atoi(match[3])
		}
	}
	return minutes*60*1000 + seconds*1000 + fractionMs, true
}

func containsMergedLyricText(merged, text string) bool {
	for _, existing := range strings.Split(merged, "\n") {
		if existing == text {
			return true
		}
	}
	return false
}

func lyricsCandidateMatchScore(req lyricsSearchRequest, candidate lyricsSearchCandidate) int {
	score := titleMatchScore(req.TrackName, candidate.TrackName)
	score += artistMatchScore(req.ArtistNames, candidate.ArtistName)
	if req.AlbumName != "" && normalizedEqual(req.AlbumName, candidate.AlbumName) {
		score += 30_000
	}
	if req.DurationMs != nil && candidate.DurationMs > 0 {
		difference := absInt(*req.DurationMs - candidate.DurationMs)
		switch {
		case difference <= 2_000:
			score += 20_000 - difference
		case difference < 60_000:
			score += 10_000 - difference/6
		}
	}
	return score
}

func titleMatchScore(requested, candidate string) int {
	if normalizedEqual(requested, candidate) {
		return 1_000_000
	}
	requestedNormalized := normalizeMatchText(requested)
	candidateNormalized := normalizeMatchText(candidate)
	if normalizedContains(candidateNormalized, requestedNormalized) || normalizedContains(requestedNormalized, candidateNormalized) {
		return 300_000
	}
	return minInt(commonWordCount(requestedNormalized, candidateNormalized)*20_000, 200_000)
}

func artistMatchScore(requested []string, candidate string) int {
	best := 0
	for _, artist := range requested {
		artistNormalized := normalizeMatchText(artist)
		candidateNormalized := normalizeMatchText(candidate)
		score := 0
		switch {
		case normalizedEqual(artist, candidate):
			score = 150_000
		case normalizedContains(candidateNormalized, artistNormalized) || normalizedContains(artistNormalized, candidateNormalized):
			score = 100_000
		default:
			score = minInt(commonWordCount(artistNormalized, candidateNormalized)*5_000, 80_000)
		}
		if score > best {
			best = score
		}
	}
	return best
}

func normalizedEqual(left, right string) bool {
	leftNormalized := normalizeMatchText(left)
	rightNormalized := normalizeMatchText(right)
	if leftNormalized == "" || rightNormalized == "" {
		return false
	}
	return leftNormalized == rightNormalized || strings.ReplaceAll(leftNormalized, " ", "") == strings.ReplaceAll(rightNormalized, " ", "")
}

func normalizedContains(value, substring string) bool {
	return value != "" && substring != "" && strings.Contains(value, substring)
}

func normalizeMatchText(value string) string {
	var builder strings.Builder
	spacePending := false
	for _, char := range strings.TrimSpace(value) {
		switch {
		case unicode.IsLetter(char) || unicode.IsNumber(char):
			if spacePending && builder.Len() > 0 {
				builder.WriteByte(' ')
			}
			builder.WriteRune(unicode.ToLower(char))
			spacePending = false
		default:
			spacePending = true
		}
	}
	return builder.String()
}

func commonWordCount(left, right string) int {
	leftWords := strings.Fields(left)
	rightSet := make(map[string]struct{}, len(strings.Fields(right)))
	for _, word := range strings.Fields(right) {
		rightSet[word] = struct{}{}
	}
	seen := make(map[string]struct{}, len(leftWords))
	count := 0
	for _, word := range leftWords {
		if _, duplicate := seen[word]; duplicate {
			continue
		}
		seen[word] = struct{}{}
		if _, ok := rightSet[word]; ok {
			count++
		}
	}
	return count
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
