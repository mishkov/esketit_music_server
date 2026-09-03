package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/getsentry/sentry-go"
	youtube "github.com/kkdai/youtube/v2"
)

const (
	youtubeImportStatusActive     = "active"
	youtubeImportStatusCompleted  = "completed"
	maxYouTubeArtistHTMLBytes     = 8 << 20
	maxYouTubeMetadataBytes       = 16 << 20
	maxYouTubeYTDLPJSONBytes      = 16 << 20
	maxYouTubeCommandOutputBytes  = 64 << 10
	maxYouTubeCookiesBytes        = 1 << 20
	maxYouTubeCookiesUploadBytes  = maxYouTubeCookiesBytes + (64 << 10)
	youtubeDependencyCheckTimeout = 15 * time.Second

	youtubeImportSourceTrack    = "track"
	youtubeImportSourcePlaylist = "playlist"
	youtubeImportSourceArtist   = "artist"

	youtubeAddModeCreate = "create"
	youtubeAddModeAttach = "attach"

	youtubeSuggestionTypeExactSourceMatch = "exact_source_match"
	youtubeSuggestionTypePossibleTrack    = "possible_track_match"
)

var (
	errYouTubeInvalidURL       = errors.New("invalid youtube or youtube music url")
	errYouTubeNoTracks         = errors.New("no importable youtube tracks found")
	errYouTubeSessionActive    = errors.New("youtube import session is already active")
	errYouTubeSessionNotFound  = errors.New("youtube import session not found")
	errYouTubeCurrentConflict  = errors.New("current youtube item is already connected to an existing track")
	errYouTubeUnsupportedMode  = errors.New("unsupported youtube import add mode")
	errYouTubeCurrentNotFound  = errors.New("youtube import item not found")
	errYouTubeDownloadFailed   = errors.New("failed to download youtube track audio")
	errYouTubeTranscodeFailed  = errors.New("failed to transcode youtube track audio")
	errYouTubeUpstreamFailed   = errors.New("youtube upstream request failed")
	errYouTubeUpstreamTimeout  = errors.New("youtube upstream request timed out")
	errYouTubeDependency       = errors.New("youtube import dependency is unavailable")
	errYouTubeStorageFailed    = errors.New("youtube import storage operation failed")
	errYouTubeCookiesNotSet    = errors.New("youtube cookies file is not configured")
	errYouTubeCookiesEmpty     = errors.New("uploaded file is empty")
	errYouTubeCookiesTooLarge  = errors.New("youtube cookies file exceeds the maximum allowed size")
	errYouTubeResponseTooLarge = errors.New("youtube upstream response is too large")
	errYouTubeAudioTooLarge    = errors.New("youtube audio exceeds the maximum allowed size")
	errYouTubeSessionChanged   = errors.New("youtube import session changed during operation")
	errYouTubeShuttingDown     = errors.New("youtube import service is shutting down")
	errYouTubeRateLimited      = errors.New("youtube is rate limiting requests")
	errYouTubeAuthentication   = errors.New("youtube authentication or cookies are invalid")
	errYouTubeChallenge        = errors.New("youtube verification challenge failed")
	errYouTubeUnavailable      = errors.New("youtube video is unavailable")
)

type youtubeCommandError struct {
	cause           error
	category        error
	stderrTruncated bool
}

func (e *youtubeCommandError) Error() string {
	category := "external command failed"
	if e.category != nil {
		category = e.category.Error()
	}
	if e.stderrTruncated {
		return fmt.Sprintf("%s (diagnostic output truncated): %v", category, e.cause)
	}
	return fmt.Sprintf("%s: %v", category, e.cause)
}

func (e *youtubeCommandError) Unwrap() []error {
	errs := []error{e.cause}
	if e.category != nil {
		errs = append(errs, e.category)
	}
	return errs
}

func classifyYouTubeCommandError(cause error, stderr []byte, stderrTruncated bool) error {
	if cause == nil {
		return nil
	}
	diagnostic := strings.ToLower(string(stderr))
	var category error
	switch {
	case containsAnyString(diagnostic, "http error 429", "too many requests", "rate limit", "rate-limit"):
		category = errYouTubeRateLimited
	case containsAnyString(diagnostic, "po token", "proof of origin", "javascript runtime", "challenge solver", "n challenge", "yt-dlp-ejs"):
		category = errYouTubeChallenge
	case containsAnyString(diagnostic, "sign in to confirm", "login required", "cookies are no longer valid", "authentication required", "use --cookies"):
		category = errYouTubeAuthentication
	case containsAnyString(diagnostic, "video unavailable", "private video", "has been removed", "not available in your country"):
		category = errYouTubeUnavailable
	}
	return &youtubeCommandError{cause: cause, category: category, stderrTruncated: stderrTruncated}
}

func containsAnyString(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}

type youtubeCleanupError struct {
	operation string
	err       error
}

func newYouTubeCleanupError(operation string, err error) error {
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return &youtubeCleanupError{operation: operation, err: err}
}

func (e *youtubeCleanupError) Error() string {
	return fmt.Sprintf("youtube cleanup failed during %s: %v", e.operation, e.err)
}

func (e *youtubeCleanupError) Unwrap() error {
	return e.err
}

func hasYouTubeCleanupError(err error) bool {
	var cleanupErr *youtubeCleanupError
	return errors.As(err, &cleanupErr)
}

func youtubeStageError(kind error, stage string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", kind, stage)
	}
	return fmt.Errorf("%w: %s: %w", kind, stage, cause)
}

func sanitizeYouTubeCause(err error) (error, bool) {
	if err == nil {
		return nil, false
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		cause, _ := sanitizeYouTubeCause(urlErr.Err)
		return fmt.Errorf("youtube upstream %s failed: %w", urlErr.Op, cause), true
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return fmt.Errorf("filesystem %s failed: %w", pathErr.Op, pathErr.Err), true
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return fmt.Errorf("filesystem %s failed: %w", linkErr.Op, linkErr.Err), true
	}
	var lookupErr *exec.Error
	if errors.As(err, &lookupErr) {
		return fmt.Errorf("executable lookup failed: %w", lookupErr.Err), true
	}
	return err, false
}

func sanitizeYouTubeCapturedError(err error) error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		sanitized := make([]error, 0, len(causes))
		for _, cause := range causes {
			if cause != nil {
				sanitized = append(sanitized, sanitizeYouTubeCapturedError(cause))
			}
		}
		return errors.Join(sanitized...)
	}
	if cleanupErr, ok := err.(*youtubeCleanupError); ok {
		return &youtubeCleanupError{
			operation: cleanupErr.operation,
			err:       sanitizeYouTubeCapturedError(cleanupErr.err),
		}
	}
	sanitized, changed := sanitizeYouTubeCause(err)
	if !changed {
		return err
	}
	causes := []error{sanitized}
	for _, known := range []error{
		errYouTubeDownloadFailed,
		errYouTubeTranscodeFailed,
		errYouTubeUpstreamFailed,
		errYouTubeUpstreamTimeout,
		errYouTubeDependency,
		errYouTubeStorageFailed,
	} {
		if errors.Is(err, known) {
			causes = append(causes, known)
		}
	}
	return errors.Join(causes...)
}

func youtubeErrorDiagnostic(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.Canceled) {
		return "type=context_canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "type=context_deadline_exceeded"
	}
	var commandErr *youtubeCommandError
	if errors.As(err, &commandErr) {
		category := "process_failed"
		if commandErr.category != nil {
			category = strings.ReplaceAll(commandErr.category.Error(), " ", "_")
		}
		var exitErr *exec.ExitError
		if errors.As(commandErr.cause, &exitErr) {
			return fmt.Sprintf("type=process_exit exit_code=%d category=%s stderr_truncated=%t", exitErr.ExitCode(), category, commandErr.stderrTruncated)
		}
		return fmt.Sprintf("type=process_failure category=%s stderr_truncated=%t", category, commandErr.stderrTruncated)
	}
	var statusErr youtube.ErrUnexpectedStatusCode
	if errors.As(err, &statusErr) {
		return fmt.Sprintf("type=upstream_http_status status=%d", int(statusErr))
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Sprintf("type=process_exit exit_code=%d", exitErr.ExitCode())
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Sprintf("type=%T timeout=true", err)
	}
	return fmt.Sprintf("type=%T", err)
}

func sanitizeYouTubeFilesystemError(operation string, err error) error {
	if err == nil {
		return nil
	}
	cause := err
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		cause = pathErr.Err
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		cause = linkErr.Err
	}
	return youtubeStageError(errYouTubeStorageFailed, operation, cause)
}

func removeYouTubeFile(path, operation string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return newYouTubeCleanupError(operation, sanitizeYouTubeFilesystemError(operation, err))
}

func classifyYouTubeScanError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("youtube request canceled: %w", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		cause, _ := sanitizeYouTubeCause(err)
		return youtubeStageError(errYouTubeUpstreamTimeout, "scan", cause)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		cause, _ := sanitizeYouTubeCause(err)
		return youtubeStageError(errYouTubeUpstreamTimeout, "scan", cause)
	}

	for _, known := range []error{
		errYouTubeInvalidURL,
		errYouTubeNoTracks,
		errYouTubeUpstreamFailed,
		errYouTubeUpstreamTimeout,
		errYouTubeDependency,
		errYouTubeStorageFailed,
		errYouTubeRateLimited,
		errYouTubeAuthentication,
		errYouTubeChallenge,
		errYouTubeUnavailable,
	} {
		if errors.Is(err, known) {
			return err
		}
	}

	var statusErr youtube.ErrUnexpectedStatusCode
	if errors.As(err, &statusErr) {
		status := int(statusErr)
		if status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout {
			return youtubeStageError(errYouTubeUpstreamTimeout, "scan", statusErr)
		}
		if status == http.StatusTooManyRequests || status >= http.StatusInternalServerError || status < http.StatusBadRequest {
			return youtubeStageError(errYouTubeUpstreamFailed, "scan", statusErr)
		}
		return youtubeStageError(errYouTubeInvalidURL, "upstream rejected source", statusErr)
	}

	for _, expected := range []error{
		youtube.ErrInvalidCharactersInVideoID,
		youtube.ErrVideoIDMinLength,
		youtube.ErrLoginRequired,
		youtube.ErrVideoPrivate,
		youtube.ErrInvalidPlaylist,
	} {
		if errors.Is(err, expected) {
			return youtubeStageError(errYouTubeInvalidURL, "upstream rejected source", err)
		}
	}
	var playbackErr youtube.ErrPlayabiltyStatus
	if errors.As(err, &playbackErr) {
		return youtubeStageError(errYouTubeInvalidURL, "upstream rejected source", err)
	}
	var playlistErr youtube.ErrPlaylistStatus
	if errors.As(err, &playlistErr) {
		return youtubeStageError(errYouTubeInvalidURL, "upstream rejected source", err)
	}

	cause, _ := sanitizeYouTubeCause(err)
	return youtubeStageError(errYouTubeUpstreamFailed, "scan", cause)
}

