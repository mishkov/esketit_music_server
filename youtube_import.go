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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	youtube "github.com/kkdai/youtube/v2"
)

const (
	youtubeImportStatusActive    = "active"
	youtubeImportStatusCompleted = "completed"

	youtubeImportSourceTrack    = "track"
	youtubeImportSourcePlaylist = "playlist"
	youtubeImportSourceArtist   = "artist"

	youtubeAddModeCreate = "create"
	youtubeAddModeAttach = "attach"

	youtubeSuggestionTypeExactSourceMatch = "exact_source_match"
	youtubeSuggestionTypePossibleTrack    = "possible_track_match"
)

var (
	errYouTubeInvalidURL      = errors.New("invalid youtube or youtube music url")
	errYouTubeNoTracks        = errors.New("no importable youtube tracks found")
	errYouTubeSessionActive   = errors.New("youtube import session is already active")
	errYouTubeSessionNotFound = errors.New("youtube import session not found")
	errYouTubeCurrentConflict = errors.New("current youtube item is already connected to an existing track")
	errYouTubeUnsupportedMode = errors.New("unsupported youtube import add mode")
	errYouTubeCurrentNotFound = errors.New("youtube import item not found")
	errYouTubeDownloadFailed  = errors.New("failed to download youtube track audio")
	errYouTubeTranscodeFailed = errors.New("failed to transcode youtube track audio")
)

var (
	youtubePlaylistIDPattern = regexp.MustCompile(`(?:playlist\?list=|["']playlistId["']\s*:\s*["'])([A-Za-z0-9_-]{10,})`)
	youtubeVideoIDPattern    = regexp.MustCompile(`(?:watch\?v=|youtu\.be/|["']videoId["']\s*:\s*["'])([A-Za-z0-9_-]{10,})`)
)

type youtubeImportConfig struct {
	ImportTempDir   string
	RequestTimeout  time.Duration
	DownloadTimeout time.Duration
	YTDLPBinary     string
	FFmpegBinary    string
}

type ytdlpFlatPlaylistDump struct {
	Title    string                   `json:"title"`
	Channel  string                   `json:"channel"`
	Uploader string                   `json:"uploader"`
	Entries  []ytdlpFlatPlaylistEntry `json:"entries"`
}

type ytdlpFlatPlaylistEntry struct {
	Type       string                   `json:"_type"`
	ID         string                   `json:"id"`
	Title      string                   `json:"title"`
	URL        string                   `json:"url"`
	WebpageURL string                   `json:"webpage_url"`
	Channel    string                   `json:"channel"`
	Uploader   string                   `json:"uploader"`
	Duration   float64                  `json:"duration"`
	Timestamp  int64                    `json:"timestamp"`
	UploadDate string                   `json:"upload_date"`
	Entries    []ytdlpFlatPlaylistEntry `json:"entries"`
	Thumbnails []struct {
		URL string `json:"url"`
	} `json:"thumbnails"`
}

type youtubeImportItem struct {
	VideoID           string
	SourceURL         string
	OriginalSourceURL string
	LinkProvider      string
	ParsedTitle       string
	ParsedAuthorNames []string
	ParsedAlbumTitle  string
	ParsedReleaseDate *time.Time
	CoverImageURL     string
	DurationSeconds   int
}

type youtubeImportScanSource struct {
	SourceType   string
	CanonicalURL string
}