func combineYouTubeScanErrors(errs ...error) error {
	classified := make([]error, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			classified = append(classified, classifyYouTubeScanError(err))
		}
	}
	return errors.Join(classified...)
}

func withYouTubeTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}

func withYouTubeDownloadTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return withYouTubeTimeout(parent, timeout)
}

type boundedYouTubeOutput struct {
	limit    int
	data     []byte
	overflow bool
}

type boundedYouTubeDiagnostic struct {
	limit    int
	data     []byte
	overflow bool
}

type boundedYouTubeAudioWriter struct {
	destination io.Writer
	remaining   int64
	overflow    bool
}

func (w *boundedYouTubeAudioWriter) Write(p []byte) (int, error) {
	if w.remaining <= 0 {
		w.overflow = true
		return 0, errYouTubeAudioTooLarge
	}
	if int64(len(p)) <= w.remaining {
		n, err := w.destination.Write(p)
		w.remaining -= int64(n)
		return n, err
	}
	n, err := w.destination.Write(p[:w.remaining])
	w.remaining -= int64(n)
	w.overflow = true
	if err != nil {
		return n, err
	}
	return n, errYouTubeAudioTooLarge
}

func copyYouTubeAudio(destination io.Writer, source io.Reader, limit int64) (int64, error) {
	if limit < 0 {
		limit = 0
	}
	bounded := &boundedYouTubeAudioWriter{destination: destination, remaining: limit}
	written, err := io.Copy(bounded, source)
	if bounded.overflow {
		return written, errYouTubeAudioTooLarge
	}
	return written, err
}

func newBoundedYouTubeOutput(limit int) *boundedYouTubeOutput {
	if limit < 0 {
		limit = 0
	}
	return &boundedYouTubeOutput{
		limit: limit,
		data:  make([]byte, 0, min(limit, 64<<10)),
	}
}

func (w *boundedYouTubeOutput) Write(p []byte) (int, error) {
	written := len(p)
	remaining := w.limit - len(w.data)
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 0 {
		retained := min(remaining, len(p))
		w.data = append(w.data, p[:retained]...)
	}
	if len(p) > remaining {
		w.overflow = true
	}
	return written, nil
}

func newBoundedYouTubeDiagnostic(limit int) *boundedYouTubeDiagnostic {
	if limit < 0 {
		limit = 0
	}
	return &boundedYouTubeDiagnostic{limit: limit, data: make([]byte, 0, min(limit, 64<<10))}
}

func (w *boundedYouTubeDiagnostic) Write(p []byte) (int, error) {
	written := len(p)
	if w.limit == 0 {
		w.overflow = w.overflow || len(p) > 0
		return written, nil
	}
	if len(p) >= w.limit {
		w.data = append(w.data[:0], p[len(p)-w.limit:]...)
		w.overflow = true
		return written, nil
	}
	if excess := len(w.data) + len(p) - w.limit; excess > 0 {
		copy(w.data, w.data[excess:])
		w.data = w.data[:len(w.data)-excess]
		w.overflow = true
	}
	w.data = append(w.data, p...)
	return written, nil
}

func runYouTubeCommandWithOutputLimit(ctx context.Context, binaryName string, args []string, limit int) ([]byte, error) {
	output := newBoundedYouTubeOutput(limit)
	stderr := newBoundedYouTubeDiagnostic(maxYouTubeCommandOutputBytes)
	cmd := exec.CommandContext(ctx, binaryName, args...)
	cmd.Stdout = output
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return nil, classifyYouTubeCommandError(err, stderr.data, stderr.overflow)
	}
	if output.overflow {
		return nil, errYouTubeResponseTooLarge
	}
	return output.data, nil
}

func readYouTubeHTML(body io.Reader, limit int64) (string, error) {
	content, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(content)) > limit {
		return "", youtubeStageError(errYouTubeUpstreamFailed, "artist html response", errYouTubeResponseTooLarge)
	}
	return string(content), nil
}

type youtubeLimitedResponseBody struct {
	body      io.ReadCloser
	remaining int64
}

func (b *youtubeLimitedResponseBody) Read(p []byte) (int, error) {
	if b.remaining < 0 {
		return 0, errYouTubeResponseTooLarge
	}
	if b.remaining == 0 {
		var probe [1]byte
		n, err := b.body.Read(probe[:])
		if n > 0 {
			b.remaining = -1
			return 0, errYouTubeResponseTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.body.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func (b *youtubeLimitedResponseBody) Close() error {
	return b.body.Close()
}

type youtubeMetadataLimitTransport struct {
	base  http.RoundTripper
	limit int64
}

func (t youtubeMetadataLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	response, err := base.RoundTrip(req)
	if err != nil || response == nil || isYouTubeMediaRequest(req) {
		return response, err
	}
	limit := t.limit
	if limit <= 0 {
		limit = maxYouTubeMetadataBytes
	}
	if response.ContentLength > limit {
		closeErr := response.Body.Close()
		if closeErr != nil {
			return nil, errors.Join(errYouTubeResponseTooLarge, closeErr)
		}
		return nil, errYouTubeResponseTooLarge
	}
	response.Body = &youtubeLimitedResponseBody{body: response.Body, remaining: limit}
	return response, nil
}

func isYouTubeMediaRequest(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	host := strings.ToLower(req.URL.Hostname())
	return host == "googlevideo.com" || strings.HasSuffix(host, ".googlevideo.com")
}

func addYouTubeBreadcrumb(ctx context.Context, operation, message string, count int) {
	hub := sentry.GetHubFromContext(ctx)
	if hub == nil {
		return
	}
	data := map[string]any{}
	if count > 0 {
		data["failure_count"] = count
	}
	hub.AddBreadcrumb(&sentry.Breadcrumb{
		Category: "youtube." + operation,
		Message:  message,
		Level:    sentry.LevelWarning,
		Data:     data,
	}, nil)
}

var (
	youtubePlaylistIDPattern = regexp.MustCompile(`(?:playlist\?list=|["']playlistId["']\s*:\s*["'])([A-Za-z0-9_-]{10,})`)
	youtubeVideoIDPattern    = regexp.MustCompile(`(?:watch\?v=|youtu\.be/|["']videoId["']\s*:\s*["'])([A-Za-z0-9_-]{10,})`)
)

type youtubeImportConfig struct {
	ImportTempDir         string
	RequestTimeout        time.Duration
	DownloadTimeout       time.Duration
	YTDLPBinary           string
	YTDLPJSRuntime        string
	YTDLPRemoteComponents string
	FFmpegBinary          string
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

type youtubeDownloaderUpdater interface {
	UpdateDownloader(ctx context.Context) error
}

type youtubeCookieSnapshotter interface {
	SnapshotCookies(destinationPath string) (bool, error)
}

type youtubeImportService struct {
	closeMu          sync.Mutex
	lifecycleMu      sync.Mutex
	operations       sync.WaitGroup
	closing          bool
	mu               sync.Mutex
	operationLocksMu sync.Mutex
	operationLocks   map[int64]*youtubeImportUserLock
	filesMu          sync.Mutex
	activeFiles      map[string]struct{}
	cfg              youtubeImportConfig
	gateway          youtubeImportGateway
	store            *trackStore
	songsDir         string
	sessions         map[int64]*youtubeImportSession
}

func (s *youtubeImportService) beginOperation() (func(), error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closing {
		return nil, errYouTubeShuttingDown
	}
	s.operations.Add(1)
	var finishOnce sync.Once
	return func() {
		finishOnce.Do(s.operations.Done)
	}, nil
}

type youtubeImportUserLock struct {
	token chan struct{}
	refs  int
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

func (s *youtubeCookieStore) pathIfPresent() (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(s.path) == "" {
		return "", false, nil
	}
	info, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, sanitizeYouTubeFilesystemError("inspect configured cookies file", err)
	}
	if info.IsDir() || info.Size() == 0 {
		return "", false, nil
	}
	return s.path, true, nil
}

func (s *youtubeCookieStore) Status() youtubeCookiesStatus {
	status, _ := s.status()
	return status
}

func (s *youtubeCookieStore) status() (youtubeCookiesStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	status := youtubeCookiesStatus{Configured: strings.TrimSpace(s.path) != ""}
	if !status.Configured {
		return status, nil
	}
	info, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return status, nil
	}
	if err != nil {
		return status, sanitizeYouTubeFilesystemError("inspect configured cookies file", err)
	}
	if info.IsDir() || info.Size() == 0 {
		return status, nil
	}
	lastModified := info.ModTime()
	status.FilePresent = true
	status.LastModified = &lastModified
	return status, nil
}

func (s *youtubeCookieStore) Replace(r io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(s.path) == "" {
		return errYouTubeCookiesNotSet
	}
	dir := filepath.Dir(s.path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return sanitizeYouTubeFilesystemError("create cookies directory", err)
		}
	}
	tempPath := s.path + ".tmp"
	file, err := os.OpenFile(tempPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return sanitizeYouTubeFilesystemError("create temporary cookies file", err)
	}
	copied, copyErr := io.Copy(file, io.LimitReader(r, maxYouTubeCookiesBytes+1))
	if copyErr != nil {
		closeErr := file.Close()
		removeErr := removeYouTubeFile(tempPath, "remove temporary cookies file")
		return errors.Join(
			sanitizeYouTubeFilesystemError("write cookies file", copyErr),
			sanitizeYouTubeFilesystemError("close temporary cookies file", closeErr),
			removeErr,
		)
	}
	if copied > maxYouTubeCookiesBytes {
		closeErr := file.Close()
		return errors.Join(
			errYouTubeCookiesTooLarge,
			newYouTubeCleanupError("close oversized temporary cookies file", sanitizeYouTubeFilesystemError("close temporary cookies file", closeErr)),
			removeYouTubeFile(tempPath, "remove oversized temporary cookies file"),
		)
	}
	if err := file.Close(); err != nil {
		return errors.Join(
			sanitizeYouTubeFilesystemError("close temporary cookies file", err),
			removeYouTubeFile(tempPath, "remove temporary cookies file"),
		)
	}
	info, err := os.Stat(tempPath)
	if err != nil {
		return errors.Join(
			sanitizeYouTubeFilesystemError("inspect temporary cookies file", err),
			removeYouTubeFile(tempPath, "remove temporary cookies file"),
		)
	}
	if info.Size() == 0 {
		return errors.Join(
			errYouTubeCookiesEmpty,
			removeYouTubeFile(tempPath, "remove temporary cookies file"),
		)
	}
	if err := os.Rename(tempPath, s.path); err != nil {
		return errors.Join(
			sanitizeYouTubeFilesystemError("replace cookies file", err),
			removeYouTubeFile(tempPath, "remove temporary cookies file"),
		)
	}
	return nil
}

func (s *youtubeCookieStore) Delete() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(s.path) == "" {
		return errYouTubeCookiesNotSet
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return sanitizeYouTubeFilesystemError("delete cookies file", err)
	}
	return nil
}

func (s *youtubeCookieStore) snapshotTo(destinationPath string) (resultErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	destinationCreated := false
	defer func() {
		if resultErr != nil && destinationCreated {
			resultErr = errors.Join(resultErr, removeYouTubeFile(destinationPath, "remove failed session cookies snapshot"))
		}
	}()

	if strings.TrimSpace(s.path) == "" {
		return os.ErrNotExist
	}
	source, err := os.Open(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.ErrNotExist
		}
		return sanitizeYouTubeFilesystemError("open configured cookies file", err)
	}
	defer func() {
		if err := source.Close(); err != nil {
			resultErr = errors.Join(resultErr, sanitizeYouTubeFilesystemError("close configured cookies file", err))
		}
	}()

	info, err := source.Stat()
	if err != nil {
		return sanitizeYouTubeFilesystemError("inspect configured cookies file", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return os.ErrNotExist
	}
	if info.Size() > maxYouTubeCookiesBytes {
		return errYouTubeCookiesTooLarge
	}

	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return sanitizeYouTubeFilesystemError("create session cookies snapshot", err)
	}
	destinationCreated = true
	copied, copyErr := io.CopyN(destination, source, maxYouTubeCookiesBytes+1)
	closeErr := destination.Close()
	if copyErr != nil && !errors.Is(copyErr, io.EOF) {
		return errors.Join(
			sanitizeYouTubeFilesystemError("copy configured cookies file", copyErr),
			sanitizeYouTubeFilesystemError("close session cookies snapshot", closeErr),
			removeYouTubeFile(destinationPath, "remove incomplete session cookies snapshot"),
		)
	}
	if copied > maxYouTubeCookiesBytes {
		return errors.Join(
			errYouTubeCookiesTooLarge,
			sanitizeYouTubeFilesystemError("close session cookies snapshot", closeErr),
			removeYouTubeFile(destinationPath, "remove oversized session cookies snapshot"),
		)
	}
	if closeErr != nil {
		return errors.Join(
			sanitizeYouTubeFilesystemError("close session cookies snapshot", closeErr),
			removeYouTubeFile(destinationPath, "remove incomplete session cookies snapshot"),
		)
	}
	return nil
}

func newYouTubeImportService(cfg youtubeImportConfig, gateway youtubeImportGateway, store *trackStore, songsDir string) *youtubeImportService {
	return &youtubeImportService{
		cfg:            cfg,
		gateway:        gateway,
		store:          store,
		songsDir:       songsDir,
		sessions:       make(map[int64]*youtubeImportSession),
		operationLocks: make(map[int64]*youtubeImportUserLock),
		activeFiles:    make(map[string]struct{}),
	}
}

func (s *youtubeImportService) lockUserOperation(ctx context.Context, userID int64) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.operationLocksMu.Lock()
	if s.operationLocks == nil {
		s.operationLocks = make(map[int64]*youtubeImportUserLock)
	}
	operationLock := s.operationLocks[userID]
	if operationLock == nil {
		operationLock = &youtubeImportUserLock{token: make(chan struct{}, 1)}
		operationLock.token <- struct{}{}
		s.operationLocks[userID] = operationLock
	}
	operationLock.refs++
	s.operationLocksMu.Unlock()

	select {
	case <-operationLock.token:
		return func() {
			operationLock.token <- struct{}{}
			s.releaseUserOperationRef(userID, operationLock)
		}, nil
	case <-ctx.Done():
		s.releaseUserOperationRef(userID, operationLock)
		return nil, ctx.Err()
	}
}

func (s *youtubeImportService) releaseUserOperationRef(userID int64, operationLock *youtubeImportUserLock) {
	s.operationLocksMu.Lock()
	defer s.operationLocksMu.Unlock()

	operationLock.refs--
	if operationLock.refs == 0 && s.operationLocks[userID] == operationLock {
		delete(s.operationLocks, userID)
	}
}

func (s *youtubeImportService) registerActiveFile(path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	s.filesMu.Lock()
	if s.activeFiles == nil {
		s.activeFiles = make(map[string]struct{})
	}
	s.activeFiles[path] = struct{}{}
	s.filesMu.Unlock()
}

func (s *youtubeImportService) removeActiveFile(path, operation string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	err := removeYouTubeFile(path, operation)
	if err == nil {
		s.filesMu.Lock()
		delete(s.activeFiles, path)
		s.filesMu.Unlock()
	}
	return err
}

// Close prevents new mutations, waits for in-flight mutations to finish, and
// removes all ephemeral YouTube import files.
func (s *youtubeImportService) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()

	s.lifecycleMu.Lock()
	s.closing = true
	s.lifecycleMu.Unlock()
	s.operations.Wait()

	s.mu.Lock()
	cleanupErrs := make([]error, 0)
	for userID, session := range s.sessions {
		if err := s.cleanupSessionFilesLocked(session); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
		delete(s.sessions, userID)
	}
	s.mu.Unlock()

	s.filesMu.Lock()
	paths := make([]string, 0, len(s.activeFiles))
	for path := range s.activeFiles {
		paths = append(paths, path)
	}
	s.filesMu.Unlock()
	for _, path := range paths {
		if err := s.removeActiveFile(path, "remove active youtube import file"); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
	}
	if err := cleanupStaleYouTubeSongStaging(s.songsDir); err != nil {
		cleanupErrs = append(cleanupErrs, err)
	}
	return errors.Join(cleanupErrs...)
}

func cleanupStaleYouTubeSongStaging(songsDir string) error {
	stagingDir := filepath.Join(songsDir, ".youtube-import-staging")
	entries, err := os.ReadDir(stagingDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return sanitizeYouTubeFilesystemError("read youtube audio staging directory", err)
	}

	cleanupErrs := make([]error, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			cleanupErrs = append(cleanupErrs, youtubeStageError(
				errYouTubeStorageFailed,
				"clean youtube audio staging directory",
				errors.New("unexpected nested directory"),
			))
			continue
		}
		if err := removeYouTubeFile(filepath.Join(stagingDir, entry.Name()), "remove stale staged audio file"); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
	}
	if len(cleanupErrs) > 0 {
		return errors.Join(cleanupErrs...)
	}
	if err := os.Remove(stagingDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return sanitizeYouTubeFilesystemError("remove youtube audio staging directory", err)
	}
	return nil
}

func (s *youtubeImportService) StartSession(ctx context.Context, userID int64, rawURL string, cutoff *time.Time, replaceExisting bool, youtubeCookies string) (result youtubeImportSessionDTO, resultErr error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return youtubeImportSessionDTO{}, errYouTubeInvalidURL
	}
	finishOperation, err := s.beginOperation()
	if err != nil {
		return youtubeImportSessionDTO{}, err
	}
	defer finishOperation()
	unlockOperation, err := s.lockUserOperation(ctx, userID)
	if err != nil {
		return youtubeImportSessionDTO{}, err
	}
	defer unlockOperation()
	if err := ctx.Err(); err != nil {
		return youtubeImportSessionDTO{}, err
	}
	s.mu.Lock()
	if existing, ok := s.sessions[userID]; ok && existing.Status == youtubeImportStatusActive && !replaceExisting {
		s.mu.Unlock()
		return youtubeImportSessionDTO{}, errYouTubeSessionActive
	}
	s.mu.Unlock()
	if updater, ok := s.gateway.(youtubeDownloaderUpdater); ok {
		if err := updater.UpdateDownloader(ctx); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return youtubeImportSessionDTO{}, ctxErr
			}
			log.Printf("youtube downloader update failed diagnostic=%s", youtubeErrorDiagnostic(err))
			addYouTubeBreadcrumb(ctx, "update_downloader", "yt-dlp update failed; continuing with installed version", 1)
		}
	}
	if err := ctx.Err(); err != nil {
		return youtubeImportSessionDTO{}, err
	}

	cookiesPath, cleanupCookies, err := s.writeSessionCookies(userID, youtubeCookies)
	if err != nil {
		return youtubeImportSessionDTO{}, err
	}
	committedCookies := false
	defer func() {
		if !committedCookies {
			resultErr = errors.Join(resultErr, cleanupCookies())
		}
	}()

	items, source, err := s.gateway.Scan(ctx, rawURL, cutoff, cookiesPath)
	if err != nil {
		return youtubeImportSessionDTO{}, err
	}
	if len(items) == 0 {
		return youtubeImportSessionDTO{}, errYouTubeNoTracks
	}
	if err := ctx.Err(); err != nil {
		return youtubeImportSessionDTO{}, err
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

	if err := ctx.Err(); err != nil {
		return youtubeImportSessionDTO{}, err
	}
	var replacedSessionCleanupErr error
	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return youtubeImportSessionDTO{}, err
	}
	if existing, ok := s.sessions[userID]; ok && existing.Status == youtubeImportStatusActive && !replaceExisting {
		s.mu.Unlock()
		return youtubeImportSessionDTO{}, errYouTubeSessionActive
	}
	if existing, ok := s.sessions[userID]; ok {
		replacedSessionCleanupErr = s.cleanupSessionFilesLocked(existing)
	}
	s.sessions[userID] = session
	s.mu.Unlock()
	committedCookies = true
	if replacedSessionCleanupErr != nil {
		captureSentryError(ctx, replacedSessionCleanupErr, "youtube", "import.start.cleanup")
	}

	return s.CurrentSession(userID)
}

func (s *youtubeImportService) writeSessionCookies(userID int64, rawCookies string) (string, func() error, error) {
	rawCookies = strings.TrimSpace(rawCookies)
	if len(rawCookies) > maxYouTubeCookiesBytes {
		return "", func() error { return nil }, errYouTubeCookiesTooLarge
	}
	if err := os.MkdirAll(s.cfg.ImportTempDir, 0o700); err != nil {
		return "", func() error { return nil }, sanitizeYouTubeFilesystemError("create import directory", err)
	}
	token, err := randomToken(12)
	if err != nil {
		return "", func() error { return nil }, fmt.Errorf("generate session cookies file token: %w", err)
	}
	path := filepath.Join(s.cfg.ImportTempDir, fmt.Sprintf("youtube-cookies-%d-%s.txt", userID, token))
	if rawCookies != "" {
		if err := os.WriteFile(path, []byte(rawCookies+"\n"), 0o600); err != nil {
			return "", func() error { return nil }, errors.Join(
				sanitizeYouTubeFilesystemError("write session cookies file", err),
				removeYouTubeFile(path, "remove incomplete session cookies file"),
			)
		}
	} else {
		snapshotter, ok := s.gateway.(youtubeCookieSnapshotter)
		if !ok {
			return "", func() error { return nil }, nil
		}
		present, err := snapshotter.SnapshotCookies(path)
		if err != nil {
			return "", func() error { return nil }, err
		}
		if !present {
			return "", func() error { return nil }, nil
		}
	}
	s.registerActiveFile(path)
	return path, func() error { return s.removeActiveFile(path, "remove session cookies file") }, nil
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
	return s.skipCurrent(context.Background(), userID)
}