type youtubeImportSuggestion struct {
	Type       string         `json:"type"`
	TrackID    int64          `json:"trackId"`
	Confidence float64        `json:"confidence"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

type youtubeCurrentItemDTO struct {
	SourceType        string                    `json:"sourceType"`
	SourceURL         string                    `json:"sourceUrl"`
	OriginalSourceURL string                    `json:"originalSourceUrl"`
	VideoID           string                    `json:"videoId"`
	ParsedTitle       string                    `json:"parsedTitle"`
	ParsedAuthorNames []string                  `json:"parsedAuthorNames"`
	ParsedAlbumTitle  string                    `json:"parsedAlbumTitle,omitempty"`
	ParsedReleaseDate *time.Time                `json:"parsedReleaseDate,omitempty"`
	CoverImageURL     string                    `json:"coverImageUrl,omitempty"`
	DurationSeconds   int                       `json:"durationSeconds,omitempty"`
	Suggestions       []youtubeImportSuggestion `json:"suggestions"`
}

type youtubeImportProgressDTO struct {
	Total     int `json:"total"`
	Processed int `json:"processed"`
	Remaining int `json:"remaining"`
	Skipped   int `json:"skipped"`
	Saved     int `json:"saved"`
}

type youtubeImportSessionDTO struct {
	SessionID   string                   `json:"sessionId"`
	Status      string                   `json:"status"`
	SourceType  string                   `json:"sourceType"`
	SourceURL   string                   `json:"sourceUrl"`
	CurrentItem *youtubeCurrentItemDTO   `json:"currentItem,omitempty"`
	Progress    youtubeImportProgressDTO `json:"progress"`
	CreatedAt   time.Time                `json:"createdAt"`
	UpdatedAt   time.Time                `json:"updatedAt"`
}

type youtubeStartImportRequest struct {
	URL               string     `json:"url"`
	ReleaseDateCutoff *time.Time `json:"releaseDateCutoff,omitempty"`
	ReplaceExisting   bool       `json:"replaceExisting"`
	YouTubeCookies    string     `json:"youtubeCookies,omitempty"`
}

type youtubeAddCurrentRequest struct {
	Mode       string  `json:"mode"`
	TrackID    int64   `json:"trackId,omitempty"`
	Name       string  `json:"name,omitempty"`
	AuthorIDs  []int64 `json:"authorIds,omitempty"`
	AlbumID    int64   `json:"albumId,omitempty"`
	AlbumOrder int     `json:"albumOrder,omitempty"`
}

type youtubeSkippedItem struct {
	VideoID   string `json:"videoId"`
	SourceURL string `json:"sourceUrl"`
	Title     string `json:"title"`
}

type youtubeImportSession struct {
	ID                string
	UserID            int64
	Status            string
	SourceType        string
	SourceURL         string
	ReleaseDateCutoff *time.Time
	CookiesPath       string
	Items             []youtubeImportItem
	CurrentIndex      int
	SkippedItems      []youtubeSkippedItem
	SavedCount        int
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type youtubeImportGateway interface {
	Scan(ctx context.Context, rawURL string, cutoff *time.Time, cookiesPath string) ([]youtubeImportItem, youtubeImportScanSource, error)
	DownloadAudio(ctx context.Context, item youtubeImportItem, destinationPath string, cookiesPath string) error
}

type youtubeImportService struct {
	mu       sync.Mutex
	cfg      youtubeImportConfig
	gateway  youtubeImportGateway
	store    *trackStore
	songsDir string
	sessions map[int64]*youtubeImportSession
}

type youtubeCookiesStatus struct {
	Configured   bool       `json:"configured"`
	FilePresent  bool       `json:"filePresent"`
	LastModified *time.Time `json:"lastModified,omitempty"`
}

type youtubeCookieStore struct {
	mu   sync.Mutex
	path string
}

func newYouTubeCookieStore(path string) *youtubeCookieStore {
	return &youtubeCookieStore{path: strings.TrimSpace(path)}
}

func (s *youtubeCookieStore) pathIfPresent() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(s.path) == "" {
		return "", false
	}
	info, err := os.Stat(s.path)
	if err != nil || info.IsDir() || info.Size() == 0 {
		return "", false
	}
	return s.path, true
}

func (s *youtubeCookieStore) Status() youtubeCookiesStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	status := youtubeCookiesStatus{Configured: strings.TrimSpace(s.path) != ""}
	if !status.Configured {
		return status
	}
	info, err := os.Stat(s.path)
	if err != nil || info.IsDir() || info.Size() == 0 {
		return status
	}
	lastModified := info.ModTime()
	status.FilePresent = true
	status.LastModified = &lastModified
	return status
}

func (s *youtubeCookieStore) Replace(r io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(s.path) == "" {
		return errors.New("youtube cookies file is not configured")
	}
	dir := filepath.Dir(s.path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tempPath := s.path + ".tmp"
	file, err := os.OpenFile(tempPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, r); err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	info, err := os.Stat(tempPath)
	if err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	if info.Size() == 0 {
		_ = os.Remove(tempPath)
		return errors.New("uploaded file is empty")
	}
	return os.Rename(tempPath, s.path)
}

func (s *youtubeCookieStore) Delete() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(s.path) == "" {
		return errors.New("youtube cookies file is not configured")
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func newYouTubeImportService(cfg youtubeImportConfig, gateway youtubeImportGateway, store *trackStore, songsDir string) *youtubeImportService {
	return &youtubeImportService{
		cfg:      cfg,
		gateway:  gateway,
		store:    store,
		songsDir: songsDir,
		sessions: make(map[int64]*youtubeImportSession),
	}
}

func (s *youtubeImportService) StartSession(ctx context.Context, userID int64, rawURL string, cutoff *time.Time, replaceExisting bool, youtubeCookies string) (youtubeImportSessionDTO, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return youtubeImportSessionDTO{}, errors.New("url is required")
	}

	cookiesPath, cleanupCookies, err := s.writeSessionCookies(userID, youtubeCookies)
	if err != nil {
		return youtubeImportSessionDTO{}, err
	}
	committedCookies := false
	defer func() {
		if !committedCookies {
			cleanupCookies()
		}
	}()

	s.mu.Lock()
	if existing, ok := s.sessions[userID]; ok && existing.Status == youtubeImportStatusActive && !replaceExisting {
		s.mu.Unlock()
		return youtubeImportSessionDTO{}, errYouTubeSessionActive
	}
	s.mu.Unlock()

	items, source, err := s.gateway.Scan(ctx, rawURL, cutoff, cookiesPath)
	if err != nil {
		return youtubeImportSessionDTO{}, err
	}
	if len(items) == 0 {
		return youtubeImportSessionDTO{}, errYouTubeNoTracks
	}

	sessionID, err := randomToken(16)
	if err != nil {
		return youtubeImportSessionDTO{}, err
	}

	now := time.Now().UTC()
	session := &youtubeImportSession{
		ID:                sessionID,
		UserID:            userID,
		Status:            youtubeImportStatusActive,
		SourceType:        source.SourceType,
		SourceURL:         source.CanonicalURL,
		ReleaseDateCutoff: cutoff,
		CookiesPath:       cookiesPath,
		Items:             items,
		SkippedItems:      []youtubeSkippedItem{},
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	s.mu.Lock()
	if existing, ok := s.sessions[userID]; ok {
		s.cleanupSessionFilesLocked(existing)
	}
	s.sessions[userID] = session
	s.mu.Unlock()
	committedCookies = true

	return s.CurrentSession(userID)
}

func (s *youtubeImportService) writeSessionCookies(userID int64, rawCookies string) (string, func(), error) {
	rawCookies = strings.TrimSpace(rawCookies)
	if rawCookies == "" {
		return "", func() {}, nil
	}
	if err := os.MkdirAll(s.cfg.ImportTempDir, 0o700); err != nil {
		return "", func() {}, err
	}
	token, err := randomToken(12)
	if err != nil {
		return "", func() {}, err
	}
	path := filepath.Join(s.cfg.ImportTempDir, fmt.Sprintf("youtube-cookies-%d-%s.txt", userID, token))
	if err := os.WriteFile(path, []byte(rawCookies+"\n"), 0o600); err != nil {
		return "", func() {}, err
	}
	return path, func() { _ = os.Remove(path) }, nil
}

func (s *youtubeImportService) CurrentSession(userID int64) (youtubeImportSessionDTO, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	session, ok := s.sessions[userID]
	if !ok {
		return youtubeImportSessionDTO{}, errYouTubeSessionNotFound
	}
	return s.buildSessionDTOLocked(session), nil
}

func (s *youtubeImportService) SkipCurrent(userID int64) (youtubeImportSessionDTO, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	session, ok := s.sessions[userID]
	if !ok {
		return youtubeImportSessionDTO{}, errYouTubeSessionNotFound
	}
	item, ok := session.currentItem()
	if !ok {
		return s.buildSessionDTOLocked(session), nil
	}
	session.SkippedItems = append(session.SkippedItems, youtubeSkippedItem{
		VideoID:   item.VideoID,
		SourceURL: item.SourceURL,
		Title:     item.ParsedTitle,
	})
	session.CurrentIndex++
	session.UpdatedAt = time.Now().UTC()
	if session.CurrentIndex >= len(session.Items) {
		session.Status = youtubeImportStatusCompleted
		s.cleanupSessionFilesLocked(session)
	}
	return s.buildSessionDTOLocked(session), nil
}

func (s *youtubeImportService) AddCurrent(ctx context.Context, userID int64, req youtubeAddCurrentRequest) (youtubeImportSessionDTO, track, error) {
	s.mu.Lock()
	session, ok := s.sessions[userID]
	if !ok {
		s.mu.Unlock()
		return youtubeImportSessionDTO{}, track{}, errYouTubeSessionNotFound
	}
	itemIndex := session.CurrentIndex
	item, ok := session.currentItem()
	cookiesPath := session.CookiesPath
	s.mu.Unlock()
	if !ok {
		current, currentErr := s.CurrentSession(userID)
		return current, track{}, currentErr
	}

	generatedAdditionalInfo, generatedSourceMetadata, err := buildYouTubeImportMetadata(item)
	if err != nil {
		return youtubeImportSessionDTO{}, track{}, err
	}

	switch strings.ToLower(strings.TrimSpace(req.Mode)) {
	case youtubeAddModeCreate:
		if _, exists := s.store.findTrackBySourceMetadata(generatedSourceMetadata[0]); exists {
			return youtubeImportSessionDTO{}, track{}, errYouTubeCurrentConflict
		}

		audioPath, cleanup, err := s.downloadCurrentToSongs(ctx, itemIndex, item, cookiesPath)
		if err != nil {
			return youtubeImportSessionDTO{}, track{}, err
		}
		committed := false
		defer func() {
			cleanup(committed)
		}()

		createdTrack, err := s.store.create(upsertTrackRequest{
			Name:           strings.TrimSpace(req.Name),
			AuthorIDs:      req.AuthorIDs,
			AlbumID:        req.AlbumID,
			AlbumOrder:     req.AlbumOrder,
			AudioFilePath:  audioPath,
			AdditionalInfo: generatedAdditionalInfo,
			SourceMetadata: generatedSourceMetadata,
		})
		if err != nil {
			return youtubeImportSessionDTO{}, track{}, err
		}
		committed = true
		return s.advanceSaved(userID, createdTrack)
	case youtubeAddModeAttach:
		updatedTrack, exists, err := s.store.attachTrackImportMetadata(req.TrackID, generatedAdditionalInfo, generatedSourceMetadata)
		if err != nil {
			return youtubeImportSessionDTO{}, track{}, err
		}
		if !exists {
			return youtubeImportSessionDTO{}, track{}, errTrackNotFound
		}
		return s.advanceSaved(userID, updatedTrack)
	default:
		return youtubeImportSessionDTO{}, track{}, errYouTubeUnsupportedMode
	}
}

func (s *youtubeImportService) advanceSaved(userID int64, savedTrack track) (youtubeImportSessionDTO, track, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	session, ok := s.sessions[userID]
	if !ok {
		return youtubeImportSessionDTO{}, track{}, errYouTubeSessionNotFound
	}
	session.CurrentIndex++
	session.SavedCount++
	session.UpdatedAt = time.Now().UTC()
	if session.CurrentIndex >= len(session.Items) {
		session.Status = youtubeImportStatusCompleted
		s.cleanupSessionFilesLocked(session)
	}
	return s.buildSessionDTOLocked(session), savedTrack, nil
}

func (s *youtubeImportService) CancelSession(userID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.sessions[userID]; !ok {
		return errYouTubeSessionNotFound
	}
	s.cleanupSessionFilesLocked(s.sessions[userID])
	delete(s.sessions, userID)
	return nil
}

func (s *youtubeImportService) cleanupSessionFilesLocked(session *youtubeImportSession) {
	if strings.TrimSpace(session.CookiesPath) != "" {
		_ = os.Remove(session.CookiesPath)
		session.CookiesPath = ""
	}
}

func (s *youtubeImportService) buildSessionDTOLocked(session *youtubeImportSession) youtubeImportSessionDTO {
	progress := youtubeImportProgressDTO{
		Total:     len(session.Items),
		Processed: session.CurrentIndex,
		Remaining: max(0, len(session.Items)-session.CurrentIndex),
		Skipped:   len(session.SkippedItems),
		Saved:     session.SavedCount,
	}
	dto := youtubeImportSessionDTO{
		SessionID:  session.ID,
		Status:     session.Status,
		SourceType: session.SourceType,
		SourceURL:  session.SourceURL,
		Progress:   progress,
		CreatedAt:  session.CreatedAt,
		UpdatedAt:  session.UpdatedAt,
	}
	item, ok := session.currentItem()
	if !ok {
		return dto
	}
	dto.CurrentItem = &youtubeCurrentItemDTO{
		SourceType:        session.SourceType,
		SourceURL:         item.SourceURL,
		OriginalSourceURL: item.OriginalSourceURL,
		VideoID:           item.VideoID,
		ParsedTitle:       item.ParsedTitle,
		ParsedAuthorNames: append([]string(nil), item.ParsedAuthorNames...),
		ParsedAlbumTitle:  item.ParsedAlbumTitle,
		ParsedReleaseDate: cloneTimePointer(item.ParsedReleaseDate),
		CoverImageURL:     item.CoverImageURL,
		DurationSeconds:   item.DurationSeconds,
		Suggestions:       s.store.youtubeImportSuggestions(item),
	}
	return dto
}

func (s *youtubeImportService) downloadCurrentToSongs(ctx context.Context, index int, item youtubeImportItem, cookiesPath string) (string, func(bool), error) {
	if err := os.MkdirAll(s.cfg.ImportTempDir, 0o755); err != nil {
		return "", func(bool) {}, err
	}
	fileName, err := youtubeImportFileName(item)
	if err != nil {
		return "", func(bool) {}, err
	}
	tempSourceName, err := randomizedStoredFileName("youtube-source-" + item.VideoID + ".bin")
	if err != nil {
		return "", func(bool) {}, err
	}
	tempSourcePath := filepath.Join(s.cfg.ImportTempDir, fmt.Sprintf("%03d-%s", index+1, tempSourceName))
	if err := s.gateway.DownloadAudio(ctx, item, tempSourcePath, cookiesPath); err != nil {
		_ = os.Remove(tempSourcePath)
		return "", func(bool) {}, err
	}

	finalName, err := uniqueFileName(s.songsDir, fileName)
	if err != nil {
		_ = os.Remove(tempSourcePath)
		return "", func(bool) {}, err
	}
	finalPath := filepath.Join(s.songsDir, finalName)
	if err := s.transcodeToMP3(ctx, tempSourcePath, finalPath); err != nil {
		_ = os.Remove(tempSourcePath)
		_ = os.Remove(finalPath)
		return "", func(bool) {}, err
	}
	_ = os.Remove(tempSourcePath)

	cleanup := func(committed bool) {
		if committed {
			return
		}
		_ = os.Remove(finalPath)
	}
	return "/api/songs/" + url.PathEscape(finalName), cleanup, nil
}

func youtubeImportFileName(item youtubeImportItem) (string, error) {
	baseName := strings.TrimSpace(item.ParsedTitle)
	if baseName == "" {
		baseName = "youtube-track-" + item.VideoID
	}
	baseName = strings.ReplaceAll(baseName, "/", "-")
	baseName = strings.ReplaceAll(baseName, "\\", "-")
	ext := ".mp3"
	return sanitizeSongFileName(baseName + ext)
}

func (s *youtubeImportService) transcodeToMP3(ctx context.Context, sourcePath, targetPath string) error {
	binaryName := strings.TrimSpace(s.cfg.FFmpegBinary)
	if binaryName == "" {
		binaryName = "ffmpeg"
	}
	if _, err := exec.LookPath(binaryName); err != nil {
		log.Printf("youtube transcode failed stage=ffmpeg_lookup source=%q target=%q err=%v", sourcePath, targetPath, err)
		return fmt.Errorf("%w: ffmpeg_lookup", errYouTubeTranscodeFailed)
	}
	args := []string{
		"-y",
		"-i", sourcePath,
		"-vn",
		"-codec:a", "libmp3lame",
		"-q:a", "2",
		targetPath,
	}
	cmd := exec.CommandContext(ctx, binaryName, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("youtube transcode failed stage=ffmpeg_run source=%q target=%q err=%v output=%s", sourcePath, targetPath, err, strings.TrimSpace(string(output)))
		return fmt.Errorf("%w: ffmpeg_run", errYouTubeTranscodeFailed)
	}
	info, err := os.Stat(targetPath)
	if err != nil || info.Size() == 0 {
		if err == nil {
			err = errors.New("ffmpeg produced empty output file")
		}
		log.Printf("youtube transcode failed stage=ffmpeg_output source=%q target=%q err=%v", sourcePath, targetPath, err)
		return fmt.Errorf("%w: ffmpeg_output", errYouTubeTranscodeFailed)
	}
	return nil
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func buildYouTubeImportMetadata(item youtubeImportItem) ([]additionalInfo, []sourceMetadata, error) {
	sourceMetadataItem := sourceMetadata{
		"provider": "youtube",
		"kind":     "track",
		"identity": map[string]any{
			"videoId": item.VideoID,
		},
		"url": item.SourceURL,
	}
	if err := validateSourceMetadata([]sourceMetadata{sourceMetadataItem}); err != nil {
		return nil, nil, err
	}
	link := additionalInfo{
		"type":     "external_link",
		"provider": item.LinkProvider,
		"url":      item.SourceURL,
	}
	if title := externalLinkTitle(item.LinkProvider); title != "" {
		link["title"] = title
	}
	if id, err := randomToken(8); err == nil {
		link["id"] = id
	}
	if err := validateAdditionalInfo([]additionalInfo{link}); err != nil {
		return nil, nil, err
	}
	return []additionalInfo{link}, []sourceMetadata{sourceMetadataItem}, nil
}

func externalLinkTitle(provider string) string {
	switch provider {
	case "youtube_music":
		return "YouTube Music"
	case "youtube":
		return "YouTube"
	default:
		return ""
	}
}

func (s *youtubeImportSession) currentItem() (youtubeImportItem, bool) {
	if s.CurrentIndex < 0 || s.CurrentIndex >= len(s.Items) {
		return youtubeImportItem{}, false
	}
	return s.Items[s.CurrentIndex], true
}

type liveYouTubeImportGateway struct {
	client      *youtube.Client
	httpClient  *http.Client
	cfg         youtubeImportConfig
	cookieStore *youtubeCookieStore
}

func newLiveYouTubeImportGateway(cfg youtubeImportConfig, cookieStore *youtubeCookieStore) *liveYouTubeImportGateway {
	httpClient := &http.Client{Timeout: cfg.RequestTimeout}
	return &liveYouTubeImportGateway{
		client:      &youtube.Client{HTTPClient: httpClient},
		httpClient:  httpClient,
		cfg:         cfg,
		cookieStore: cookieStore,
	}
}

func (g *liveYouTubeImportGateway) Scan(ctx context.Context, rawURL string, cutoff *time.Time, cookiesPath string) ([]youtubeImportItem, youtubeImportScanSource, error) {
	sourceType, canonicalURL, err := classifyYouTubeImportURL(rawURL)
	if err != nil {
		return nil, youtubeImportScanSource{}, err
	}

	switch sourceType {
	case youtubeImportSourceTrack:
		video, err := g.client.GetVideoContext(ctx, canonicalURL)
		if err != nil {
			item, fallbackErr := g.scanTrackWithYTDLP(ctx, canonicalURL, rawURL, cookiesPath)
			if fallbackErr != nil {
				log.Printf("youtube track scan yt-dlp fallback failed source_url=%q err=%v fallback_err=%v", rawURL, err, fallbackErr)
				return nil, youtubeImportScanSource{}, err
			}
			if !isImportableYouTubeVideo(item) || !passesYouTubeCutoff(item, cutoff) {
				return nil, youtubeImportScanSource{}, errYouTubeNoTracks
			}
			return []youtubeImportItem{item}, youtubeImportScanSource{SourceType: youtubeImportSourceTrack, CanonicalURL: canonicalURL}, nil
		}
		item := buildYouTubeImportItem(video, canonicalURL, rawURL, linkProviderFromURL(rawURL), "")
		if !isImportableYouTubeVideo(item) || !passesYouTubeCutoff(item, cutoff) {
			return nil, youtubeImportScanSource{}, errYouTubeNoTracks
		}
		return []youtubeImportItem{item}, youtubeImportScanSource{SourceType: youtubeImportSourceTrack, CanonicalURL: canonicalURL}, nil
	case youtubeImportSourcePlaylist:
		items, err := g.scanPlaylist(ctx, canonicalURL, rawURL, linkProviderFromURL(rawURL), cutoff)
		return items, youtubeImportScanSource{SourceType: youtubeImportSourcePlaylist, CanonicalURL: canonicalURL}, err
	case youtubeImportSourceArtist:
		items, err := g.scanArtist(ctx, canonicalURL, cutoff, cookiesPath)
		return items, youtubeImportScanSource{SourceType: youtubeImportSourceArtist, CanonicalURL: canonicalURL}, err
	default:
		return nil, youtubeImportScanSource{}, errYouTubeInvalidURL
	}
}

func (g *liveYouTubeImportGateway) DownloadAudio(ctx context.Context, item youtubeImportItem, destinationPath string, cookiesPath string) error {
	if resolvedCookiesPath, ok := g.resolveCookiesPath(cookiesPath); ok {
		return g.downloadAudioWithYTDLP(ctx, item, destinationPath, resolvedCookiesPath)
	}

	video, err := g.client.GetVideoContext(ctx, item.SourceURL)
	if err != nil {
		return youtubeDownloadError(item, "get_video", nil, err)
	}
	format := selectYouTubeDownloadFormat(video.Formats)
	if format == nil {
		return youtubeDownloadError(item, "select_format", nil, errors.New("no downloadable audio format found"))
	}
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
		return err
	}
	reader, _, err := g.client.GetStreamContext(ctx, video, format)
	if err != nil {
		return youtubeDownloadError(item, "get_stream", format, err)
	}
	defer reader.Close()

	output, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, reader); err != nil {
		_ = output.Close()
		_ = os.Remove(destinationPath)
		return youtubeDownloadError(item, "copy_stream", format, err)
	}
	if err := output.Close(); err != nil {
		_ = os.Remove(destinationPath)
		return youtubeDownloadError(item, "close_output", format, err)
	}
	return nil
}

func (g *liveYouTubeImportGateway) resolveCookiesPath(cookiesPath string) (string, bool) {
	cookiesPath = strings.TrimSpace(cookiesPath)
	if cookiesPath != "" {
		info, err := os.Stat(cookiesPath)
		if err == nil && !info.IsDir() && info.Size() > 0 {
			return cookiesPath, true
		}
	}
	return g.cookieStore.pathIfPresent()
}

func (g *liveYouTubeImportGateway) downloadAudioWithYTDLP(ctx context.Context, item youtubeImportItem, destinationPath, cookiesPath string) error {
	binaryName := strings.TrimSpace(g.cfg.YTDLPBinary)
	if binaryName == "" {
		binaryName = "yt-dlp"
	}
	if _, err := exec.LookPath(binaryName); err != nil {
		return youtubeDownloadError(item, "ytdlp_lookup", nil, err)
	}
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
		return err
	}
	args := []string{
		"--cookies", cookiesPath,
		"--no-playlist",
		"--no-part",
		"-f", "bestaudio/best",
		"-o", destinationPath,
		item.SourceURL,
	}
	cmd := exec.CommandContext(ctx, binaryName, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("yt-dlp failed video_id=%q source_url=%q err=%v output=%s", item.VideoID, item.SourceURL, err, strings.TrimSpace(string(output)))
		return youtubeDownloadError(item, "ytdlp_run", nil, err)
	}
	info, err := os.Stat(destinationPath)
	if err != nil || info.Size() == 0 {
		if err == nil {
			err = errors.New("yt-dlp produced empty output file")
		}
		return youtubeDownloadError(item, "ytdlp_output", nil, err)
	}
	return nil
}

func youtubeDownloadError(item youtubeImportItem, stage string, format *youtube.Format, err error) error {
	formatDetails := "none"
	if format != nil {
		formatDetails = fmt.Sprintf("itag=%d mime=%q bitrate=%d channels=%d", format.ItagNo, format.MimeType, format.Bitrate, format.AudioChannels)
	}
	log.Printf(
		"youtube download failed stage=%s video_id=%q source_url=%q provider=%q format={%s} err=%v",
		stage,
		item.VideoID,
		item.SourceURL,
		item.LinkProvider,
		formatDetails,
		err,
	)
	return fmt.Errorf("%w: %s", errYouTubeDownloadFailed, stage)
}

func selectYouTubeDownloadFormat(formats youtube.FormatList) *youtube.Format {
	audioFormats := formats.WithAudioChannels()
	if len(audioFormats) == 0 {
		return nil
	}
	audioFormats.Sort()
	for index := range audioFormats {
		if strings.HasPrefix(audioFormats[index].MimeType, "audio/") {
			return &audioFormats[index]
		}
	}
	return &audioFormats[0]
}

func (g *liveYouTubeImportGateway) scanPlaylist(ctx context.Context, playlistURL, originalURL, linkProvider string, cutoff *time.Time) ([]youtubeImportItem, error) {
	playlist, err := g.client.GetPlaylistContext(ctx, playlistURL)
	if err != nil {
		return nil, err
	}
	items := make([]youtubeImportItem, 0, len(playlist.Videos))
	seen := make(map[string]struct{}, len(playlist.Videos))
	for _, entry := range playlist.Videos {
		item := youtubeImportItem{}
		video, err := g.client.VideoFromPlaylistEntryContext(ctx, entry)
		if err == nil {
			item = buildYouTubeImportItem(video, buildYouTubeWatchURL(video.ID, linkProvider), originalURL, linkProvider, playlist.Title)
			item = mergePlaylistEntryFallback(item, entry, originalURL, linkProvider, playlist.Title)
		} else {
			item = buildYouTubeImportItemFromPlaylistEntry(entry, originalURL, linkProvider, playlist.Title)
		}
		if !isImportableYouTubeVideo(item) || !passesYouTubeCutoff(item, cutoff) {
			continue
		}
		if _, ok := seen[item.VideoID]; ok {
			continue
		}
		seen[item.VideoID] = struct{}{}
		items = append(items, item)
	}
	if len(items) == 0 {
		return nil, errYouTubeNoTracks
	}
	return items, nil
}

func (g *liveYouTubeImportGateway) scanArtist(ctx context.Context, artistURL string, cutoff *time.Time, cookiesPath string) ([]youtubeImportItem, error) {
	items, err := g.scanArtistWithYTDLP(ctx, artistURL, cutoff, cookiesPath)
	if err == nil && len(items) > 0 {
		return items, nil
	}
	if err != nil {
		log.Printf("youtube artist scan yt-dlp fallback source_url=%q err=%v", artistURL, err)
	}
	return g.scanArtistByHTML(ctx, artistURL, cutoff)
}

func (g *liveYouTubeImportGateway) scanArtistWithYTDLP(ctx context.Context, artistURL string, cutoff *time.Time, cookiesPath string) ([]youtubeImportItem, error) {
	data, err := g.dumpFlatPlaylistJSON(ctx, artistURL, cookiesPath)
	if err != nil {
		return nil, err
	}
	var dump ytdlpFlatPlaylistDump
	if err := json.Unmarshal(data, &dump); err != nil {
		return nil, err
	}

	entries := collectYTDLPArtistLeafEntries(dump.Entries)
	if len(entries) == 0 {
		return nil, errYouTubeNoTracks
	}

	linkProvider := linkProviderFromURL(artistURL)
	defaultAuthor := firstNonEmpty(dump.Channel, dump.Uploader, dump.Title)
	items := make([]youtubeImportItem, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		item := buildYouTubeImportItemFromYTDLPEntry(entry, artistURL, linkProvider, defaultAuthor)
		if cutoff != nil {
			video, videoErr := g.client.GetVideoContext(ctx, item.VideoID)
			if videoErr == nil {
				item = mergeYTDLPEntryFallback(
					buildYouTubeImportItem(video, buildYouTubeWatchURL(video.ID, linkProvider), artistURL, linkProvider, ""),
					entry,
					artistURL,
					linkProvider,
					defaultAuthor,
				)
			}
		}
		if !isImportableYouTubeVideo(item) || !passesYouTubeCutoff(item, cutoff) {
			continue
		}
		if _, ok := seen[item.VideoID]; ok {
			continue
		}
		seen[item.VideoID] = struct{}{}
		items = append(items, item)
	}
	sortYouTubeImportItems(items)
	if len(items) == 0 {
		return nil, errYouTubeNoTracks
	}
	return items, nil
}

func (g *liveYouTubeImportGateway) scanTrackWithYTDLP(ctx context.Context, canonicalURL, originalURL string, cookiesPath string) (youtubeImportItem, error) {
	data, err := g.dumpTrackJSON(ctx, canonicalURL, cookiesPath)
	if err != nil {
		return youtubeImportItem{}, err
	}
	var dump ytdlpFlatPlaylistEntry
	if err := json.Unmarshal(data, &dump); err != nil {
		return youtubeImportItem{}, err
	}
	linkProvider := linkProviderFromURL(originalURL)
	item := buildYouTubeImportItemFromYTDLPEntry(dump, originalURL, linkProvider, firstNonEmpty(dump.Channel, dump.Uploader))
	if strings.TrimSpace(item.VideoID) == "" {
		_, parsedCanonicalURL, err := classifyYouTubeImportURL(canonicalURL)
		if err == nil {
			if parsed, parseErr := url.Parse(parsedCanonicalURL); parseErr == nil {
				item.VideoID = strings.TrimSpace(parsed.Query().Get("v"))
			}
		}
	}
	if strings.TrimSpace(item.SourceURL) == "" && strings.TrimSpace(item.VideoID) != "" {
		item.SourceURL = buildYouTubeWatchURL(item.VideoID, linkProvider)
	}
	if item.ParsedReleaseDate == nil {
		item.ParsedReleaseDate = ytdlpReleaseDate(dump)
	}
	return item, nil
}

func (g *liveYouTubeImportGateway) scanArtistByHTML(ctx context.Context, artistURL string, cutoff *time.Time) ([]youtubeImportItem, error) {
	body, err := g.fetchHTML(ctx, artistURL)
	if err != nil {
		return nil, err
	}

	playlistIDs := extractMatches(body, youtubePlaylistIDPattern)
	videoIDs := extractMatches(body, youtubeVideoIDPattern)
	items := make([]youtubeImportItem, 0)
	seen := make(map[string]struct{})

	for _, playlistID := range playlistIDs {
		if strings.HasPrefix(playlistID, "RD") {
			continue
		}
		playlistItems, err := g.scanPlaylist(ctx, "https://www.youtube.com/playlist?list="+playlistID, artistURL, "youtube_music", cutoff)
		if err != nil {
			continue
		}
		for _, item := range playlistItems {
			if _, ok := seen[item.VideoID]; ok {
				continue
			}
			seen[item.VideoID] = struct{}{}
			items = append(items, item)
		}
	}

	if len(items) == 0 {
		for _, videoID := range videoIDs {
			if _, ok := seen[videoID]; ok {
				continue
			}
			video, err := g.client.GetVideoContext(ctx, videoID)
			if err != nil {
				continue
			}
			item := buildYouTubeImportItem(video, buildYouTubeWatchURL(video.ID, "youtube_music"), artistURL, "youtube_music", "")
			if !isImportableYouTubeVideo(item) || !passesYouTubeCutoff(item, cutoff) {
				continue
			}
			seen[item.VideoID] = struct{}{}
			items = append(items, item)
		}
	}

	sortYouTubeImportItems(items)

	if len(items) == 0 {
		return nil, errYouTubeNoTracks
	}
	return items, nil
}

func (g *liveYouTubeImportGateway) dumpFlatPlaylistJSON(ctx context.Context, rawURL string, cookiesPath string) ([]byte, error) {
	return g.dumpYTDLPJSON(ctx, rawURL, []string{"--no-warnings", "--flat-playlist", "--dump-single-json"}, "artist scan", cookiesPath)
}

func (g *liveYouTubeImportGateway) dumpTrackJSON(ctx context.Context, rawURL string, cookiesPath string) ([]byte, error) {
	return g.dumpYTDLPJSON(ctx, rawURL, []string{"--no-warnings", "--no-playlist", "--dump-single-json"}, "track scan", cookiesPath)
}

func (g *liveYouTubeImportGateway) dumpYTDLPJSON(ctx context.Context, rawURL string, args []string, label string, cookiesPath string) ([]byte, error) {
	binaryName := strings.TrimSpace(g.cfg.YTDLPBinary)
	if binaryName == "" {
		binaryName = "yt-dlp"
	}
	if _, err := exec.LookPath(binaryName); err != nil {
		return nil, err
	}
	args = append([]string{}, args...)
	if resolvedCookiesPath, ok := g.resolveCookiesPath(cookiesPath); ok {
		args = append(args, "--cookies", resolvedCookiesPath)
	}
	args = append(args, rawURL)
	cmd := exec.CommandContext(ctx, binaryName, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("yt-dlp %s failed source_url=%q err=%v output=%s", label, rawURL, err, strings.TrimSpace(string(output)))
		return nil, err
	}
	jsonPayload := trimJSONOutput(output)
	if len(jsonPayload) == 0 {
		return nil, errors.New("yt-dlp did not return json output")
	}
	return jsonPayload, nil
}

func trimJSONOutput(output []byte) []byte {
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return nil
	}
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start == -1 || end == -1 || end < start {
		return nil
	}
	return []byte(trimmed[start : end+1])
}

func (g *liveYouTubeImportGateway) fetchHTML(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", errYouTubeInvalidURL
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", errYouTubeInvalidURL
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func buildYouTubeImportItem(video *youtube.Video, sourceURL, originalURL, linkProvider, albumTitle string) youtubeImportItem {
	item := youtubeImportItem{
		VideoID:           video.ID,
		SourceURL:         sourceURL,
		OriginalSourceURL: originalURL,
		LinkProvider:      linkProvider,
		ParsedTitle:       strings.TrimSpace(video.Title),
		ParsedAuthorNames: normalizeNames([]string{video.Author}),
		ParsedAlbumTitle:  strings.TrimSpace(albumTitle),
		DurationSeconds:   int(video.Duration.Seconds()),
	}
	if !video.PublishDate.IsZero() {
		releaseDate := video.PublishDate.UTC()
		item.ParsedReleaseDate = &releaseDate
	}
	if len(video.Thumbnails) > 0 {
		item.CoverImageURL = video.Thumbnails[len(video.Thumbnails)-1].URL
	}
	return item
}

func buildYouTubeImportItemFromPlaylistEntry(entry *youtube.PlaylistEntry, originalURL, linkProvider, albumTitle string) youtubeImportItem {
	item := youtubeImportItem{
		VideoID:           strings.TrimSpace(entry.ID),
		SourceURL:         buildYouTubeWatchURL(strings.TrimSpace(entry.ID), linkProvider),
		OriginalSourceURL: originalURL,
		LinkProvider:      linkProvider,
		ParsedTitle:       strings.TrimSpace(entry.Title),
		ParsedAuthorNames: normalizeNames([]string{entry.Author}),
		ParsedAlbumTitle:  strings.TrimSpace(albumTitle),
		DurationSeconds:   int(entry.Duration.Seconds()),
	}
	if len(entry.Thumbnails) > 0 {
		item.CoverImageURL = entry.Thumbnails[len(entry.Thumbnails)-1].URL
	}
	return item
}

func mergePlaylistEntryFallback(item youtubeImportItem, entry *youtube.PlaylistEntry, originalURL, linkProvider, albumTitle string) youtubeImportItem {
	fallback := buildYouTubeImportItemFromPlaylistEntry(entry, originalURL, linkProvider, albumTitle)
	if strings.TrimSpace(item.VideoID) == "" {
		item.VideoID = fallback.VideoID
	}
	if strings.TrimSpace(item.SourceURL) == "" {
		item.SourceURL = fallback.SourceURL
	}
	if strings.TrimSpace(item.OriginalSourceURL) == "" {
		item.OriginalSourceURL = fallback.OriginalSourceURL
	}
	if strings.TrimSpace(item.LinkProvider) == "" {
		item.LinkProvider = fallback.LinkProvider
	}
	if strings.TrimSpace(item.ParsedTitle) == "" {
		item.ParsedTitle = fallback.ParsedTitle
	}
	if len(item.ParsedAuthorNames) == 0 {
		item.ParsedAuthorNames = fallback.ParsedAuthorNames
	}
	if strings.TrimSpace(item.ParsedAlbumTitle) == "" {
		item.ParsedAlbumTitle = fallback.ParsedAlbumTitle
	}
	if strings.TrimSpace(item.CoverImageURL) == "" {
		item.CoverImageURL = fallback.CoverImageURL
	}
	if item.DurationSeconds <= 0 {
		item.DurationSeconds = fallback.DurationSeconds
	}
	return item
}

func buildYouTubeImportItemFromYTDLPEntry(entry ytdlpFlatPlaylistEntry, originalURL, linkProvider, defaultAuthor string) youtubeImportItem {
	author := strings.TrimSpace(firstNonEmpty(entry.Channel, entry.Uploader, defaultAuthor))
	videoID := strings.TrimSpace(entry.ID)
	item := youtubeImportItem{
		VideoID:           videoID,
		OriginalSourceURL: originalURL,
		LinkProvider:      linkProvider,
		ParsedTitle:       strings.TrimSpace(entry.Title),
		ParsedAuthorNames: normalizeNames([]string{author}),
		DurationSeconds:   int(entry.Duration),
	}
	if videoID != "" {
		item.SourceURL = buildYouTubeWatchURL(videoID, linkProvider)
	}
	if len(entry.Thumbnails) > 0 {
		item.CoverImageURL = strings.TrimSpace(entry.Thumbnails[len(entry.Thumbnails)-1].URL)
	}
	item.ParsedReleaseDate = ytdlpReleaseDate(entry)
	return item
}

func mergeYTDLPEntryFallback(item youtubeImportItem, entry ytdlpFlatPlaylistEntry, originalURL, linkProvider, defaultAuthor string) youtubeImportItem {
	fallback := buildYouTubeImportItemFromYTDLPEntry(entry, originalURL, linkProvider, defaultAuthor)
	if strings.TrimSpace(item.VideoID) == "" {
		item.VideoID = fallback.VideoID
	}
	if strings.TrimSpace(item.SourceURL) == "" {
		item.SourceURL = fallback.SourceURL
	}
	if strings.TrimSpace(item.OriginalSourceURL) == "" {
		item.OriginalSourceURL = fallback.OriginalSourceURL
	}
	if strings.TrimSpace(item.LinkProvider) == "" {
		item.LinkProvider = fallback.LinkProvider
	}
	if strings.TrimSpace(item.ParsedTitle) == "" {
		item.ParsedTitle = fallback.ParsedTitle
	}
	if len(item.ParsedAuthorNames) == 0 {
		item.ParsedAuthorNames = fallback.ParsedAuthorNames
	}
	if strings.TrimSpace(item.CoverImageURL) == "" {
		item.CoverImageURL = fallback.CoverImageURL
	}
	if item.DurationSeconds <= 0 {
		item.DurationSeconds = fallback.DurationSeconds
	}
	return item
}

func collectYTDLPArtistLeafEntries(entries []ytdlpFlatPlaylistEntry) []ytdlpFlatPlaylistEntry {
	flat := make([]ytdlpFlatPlaylistEntry, 0)
	for _, entry := range entries {
		collectYTDLPArtistLeafEntriesInto(entry, false, &flat)
	}
	return flat
}

func collectYTDLPArtistLeafEntriesInto(entry ytdlpFlatPlaylistEntry, insideShorts bool, flat *[]ytdlpFlatPlaylistEntry) {
	currentShorts := insideShorts || isYTDLPShortsEntry(entry)
	if len(entry.Entries) > 0 {
		for _, nested := range entry.Entries {
			collectYTDLPArtistLeafEntriesInto(nested, currentShorts, flat)
		}
		return
	}
	if currentShorts {
		return
	}
	if strings.TrimSpace(entry.ID) == "" {
		return
	}
	if leafURL := firstNonEmpty(entry.URL, entry.WebpageURL); leafURL != "" && strings.Contains(strings.ToLower(leafURL), "/shorts/") {
		return
	}
	*flat = append(*flat, entry)
}

func isYTDLPShortsEntry(entry ytdlpFlatPlaylistEntry) bool {
	title := strings.ToLower(strings.TrimSpace(entry.Title))
	if strings.Contains(title, "shorts") {
		return true
	}
	for _, candidate := range []string{entry.URL, entry.WebpageURL} {
		if strings.Contains(strings.ToLower(strings.TrimSpace(candidate)), "/shorts") {
			return true
		}
	}
	return false
}

func sortYouTubeImportItems(items []youtubeImportItem) {
	sort.SliceStable(items, func(i, j int) bool {
		left := items[i].ParsedReleaseDate
		right := items[j].ParsedReleaseDate
		switch {
		case left == nil && right == nil:
			return items[i].ParsedTitle < items[j].ParsedTitle
		case left == nil:
			return false
		case right == nil:
			return true
		default:
			return left.Before(*right)
		}
	})
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func ytdlpReleaseDate(entry ytdlpFlatPlaylistEntry) *time.Time {
	if entry.Timestamp > 0 {
		releaseDate := time.Unix(entry.Timestamp, 0).UTC()
		return &releaseDate
	}
	uploadDate := strings.TrimSpace(entry.UploadDate)
	if uploadDate == "" {
		return nil
	}
	releaseDate, err := time.Parse("20060102", uploadDate)
	if err != nil {
		return nil
	}
	releaseDate = releaseDate.UTC()
	return &releaseDate
}

func buildYouTubeWatchURL(videoID, provider string) string {
	if provider == "youtube_music" {
		return "https://music.youtube.com/watch?v=" + url.QueryEscape(videoID)
	}
	return "https://www.youtube.com/watch?v=" + url.QueryEscape(videoID)
}

func classifyYouTubeImportURL(rawURL string) (string, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", "", errYouTubeInvalidURL
	}
	host := strings.ToLower(parsed.Hostname())
	switch host {
	case "music.youtube.com", "www.youtube.com", "youtube.com", "m.youtube.com", "youtu.be":
	default:
		return "", "", errYouTubeInvalidURL
	}

	switch {
	case host == "youtu.be":
		videoID := strings.Trim(strings.TrimSpace(parsed.Path), "/")
		if videoID == "" {
			return "", "", errYouTubeInvalidURL
		}
		return youtubeImportSourceTrack, buildYouTubeWatchURL(videoID, linkProviderFromURL(rawURL)), nil
	case parsed.Query().Get("v") != "":
		return youtubeImportSourceTrack, buildYouTubeWatchURL(parsed.Query().Get("v"), linkProviderFromURL(rawURL)), nil
	case parsed.Query().Get("list") != "":
		return youtubeImportSourcePlaylist, "https://www.youtube.com/playlist?list=" + url.QueryEscape(parsed.Query().Get("list")), nil
	case strings.Contains(parsed.Path, "/playlist"):
		listID := parsed.Query().Get("list")
		if listID == "" {
			return "", "", errYouTubeInvalidURL
		}
		return youtubeImportSourcePlaylist, "https://www.youtube.com/playlist?list=" + url.QueryEscape(listID), nil
	case strings.Contains(parsed.Path, "/channel/"), strings.Contains(parsed.Path, "/@"), strings.Contains(parsed.Path, "/browse/"):
		return youtubeImportSourceArtist, rawURL, nil
	default:
		return "", "", errYouTubeInvalidURL
	}
}

func linkProviderFromURL(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "youtube"
	}
	if strings.EqualFold(parsed.Hostname(), "music.youtube.com") {
		return "youtube_music"
	}
	return "youtube"
}

func extractMatches(body string, pattern *regexp.Regexp) []string {
	matches := pattern.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	result := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		value := strings.TrimSpace(match[1])
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func isImportableYouTubeVideo(item youtubeImportItem) bool {
	if strings.TrimSpace(item.VideoID) == "" || strings.TrimSpace(item.ParsedTitle) == "" {
		return false
	}
	if len(item.ParsedAuthorNames) == 0 {
		return false
	}
	if item.DurationSeconds > 0 && item.DurationSeconds < 30 {
		return false
	}
	title := strings.ToLower(item.ParsedTitle)
	if strings.Contains(title, "#shorts") || strings.Contains(title, "shorts") {
		return false
	}
	return true
}

func passesYouTubeCutoff(item youtubeImportItem, cutoff *time.Time) bool {
	if cutoff == nil {
		return true
	}
	if item.ParsedReleaseDate == nil {
		return false
	}
	return !item.ParsedReleaseDate.Before(cutoff.UTC())
}

func (s *trackStore) getTrack(trackID int64) (track, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	t, ok := s.tracks[trackID]
	if !ok {
		return track{}, false
	}
	return cloneTrack(t), true
}

func (s *trackStore) getTrackAlbumOrder(trackID int64) (int, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	t, ok := s.tracks[trackID]
	if !ok {
		return 0, false
	}
	albumItem, ok := s.albums[t.AlbumID]
	if !ok {
		return 0, false
	}
	for index, existingTrackID := range albumItem.TrackIDs {
		if existingTrackID == trackID {
			return index, true
		}
	}
	return 0, false
}

func (s *trackStore) findTrackBySourceMetadata(target sourceMetadata) (track, bool) {
	targetProvider, ok := target["provider"].(string)
	if !ok || strings.TrimSpace(targetProvider) == "" {
		return track{}, false
	}
	targetIdentity, ok := target["identity"].(map[string]any)
	if !ok {
		return track{}, false
	}
	targetKey, err := sourceMetadataIdentityKey(targetIdentity)
	if err != nil {
		return track{}, false
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, item := range s.tracks {
		for _, metadata := range item.SourceMetadata {
			provider, ok := metadata["provider"].(string)
			if !ok || !strings.EqualFold(strings.TrimSpace(provider), strings.TrimSpace(targetProvider)) {
				continue
			}
			identity, ok := metadata["identity"].(map[string]any)
			if !ok {
				continue
			}
			identityKey, err := sourceMetadataIdentityKey(identity)
			if err != nil {
				continue
			}
			if identityKey == targetKey {
				return cloneTrack(item), true
			}
		}
	}
	return track{}, false
}

func (s *trackStore) attachTrackImportMetadata(trackID int64, infos []additionalInfo, metadata []sourceMetadata) (track, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.tracks[trackID]
	if !ok {
		return track{}, false, nil
	}

	tracksSnapshot := cloneTracksMap(s.tracks)
	updated := cloneTrack(current)
	updated.AdditionalInfo = mergeAdditionalInfoItems(updated.AdditionalInfo, infos)
	updated.SourceMetadata = mergeSourceMetadataItems(updated.SourceMetadata, metadata)
	if err := s.validateTrackLocked(updated); err != nil {
		return track{}, true, err
	}
	s.tracks[trackID] = updated
	if err := s.persistLocked(); err != nil {
		s.tracks = tracksSnapshot
		return track{}, true, err
	}
	return s.tracks[trackID], true, nil
}

func mergeAdditionalInfoItems(existing, additions []additionalInfo) []additionalInfo {
	merged := normalizeAdditionalInfo(existing)
	seen := make(map[string]struct{}, len(merged))
	for _, item := range merged {
		seen[additionalInfoDedupKey(item)] = struct{}{}
	}
	for _, item := range normalizeAdditionalInfo(additions) {
		key := additionalInfoDedupKey(item)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, item)
	}
	return merged
}

func additionalInfoDedupKey(item additionalInfo) string {
	normalized := normalizeMetadataValue(item)
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return fmt.Sprintf("%v", normalized)
	}
	return string(encoded)
}

func mergeSourceMetadataItems(existing, additions []sourceMetadata) []sourceMetadata {
	merged := normalizeSourceMetadata(existing)
	seen := make(map[string]struct{}, len(merged))
	for _, item := range merged {
		key := sourceMetadataDedupKey(item)
		if key == "" {
			continue
		}
		seen[key] = struct{}{}
	}
	for _, item := range normalizeSourceMetadata(additions) {
		key := sourceMetadataDedupKey(item)
		if key == "" {
			merged = append(merged, item)
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, item)
	}
	return merged
}

func sourceMetadataDedupKey(item sourceMetadata) string {
	provider, ok := item["provider"].(string)
	if !ok || strings.TrimSpace(provider) == "" {
		return ""
	}
	identity, ok := item["identity"].(map[string]any)
	if !ok {
		return ""
	}
	identityKey, err := sourceMetadataIdentityKey(identity)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(provider)) + "\x00" + identityKey
}

func (s *trackStore) youtubeImportSuggestions(item youtubeImportItem) []youtubeImportSuggestion {
	_, generatedSourceMetadata, err := buildYouTubeImportMetadata(item)
	if err != nil || len(generatedSourceMetadata) == 0 {
		return nil
	}
	if existingTrack, ok := s.findTrackBySourceMetadata(generatedSourceMetadata[0]); ok {
		return []youtubeImportSuggestion{{
			Type:       youtubeSuggestionTypeExactSourceMatch,
			TrackID:    existingTrack.ID,
			Confidence: 1,
			Metadata: map[string]any{
				"trackName": existingTrack.Name,
			},
		}}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	targetName := strings.ToLower(strings.TrimSpace(item.ParsedTitle))
	targetAuthors := make(map[string]struct{}, len(item.ParsedAuthorNames))
	for _, authorName := range item.ParsedAuthorNames {
		normalized := strings.ToLower(strings.TrimSpace(authorName))
		if normalized == "" {
			continue
		}
		targetAuthors[normalized] = struct{}{}
	}

	suggestions := make([]youtubeImportSuggestion, 0, 5)
	for _, current := range s.tracks {
		if strings.ToLower(strings.TrimSpace(current.Name)) != targetName {
			continue
		}
		confidence := 0.6
		matchedAuthor := false
		for _, authorID := range current.AuthorIDs {
			authorItem, ok := s.authors[authorID]
			if !ok {
				continue
			}
			if _, ok := targetAuthors[strings.ToLower(strings.TrimSpace(authorItem.CurrentName))]; ok {
				confidence = 0.8
				matchedAuthor = true
				break
			}
		}
		metadata := map[string]any{
			"trackName": current.Name,
		}
		if matchedAuthor {
			metadata["matchedAuthor"] = true
		}
		suggestions = append(suggestions, youtubeImportSuggestion{
			Type:       youtubeSuggestionTypePossibleTrack,
			TrackID:    current.ID,
			Confidence: confidence,
			Metadata:   metadata,
		})
		if len(suggestions) >= 5 {
			break
		}
	}
	sort.SliceStable(suggestions, func(i, j int) bool {
		if suggestions[i].Confidence == suggestions[j].Confidence {
			return suggestions[i].TrackID < suggestions[j].TrackID
		}
		return suggestions[i].Confidence > suggestions[j].Confidence
	})
	return suggestions
}

func youtubeCookiesStatusHandler(store *youtubeCookieStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, store.Status())
	})
}

func youtubeCookiesUploadHandler(store *youtubeCookieStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxSongUploadSize)
		if err := r.ParseMultipartForm(maxSongUploadSize); err != nil {
			http.Error(w, "invalid multipart form", http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "file is required", http.StatusBadRequest)
			return
		}
		defer file.Close()
		if err := store.Replace(file); err != nil {
			switch err.Error() {
			case "uploaded file is empty", "youtube cookies file is not configured":
				http.Error(w, err.Error(), http.StatusBadRequest)
			default:
				http.Error(w, "failed to store youtube cookies", http.StatusInternalServerError)
			}
			return
		}
		writeJSON(w, http.StatusCreated, store.Status())
	})
}

func youtubeCookiesDeleteHandler(store *youtubeCookieStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := store.Delete(); err != nil {
			http.Error(w, "failed to delete youtube cookies", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func youtubeStartImportHandler(service *youtubeImportService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}

		var req youtubeStartImportRequest
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		session, err := service.StartSession(
			r.Context(),
			userID,
			req.URL,
			req.ReleaseDateCutoff,
			req.ReplaceExisting,
			req.YouTubeCookies,
		)
		if err != nil {
			writeYouTubeImportError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, session)
	})
}

func youtubeCurrentImportHandler(service *youtubeImportService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		session, err := service.CurrentSession(userID)
		if err != nil {
			writeYouTubeImportError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, session)
	})
}

func youtubeSkipImportHandler(service *youtubeImportService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		session, err := service.SkipCurrent(userID)
		if err != nil {
			writeYouTubeImportError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, session)
	})
}

func youtubeAddImportHandler(service *youtubeImportService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		var req youtubeAddCurrentRequest
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		session, trackItem, err := service.AddCurrent(r.Context(), userID, req)
		if err != nil {
			writeYouTubeImportError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"session": session,
			"track":   toTrackResponse(trackItem, false, true),
		})
	})
}

func youtubeCancelImportHandler(service *youtubeImportService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if err := service.CancelSession(userID); err != nil {
			writeYouTubeImportError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func writeYouTubeImportError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errYouTubeInvalidURL), errors.Is(err, errYouTubeNoTracks), errors.Is(err, errYouTubeUnsupportedMode):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, errYouTubeSessionActive), errors.Is(err, errYouTubeCurrentConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, errYouTubeSessionNotFound), errors.Is(err, errTrackNotFound), errors.Is(err, errYouTubeCurrentNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errInvalidTrack):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, errYouTubeDownloadFailed):
		http.Error(w, errYouTubeDownloadFailed.Error(), http.StatusInternalServerError)
	case errors.Is(err, errYouTubeTranscodeFailed):
		http.Error(w, errYouTubeTranscodeFailed.Error(), http.StatusInternalServerError)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