func (s *youtubeImportService) skipCurrent(ctx context.Context, userID int64) (youtubeImportSessionDTO, error) {
	finishOperation, err := s.beginOperation()
	if err != nil {
		return youtubeImportSessionDTO{}, err
	}
	defer finishOperation()

	unlockOperation, err := s.lockUserOperation(ctx, userID)
	if err != nil {
		return youtubeImportSessionDTO{}, err
	}
	defer unlockOperation()
	if err := ctx.Err(); err != nil {
		return youtubeImportSessionDTO{}, err
	}

	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return youtubeImportSessionDTO{}, err
	}
	session, ok := s.sessions[userID]
	if !ok {
		s.mu.Unlock()
		return youtubeImportSessionDTO{}, errYouTubeSessionNotFound
	}
	item, ok := session.currentItem()
	if !ok {
		result := s.buildSessionDTOLocked(session)
		s.mu.Unlock()
		return result, nil
	}
	session.SkippedItems = append(session.SkippedItems, youtubeSkippedItem{
		VideoID:   item.VideoID,
		SourceURL: item.SourceURL,
		Title:     item.ParsedTitle,
	})
	session.CurrentIndex++
	session.UpdatedAt = time.Now().UTC()
	var cleanupErr error
	if session.CurrentIndex >= len(session.Items) {
		session.Status = youtubeImportStatusCompleted
		cleanupErr = s.cleanupSessionFilesLocked(session)
	}
	result := s.buildSessionDTOLocked(session)
	s.mu.Unlock()
	if cleanupErr != nil {
		captureSentryError(ctx, cleanupErr, "youtube", "import.skip.cleanup")
	}
	return result, nil
}

func (s *youtubeImportService) AddCurrent(ctx context.Context, userID int64, req youtubeAddCurrentRequest) (result youtubeImportSessionDTO, savedTrack track, resultErr error) {
	finishOperation, err := s.beginOperation()
	if err != nil {
		return youtubeImportSessionDTO{}, track{}, err
	}
	defer finishOperation()

	unlockOperation, err := s.lockUserOperation(ctx, userID)
	if err != nil {
		return youtubeImportSessionDTO{}, track{}, err
	}
	defer unlockOperation()
	if err := ctx.Err(); err != nil {
		return youtubeImportSessionDTO{}, track{}, err
	}

	s.mu.Lock()
	session, ok := s.sessions[userID]
	if !ok {
		s.mu.Unlock()
		return youtubeImportSessionDTO{}, track{}, errYouTubeSessionNotFound
	}
	itemIndex := session.CurrentIndex
	sessionID := session.ID
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

		preparedAudio, err := s.downloadCurrentToSongs(ctx, itemIndex, item, cookiesPath)
		if err != nil {
			return youtubeImportSessionDTO{}, track{}, err
		}
		committed := false
		defer func() {
			cleanupErr := preparedAudio.cleanup(committed)
			if cleanupErr == nil {
				return
			}
			if resultErr != nil {
				resultErr = errors.Join(resultErr, cleanupErr)
				return
			}
			captureSentryError(ctx, cleanupErr, "youtube", "import.add.cleanup")
		}()
		if err := s.validateCurrentSession(userID, sessionID, itemIndex); err != nil {
			return youtubeImportSessionDTO{}, track{}, err
		}
		if err := ctx.Err(); err != nil {
			return youtubeImportSessionDTO{}, track{}, err
		}

		songsMutationMu.Lock()
		if err := ctx.Err(); err != nil {
			songsMutationMu.Unlock()
			return youtubeImportSessionDTO{}, track{}, err
		}
		finalName, finalPath, err := availableYouTubeSongDestination(s.songsDir, preparedAudio.fileName)
		if err != nil {
			songsMutationMu.Unlock()
			return youtubeImportSessionDTO{}, track{}, err
		}
		preparedAudio.finalPath = finalPath
		createdTrack, err := s.store.createTrackIfSourceAbsent(upsertTrackRequest{
			Name:           strings.TrimSpace(req.Name),
			AuthorIDs:      req.AuthorIDs,
			AlbumID:        req.AlbumID,
			AlbumOrder:     req.AlbumOrder,
			AudioFilePath:  "/api/songs/" + url.PathEscape(finalName),
			AdditionalInfo: generatedAdditionalInfo,
			SourceMetadata: generatedSourceMetadata,
		}, generatedSourceMetadata[0], func() error {
			if err := os.Link(preparedAudio.stagingPath, finalPath); err != nil {
				return sanitizeYouTubeFilesystemError("publish imported audio file", err)
			}
			return nil
		})
		songsMutationMu.Unlock()
		if err != nil {
			return youtubeImportSessionDTO{}, track{}, err
		}
		committed = true
		return s.advanceSaved(ctx, userID, sessionID, itemIndex, createdTrack)
	case youtubeAddModeAttach:
		if err := s.validateCurrentSession(userID, sessionID, itemIndex); err != nil {
			return youtubeImportSessionDTO{}, track{}, err
		}
		if err := ctx.Err(); err != nil {
			return youtubeImportSessionDTO{}, track{}, err
		}
		updatedTrack, exists, err := s.store.attachTrackImportMetadataIfSourceAbsent(req.TrackID, generatedAdditionalInfo, generatedSourceMetadata, generatedSourceMetadata[0])
		if err != nil {
			return youtubeImportSessionDTO{}, track{}, err
		}
		if !exists {
			return youtubeImportSessionDTO{}, track{}, errTrackNotFound
		}
		return s.advanceSaved(ctx, userID, sessionID, itemIndex, updatedTrack)
	default:
		return youtubeImportSessionDTO{}, track{}, errYouTubeUnsupportedMode
	}
}

func (s *youtubeImportService) validateCurrentSession(userID int64, expectedSessionID string, expectedIndex int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	session, ok := s.sessions[userID]
	if !ok {
		return errYouTubeSessionNotFound
	}
	if session.ID != expectedSessionID || session.Status != youtubeImportStatusActive || session.CurrentIndex != expectedIndex {
		return errYouTubeSessionChanged
	}
	if _, ok := session.currentItem(); !ok {
		return errYouTubeCurrentNotFound
	}
	return nil
}

func (s *youtubeImportService) advanceSaved(ctx context.Context, userID int64, expectedSessionID string, expectedIndex int, savedTrack track) (youtubeImportSessionDTO, track, error) {
	s.mu.Lock()
	session, ok := s.sessions[userID]
	if !ok {
		s.mu.Unlock()
		return youtubeImportSessionDTO{}, track{}, errYouTubeSessionNotFound
	}
	if session.ID != expectedSessionID || session.Status != youtubeImportStatusActive || session.CurrentIndex != expectedIndex {
		s.mu.Unlock()
		return youtubeImportSessionDTO{}, track{}, errYouTubeSessionChanged
	}
	session.CurrentIndex++
	session.SavedCount++
	session.UpdatedAt = time.Now().UTC()
	var cleanupErr error
	if session.CurrentIndex >= len(session.Items) {
		session.Status = youtubeImportStatusCompleted
		cleanupErr = s.cleanupSessionFilesLocked(session)
	}
	result := s.buildSessionDTOLocked(session)
	s.mu.Unlock()
	if cleanupErr != nil {
		captureSentryError(ctx, cleanupErr, "youtube", "import.add.cleanup")
	}
	return result, savedTrack, nil
}

func (s *youtubeImportService) CancelSession(userID int64) error {
	return s.cancelSession(context.Background(), userID)
}

func (s *youtubeImportService) cancelSession(ctx context.Context, userID int64) error {
	finishOperation, err := s.beginOperation()
	if err != nil {
		return err
	}
	defer finishOperation()

	unlockOperation, err := s.lockUserOperation(ctx, userID)
	if err != nil {
		return err
	}
	defer unlockOperation()
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	if _, ok := s.sessions[userID]; !ok {
		return errYouTubeSessionNotFound
	}
	if err := s.cleanupSessionFilesLocked(s.sessions[userID]); err != nil {
		return err
	}
	delete(s.sessions, userID)
	return nil
}

func (s *youtubeImportService) cleanupSessionFilesLocked(session *youtubeImportSession) error {
	if strings.TrimSpace(session.CookiesPath) != "" {
		if err := s.removeActiveFile(session.CookiesPath, "remove session cookies file"); err != nil {
			return err
		}
		session.CookiesPath = ""
	}
	return nil
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

type preparedYouTubeAudio struct {
	service     *youtubeImportService
	fileName    string
	sourcePath  string
	stagingPath string
	finalPath   string
}

func (a *preparedYouTubeAudio) cleanup(committed bool) error {
	cleanupErr := errors.Join(
		a.service.removeActiveFile(a.sourcePath, "remove temporary audio file"),
		a.service.removeActiveFile(a.stagingPath, "remove staged audio file"),
	)
	if !committed && strings.TrimSpace(a.finalPath) != "" {
		cleanupErr = errors.Join(cleanupErr, removeYouTubeFile(a.finalPath, "remove uncommitted audio file"))
	}
	return cleanupErr
}

func availableYouTubeSongDestination(dir, fileName string) (string, string, error) {
	base := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	ext := filepath.Ext(fileName)
	for attempt := 0; attempt < 10_000; attempt++ {
		candidate := fileName
		if attempt > 0 {
			candidate = fmt.Sprintf("%s-%d%s", base, attempt, ext)
		}
		path := filepath.Join(dir, candidate)
		_, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return candidate, path, nil
		}
		if err != nil {
			return "", "", sanitizeYouTubeFilesystemError("inspect destination audio file", err)
		}
	}
	return "", "", youtubeStageError(errYouTubeStorageFailed, "select destination audio file name", errors.New("unique filename attempts exhausted"))
}

func (s *youtubeImportService) downloadCurrentToSongs(ctx context.Context, index int, item youtubeImportItem, cookiesPath string) (*preparedYouTubeAudio, error) {
	operationCtx, cancel := withYouTubeDownloadTimeout(ctx, s.cfg.DownloadTimeout)
	defer cancel()

	if err := os.MkdirAll(s.cfg.ImportTempDir, 0o755); err != nil {
		return nil, sanitizeYouTubeFilesystemError("create import directory", err)
	}
	if err := os.MkdirAll(s.songsDir, 0o755); err != nil {
		return nil, sanitizeYouTubeFilesystemError("create songs directory", err)
	}
	stagingDir := filepath.Join(s.songsDir, ".youtube-import-staging")
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		return nil, sanitizeYouTubeFilesystemError("create audio staging directory", err)
	}
	fileName, err := youtubeImportFileName(item)
	if err != nil {
		return nil, err
	}
	tempSourceName, err := randomizedStoredFileName("youtube-source-" + item.VideoID + ".bin")
	if err != nil {
		return nil, fmt.Errorf("generate temporary audio file name: %w", err)
	}
	tempSourcePath := filepath.Join(s.cfg.ImportTempDir, fmt.Sprintf("%03d-%s", index+1, tempSourceName))
	s.registerActiveFile(tempSourcePath)
	downloadSpan := sentry.StartSpan(operationCtx, "youtube.download", sentry.WithDescription("audio"))
	err = s.gateway.DownloadAudio(downloadSpan.Context(), item, tempSourcePath, cookiesPath)
	downloadSpan.Finish()
	if err != nil {
		if ctxErr := operationCtx.Err(); ctxErr != nil {
			err = errors.Join(err, ctxErr)
		}
		if !errors.Is(err, errYouTubeDownloadFailed) {
			err = youtubeStageError(errYouTubeDownloadFailed, "download", err)
		}
		return nil, errors.Join(err, s.removeActiveFile(tempSourcePath, "remove temporary audio file"))
	}

	stagingFile, err := os.CreateTemp(stagingDir, "youtube-import-*.mp3")
	if err != nil {
		return nil, errors.Join(
			sanitizeYouTubeFilesystemError("create staged audio file", err),
			s.removeActiveFile(tempSourcePath, "remove temporary audio file"),
		)
	}
	stagingPath := stagingFile.Name()
	s.registerActiveFile(stagingPath)
	if err := stagingFile.Chmod(0o644); err != nil {
		return nil, errors.Join(
			sanitizeYouTubeFilesystemError("set staged audio file permissions", err),
			sanitizeYouTubeFilesystemError("close staged audio file", stagingFile.Close()),
			s.removeActiveFile(tempSourcePath, "remove temporary audio file"),
			s.removeActiveFile(stagingPath, "remove staged audio file"),
		)
	}
	if err := stagingFile.Close(); err != nil {
		return nil, errors.Join(
			sanitizeYouTubeFilesystemError("close staged audio file", err),
			s.removeActiveFile(tempSourcePath, "remove temporary audio file"),
			s.removeActiveFile(stagingPath, "remove staged audio file"),
		)
	}
	if err := s.transcodeToMP3(operationCtx, tempSourcePath, stagingPath); err != nil {
		return nil, errors.Join(
			err,
			s.removeActiveFile(tempSourcePath, "remove temporary audio file"),
			s.removeActiveFile(stagingPath, "remove incomplete staged audio file"),
		)
	}
	return &preparedYouTubeAudio{
		service:     s,
		fileName:    fileName,
		sourcePath:  tempSourcePath,
		stagingPath: stagingPath,
	}, nil
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
	span := sentry.StartSpan(ctx, "youtube.transcode", sentry.WithDescription("mp3"))
	defer span.Finish()
	ctx = span.Context()

	binaryName := strings.TrimSpace(s.cfg.FFmpegBinary)
	if binaryName == "" {
		binaryName = "ffmpeg"
	}
	if _, err := exec.LookPath(binaryName); err != nil {
		log.Printf("youtube transcode failed stage=ffmpeg_lookup diagnostic=%s", youtubeErrorDiagnostic(err))
		cause, _ := sanitizeYouTubeCause(err)
		dependencyErr := youtubeStageError(errYouTubeDependency, "ffmpeg lookup", cause)
		return youtubeStageError(errYouTubeTranscodeFailed, "ffmpeg lookup", dependencyErr)
	}
	args := []string{
		"-y",
		"-i", sourcePath,
		"-vn",
		"-codec:a", "libmp3lame",
		"-q:a", "2",
		"-fs", strconv.FormatInt(maxSongUploadSize, 10),
		targetPath,
	}
	stderr := newBoundedYouTubeDiagnostic(maxYouTubeCommandOutputBytes)
	cmd := exec.CommandContext(ctx, binaryName, args...)
	cmd.Stderr = stderr
	err := cmd.Run()
	if err != nil {
		err = classifyYouTubeCommandError(err, stderr.data, stderr.overflow)
		log.Printf("youtube transcode failed stage=ffmpeg_run diagnostic=%s", youtubeErrorDiagnostic(err))
		cause := err
		if ctxErr := ctx.Err(); ctxErr != nil {
			cause = errors.Join(cause, ctxErr)
		}
		return youtubeStageError(errYouTubeTranscodeFailed, "ffmpeg run", cause)
	}
	info, err := os.Stat(targetPath)
	if err != nil || info.Size() == 0 {
		if err == nil {
			err = errors.New("ffmpeg produced empty output file")
		} else {
			err = sanitizeYouTubeFilesystemError("inspect transcoded audio file", err)
		}
		log.Printf("youtube transcode failed stage=ffmpeg_output diagnostic=%s", youtubeErrorDiagnostic(err))
		return youtubeStageError(errYouTubeTranscodeFailed, "ffmpeg output", err)
	}
	if info.Size() > maxSongUploadSize {
		return youtubeStageError(errYouTubeTranscodeFailed, "ffmpeg output", errYouTubeAudioTooLarge)
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
	httpClient  *http.Client
	cfg         youtubeImportConfig
	cookieStore *youtubeCookieStore
	updateMu    sync.Mutex
}

func newLiveYouTubeImportGateway(cfg youtubeImportConfig, cookieStore *youtubeCookieStore) *liveYouTubeImportGateway {
	httpClient := &http.Client{
		Transport: youtubeMetadataLimitTransport{
			base:  http.DefaultTransport,
			limit: maxYouTubeMetadataBytes,
		},
	}
	return &liveYouTubeImportGateway{
		httpClient:  httpClient,
		cfg:         cfg,
		cookieStore: cookieStore,
	}
}

func (g *liveYouTubeImportGateway) SnapshotCookies(destinationPath string) (bool, error) {
	if g.cookieStore == nil {
		return false, nil
	}
	err := g.cookieStore.snapshotTo(destinationPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (g *liveYouTubeImportGateway) newClient() *youtube.Client {
	return &youtube.Client{HTTPClient: g.httpClient}
}

func (g *liveYouTubeImportGateway) UpdateDownloader(ctx context.Context) error {
	g.updateMu.Lock()
	defer g.updateMu.Unlock()

	ctx, cancel := withYouTubeTimeout(ctx, g.cfg.RequestTimeout)
	defer cancel()
	span := sentry.StartSpan(ctx, "youtube.update_downloader", sentry.WithDescription("yt-dlp"))
	defer span.Finish()
	ctx = span.Context()

	binaryName := strings.TrimSpace(g.cfg.YTDLPBinary)
	if binaryName == "" {
		binaryName = "yt-dlp"
	}
	binaryPath, err := exec.LookPath(binaryName)
	if err != nil {
		cause, _ := sanitizeYouTubeCause(err)
		return youtubeStageError(errYouTubeDependency, "yt-dlp update lookup", cause)
	}
	stderr := newBoundedYouTubeDiagnostic(maxYouTubeCommandOutputBytes)
	cmd := exec.CommandContext(ctx, binaryPath, "--ignore-config", "--update")
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		err = classifyYouTubeCommandError(err, stderr.data, stderr.overflow)
		cause := err
		if ctxErr := ctx.Err(); ctxErr != nil {
			cause = errors.Join(cause, ctxErr)
		}
		return youtubeStageError(errYouTubeDependency, "yt-dlp update", cause)
	}
	return nil
}

func (g *liveYouTubeImportGateway) ytdlpArgs(args ...string) []string {
	result := make([]string, 0, len(args)+7)
	result = append(result, "--ignore-config", "--no-js-runtimes", "--no-remote-components")
	if runtime := strings.TrimSpace(g.cfg.YTDLPJSRuntime); runtime != "" {
		result = append(result, "--js-runtimes", runtime)
	}
	if components := strings.TrimSpace(g.cfg.YTDLPRemoteComponents); components != "" && !strings.EqualFold(components, "none") {
		result = append(result, "--remote-components", components)
	}
	return append(result, args...)
}

func (g *liveYouTubeImportGateway) ValidateDependencies(ctx context.Context) error {
	ctx, cancel := withYouTubeTimeout(ctx, youtubeDependencyCheckTimeout)
	defer cancel()

	ytdlpPath, err := exec.LookPath(firstNonEmpty(g.cfg.YTDLPBinary, "yt-dlp"))
	if err != nil {
		cause, _ := sanitizeYouTubeCause(err)
		return youtubeStageError(errYouTubeDependency, "yt-dlp lookup", cause)
	}
	ffmpegPath, err := exec.LookPath(firstNonEmpty(g.cfg.FFmpegBinary, "ffmpeg"))
	if err != nil {
		cause, _ := sanitizeYouTubeCause(err)
		return youtubeStageError(errYouTubeDependency, "ffmpeg lookup", cause)
	}
	runtimeSpec, err := validateYouTubeJSRuntime(ctx, g.cfg.YTDLPJSRuntime)
	if err != nil {
		return err
	}
	g.cfg.YTDLPJSRuntime = runtimeSpec

	ytdlpVersion, err := runYouTubeCommandWithOutputLimit(ctx, ytdlpPath, g.ytdlpArgs("--version"), maxYouTubeCommandOutputBytes)
	if err != nil {
		return youtubeStageError(errYouTubeDependency, "yt-dlp version check", err)
	}
	if len(strings.TrimSpace(string(ytdlpVersion))) == 0 {
		return youtubeStageError(errYouTubeDependency, "yt-dlp version check", errors.New("yt-dlp returned an empty version"))
	}
	ffmpegVersion, err := runYouTubeCommandWithOutputLimit(ctx, ffmpegPath, []string{"-version"}, maxYouTubeCommandOutputBytes)
	if err != nil {
		return youtubeStageError(errYouTubeDependency, "ffmpeg version check", err)
	}
	if !strings.Contains(strings.ToLower(string(ffmpegVersion)), "ffmpeg version") {
		return youtubeStageError(errYouTubeDependency, "ffmpeg version check", errors.New("unexpected ffmpeg version output"))
	}
	return nil
}

func validateYouTubeJSRuntime(ctx context.Context, runtimeSpec string) (string, error) {
	runtimeSpec = strings.TrimSpace(runtimeSpec)
	if runtimeSpec == "" {
		return "", youtubeStageError(errYouTubeDependency, "yt-dlp JavaScript runtime", errors.New("JavaScript runtime is not configured"))
	}
	runtimeName, binaryName, hasPath := strings.Cut(runtimeSpec, ":")
	runtimeName = strings.ToLower(strings.TrimSpace(runtimeName))
	switch runtimeName {
	case "deno", "node", "quickjs", "bun":
	default:
		return "", youtubeStageError(errYouTubeDependency, "yt-dlp JavaScript runtime", errors.New("unsupported JavaScript runtime"))
	}
	if !hasPath || strings.TrimSpace(binaryName) == "" {
		binaryName = runtimeName
		if runtimeName == "quickjs" {
			binaryName = "qjs"
		}
	}
	binaryPath, err := exec.LookPath(strings.TrimSpace(binaryName))
	if err != nil {
		cause, _ := sanitizeYouTubeCause(err)
		return "", youtubeStageError(errYouTubeDependency, "yt-dlp JavaScript runtime lookup", cause)
	}
	output, err := runYouTubeCommandWithOutputLimit(ctx, binaryPath, []string{"--version"}, maxYouTubeCommandOutputBytes)
	if err != nil {
		return "", youtubeStageError(errYouTubeDependency, "yt-dlp JavaScript runtime version check", err)
	}
	versionFields := strings.Fields(string(output))
	switch runtimeName {
	case "deno":
		if len(versionFields) < 2 || !strings.EqualFold(versionFields[0], "deno") || compareNumericVersion(versionFields[1], "2.3.0") < 0 {
			return "", youtubeStageError(errYouTubeDependency, "yt-dlp JavaScript runtime version check", errors.New("Deno 2.3.0 or newer is required"))
		}
	case "node":
		if len(versionFields) == 0 || compareNumericVersion(versionFields[0], "22.0.0") < 0 {
			return "", youtubeStageError(errYouTubeDependency, "yt-dlp JavaScript runtime version check", errors.New("Node 22.0.0 or newer is required"))
		}
	case "bun":
		if len(versionFields) == 0 || compareNumericVersion(versionFields[0], "1.2.11") < 0 || compareNumericVersion(versionFields[0], "1.3.14") > 0 {
			return "", youtubeStageError(errYouTubeDependency, "yt-dlp JavaScript runtime version check", errors.New("Bun 1.2.11 through 1.3.14 is required"))
		}
	case "quickjs":
		if len(versionFields) == 0 {
			return "", youtubeStageError(errYouTubeDependency, "yt-dlp JavaScript runtime version check", errors.New("QuickJS returned an empty version"))
		}
	}
	return runtimeName + ":" + binaryPath, nil
}

func compareNumericVersion(left, right string) int {
	parse := func(value string) []int {
		parts := strings.Split(value, ".")
		result := make([]int, len(parts))
		for index, part := range parts {
			digits := strings.TrimLeftFunc(part, func(r rune) bool { return r < '0' || r > '9' })
			digits = strings.TrimRightFunc(digits, func(r rune) bool { return r < '0' || r > '9' })
			result[index], _ = strconv.Atoi(digits)
		}
		return result
	}
	leftParts, rightParts := parse(left), parse(right)
	for index := 0; index < max(len(leftParts), len(rightParts)); index++ {
		var leftPart, rightPart int
		if index < len(leftParts) {
			leftPart = leftParts[index]
		}
		if index < len(rightParts) {
			rightPart = rightParts[index]
		}
		if leftPart < rightPart {
			return -1
		}
		if leftPart > rightPart {
			return 1
		}
	}
	return 0
}

func (g *liveYouTubeImportGateway) Scan(ctx context.Context, rawURL string, cutoff *time.Time, cookiesPath string) ([]youtubeImportItem, youtubeImportScanSource, error) {
	sourceType, canonicalURL, err := classifyYouTubeImportURL(rawURL)
	if err != nil {
		return nil, youtubeImportScanSource{}, err
	}
	ctx, cancel := withYouTubeTimeout(ctx, g.cfg.RequestTimeout)
	defer cancel()
	span := sentry.StartSpan(ctx, "youtube.scan", sentry.WithDescription(sourceType))
	defer span.Finish()
	ctx = span.Context()
	client := g.newClient()

	switch sourceType {
	case youtubeImportSourceTrack:
		video, err := client.GetVideoContext(ctx, canonicalURL)
		if err != nil {
			item, fallbackErr := g.scanTrackWithYTDLP(ctx, canonicalURL, rawURL, cookiesPath)
			if fallbackErr != nil {
				log.Printf(
					"youtube track scan fallbacks failed primary_diagnostic=%s fallback_diagnostic=%s",
					youtubeErrorDiagnostic(err),
					youtubeErrorDiagnostic(fallbackErr),
				)
				return nil, youtubeImportScanSource{}, combineYouTubeScanErrors(err, fallbackErr)
			}
			addYouTubeBreadcrumb(ctx, "scan", "track metadata fallback succeeded", 1)
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
		linkProvider := linkProviderFromURL(rawURL)
		items, primaryErr := g.scanPlaylist(ctx, client, canonicalURL, rawURL, linkProvider, cutoff)
		if primaryErr == nil {
			return items, youtubeImportScanSource{SourceType: youtubeImportSourcePlaylist, CanonicalURL: canonicalURL}, nil
		}
		fallbackItems, fallbackErr := g.scanPlaylistWithYTDLP(ctx, canonicalURL, rawURL, linkProvider, cutoff, cookiesPath)
		if fallbackErr == nil {
			addYouTubeBreadcrumb(ctx, "scan", "yt-dlp playlist fallback succeeded", 1)
			return fallbackItems, youtubeImportScanSource{SourceType: youtubeImportSourcePlaylist, CanonicalURL: canonicalURL}, nil
		}
		log.Printf(
			"youtube playlist scan fallbacks failed primary_diagnostic=%s fallback_diagnostic=%s",
			youtubeErrorDiagnostic(primaryErr),
			youtubeErrorDiagnostic(fallbackErr),
		)
		return nil, youtubeImportScanSource{}, combineYouTubeScanErrors(primaryErr, fallbackErr)
	case youtubeImportSourceArtist:
		items, err := g.scanArtist(ctx, client, canonicalURL, cutoff, cookiesPath)
		return items, youtubeImportScanSource{SourceType: youtubeImportSourceArtist, CanonicalURL: canonicalURL}, classifyYouTubeScanError(err)
	default:
		return nil, youtubeImportScanSource{}, errYouTubeInvalidURL
	}
}

func (g *liveYouTubeImportGateway) DownloadAudio(ctx context.Context, item youtubeImportItem, destinationPath string, cookiesPath string) (resultErr error) {
	ctx, cancel := withYouTubeDownloadTimeout(ctx, g.cfg.DownloadTimeout)
	defer cancel()

	resolvedCookiesPath, hasCookies, err := g.resolveCookiesPath(cookiesPath)
	if err != nil {
		return youtubeDownloadError(item, "resolve_cookies", nil, err)
	}
	if hasCookies {
		ytdlpErr := g.downloadAudioWithYTDLP(ctx, item, destinationPath, resolvedCookiesPath)
		if ytdlpErr == nil || !shouldTryYouTubeDownloadFallback(ctx, ytdlpErr) {
			return ytdlpErr
		}
		nativeErr := g.downloadAudioWithClient(ctx, item, destinationPath)
		if nativeErr == nil {
			addYouTubeBreadcrumb(ctx, "download", "native download fallback succeeded", 1)
			return nil
		}
		return errors.Join(ytdlpErr, nativeErr)
	}
	nativeErr := g.downloadAudioWithClient(ctx, item, destinationPath)
	if nativeErr == nil || !shouldTryYouTubeDownloadFallback(ctx, nativeErr) {
		return nativeErr
	}
	ytdlpErr := g.downloadAudioWithYTDLP(ctx, item, destinationPath, "")
	if ytdlpErr == nil {
		addYouTubeBreadcrumb(ctx, "download", "yt-dlp download fallback succeeded", 1)
		return nil
	}
	return errors.Join(nativeErr, ytdlpErr)
}

func (g *liveYouTubeImportGateway) downloadAudioWithClient(ctx context.Context, item youtubeImportItem, destinationPath string) (resultErr error) {
	client := g.newClient()
	video, err := client.GetVideoContext(ctx, item.SourceURL)
	if err != nil {
		return youtubeDownloadError(item, "get_video", nil, err)
	}
	format := selectYouTubeDownloadFormat(video.Formats)
	if format == nil {
		return youtubeDownloadError(item, "select_format", nil, errors.New("no downloadable audio format found"))
	}
	if format.ContentLength > maxSongUploadSize {
		return youtubeDownloadError(item, "format_size", format, errYouTubeAudioTooLarge)
	}
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
		return sanitizeYouTubeFilesystemError("create audio download directory", err)
	}
	reader, contentLength, err := client.GetStreamContext(ctx, video, format)
	if err != nil {
		return youtubeDownloadError(item, "get_stream", format, err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			resultErr = errors.Join(
				resultErr,
				newYouTubeCleanupError("close downloaded audio stream", youtubeDownloadError(item, "close_stream", format, err)),
				removeYouTubeFile(destinationPath, "remove audio after stream close failure"),
			)
		}
	}()
	if contentLength > maxSongUploadSize {
		return youtubeDownloadError(item, "stream_size", format, errYouTubeAudioTooLarge)
	}

	output, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return sanitizeYouTubeFilesystemError("create downloaded audio file", err)
	}
	if _, err := copyYouTubeAudio(output, reader, maxSongUploadSize); err != nil {
		copyErr := err
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			copyErr = sanitizeYouTubeFilesystemError("write downloaded audio file", err)
		}
		return errors.Join(
			youtubeDownloadError(item, "copy_stream", format, copyErr),
			newYouTubeCleanupError("close incomplete downloaded audio file", sanitizeYouTubeFilesystemError("close incomplete downloaded audio file", output.Close())),
			removeYouTubeFile(destinationPath, "remove incomplete downloaded audio file"),
		)
	}
	if err := output.Close(); err != nil {
		return errors.Join(
			youtubeDownloadError(item, "close_output", format, sanitizeYouTubeFilesystemError("close downloaded audio file", err)),
			removeYouTubeFile(destinationPath, "remove incomplete downloaded audio file"),
		)
	}
	return nil
}

func shouldTryYouTubeDownloadFallback(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	return !errors.Is(err, errYouTubeAudioTooLarge) && !errors.Is(err, errYouTubeStorageFailed)
}

func (g *liveYouTubeImportGateway) resolveCookiesPath(cookiesPath string) (string, bool, error) {
	cookiesPath = strings.TrimSpace(cookiesPath)
	if cookiesPath != "" {
		info, err := os.Stat(cookiesPath)
		if err != nil {
			return "", false, sanitizeYouTubeFilesystemError("inspect session cookies file", err)
		}
		if info.IsDir() || info.Size() == 0 {
			return "", false, youtubeStageError(errYouTubeStorageFailed, "inspect session cookies file", errors.New("cookies file is not a non-empty regular file"))
		}
		return cookiesPath, true, nil
	}
	if g.cookieStore == nil {
		return "", false, nil
	}
	return g.cookieStore.pathIfPresent()
}

func (g *liveYouTubeImportGateway) downloadAudioWithYTDLP(ctx context.Context, item youtubeImportItem, destinationPath, cookiesPath string) error {
	binaryName := strings.TrimSpace(g.cfg.YTDLPBinary)
	if binaryName == "" {
		binaryName = "yt-dlp"
	}
	if _, err := exec.LookPath(binaryName); err != nil {
		cause, _ := sanitizeYouTubeCause(err)
		return youtubeDownloadError(item, "ytdlp_lookup", nil, youtubeStageError(errYouTubeDependency, "yt-dlp lookup", cause))
	}
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
		return sanitizeYouTubeFilesystemError("create audio download directory", err)
	}
	args := []string{
		"--no-playlist",
		"--no-part",
		"--no-progress",
		"--max-filesize", strconv.FormatInt(maxSongUploadSize, 10),
		"-f", "bestaudio/best",
		"-o", "-",
		item.SourceURL,
	}
	if strings.TrimSpace(cookiesPath) != "" {
		args = append([]string{"--cookies", cookiesPath}, args...)
	}
	args = g.ytdlpArgs(args...)
	if err := runYTDLPDownload(ctx, binaryName, args, destinationPath, maxSongUploadSize); err != nil {
		log.Printf("yt-dlp failed stage=download diagnostic=%s", youtubeErrorDiagnostic(err))
		cause := err
		if ctxErr := ctx.Err(); ctxErr != nil {
			cause = errors.Join(cause, ctxErr)
		}
		return youtubeDownloadError(item, "ytdlp_run", nil, cause)
	}
	return nil
}

func runYTDLPDownload(ctx context.Context, binaryName string, args []string, destinationPath string, limit int64) (resultErr error) {
	output, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return sanitizeYouTubeFilesystemError("create downloaded audio file", err)
	}
	removeOutput := true
	closed := false
	defer func() {
		if !closed {
			if err := output.Close(); err != nil {
				resultErr = errors.Join(resultErr, newYouTubeCleanupError("close incomplete downloaded audio file", sanitizeYouTubeFilesystemError("close downloaded audio file", err)))
			}
		}
		if removeOutput {
			resultErr = errors.Join(resultErr, removeYouTubeFile(destinationPath, "remove incomplete downloaded audio file"))
		}
	}()

	bounded := &boundedYouTubeAudioWriter{destination: output, remaining: limit}
	stderr := newBoundedYouTubeDiagnostic(maxYouTubeCommandOutputBytes)
	cmd := exec.CommandContext(ctx, binaryName, args...)
	cmd.Stdout = bounded
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if bounded.overflow {
			return errYouTubeAudioTooLarge
		}
		return classifyYouTubeCommandError(err, stderr.data, stderr.overflow)
	}
	if bounded.overflow {
		return errYouTubeAudioTooLarge
	}
	info, err := output.Stat()
	if err != nil {
		return sanitizeYouTubeFilesystemError("inspect downloaded audio file", err)
	}
	if info.Size() == 0 {
		return errors.New("yt-dlp produced empty output file")
	}
	if info.Size() > limit {
		return errYouTubeAudioTooLarge
	}
	closeErr := output.Close()
	closed = true
	if closeErr != nil {
		return sanitizeYouTubeFilesystemError("close downloaded audio file", closeErr)
	}
	removeOutput = false
	return nil
}

func youtubeDownloadError(_ youtubeImportItem, stage string, format *youtube.Format, err error) error {
	formatDetails := "none"
	if format != nil {
		formatDetails = fmt.Sprintf("itag=%d bitrate=%d channels=%d", format.ItagNo, format.Bitrate, format.AudioChannels)
	}
	log.Printf(
		"youtube download failed stage=%s format={%s} diagnostic=%s",
		stage,
		formatDetails,
		youtubeErrorDiagnostic(err),
	)
	cause, _ := sanitizeYouTubeCause(err)
	return youtubeStageError(errYouTubeDownloadFailed, stage, cause)
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

func (g *liveYouTubeImportGateway) scanPlaylist(ctx context.Context, client *youtube.Client, playlistURL, originalURL, linkProvider string, cutoff *time.Time) ([]youtubeImportItem, error) {
	playlist, err := client.GetPlaylistContext(ctx, playlistURL)
	if err != nil {
		return nil, err
	}
	items := make([]youtubeImportItem, 0, len(playlist.Videos))
	seen := make(map[string]struct{}, len(playlist.Videos))
	entryFailures := make([]error, 0)
	for _, entry := range playlist.Videos {
		item := youtubeImportItem{}
		video, err := client.VideoFromPlaylistEntryContext(ctx, entry)
		if err == nil {
			item = buildYouTubeImportItem(video, buildYouTubeWatchURL(video.ID, linkProvider), originalURL, linkProvider, playlist.Title)
			item = mergePlaylistEntryFallback(item, entry, originalURL, linkProvider, playlist.Title)
		} else {
			entryFailures = append(entryFailures, err)
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
		if len(entryFailures) > 0 {
			return nil, combineYouTubeScanErrors(append(entryFailures, errYouTubeNoTracks)...)
		}
		return nil, errYouTubeNoTracks
	}
	if len(entryFailures) > 0 {
		addYouTubeBreadcrumb(ctx, "scan", "playlist entries used fallback metadata", len(entryFailures))
	}
	return items, nil
}

func (g *liveYouTubeImportGateway) scanPlaylistWithYTDLP(ctx context.Context, playlistURL, originalURL, linkProvider string, cutoff *time.Time, cookiesPath string) ([]youtubeImportItem, error) {
	data, err := g.dumpPlaylistJSON(ctx, playlistURL, cookiesPath)
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

	defaultAuthor := firstNonEmpty(dump.Channel, dump.Uploader)
	items := make([]youtubeImportItem, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		item := buildYouTubeImportItemFromYTDLPEntry(entry, originalURL, linkProvider, defaultAuthor)
		item.ParsedAlbumTitle = strings.TrimSpace(dump.Title)
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

func (g *liveYouTubeImportGateway) scanArtist(ctx context.Context, client *youtube.Client, artistURL string, cutoff *time.Time, cookiesPath string) ([]youtubeImportItem, error) {
	ytdlpItems, ytdlpErr := g.scanArtistWithYTDLP(ctx, client, artistURL, cutoff, cookiesPath)
	if ytdlpErr == nil && len(ytdlpItems) > 0 {
		return ytdlpItems, nil
	}
	if ytdlpErr != nil {
		log.Printf("youtube artist scan switching to HTML fallback diagnostic=%s", youtubeErrorDiagnostic(ytdlpErr))
	}
	htmlItems, htmlErr := g.scanArtistByHTML(ctx, client, artistURL, cutoff)
	if htmlErr == nil && len(htmlItems) > 0 {
		addYouTubeBreadcrumb(ctx, "scan", "artist HTML fallback succeeded", 1)
		return htmlItems, nil
	}
	return nil, combineYouTubeScanErrors(ytdlpErr, htmlErr)
}

func (g *liveYouTubeImportGateway) scanArtistWithYTDLP(ctx context.Context, client *youtube.Client, artistURL string, cutoff *time.Time, cookiesPath string) ([]youtubeImportItem, error) {
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
	enrichmentFailures := make([]error, 0)
	for _, entry := range entries {
		item := buildYouTubeImportItemFromYTDLPEntry(entry, artistURL, linkProvider, defaultAuthor)
		if cutoff != nil {
			video, videoErr := client.GetVideoContext(ctx, item.VideoID)
			if videoErr == nil {
				item = mergeYTDLPEntryFallback(
					buildYouTubeImportItem(video, buildYouTubeWatchURL(video.ID, linkProvider), artistURL, linkProvider, ""),
					entry,
					artistURL,
					linkProvider,
					defaultAuthor,
				)
			} else {
				enrichmentFailures = append(enrichmentFailures, videoErr)
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
		if len(enrichmentFailures) > 0 {
			return nil, combineYouTubeScanErrors(append(enrichmentFailures, errYouTubeNoTracks)...)
		}
		return nil, errYouTubeNoTracks
	}
	if len(enrichmentFailures) > 0 {
		addYouTubeBreadcrumb(ctx, "scan", "artist entries used flat metadata", len(enrichmentFailures))
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

func (g *liveYouTubeImportGateway) scanArtistByHTML(ctx context.Context, client *youtube.Client, artistURL string, cutoff *time.Time) ([]youtubeImportItem, error) {
	body, err := g.fetchHTML(ctx, artistURL)
	if err != nil {
		return nil, err
	}

	playlistIDs := extractMatches(body, youtubePlaylistIDPattern)
	videoIDs := extractMatches(body, youtubeVideoIDPattern)
	items := make([]youtubeImportItem, 0)
	seen := make(map[string]struct{})
	scanFailures := make([]error, 0)

	for _, playlistID := range playlistIDs {
		if strings.HasPrefix(playlistID, "RD") {
			continue
		}
		playlistItems, err := g.scanPlaylist(ctx, client, "https://www.youtube.com/playlist?list="+playlistID, artistURL, "youtube_music", cutoff)
		if err != nil {
			scanFailures = append(scanFailures, err)
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
			video, err := client.GetVideoContext(ctx, videoID)
			if err != nil {
				scanFailures = append(scanFailures, err)
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
		if len(scanFailures) > 0 {
			return nil, combineYouTubeScanErrors(append(scanFailures, errYouTubeNoTracks)...)
		}
		return nil, errYouTubeNoTracks
	}
	if len(scanFailures) > 0 {
		addYouTubeBreadcrumb(ctx, "scan", "artist HTML scan partially recovered", len(scanFailures))
	}
	return items, nil
}

func (g *liveYouTubeImportGateway) dumpFlatPlaylistJSON(ctx context.Context, rawURL string, cookiesPath string) ([]byte, error) {
	return g.dumpYTDLPJSON(ctx, rawURL, []string{"--no-warnings", "--flat-playlist", "--skip-download", "--dump-single-json"}, "artist scan", cookiesPath)
}

func (g *liveYouTubeImportGateway) dumpPlaylistJSON(ctx context.Context, rawURL string, cookiesPath string) ([]byte, error) {
	return g.dumpYTDLPJSON(ctx, rawURL, []string{"--no-warnings", "--yes-playlist", "--skip-download", "--dump-single-json"}, "playlist scan", cookiesPath)
}

func (g *liveYouTubeImportGateway) dumpTrackJSON(ctx context.Context, rawURL string, cookiesPath string) ([]byte, error) {
	return g.dumpYTDLPJSON(ctx, rawURL, []string{"--no-warnings", "--no-playlist", "--skip-download", "--dump-single-json"}, "track scan", cookiesPath)
}

func (g *liveYouTubeImportGateway) dumpYTDLPJSON(ctx context.Context, rawURL string, args []string, label string, cookiesPath string) ([]byte, error) {
	binaryName := strings.TrimSpace(g.cfg.YTDLPBinary)
	if binaryName == "" {
		binaryName = "yt-dlp"
	}
	if _, err := exec.LookPath(binaryName); err != nil {
		cause, _ := sanitizeYouTubeCause(err)
		return nil, youtubeStageError(errYouTubeDependency, "yt-dlp lookup", cause)
	}
	args = append([]string{}, args...)
	resolvedCookiesPath, hasCookies, err := g.resolveCookiesPath(cookiesPath)
	if err != nil {
		return nil, err
	}
	if hasCookies {
		args = append(args, "--cookies", resolvedCookiesPath)
	}
	args = append(args, rawURL)
	args = g.ytdlpArgs(args...)
	output, err := runYouTubeCommandWithOutputLimit(ctx, binaryName, args, maxYouTubeYTDLPJSONBytes)
	if err != nil {
		if errors.Is(err, errYouTubeResponseTooLarge) {
			return nil, youtubeStageError(errYouTubeUpstreamFailed, "yt-dlp json response", err)
		}
		log.Printf("yt-dlp failed operation=%q diagnostic=%s", label, youtubeErrorDiagnostic(err))
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = errors.Join(err, ctxErr)
		}
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

func (g *liveYouTubeImportGateway) fetchHTML(ctx context.Context, rawURL string) (result string, resultErr error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", errYouTubeInvalidURL
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close youtube response body: %w", err))
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", youtube.ErrUnexpectedStatusCode(resp.StatusCode)
	}
	return readYouTubeHTML(resp.Body, maxYouTubeArtistHTMLBytes)
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
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.findTrackBySourceMetadataLocked(target)
}

func (s *trackStore) findTrackBySourceMetadataLocked(target sourceMetadata) (track, bool) {
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

func (s *trackStore) createTrackIfSourceAbsent(req upsertTrackRequest, target sourceMetadata, publishAudio func() error) (track, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.findTrackBySourceMetadataLocked(target); exists {
		return track{}, errYouTubeCurrentConflict
	}
	if _, ok := s.albums[req.AlbumID]; !ok {
		return track{}, fmt.Errorf("%w: albumId %d does not exist", errInvalidTrack, req.AlbumID)
	}

	albumsSnapshot := cloneAlbumsMap(s.albums)
	tracksSnapshot := cloneTracksMap(s.tracks)
	nextTrackIDSnapshot := s.nextTrackID
	restore := func() {
		s.albums = albumsSnapshot
		s.tracks = tracksSnapshot
		s.nextTrackID = nextTrackIDSnapshot
	}

	created := track{
		ID:             s.nextTrackID,
		Name:           strings.TrimSpace(req.Name),
		AuthorIDs:      normalizeAuthorIDs(req.AuthorIDs),
		AlbumID:        req.AlbumID,
		AudioFilePath:  normalizeAudioFilePath(req.AudioFilePath),
		AdditionalInfo: normalizeAdditionalInfo(req.AdditionalInfo),
		SourceMetadata: normalizeSourceMetadata(req.SourceMetadata),
		CreatedAt:      time.Now().UTC(),
	}
	if err := s.validateTrackLocked(created); err != nil {
		return track{}, err
	}
	albumOrder, err := normalizeAlbumOrder(req.AlbumOrder, len(s.albums[req.AlbumID].TrackIDs), true)
	if err != nil {
		return track{}, fmt.Errorf("%w: %v", errInvalidTrack, err)
	}

	s.nextTrackID++
	s.tracks[created.ID] = created
	targetAlbum := s.albums[req.AlbumID]
	insertTrackIntoAlbumLocked(&targetAlbum, created.ID, albumOrder)
	s.albums[req.AlbumID] = targetAlbum
	if err := s.rebuildAlbumDerivedDataLocked(); err != nil {
		restore()
		return track{}, err
	}
	if err := s.persistLocked(); err != nil {
		restore()
		return track{}, err
	}
	if publishAudio != nil {
		if err := publishAudio(); err != nil {
			restore()
			rollbackErr := s.persistLocked()
			if rollbackErr != nil {
				rollbackErr = fmt.Errorf("rollback track after audio publish failure: %w", rollbackErr)
			}
			return track{}, errors.Join(err, rollbackErr)
		}
	}
	return cloneTrack(s.tracks[created.ID]), nil
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

func (s *trackStore) attachTrackImportMetadataIfSourceAbsent(trackID int64, infos []additionalInfo, metadata []sourceMetadata, target sourceMetadata) (track, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.tracks[trackID]
	if !ok {
		return track{}, false, nil
	}
	if _, exists := s.findTrackBySourceMetadataLocked(target); exists {
		return track{}, true, errYouTubeCurrentConflict
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
	return cloneTrack(s.tracks[trackID]), true, nil
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
		status, err := store.status()
		if err != nil {
			writeSentryHTTPError(w, r, sanitizeYouTubeCapturedError(err), "internal server error", http.StatusInternalServerError, "youtube", "cookies.status")
			return
		}
		writeJSON(w, http.StatusOK, status)
	})
}

func youtubeCookiesUploadHandler(store *youtubeCookieStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxYouTubeCookiesUploadBytes)
		if err := r.ParseMultipartForm(maxYouTubeCookiesBytes); err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				http.Error(w, errYouTubeCookiesTooLarge.Error(), http.StatusRequestEntityTooLarge)
				return
			}
			var pathErr *os.PathError
			if errors.As(err, &pathErr) {
				writeSentryHTTPError(w, r, sanitizeYouTubeFilesystemError("parse cookies upload", err), "internal server error", http.StatusInternalServerError, "youtube", "cookies.upload")
				return
			}
			http.Error(w, "invalid multipart form", http.StatusBadRequest)
			return
		}
		defer func() {
			if r.MultipartForm == nil {
				return
			}
			if err := r.MultipartForm.RemoveAll(); err != nil {
				captureSentryError(r.Context(), sanitizeYouTubeFilesystemError("remove multipart temporary files", err), "youtube", "cookies.upload.cleanup")
			}
		}()
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "file is required", http.StatusBadRequest)
			return
		}
		defer func() {
			if err := file.Close(); err != nil {
				captureSentryError(r.Context(), sanitizeYouTubeFilesystemError("close uploaded cookies file", err), "youtube", "cookies.upload.cleanup")
			}
		}()
		if err := store.Replace(file); err != nil {
			if errors.Is(err, errYouTubeCookiesTooLarge) {
				if hasYouTubeCleanupError(err) {
					captureSentryError(r.Context(), sanitizeYouTubeCapturedError(err), "youtube", "cookies.upload.cleanup")
				}
				http.Error(w, errYouTubeCookiesTooLarge.Error(), http.StatusRequestEntityTooLarge)
				return
			}
			if (errors.Is(err, errYouTubeCookiesEmpty) || errors.Is(err, errYouTubeCookiesNotSet)) && !errors.Is(err, errYouTubeStorageFailed) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeSentryHTTPError(w, r, sanitizeYouTubeCapturedError(err), "internal server error", http.StatusInternalServerError, "youtube", "cookies.upload")
			return
		}
		status, err := store.status()
		if err != nil {
			writeSentryHTTPError(w, r, sanitizeYouTubeCapturedError(err), "internal server error", http.StatusInternalServerError, "youtube", "cookies.upload")
			return
		}
		writeJSON(w, http.StatusCreated, status)
	})
}

func youtubeCookiesDeleteHandler(store *youtubeCookieStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := store.Delete(); err != nil {
			writeSentryHTTPError(w, r, sanitizeYouTubeCapturedError(err), "internal server error", http.StatusInternalServerError, "youtube", "cookies.delete")
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
			writeRequestDecodeError(w, err)
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
			writeYouTubeImportError(w, r, err, "import.start")
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
			writeYouTubeImportError(w, r, err, "import.current")
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
		session, err := service.skipCurrent(r.Context(), userID)
		if err != nil {
			writeYouTubeImportError(w, r, err, "import.skip")
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
			writeRequestDecodeError(w, err)
			return
		}
		session, trackItem, err := service.AddCurrent(r.Context(), userID, req)
		if err != nil {
			writeYouTubeImportError(w, r, err, "import.add")
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
		if err := service.cancelSession(r.Context(), userID); err != nil {
			writeYouTubeImportError(w, r, err, "import.cancel")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func writeYouTubeImportError(w http.ResponseWriter, r *http.Request, err error, operation string) {
	captureErr := sanitizeYouTubeCapturedError(err)
	if hasYouTubeCleanupError(err) {
		captureSentryError(r.Context(), captureErr, "youtube", operation)
	}
	switch {
	case errors.Is(err, context.Canceled):
		http.Error(w, "request canceled", http.StatusRequestTimeout)
	case errors.Is(err, errYouTubeShuttingDown):
		markSentryErrorHandled(r.Context())
		http.Error(w, errYouTubeShuttingDown.Error(), http.StatusServiceUnavailable)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, errYouTubeUpstreamTimeout):
		writeSentryHTTPError(w, r, captureErr, "youtube request timed out", http.StatusGatewayTimeout, "youtube", operation)
	case errors.Is(err, errYouTubeRateLimited):
		w.Header().Set("Retry-After", "30")
		writeSentryHTTPError(w, r, captureErr, errYouTubeRateLimited.Error(), http.StatusServiceUnavailable, "youtube", operation)
	case errors.Is(err, errYouTubeAuthentication):
		http.Error(w, errYouTubeAuthentication.Error(), http.StatusBadRequest)
	case errors.Is(err, errYouTubeChallenge):
		writeSentryHTTPError(w, r, captureErr, errYouTubeChallenge.Error(), http.StatusBadGateway, "youtube", operation)
	case errors.Is(err, errYouTubeUnavailable):
		http.Error(w, errYouTubeUnavailable.Error(), http.StatusBadRequest)
	case errors.Is(err, errYouTubeDependency):
		writeSentryHTTPError(w, r, captureErr, "internal server error", http.StatusInternalServerError, "youtube", operation)
	case errors.Is(err, errYouTubeStorageFailed):
		writeSentryHTTPError(w, r, captureErr, "internal server error", http.StatusInternalServerError, "youtube", operation)
	case errors.Is(err, errYouTubeUpstreamFailed):
		writeSentryHTTPError(w, r, captureErr, "youtube is temporarily unavailable", http.StatusBadGateway, "youtube", operation)
	case errors.Is(err, errYouTubeAudioTooLarge), errors.Is(err, errYouTubeCookiesTooLarge):
		http.Error(w, "youtube import exceeds the maximum allowed size", http.StatusRequestEntityTooLarge)
	case errors.Is(err, errYouTubeDownloadFailed):
		writeSentryHTTPError(w, r, captureErr, errYouTubeDownloadFailed.Error(), http.StatusBadGateway, "youtube", operation)
	case errors.Is(err, errYouTubeTranscodeFailed):
		writeSentryHTTPError(w, r, captureErr, errYouTubeTranscodeFailed.Error(), http.StatusInternalServerError, "youtube", operation)
	case errors.Is(err, errYouTubeInvalidURL):
		http.Error(w, errYouTubeInvalidURL.Error(), http.StatusBadRequest)
	case errors.Is(err, errYouTubeNoTracks):
		http.Error(w, errYouTubeNoTracks.Error(), http.StatusBadRequest)
	case errors.Is(err, errYouTubeUnsupportedMode):
		http.Error(w, errYouTubeUnsupportedMode.Error(), http.StatusBadRequest)
	case errors.Is(err, errYouTubeSessionActive), errors.Is(err, errYouTubeCurrentConflict), errors.Is(err, errYouTubeSessionChanged):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, errYouTubeSessionNotFound), errors.Is(err, errTrackNotFound), errors.Is(err, errYouTubeCurrentNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errInvalidTrack):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		writeSentryHTTPError(w, r, captureErr, "internal server error", http.StatusInternalServerError, "youtube", operation)
	}
}
