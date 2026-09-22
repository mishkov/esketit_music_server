package main

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed openapi.yaml
var openAPISpec string

const (
	defaultAccessTokenTTL           = 15 * time.Minute
	defaultRefreshTokenTTL          = 30 * 24 * time.Hour
	passwordHashIterations          = 310000
	passwordHashKeyLength           = 32
	minPasswordLength               = 8
	maxSongUploadSize               = 512 << 20
	maxPlaylistNameLength           = 200
	maxPlaylistDescLength           = 1000
	playlistShareTokenBytes         = 32
	defaultAutoplayCount            = 1
	maxAutoplayCount                = 50
	maxAnalyticsBatchSize           = 100
	maxAnalyticsIDLength            = 200
	maxAnalyticsEventTypeLen        = 64
	maxAnalyticsPlatformLen         = 64
	maxAnalyticsAppVersionLen       = 64
	maxAnalyticsSearchQueryLen      = 1000
	maxJSONRequestBodySize          = 1 << 20
	multipartUploadMemoryThreshold  = 8 << 20
	roleAdmin                       = "admin"
	roleListener                    = "listener"
	logModeVerbose                  = "verbose"
	logModeErrorOnly                = "error-only"
	playlistVisibilityPrivate       = "private"
	playlistVisibilityPublic        = "public"
	playlistVisibilityShared        = "shared"
	playlistKindCustom              = "custom"
	playlistKindFavorites           = "favorites"
	playlistKindDislikes            = "dislikes"
	trackListSortID                 = "id"
	trackListSortCreatedAt          = "createdAt"
	sortOrderAsc                    = "asc"
	sortOrderDesc                   = "desc"
	autoplaySourceMyVibe            = "my_vibe"
	autoplaySourcePlaylist          = "playlist"
	autoplaySourceAlbum             = "album"
	autoplaySourceTrack             = "track"
	autoplaySourceAuthor            = "author"
	defaultAutoplayProfile          = "default"
	analyticsEventPlay              = "play"
	analyticsEventPause             = "pause"
	analyticsEventResume            = "resume"
	analyticsEventSeek              = "seek"
	analyticsEventTrackChange       = "track_change"
	analyticsEventTrackComplete     = "track_complete"
	analyticsEventTrackSkip         = "track_skip"
	analyticsEventSearch            = "search"
	analyticsEventSearchResultClick = "search_result_click"
	analyticsEventPlaybackError     = "playback_error"
	analyticsEventTrackDislike      = "track_dislike"
	analyticsEventTrackUndislike    = "track_undislike"
	gracefulShutdownTimeout         = 30 * time.Second
	httpReadHeaderTimeout           = 10 * time.Second
	httpReadTimeout                 = 20 * time.Minute
	httpIdleTimeout                 = 2 * time.Minute
	maxHTTPListenerExitWait         = 5 * time.Second
)

// songsMutationMu keeps filesystem visibility and track-store commits ordered for
// song mutations performed by HTTP and integration import handlers.
var songsMutationMu sync.RWMutex

type contextKey string

const userContextKey contextKey = "authenticated-user-id"

type songInfo struct {
	Name         string    `json:"name"`
	SizeBytes    int64     `json:"sizeBytes"`
	LastModified time.Time `json:"lastModified"`
	Path         string    `json:"path"`
	URL          string    `json:"url"`
}

type albumCoverInfo struct {
	Name         string    `json:"name"`
	SizeBytes    int64     `json:"sizeBytes"`
	LastModified time.Time `json:"lastModified"`
	Path         string    `json:"path"`
	URL          string    `json:"url"`
}

type additionalInfo map[string]any
type sourceMetadata map[string]any

type album struct {
	ID             int64            `json:"id"`
	Title          string           `json:"title"`
	CoverImagePath string           `json:"coverImagePath"`
	AuthorIDs      []int64          `json:"authorIds"`
	ReleaseDate    time.Time        `json:"releaseDate"`
	IsPublished    bool             `json:"isPublished"`
	TrackIDs       []int64          `json:"trackIds"`
	AdditionalInfo []additionalInfo `json:"additionalInfo"`
}

type upsertAlbumRequest struct {
	Title          string           `json:"title"`
	CoverImagePath string           `json:"coverImagePath"`
	AuthorIDs      []int64          `json:"authorIds"`
	ReleaseDate    time.Time        `json:"releaseDate"`
	IsPublished    bool             `json:"isPublished"`
	TrackIDs       []int64          `json:"trackIds"`
	AdditionalInfo []additionalInfo `json:"additionalInfo"`
}

type track struct {
	ID             int64            `json:"id"`
	Name           string           `json:"name"`
	AuthorIDs      []int64          `json:"authorIds"`
	AlbumID        int64            `json:"albumId"`
	AudioFilePath  string           `json:"audioFilePath"`
	AdditionalInfo []additionalInfo `json:"additionalInfo"`
	SourceMetadata []sourceMetadata `json:"sourceMetadata"`
	CreatedAt      time.Time        `json:"createdAt"`
}

type lyrics struct {
	ID           int64             `json:"id"`
	TrackID      int64             `json:"trackId"`
	Type         string            `json:"type"`
	PlainText    *string           `json:"plainText"`
	LanguageCode *string           `json:"languageCode"`
	Source       *string           `json:"source"`
	IsVerified   bool              `json:"isVerified"`
	UpdatedAt    time.Time         `json:"updatedAt"`
	CreatedAt    time.Time         `json:"createdAt"`
	Lines        []syncedLyricLine `json:"lines"`
}

type syncedLyricLine struct {
	ID         int64  `json:"id"`
	LyricsID   int64  `json:"lyricsId"`
	StartMs    int    `json:"startMs"`
	EndMs      *int   `json:"endMs"`
	Text       string `json:"text"`
	OrderIndex int    `json:"orderIndex"`
}

type playlistTrack struct {
	TrackID          int64  `json:"trackId"`
	UnavailableTrack *track `json:"unavailableTrack,omitempty"`
}

type playlist struct {
	ID             int64           `json:"id"`
	UserID         int64           `json:"userId"`
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	CoverImagePath string          `json:"coverImagePath"`
	Visibility     string          `json:"visibility"`
	ShareToken     string          `json:"shareToken,omitempty"`
	TrackItems     []playlistTrack `json:"trackItems"`
	System         bool            `json:"system"`
	Kind           string          `json:"kind"`
}

type upsertTrackRequest struct {
	Name           string           `json:"name"`
	AuthorIDs      []int64          `json:"authorIds"`
	AlbumID        int64            `json:"albumId"`
	AlbumOrder     int              `json:"albumOrder"`
	AudioFilePath  string           `json:"audioFilePath"`
	AdditionalInfo []additionalInfo `json:"additionalInfo"`
	SourceMetadata []sourceMetadata `json:"sourceMetadata"`
}

type upsertPlaylistRequest struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	CoverImagePath string `json:"coverImagePath"`
	Visibility     string `json:"visibility"`
}

type addTrackToPlaylistsRequest struct {
	PlaylistIDs []int64 `json:"playlistIds"`
}

type reorderPlaylistTracksRequest struct {
	TrackIDs []int64 `json:"trackIds"`
}

type autoplayNextRequest struct {
	SourceType       string  `json:"sourceType"`
	SourceID         *int64  `json:"sourceId,omitempty"`
	Profile          string  `json:"profile,omitempty"`
	Count            int     `json:"count"`
	RecentTrackIDs   []int64 `json:"recentTrackIds"`
	ExcludedTrackIDs []int64 `json:"excludedTrackIds"`
}

type autoplayNextResponse struct {
	SourceType string          `json:"sourceType"`
	SourceID   *int64          `json:"sourceId,omitempty"`
	Profile    string          `json:"profile"`
	Strategy   string          `json:"strategy"`
	Tracks     []trackResponse `json:"tracks"`
}

type analyticsEventsRequest struct {
	ClientID   string                  `json:"clientId"`
	SessionID  string                  `json:"sessionId"`
	Platform   string                  `json:"platform"`
	AppVersion string                  `json:"appVersion"`
	Events     []analyticsEventRequest `json:"events"`
}

type analyticsEventRequest struct {
	EventID     string         `json:"eventId"`
	Type        string         `json:"type"`
	TrackID     *int64         `json:"trackId,omitempty"`
	PlaylistID  *int64         `json:"playlistId,omitempty"`
	AlbumID     *int64         `json:"albumId,omitempty"`
	PositionMs  *int           `json:"positionMs,omitempty"`
	DurationMs  *int           `json:"durationMs,omitempty"`
	SearchQuery *string        `json:"searchQuery,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	ClientTime  time.Time      `json:"clientTime"`
}

type analyticsEventRecord struct {
	EventID     string
	UserID      *int64
	ClientID    string
	SessionID   string
	EventType   string
	TrackID     *int64
	PlaylistID  *int64
	AlbumID     *int64
	PositionMs  *int
	DurationMs  *int
	SearchQuery *string
	Metadata    map[string]any
	ClientTime  time.Time
	ReceivedAt  time.Time
	Platform    string
	AppVersion  string
}

type analyticsEventsResponse struct {
	Accepted   int `json:"accepted"`
	Duplicates int `json:"duplicates"`
}

type author struct {
	ID          int64    `json:"id"`
	CurrentName string   `json:"currentName"`
	Photos      []string `json:"photos"`
}

type upsertAuthorRequest struct {
	CurrentName string   `json:"currentName"`
	Photos      []string `json:"photos"`
}

type user struct {
	ID           int64     `json:"id"`
	Email        string    `json:"email"`
	Role         string    `json:"role"`
	PasswordHash string    `json:"passwordHash"`
	CreatedAt    time.Time `json:"createdAt"`
}

type publicUser struct {
	ID        int64     `json:"id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"createdAt"`
}

type refreshSession struct {
	ID        string    `json:"id"`
	UserID    int64     `json:"userId"`
	TokenHash string    `json:"tokenHash"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type refreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

type authResponse struct {
	User                  publicUser `json:"user"`
	AccessToken           string     `json:"accessToken"`
	AccessTokenExpiresAt  time.Time  `json:"accessTokenExpiresAt"`
	RefreshToken          string     `json:"refreshToken"`
	RefreshTokenExpiresAt time.Time  `json:"refreshTokenExpiresAt"`
}

type trackResponse struct {
	ID             int64            `json:"id"`
	Name           string           `json:"name"`
	AuthorIDs      []int64          `json:"authorIds"`
	Authors        []author         `json:"authors,omitempty"`
	AlbumID        int64            `json:"albumId"`
	CoverImagePath string           `json:"coverImagePath,omitempty"`
	AudioFilePath  string           `json:"audioFilePath"`
	AdditionalInfo []additionalInfo `json:"additionalInfo"`
	SourceMetadata []sourceMetadata `json:"sourceMetadata"`
	CreatedAt      time.Time        `json:"createdAt"`
	IsFavorite     bool             `json:"isFavorite"`
	IsDisliked     bool             `json:"isDisliked"`
	IsAvailable    bool             `json:"isAvailable"`
}

type playlistResponse struct {
	ID             int64  `json:"id"`
	UserID         int64  `json:"userId"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	CoverImagePath string `json:"coverImagePath"`
	Visibility     string `json:"visibility"`
	TrackCount     int    `json:"trackCount"`
	System         bool   `json:"system"`
	Kind           string `json:"kind"`
	IsFavorites    bool   `json:"isFavorites"`
	ShareToken     string `json:"shareToken,omitempty"`
}

type trackStore struct {
	songsDir           string
	db                 *sql.DB
	unitOfWork         unitOfWork
	userRepository     UserRepository
	sessionRepository  RefreshSessionRepository
	authorRepository   AuthorRepository
	catalogRepository  CatalogRepository
	playlistRepository PlaylistRepository
	lyricsRepository   LyricsRepository
}

type paginatedAlbums struct {
	Items      []album `json:"items"`
	Page       int     `json:"page"`
	PageSize   int     `json:"pageSize"`
	TotalItems int     `json:"totalItems"`
	TotalPages int     `json:"totalPages"`
}

type albumListFilter struct {
	Page         int
	PageSize     int
	AuthorID     int64
	Query        string
	IsPublished  *bool
	IncludeEmpty bool
}

type playlistListFilter struct {
	Page       int
	PageSize   int
	Visibility string
	Query      string
}

type trackListFilter struct {
	Page     int
	PageSize int
	AuthorID int64
	AlbumID  int64
	Query    string
	Sort     string
	Order    string
}

type paginatedTracks struct {
	Items      []trackResponse `json:"items"`
	Page       int             `json:"page"`
	PageSize   int             `json:"pageSize"`
	TotalItems int             `json:"totalItems"`
	TotalPages int             `json:"totalPages"`
}

type paginatedPlaylists struct {
	Items      []playlistResponse `json:"items"`
	Page       int                `json:"page"`
	PageSize   int                `json:"pageSize"`
	TotalItems int                `json:"totalItems"`
	TotalPages int                `json:"totalPages"`
}

type searchListFilter struct {
	Page         int
	PageSize     int
	Query        string
	IncludeEmpty bool
}

type searchResultItem struct {
	Type     string            `json:"type"`
	Author   *author           `json:"author,omitempty"`
	Album    *album            `json:"album,omitempty"`
	Track    *trackResponse    `json:"track,omitempty"`
	Playlist *playlistResponse `json:"playlist,omitempty"`
}

type paginatedSearchResults struct {
	Items      []searchResultItem `json:"items"`
	Page       int                `json:"page"`
	PageSize   int                `json:"pageSize"`
	TotalItems int                `json:"totalItems"`
	TotalPages int                `json:"totalPages"`
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	bytes       int
	request     *http.Request
}

func (w *loggingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *loggingResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggingResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	if err != nil && w.request != nil {
		w.reportResponseError(fmt.Errorf("write HTTP response: %w", err), "write_response")
	}
	return n, err
}

func (w *loggingResponseWriter) reportResponseError(err error, operation string) {
	if err == nil || w.request == nil {
		return
	}
	if w.request.Context().Err() != nil {
		markSentryErrorHandled(w.request.Context())
		return
	}
	captureSentryError(w.request.Context(), err, "http", operation)
}

type authManager struct {
	secret          []byte
	accessTokenTTL  time.Duration
	refreshTokenTTL time.Duration
}

type accessTokenClaims struct {
	Sub int64 `json:"sub"`
	Exp int64 `json:"exp"`
	Iat int64 `json:"iat"`
}

func main() {
	if err := run(); err != nil {
		log.Printf("server failed: %s", safeOperationalError(err))
		os.Exit(1)
	}
}

func run() (runErr error) {
	if err := loadDotEnv(".env"); err != nil {
		return fmt.Errorf("failed to load .env: %w", err)
	}

	sentryEnabled, err := initializeSentry()
	if err != nil {
		return err
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if sentryEnabled {
				reportPanicAndFlushSentry(recovered)
			}
			runErr = errors.New("server terminated after panic")
			return
		}
		if sentryEnabled {
			reportAndFlushSentry(runErr)
		}
	}()

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot resolve home directory: %w", err)
	}

	defaultSongsDir := filepath.Join(home, "Projects", "esketit_music", "media_storage", "songs")
	songsDir := os.Getenv("SONGS_DIR")
	if songsDir == "" {
		songsDir = defaultSongsDir
	}

	if err := ensureDir(songsDir); err != nil {
		return fmt.Errorf("invalid songs directory: %w", err)
	}
	if err := cleanupStaleYouTubeSongStaging(songsDir); err != nil {
		return fmt.Errorf("clean stale youtube song staging files: %w", err)
	}

	defaultAlbumCoversDir := filepath.Join(home, "Projects", "esketit_music", "media_storage", "album_covers")
	albumCoversDir := os.Getenv("ALBUM_COVERS_DIR")
	if albumCoversDir == "" {
		albumCoversDir = defaultAlbumCoversDir
	}

	if err := ensureDirOrCreate(albumCoversDir); err != nil {
		return fmt.Errorf("invalid album covers directory: %w", err)
	}

	defaultAuthorPhotosDir := filepath.Join(home, "Projects", "esketit_music", "media_storage", "author_photos")
	authorPhotosDir := os.Getenv("AUTHOR_PHOTOS_DIR")
	if authorPhotosDir == "" {
		authorPhotosDir = defaultAuthorPhotosDir
	}

	if err := ensureDirOrCreate(authorPhotosDir); err != nil {
		return fmt.Errorf("invalid author photos directory: %w", err)
	}

	telegramStateDir := os.Getenv("TELEGRAM_STATE_DIR")
	if telegramStateDir == "" {
		telegramStateDir = "telegram_state"
	}
	if err := ensurePrivateDirOrCreate(telegramStateDir); err != nil {
		return fmt.Errorf("invalid telegram state directory: %w", err)
	}

	telegramImportTempDir := os.Getenv("TELEGRAM_IMPORT_TEMP_DIR")
	if telegramImportTempDir == "" {
		telegramImportTempDir = "telegram_import_tmp"
	}
	if err := ensurePrivateDirOrCreate(telegramImportTempDir); err != nil {
		return fmt.Errorf("invalid telegram import temp directory: %w", err)
	}
	if err := cleanupStaleTelegramImportEntries(telegramImportTempDir); err != nil {
		return fmt.Errorf("clean stale telegram import files: %w", err)
	}

	youtubeImportTempDir := os.Getenv("YOUTUBE_IMPORT_TEMP_DIR")
	if youtubeImportTempDir == "" {
		youtubeImportTempDir = "youtube_import_tmp"
	}
	if err := ensurePrivateDirOrCreate(youtubeImportTempDir); err != nil {
		return fmt.Errorf("invalid youtube import temp directory: %w", err)
	}
	if err := cleanupStaleYouTubeImportEntries(youtubeImportTempDir); err != nil {
		return fmt.Errorf("clean stale youtube import files: %w", err)
	}

	youtubeCookiesFile := os.Getenv("YOUTUBE_COOKIES_FILE")
	if youtubeCookiesFile == "" {
		youtubeCookiesFile = "youtube_cookies.txt"
	}
	youtubeCookiesDir := filepath.Dir(youtubeCookiesFile)
	if youtubeCookiesDir != "." {
		if err := ensurePrivateDirOrCreate(youtubeCookiesDir); err != nil {
			return fmt.Errorf("invalid youtube cookies directory: %w", err)
		}
	}
	ytdlpBinary := strings.TrimSpace(os.Getenv("YTDLP_BINARY"))
	if ytdlpBinary == "" {
		ytdlpBinary = "yt-dlp"
	}
	ytdlpJSRuntime := strings.TrimSpace(os.Getenv("YTDLP_JS_RUNTIME"))
	if ytdlpJSRuntime == "" {
		ytdlpJSRuntime = "deno"
	}
	ytdlpRemoteComponents := strings.TrimSpace(os.Getenv("YTDLP_REMOTE_COMPONENTS"))
	if ytdlpRemoteComponents == "" {
		ytdlpRemoteComponents = "ejs:github"
	}
	ffmpegBinary := strings.TrimSpace(os.Getenv("FFMPEG_BINARY"))
	if ffmpegBinary == "" {
		ffmpegBinary = "ffmpeg"
	}

	tracksDBPath := os.Getenv("TRACKS_DB_PATH")
	if tracksDBPath == "" {
		tracksDBPath = "tracks.sqlite"
	}

	authSecret := os.Getenv("AUTH_SECRET")
	if len(authSecret) < 32 {
		return errors.New("AUTH_SECRET must be set and contain at least 32 characters")
	}
	youtubeCookieStore := newYouTubeCookieStore(youtubeCookiesFile)
	youtubeImportConfig := youtubeImportConfig{
		ImportTempDir:         youtubeImportTempDir,
		RequestTimeout:        2 * time.Minute,
		DownloadTimeout:       15 * time.Minute,
		YTDLPBinary:           ytdlpBinary,
		YTDLPJSRuntime:        ytdlpJSRuntime,
		YTDLPRemoteComponents: ytdlpRemoteComponents,
		FFmpegBinary:          ffmpegBinary,
	}
	youtubeImportGateway := newLiveYouTubeImportGateway(youtubeImportConfig, youtubeCookieStore)
	if err := youtubeImportGateway.ValidateDependencies(context.Background()); err != nil {
		return fmt.Errorf("validate youtube import dependencies: %w", err)
	}

	store, err := newTrackStore(tracksDBPath)
	if err != nil {
		return fmt.Errorf("failed to initialize tracks database: %w", err)
	}
	defer func() {
		if err := store.db.Close(); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("close tracks database: %w", err))
		}
	}()
	store.songsDir = songsDir
	authorPopularityLocation, err := loadAuthorPopularityLocationFromEnv()
	if err != nil {
		return err
	}
	popularAuthors, err := store.refreshAuthorPopularity(context.Background(), time.Now())
	if err != nil {
		return fmt.Errorf("initialize author popularity: %w", err)
	}
	log.Printf(
		"author popularity initialized authors=%d window_days=%d timezone=%s",
		len(popularAuthors),
		int(authorPopularityWindow/(24*time.Hour)),
		authorPopularityLocation,
	)

	auth := newAuthManager([]byte(authSecret), defaultAccessTokenTTL, defaultRefreshTokenTTL)
	logMode := resolveLogMode(os.Getenv("LOG_MODE"))
	albumCoverService := newAlbumCoverServiceFromEnv(albumCoversDir)
	lyricsSearchService := newLyricsSearchServiceFromEnv()
	telegramConfig, err := loadTelegramConfig(telegramStateDir, telegramImportTempDir)
	if err != nil {
		return fmt.Errorf("failed to initialize telegram configuration: %w", err)
	}
	telegramGateway := newGotdTelegramGateway(telegramConfig)
	telegramImport := newTelegramImportService(telegramConfig, telegramGateway, store, songsDir)
	defer func() {
		if err := telegramImport.Close(); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("close telegram import service: %w", err))
		}
	}()
	youtubeImport := newYouTubeImportService(youtubeImportConfig, youtubeImportGateway, store, songsDir)
	defer func() {
		if err := youtubeImport.Close(); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("close youtube import service: %w", err))
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthzHandler())
	mux.HandleFunc("GET /api/songs", listSongsHandler(songsDir))
	mux.Handle("POST /api/songs", requireRole(auth, store, roleAdmin, uploadSongHandler(songsDir)))
	mux.Handle("GET /api/songs/unused", requireRole(auth, store, roleAdmin, listUnusedSongsHandler(store, songsDir)))
	mux.HandleFunc("GET /api/songs/", getSongHandler(songsDir))
	mux.Handle("DELETE /api/songs/", requireRole(auth, store, roleAdmin, deleteSongHandler(store, songsDir)))
	mux.HandleFunc("GET /api/album-covers/", getAlbumCoverHandler(albumCoversDir))
	mux.HandleFunc("GET /api/albums", listAlbumsHandler(store, auth))
	mux.HandleFunc("GET /api/search", searchHandler(store, auth))
	mux.Handle("POST /api/albums", requireRole(auth, store, roleAdmin, createAlbumHandler(store)))
	mux.HandleFunc("GET /api/albums/", getAlbumByRouteHandler(store, auth))
	mux.Handle("PUT /api/albums/", requireRole(auth, store, roleAdmin, updateAlbumByRouteHandler(store)))
	mux.Handle("DELETE /api/albums/", requireRole(auth, store, roleAdmin, deleteAlbumByRouteHandler(store)))
	mux.Handle("POST /api/album-covers", requireRole(auth, store, roleAdmin, uploadAlbumCoverHandler(albumCoversDir)))
	mux.Handle("GET /api/album-covers/suggestions", requireRole(auth, store, roleAdmin, albumCoverSuggestionsHandler(albumCoverService)))
	mux.Handle("POST /api/album-covers/import", requireRole(auth, store, roleAdmin, importAlbumCoverHandler(albumCoverService)))
	mux.Handle("GET /api/playlists", requireAuth(auth, store, listPlaylistsHandler(store)))
	mux.Handle("POST /api/playlists", requireAuth(auth, store, createPlaylistHandler(store)))
	mux.Handle("POST /api/playlists/", requireAuth(auth, store, uploadPlaylistCoverByRouteHandler(store, albumCoversDir)))
	mux.Handle("GET /api/playlists/", requireAuth(auth, store, getPlaylistByRouteHandler(store)))
	mux.Handle("PUT /api/playlists/", requireAuth(auth, store, updatePlaylistByRouteHandler(store)))
	mux.Handle("DELETE /api/playlists/", requireAuth(auth, store, deletePlaylistByRouteHandler(store)))
	mux.HandleFunc("GET /api/public/playlists/", getPublicPlaylistByRouteHandler(store, auth))
	mux.HandleFunc("GET /api/shared/playlists/", getSharedPlaylistByRouteHandler(store, auth))
	mux.Handle("POST /api/autoplay/next", requireAuth(auth, store, autoplayNextHandler(store)))
	mux.HandleFunc("POST /api/analytics/events", analyticsEventsHandler(store, auth))
	mux.HandleFunc("GET /api/tracks", listTracksHandler(store, auth))
	mux.Handle("POST /api/tracks", requireRole(auth, store, roleAdmin, createTrackHandler(store)))
	mux.Handle("GET /api/tracks/", getTrackByRouteHandler(store, auth))
	mux.Handle("POST /api/tracks/", postTrackByRouteHandler(store, auth, lyricsSearchService))
	mux.Handle("PUT /api/tracks/", putTrackByRouteHandler(store, auth))
	mux.Handle("DELETE /api/tracks/", deleteTrackByRouteHandler(store, auth))
	mux.HandleFunc("GET /api/authors", listAuthorsHandler(store))
	mux.Handle("POST /api/authors", requireRole(auth, store, roleAdmin, createAuthorHandler(store)))
	mux.HandleFunc("GET /api/authors/", getAuthorByIDHandler(store))
	mux.Handle("PUT /api/authors/", requireRole(auth, store, roleAdmin, updateAuthorHandler(store)))
	mux.Handle("DELETE /api/authors/", requireRole(auth, store, roleAdmin, deleteAuthorHandler(store)))
	mux.HandleFunc("GET /api/author-photos/", getAuthorPhotoHandler(authorPhotosDir))
	mux.Handle("POST /api/author-photos", requireRole(auth, store, roleAdmin, uploadAuthorPhotoHandler(authorPhotosDir)))
	mux.HandleFunc("POST /api/auth/register", registerHandler(store, auth))
	mux.HandleFunc("POST /api/auth/login", loginHandler(store, auth))
	mux.HandleFunc("POST /api/auth/refresh", refreshHandler(store, auth))
	mux.HandleFunc("POST /api/auth/logout", logoutHandler(store))
	mux.Handle("GET /api/auth/me", requireAuth(auth, store, meHandler(store)))
	mux.Handle("GET /api/telegram/status", requireRole(auth, store, roleAdmin, telegramStatusHandler(telegramImport)))
	mux.Handle("POST /api/telegram/auth/request", requireRole(auth, store, roleAdmin, telegramAuthRequestHandler(telegramImport)))
	mux.Handle("POST /api/telegram/auth/confirm", requireRole(auth, store, roleAdmin, telegramAuthConfirmHandler(telegramImport)))
	mux.Handle("POST /api/telegram/auth/password", requireRole(auth, store, roleAdmin, telegramAuthPasswordHandler(telegramImport)))
	mux.Handle("POST /api/telegram/import-sessions", requireRole(auth, store, roleAdmin, telegramStartImportHandler(telegramImport)))
	mux.Handle("GET /api/telegram/import-sessions/current", requireRole(auth, store, roleAdmin, telegramCurrentImportHandler(telegramImport)))
	mux.Handle("POST /api/telegram/import-sessions/current/skip", requireRole(auth, store, roleAdmin, telegramSkipImportHandler(telegramImport)))
	mux.Handle("POST /api/telegram/import-sessions/current/save", requireRole(auth, store, roleAdmin, telegramSaveImportHandler(telegramImport)))
	mux.Handle("DELETE /api/telegram/import-sessions/current", requireRole(auth, store, roleAdmin, telegramCancelImportHandler(telegramImport)))
	mux.Handle("GET /api/telegram/import-sessions/current/audio", requireRole(auth, store, roleAdmin, telegramCurrentAudioHandler(telegramImport)))
	mux.Handle("GET /api/telegram/import-sessions/current/skipped-report", requireRole(auth, store, roleAdmin, telegramSkippedReportHandler(telegramImport)))
	mux.Handle("POST /api/youtube/import-sessions", requireRole(auth, store, roleAdmin, youtubeStartImportHandler(youtubeImport)))
	mux.Handle("GET /api/youtube/import-sessions/current", requireRole(auth, store, roleAdmin, youtubeCurrentImportHandler(youtubeImport)))
	mux.Handle("POST /api/youtube/import-sessions/current/skip", requireRole(auth, store, roleAdmin, youtubeSkipImportHandler(youtubeImport)))
	mux.Handle("POST /api/youtube/import-sessions/current/add", requireRole(auth, store, roleAdmin, youtubeAddImportHandler(youtubeImport)))
	mux.Handle("DELETE /api/youtube/import-sessions/current", requireRole(auth, store, roleAdmin, youtubeCancelImportHandler(youtubeImport)))
	mux.Handle("GET /api/youtube/cookies/status", requireRole(auth, store, roleAdmin, youtubeCookiesStatusHandler(youtubeCookieStore)))
	mux.Handle("POST /api/youtube/cookies", requireRole(auth, store, roleAdmin, youtubeCookiesUploadHandler(youtubeCookieStore)))
	mux.Handle("DELETE /api/youtube/cookies", requireRole(auth, store, roleAdmin, youtubeCookiesDeleteHandler(youtubeCookieStore)))
	mux.HandleFunc("GET /api/openapi.yaml", serveOpenAPIHandler())
	mux.HandleFunc("GET /api/docs", swaggerUIHandler())
	mux.HandleFunc("GET /api/redoc", redocHandler())

	addr := ":8080"
	log.Printf("server listening on %s", addr)
	log.Print("media, database, and integration storage configured")
	log.Printf("http logging mode %s", logMode)
	log.Printf("swagger docs available at http://localhost%s/api/docs", addr)
	log.Printf("redoc available at http://localhost%s/api/redoc", addr)
	handler := buildHTTPHandler(mux, logMode, sentryEnabled)
	shutdownContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	go runAuthorPopularityScheduler(shutdownContext, store, authorPopularityLocation)
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		// WriteTimeout remains disabled so large song streams are not truncated.
		IdleTimeout: httpIdleTimeout,
		BaseContext: func(net.Listener) context.Context {
			return shutdownContext
		},
	}

	return serveHTTP(shutdownContext, server, gracefulShutdownTimeout)
}

func buildHTTPHandler(next http.Handler, logMode string, sentryEnabled bool) http.Handler {
	next = withCORS(next)
	next = withRequestLogging(next, logMode)
	next = withRecovery(next)
	if sentryEnabled {
		next = withSentry(next)
	}
	return withSentryRequestState(next)
}

type gracefulHTTPServer interface {
	ListenAndServe() error
	Shutdown(context.Context) error
	Close() error
}

func serveHTTP(ctx context.Context, server gracefulHTTPServer, shutdownTimeout time.Duration) error {
	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- listenAndServeSafely(server)
	}()

	select {
	case err := <-serveErrors:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
		log.Print("graceful shutdown started")
	}

	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	shutdownErr := server.Shutdown(shutdownContext)
	cancelShutdown()
	if shutdownErr != nil {
		log.Printf("graceful shutdown failed: %s; force-closing HTTP server", safeOperationalError(shutdownErr))
		var resultErr error
		resultErr = errors.Join(resultErr, fmt.Errorf("graceful shutdown: %w", shutdownErr))
		if closeErr := server.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
			log.Printf("force-closing HTTP server failed: %s", safeOperationalError(closeErr))
			resultErr = errors.Join(resultErr, fmt.Errorf("force-close HTTP server: %w", closeErr))
		}
		if serveErr := waitForHTTPListenerExit(serveErrors, shutdownTimeout); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			resultErr = errors.Join(resultErr, fmt.Errorf("wait for HTTP listener after force-close: %w", serveErr))
		}
		return resultErr
	}

	serveErr := <-serveErrors
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("serve HTTP: %w", serveErr)
	}

	log.Print("graceful shutdown completed")
	return nil
}

func waitForHTTPListenerExit(serveErrors <-chan error, shutdownTimeout time.Duration) error {
	wait := shutdownTimeout
	if wait <= 0 || wait > maxHTTPListenerExitWait {
		wait = maxHTTPListenerExitWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case err := <-serveErrors:
		return err
	case <-timer.C:
		return context.DeadlineExceeded
	}
}

func listenAndServeSafely(server gracefulHTTPServer) (serveErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			captureSentryPanic(context.Background(), recovered)
			serveErr = errHTTPServerPanic
		}
	}()
	return server.ListenAndServe()
}

func ensureDir(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("path is not a directory")
	}
	return nil
}

func ensureDirOrCreate(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	return ensureDir(path)
}

func ensurePrivateDirOrCreate(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("private path is not a regular directory")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	return nil
}

func cleanupStaleTelegramImportEntries(dir string) error {
	return cleanupStaleImportEntries(dir, func(entry os.DirEntry) bool {
		return entry.IsDir() && looksLikeImportSessionToken(entry.Name())
	})
}

func cleanupStaleYouTubeImportEntries(dir string) error {
	return cleanupStaleImportEntries(dir, func(entry os.DirEntry) bool {
		name := entry.Name()
		return strings.HasPrefix(name, "youtube-cookies-") || strings.Contains(name, "youtube-source-")
	})
}

func cleanupStaleImportEntries(dir string, shouldRemove func(os.DirEntry) bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var cleanupErr error
	for _, entry := range entries {
		if !shouldRemove(entry) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove stale import entry: %w", err))
		}
	}
	return cleanupErr
}

func looksLikeImportSessionToken(value string) bool {
	// randomToken(16) uses unpadded base64url, which is always 22 characters.
	if len(value) != 22 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') ||
			(char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') ||
			char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}

func withCORS(next http.Handler) http.Handler {
	allowedOrigin := os.Getenv("CORS_ALLOW_ORIGIN")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && isAllowedOrigin(origin, allowedOrigin) {
			headers := w.Header()
			headers.Set("Access-Control-Allow-Origin", origin)
			headers.Set("Vary", "Origin")
			headers.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			headers.Set(
				"Access-Control-Allow-Headers",
				"Authorization, Content-Type",
			)
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				captureSentryPanic(r.Context(), rec)
				log.Printf(
					"panic recovered method=%s path=%s",
					r.Method,
					redactRequestTarget(r.URL.RequestURI()),
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()

		next.ServeHTTP(w, r)
	})
}

func withRequestLogging(next http.Handler, mode string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = withSentryRequestCaptureState(r)
		start := time.Now()

		lw := &loggingResponseWriter{
			ResponseWriter: w,
			status:         http.StatusOK,
			request:        r,
		}

		next.ServeHTTP(lw, r)
		captureUnhandledHTTPStatus(r, lw.status)

		if mode == logModeErrorOnly && lw.status < http.StatusInternalServerError {
			return
		}

		log.Printf(
			"http request method=%s path=%s status=%d duration=%s request_bytes=%d request_body=%q response_bytes=%d response_body=%q",
			r.Method,
			redactRequestTarget(r.URL.RequestURI()),
			lw.status,
			time.Since(start).Round(time.Millisecond),
			r.ContentLength,
			"[body omitted for privacy]",
			lw.bytes,
			"[body omitted for privacy]",
		)
	})
}

func redactRequestTarget(target string) string {
	redacted := redactSentryRequestURL(target)
	parsed, err := url.Parse(redacted)
	if err != nil {
		return redacted
	}
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	return parsed.String()
}

func resolveLogMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", logModeErrorOnly:
		return logModeErrorOnly
	case logModeVerbose:
		return logModeVerbose
	default:
		log.Printf("unknown LOG_MODE=%q, defaulting to %s", value, logModeErrorOnly)
		return logModeErrorOnly
	}
}

func loadDotEnv(path string) (returnErr error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close environment file: %w", closeErr))
		}
	}()

	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("invalid environment file syntax at line %d", lineNumber)
		}

		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("empty environment variable name at line %d", lineNumber)
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}

		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("set environment variable %q: %w", key, err)
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read environment file: %w", err)
	}
	return nil
}

func isAllowedOrigin(origin, configuredOrigin string) bool {
	if configuredOrigin != "" {
		return origin == configuredOrigin
	}

	parsedOrigin, err := url.Parse(origin)
	if err != nil {
		return false
	}

	host := parsedOrigin.Hostname()
	return host == "localhost" || host == "127.0.0.1"
}

func newAuthManager(secret []byte, accessTokenTTL, refreshTokenTTL time.Duration) *authManager {
	return &authManager{
		secret:          secret,
		accessTokenTTL:  accessTokenTTL,
		refreshTokenTTL: refreshTokenTTL,
	}
}

func newTrackStore(path string) (*trackStore, error) {
	s := &trackStore{}

	db, err := openSQLiteDB(path)
	if err != nil {
		return nil, err
	}
	s.db = db
	repositories := newDomainRepositories(db)
	s.unitOfWork = &sqliteUnitOfWork{db: db}
	s.userRepository = repositories.users
	s.sessionRepository = repositories.sessions
	s.authorRepository = repositories.authors
	s.catalogRepository = repositories.catalog
	s.playlistRepository = repositories.playlists
	s.lyricsRepository = repositories.lyrics

	if err := s.initSQLiteSchema(); err != nil {
		return nil, closeDatabaseAfterError(db, err)
	}

	if err := runSQLiteStartupRepairs(context.Background(), db); err != nil {
		return nil, closeDatabaseAfterError(db, err)
	}

	return s, nil
}

func closeDatabaseAfterError(db *sql.DB, primaryErr error) error {
	if db == nil {
		return primaryErr
	}
	if closeErr := db.Close(); closeErr != nil {
		return errors.Join(primaryErr, fmt.Errorf("close SQLite database after failure: %w", closeErr))
	}
	return primaryErr
}

func joinRollbackError(returnErr *error, tx *sql.Tx, operation string) {
	if tx == nil {
		return
	}
	rollbackErr := tx.Rollback()
	if rollbackErr == nil || errors.Is(rollbackErr, sql.ErrTxDone) {
		return
	}
	*returnErr = errors.Join(*returnErr, fmt.Errorf("rollback %s transaction: %w", operation, rollbackErr))
}

func joinRowsCloseError(returnErr *error, rows *sql.Rows, operation string) {
	if rows == nil {
		return
	}
	if closeErr := rows.Close(); closeErr != nil {
		*returnErr = errors.Join(*returnErr, fmt.Errorf("close rows after %s: %w", operation, closeErr))
	}
}

func openSQLiteDB(path string) (*sql.DB, error) {
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}

	dsn := path
	if path != ":memory:" {
		dsn = (&url.URL{Scheme: "file", Path: path}).String()
	}
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	// modernc.org/sqlite applies every _pragma parameter to every connection.
	// This is stronger than issuing PRAGMA foreign_keys on one pooled handle.
	dsn += separator + "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		return nil, closeDatabaseAfterError(db, err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		return nil, closeDatabaseAfterError(db, err)
	}
	return db, nil
}

func (s *trackStore) initSQLiteSchema() error {
	return runSQLiteMigrations(context.Background(), s.db, defaultSchemaMigrations())
}

func (s *domainState) list() []track {
	s.mu.RLock()
	defer s.mu.RUnlock()

	items := make([]track, 0, len(s.tracks))
	for _, t := range s.tracks {
		items = append(items, t)
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].ID < items[j].ID
	})
	return items
}

func (s *domainState) listAlbums(filter albumListFilter) paginatedAlbums {
	s.mu.RLock()
	defer s.mu.RUnlock()

	items := make([]album, 0, len(s.albums))
	query := strings.ToLower(strings.TrimSpace(filter.Query))
	for _, a := range s.albums {
		if filter.AuthorID > 0 && !containsInt64(a.AuthorIDs, filter.AuthorID) {
			continue
		}
		if filter.IsPublished != nil && a.IsPublished != *filter.IsPublished {
			continue
		}
		if !filter.IncludeEmpty && len(a.TrackIDs) == 0 {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(a.Title), query) {
			continue
		}
		items = append(items, a)
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].ID < items[j].ID
	})

	page := normalizePage(filter.Page)
	pageSize := normalizePageSize(filter.PageSize)
	totalItems := len(items)
	totalPages := 0
	if totalItems > 0 {
		totalPages = (totalItems + pageSize - 1) / pageSize
	}

	start := (page - 1) * pageSize
	if start > totalItems {
		start = totalItems
	}
	end := start + pageSize
	if end > totalItems {
		end = totalItems
	}

	return paginatedAlbums{
		Items:      items[start:end],
		Page:       page,
		PageSize:   pageSize,
		TotalItems: totalItems,
		TotalPages: totalPages,
	}
}

func (s *domainState) getAlbum(id int64) (album, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.albums[id]
	return a, ok
}

func (s *domainState) getAlbumTracks(id, userID int64) ([]trackResponse, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	a, ok := s.albums[id]
	if !ok {
		return nil, false
	}

	tracks := make([]trackResponse, 0, len(a.TrackIDs))
	favoriteIDs := s.favoriteTrackSetLocked(userID)
	dislikedIDs := s.dislikedTrackSetLocked(userID)
	for _, trackID := range a.TrackIDs {
		t, ok := s.tracks[trackID]
		if !ok {
			continue
		}
		_, isFavorite := favoriteIDs[trackID]
		_, isDisliked := dislikedIDs[trackID]
		tracks = append(tracks, s.toTrackResponseLocked(t, isFavorite, isDisliked, true))
	}
	return tracks, true
}

func (s *domainState) get(id int64) (track, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tracks[id]
	return t, ok
}

func (s *domainState) createAlbum(req upsertAlbumRequest) (album, error) {
	id, err := s.repositories.metadata.AllocateID(s.ctx, "next_album_id")
	if err != nil {
		return album{}, err
	}
	a := album{
		ID:             id,
		Title:          strings.TrimSpace(req.Title),
		CoverImagePath: strings.TrimSpace(req.CoverImagePath),
		ReleaseDate:    req.ReleaseDate.UTC(),
		IsPublished:    req.IsPublished,
		TrackIDs:       normalizeTrackIDs(req.TrackIDs),
		AdditionalInfo: normalizeAdditionalInfo(req.AdditionalInfo),
	}
	if err := s.validateAlbumLocked(a); err != nil {
		return album{}, err
	}
	previousAlbumIDs := make(map[int64]struct{})
	for _, trackID := range a.TrackIDs {
		if item, ok := s.tracks[trackID]; ok && item.AlbumID > 0 {
			previousAlbumIDs[item.AlbumID] = struct{}{}
		}
	}
	s.albums[a.ID] = a
	if err := s.applyAlbumTrackIDsLocked(a.ID, a.TrackIDs, false); err != nil {
		return album{}, err
	}
	if err := s.rebuildAlbumDerivedDataLocked(); err != nil {
		return album{}, err
	}
	if err := s.repositories.catalog.InsertAlbum(s.ctx, s.albums[a.ID]); err != nil {
		return album{}, err
	}
	for _, trackID := range a.TrackIDs {
		if err := s.repositories.catalog.UpdateTrack(s.ctx, s.tracks[trackID]); err != nil {
			return album{}, err
		}
	}
	delete(previousAlbumIDs, a.ID)
	for previousID := range previousAlbumIDs {
		if previous, ok := s.albums[previousID]; ok {
			if err := s.repositories.catalog.UpdateAlbum(s.ctx, previous); err != nil {
				return album{}, err
			}
		}
	}
	return s.albums[a.ID], nil
}

func (s *domainState) updateAlbum(id int64, req upsertAlbumRequest) (album, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.albums[id]
	if !ok {
		return album{}, false, nil
	}
	updated := album{
		ID:             id,
		Title:          strings.TrimSpace(req.Title),
		CoverImagePath: strings.TrimSpace(req.CoverImagePath),
		ReleaseDate:    req.ReleaseDate.UTC(),
		IsPublished:    req.IsPublished,
		TrackIDs:       normalizeTrackIDs(req.TrackIDs),
		AdditionalInfo: normalizeAdditionalInfo(req.AdditionalInfo),
	}
	if err := s.validateAlbumLocked(updated); err != nil {
		return album{}, true, err
	}
	if removedTrackIDs := subtractTrackIDs(current.TrackIDs, updated.TrackIDs); len(removedTrackIDs) > 0 {
		return album{}, true, fmt.Errorf("%w: cannot remove tracks from album using album update", errInvalidAlbum)
	}

	affectedAlbumIDs := map[int64]struct{}{id: {}}
	movedTrackIDs := make([]int64, 0)
	for _, trackID := range updated.TrackIDs {
		if item, ok := s.tracks[trackID]; ok && item.AlbumID != id {
			movedTrackIDs = append(movedTrackIDs, trackID)
			if item.AlbumID > 0 {
				affectedAlbumIDs[item.AlbumID] = struct{}{}
			}
		}
	}
	s.albums[id] = updated
	if err := s.applyAlbumTrackIDsLocked(id, updated.TrackIDs, false); err != nil {
		return album{}, true, err
	}
	if err := s.rebuildAlbumDerivedDataLocked(); err != nil {
		return album{}, true, err
	}
	for _, trackID := range movedTrackIDs {
		if err := s.repositories.catalog.UpdateTrack(s.ctx, s.tracks[trackID]); err != nil {
			return album{}, true, err
		}
	}
	for albumID := range affectedAlbumIDs {
		if err := s.repositories.catalog.UpdateAlbum(s.ctx, s.albums[albumID]); err != nil {
			return album{}, true, err
		}
	}
	return s.albums[id], true, nil
}

func (s *domainState) deleteAlbum(id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.albums[id]
	if !ok {
		return false, nil
	}
	if len(a.TrackIDs) > 0 {
		return false, errAlbumInUse
	}
	if err := s.repositories.catalog.DeleteAlbum(s.ctx, id); err != nil {
		if errors.Is(err, errRepositoryNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *domainState) create(req upsertTrackRequest) (track, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.albums[req.AlbumID]; !ok {
		return track{}, fmt.Errorf("%w: albumId %d does not exist", errInvalidTrack, req.AlbumID)
	}
	id, err := s.repositories.metadata.AllocateID(s.ctx, "next_track_id")
	if err != nil {
		return track{}, err
	}
	t := track{
		ID:             id,
		Name:           strings.TrimSpace(req.Name),
		AuthorIDs:      normalizeAuthorIDs(req.AuthorIDs),
		AlbumID:        req.AlbumID,
		AudioFilePath:  normalizeAudioFilePath(req.AudioFilePath),
		AdditionalInfo: normalizeAdditionalInfo(req.AdditionalInfo),
		SourceMetadata: normalizeSourceMetadata(req.SourceMetadata),
		CreatedAt:      time.Now().UTC(),
	}
	if err := s.validateTrackLocked(t); err != nil {
		return track{}, err
	}

	albumOrder, err := normalizeAlbumOrder(req.AlbumOrder, len(s.albums[req.AlbumID].TrackIDs), true)
	if err != nil {
		return track{}, fmt.Errorf("%w: %v", errInvalidTrack, err)
	}

	s.tracks[t.ID] = t
	targetAlbum := s.albums[req.AlbumID]
	insertTrackIntoAlbumLocked(&targetAlbum, t.ID, albumOrder)
	s.albums[req.AlbumID] = targetAlbum
	if err := s.rebuildAlbumDerivedDataLocked(); err != nil {
		return track{}, err
	}
	if err := s.repositories.catalog.InsertTrack(s.ctx, t); err != nil {
		return track{}, err
	}
	if err := s.repositories.catalog.UpdateAlbum(s.ctx, s.albums[req.AlbumID]); err != nil {
		return track{}, err
	}
	return s.tracks[t.ID], nil
}

func (s *domainState) update(id int64, req upsertTrackRequest) (track, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.tracks[id]
	if !ok {
		return track{}, false, nil
	}
	if _, ok := s.albums[req.AlbumID]; !ok {
		return track{}, true, fmt.Errorf("%w: albumId %d does not exist", errInvalidTrack, req.AlbumID)
	}

	updated := track{
		ID:             id,
		Name:           strings.TrimSpace(req.Name),
		AuthorIDs:      normalizeAuthorIDs(req.AuthorIDs),
		AlbumID:        req.AlbumID,
		AudioFilePath:  normalizeAudioFilePath(req.AudioFilePath),
		AdditionalInfo: normalizeAdditionalInfo(req.AdditionalInfo),
		SourceMetadata: normalizeSourceMetadata(req.SourceMetadata),
		CreatedAt:      current.CreatedAt,
	}
	if err := s.validateTrackLocked(updated); err != nil {
		return track{}, true, err
	}

	currentAlbum := s.albums[current.AlbumID]
	targetAlbum := s.albums[updated.AlbumID]
	targetLength := len(targetAlbum.TrackIDs)
	if current.AlbumID == updated.AlbumID {
		targetLength--
	}
	albumOrder, err := normalizeAlbumOrder(req.AlbumOrder, targetLength, true)
	if err != nil {
		return track{}, true, fmt.Errorf("%w: %v", errInvalidTrack, err)
	}

	s.tracks[id] = updated
	removeTrackFromAlbumLocked(&currentAlbum, id)
	s.albums[current.AlbumID] = currentAlbum
	targetAlbum = s.albums[updated.AlbumID]
	insertTrackIntoAlbumLocked(&targetAlbum, id, albumOrder)
	s.albums[updated.AlbumID] = targetAlbum
	if err := s.rebuildAlbumDerivedDataLocked(); err != nil {
		return track{}, true, err
	}
	if err := s.repositories.catalog.UpdateTrack(s.ctx, s.tracks[id]); err != nil {
		return track{}, true, err
	}
	if err := s.repositories.catalog.UpdateAlbum(s.ctx, s.albums[current.AlbumID]); err != nil {
		return track{}, true, err
	}
	if updated.AlbumID != current.AlbumID {
		if err := s.repositories.catalog.UpdateAlbum(s.ctx, s.albums[updated.AlbumID]); err != nil {
			return track{}, true, err
		}
	}
	return s.tracks[id], true, nil
}

func (s *domainState) delete(id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tracks[id]
	if !ok {
		return false, nil
	}
	_, hadLyrics := s.lyricsByTrack[id]
	delete(s.tracks, id)
	delete(s.lyricsByTrack, id)
	if albumItem, ok := s.albums[t.AlbumID]; ok {
		removeTrackFromAlbumLocked(&albumItem, id)
		s.albums[t.AlbumID] = albumItem
	}
	s.markTrackUnavailableLocked(t)
	if err := s.rebuildAlbumDerivedDataLocked(); err != nil {
		return false, err
	}
	if err := s.repositories.catalog.UpdateAlbum(s.ctx, s.albums[t.AlbumID]); err != nil {
		return false, err
	}
	for _, p := range s.playlists {
		for _, item := range p.TrackItems {
			if item.TrackID == id {
				if err := s.repositories.playlists.Update(s.ctx, p); err != nil {
					return false, err
				}
				break
			}
		}
	}
	if hadLyrics {
		if err := s.repositories.lyrics.DeleteByTrackID(s.ctx, id); err != nil {
			return false, err
		}
	}
	if err := s.repositories.catalog.DeleteTrack(s.ctx, id); err != nil {
		return false, err
	}
	return true, nil
}

func (s *domainState) listPlaylists(userID int64, filter playlistListFilter) paginatedPlaylists {
	s.mu.RLock()
	defer s.mu.RUnlock()

	items := make([]playlistResponse, 0)
	query := strings.ToLower(strings.TrimSpace(filter.Query))
	visibility := normalizePlaylistVisibility(filter.Visibility)
	for _, p := range s.playlists {
		if p.UserID != userID {
			continue
		}
		if visibility != "" && p.Visibility != visibility {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(p.Name), query) && !strings.Contains(strings.ToLower(p.Description), query) {
			continue
		}
		items = append(items, s.buildPlaylistResponseLocked(p))
	}

	sort.Slice(items, func(i, j int) bool {
		leftRank := playlistKindSortRank(items[i].Kind)
		rightRank := playlistKindSortRank(items[j].Kind)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if items[i].Name == items[j].Name {
			return items[i].ID < items[j].ID
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})

	page := normalizePage(filter.Page)
	pageSize := normalizePageSize(filter.PageSize)
	totalItems := len(items)
	totalPages := 0
	if totalItems > 0 {
		totalPages = (totalItems + pageSize - 1) / pageSize
	}
	start := (page - 1) * pageSize
	if start > totalItems {
		start = totalItems
	}
	end := start + pageSize
	if end > totalItems {
		end = totalItems
	}

	return paginatedPlaylists{
		Items:      items[start:end],
		Page:       page,
		PageSize:   pageSize,
		TotalItems: totalItems,
		TotalPages: totalPages,
	}
}

func (s *domainState) getPlaylist(userID, playlistID int64) (playlistResponse, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	p, ok := s.playlists[playlistID]
	if !ok || p.UserID != userID {
		return playlistResponse{}, false
	}
	return s.buildPlaylistResponseLocked(p), true
}

func (s *domainState) createPlaylist(userID int64, req upsertPlaylistRequest) (playlistResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.users[userID]; !ok {
		return playlistResponse{}, errPlaylistNotFound
	}

	id, err := s.repositories.metadata.AllocateID(s.ctx, "next_playlist_id")
	if err != nil {
		return playlistResponse{}, err
	}
	p := playlist{
		ID:             id,
		UserID:         userID,
		Name:           strings.TrimSpace(req.Name),
		Description:    strings.TrimSpace(req.Description),
		CoverImagePath: strings.TrimSpace(req.CoverImagePath),
		Visibility:     normalizePlaylistVisibility(req.Visibility),
		TrackItems:     []playlistTrack{},
		System:         false,
		Kind:           playlistKindCustom,
	}
	if err := s.normalizePlaylistSharingLocked(&p); err != nil {
		return playlistResponse{}, err
	}
	if err := validatePlaylist(p); err != nil {
		return playlistResponse{}, err
	}

	s.playlists[p.ID] = p
	if err := s.repositories.playlists.Insert(s.ctx, p); err != nil {
		return playlistResponse{}, err
	}
	return s.buildPlaylistResponseLocked(p), nil
}

func (s *domainState) updatePlaylist(userID, playlistID int64, req upsertPlaylistRequest) (playlistResponse, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.playlists[playlistID]
	if !ok || current.UserID != userID {
		return playlistResponse{}, false, nil
	}
	if current.System {
		return playlistResponse{}, true, errSystemPlaylistImmutable
	}

	updated := current
	updated.Name = strings.TrimSpace(req.Name)
	updated.Description = strings.TrimSpace(req.Description)
	updated.CoverImagePath = strings.TrimSpace(req.CoverImagePath)
	updated.Visibility = normalizePlaylistVisibility(req.Visibility)
	if err := s.normalizePlaylistSharingLocked(&updated); err != nil {
		return playlistResponse{}, true, err
	}
	if err := validatePlaylist(updated); err != nil {
		return playlistResponse{}, true, err
	}

	s.playlists[playlistID] = updated
	if err := s.repositories.playlists.Update(s.ctx, updated); err != nil {
		return playlistResponse{}, true, err
	}
	return s.buildPlaylistResponseLocked(updated), true, nil
}

func (s *domainState) validatePlaylistCoverUploadTarget(userID, playlistID int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.playlists[playlistID]
	if !ok || current.UserID != userID {
		return false, nil
	}
	if current.System {
		return true, errSystemPlaylistImmutable
	}
	return true, nil
}

func (s *domainState) updatePlaylistCoverImage(userID, playlistID int64, coverImagePath string) (playlistResponse, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.playlists[playlistID]
	if !ok || current.UserID != userID {
		return playlistResponse{}, false, nil
	}
	if current.System {
		return playlistResponse{}, true, errSystemPlaylistImmutable
	}

	updated := current
	updated.CoverImagePath = strings.TrimSpace(coverImagePath)
	s.playlists[playlistID] = updated
	if err := s.repositories.playlists.Update(s.ctx, updated); err != nil {
		return playlistResponse{}, true, err
	}
	return s.buildPlaylistResponseLocked(updated), true, nil
}

func (s *domainState) deletePlaylist(userID, playlistID int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.playlists[playlistID]
	if !ok || current.UserID != userID {
		return false, nil
	}
	if current.System {
		return false, errSystemPlaylistImmutable
	}
	if err := s.repositories.playlists.Delete(s.ctx, playlistID); err != nil {
		return false, err
	}
	return true, nil
}

func (s *domainState) getPlaylistTracks(userID, playlistID int64, page, pageSize int64) (paginatedTracks, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	p, ok := s.playlists[playlistID]
	if !ok || p.UserID != userID {
		return paginatedTracks{}, false
	}

	items := make([]trackResponse, 0, len(p.TrackItems))
	favoriteIDs := s.favoriteTrackSetLocked(userID)
	dislikedIDs := s.dislikedTrackSetLocked(userID)
	for _, item := range p.TrackItems {
		items = append(items, s.buildPlaylistTrackResponseLocked(item, favoriteIDs, dislikedIDs))
	}

	normalizedPage := normalizePage(int(page))
	normalizedPageSize := normalizePageSize(int(pageSize))
	totalItems := len(items)
	totalPages := 0
	if totalItems > 0 {
		totalPages = (totalItems + normalizedPageSize - 1) / normalizedPageSize
	}
	start := (normalizedPage - 1) * normalizedPageSize
	if start > totalItems {
		start = totalItems
	}
	end := start + normalizedPageSize
	if end > totalItems {
		end = totalItems
	}

	return paginatedTracks{
		Items:      items[start:end],
		Page:       normalizedPage,
		PageSize:   normalizedPageSize,
		TotalItems: totalItems,
		TotalPages: totalPages,
	}, true
}

func (s *domainState) getPublicPlaylist(playlistID int64) (playlistResponse, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	p, ok := s.playlists[playlistID]
	if !ok || p.Visibility != playlistVisibilityPublic {
		return playlistResponse{}, false
	}
	return publicPlaylistResponse(s.buildPlaylistResponseLocked(p)), true
}

func (s *domainState) getPublicPlaylistTracks(playlistID, userID, page, pageSize int64) (paginatedTracks, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	p, ok := s.playlists[playlistID]
	if !ok || p.Visibility != playlistVisibilityPublic {
		return paginatedTracks{}, false
	}
	return s.buildPublicPlaylistTracksPageLocked(p, userID, page, pageSize), true
}

func (s *domainState) getSharedPlaylist(shareToken string) (playlistResponse, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	p, ok := s.findSharedPlaylistByTokenLocked(shareToken)
	if !ok {
		return playlistResponse{}, false
	}
	return publicPlaylistResponse(s.buildPlaylistResponseLocked(p)), true
}

func (s *domainState) getSharedPlaylistTracks(shareToken string, userID, page, pageSize int64) (paginatedTracks, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	p, ok := s.findSharedPlaylistByTokenLocked(shareToken)
	if !ok {
		return paginatedTracks{}, false
	}
	return s.buildPublicPlaylistTracksPageLocked(p, userID, page, pageSize), true
}

func (s *domainState) buildPublicPlaylistTracksPageLocked(p playlist, userID, page, pageSize int64) paginatedTracks {
	items := make([]trackResponse, 0, len(p.TrackItems))
	favoriteIDs := s.favoriteTrackSetLocked(userID)
	dislikedIDs := s.dislikedTrackSetLocked(userID)
	for _, item := range p.TrackItems {
		items = append(items, s.buildPlaylistTrackResponseLocked(item, favoriteIDs, dislikedIDs))
	}

	normalizedPage := normalizePage(int(page))
	normalizedPageSize := normalizePageSize(int(pageSize))
	totalItems := len(items)
	totalPages := 0
	if totalItems > 0 {
		totalPages = (totalItems + normalizedPageSize - 1) / normalizedPageSize
	}
	start := (normalizedPage - 1) * normalizedPageSize
	if start > totalItems {
		start = totalItems
	}
	end := start + normalizedPageSize
	if end > totalItems {
		end = totalItems
	}

	return paginatedTracks{
		Items:      items[start:end],
		Page:       normalizedPage,
		PageSize:   normalizedPageSize,
		TotalItems: totalItems,
		TotalPages: totalPages,
	}
}

func (s *domainState) addTrackToPlaylists(userID, trackID int64, playlistIDs []int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tracks[trackID]; !ok {
		return errTrackNotFound
	}
	if len(playlistIDs) == 0 {
		return fmt.Errorf("%w: playlistIds are required", errInvalidPlaylistPayload)
	}

	normalizedPlaylistIDs := normalizeTrackIDs(playlistIDs)
	for _, playlistID := range normalizedPlaylistIDs {
		p, ok := s.playlists[playlistID]
		if !ok || p.UserID != userID {
			return errPlaylistNotFound
		}
		if p.System {
			return errSystemPlaylistImmutable
		}
	}

	for _, playlistID := range normalizedPlaylistIDs {
		p := s.playlists[playlistID]
		updated := appendPlaylistTrack(p.TrackItems, playlistTrack{TrackID: trackID})
		if len(updated) == len(p.TrackItems) {
			continue
		}
		p.TrackItems = updated
		s.playlists[playlistID] = p
		if err := s.repositories.playlists.Update(s.ctx, p); err != nil {
			return err
		}
	}
	return nil
}

func (s *domainState) removeTrackFromPlaylist(userID, playlistID, trackID int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.playlists[playlistID]
	if !ok || p.UserID != userID {
		return false, nil
	}
	if p.System {
		return false, errSystemPlaylistImmutable
	}
	updated := removePlaylistTrack(p.TrackItems, trackID)
	if len(updated) == len(p.TrackItems) {
		return false, nil
	}
	p.TrackItems = updated
	s.playlists[playlistID] = p
	if err := s.repositories.playlists.Update(s.ctx, p); err != nil {
		return false, err
	}
	return true, nil
}

func (s *domainState) setFavoriteTrack(userID, trackID int64, favorite bool) error {
	return s.setTrackPreference(userID, trackID, playlistKindFavorites, favorite)
}

func (s *domainState) setDislikedTrack(userID, trackID int64, disliked bool) error {
	return s.setTrackPreference(userID, trackID, playlistKindDislikes, disliked)
}

func (s *domainState) setTrackPreference(userID, trackID int64, kind string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tracks[trackID]; !ok {
		return errTrackNotFound
	}
	if kind != playlistKindFavorites && kind != playlistKindDislikes {
		return errPlaylistNotFound
	}

	p, ok := s.findPlaylistByKindLocked(userID, kind)
	if !ok {
		return errPlaylistNotFound
	}
	currentLength := len(p.TrackItems)
	if enabled {
		p.TrackItems = appendPlaylistTrack(p.TrackItems, playlistTrack{TrackID: trackID})
	} else {
		p.TrackItems = removePlaylistTrack(p.TrackItems, trackID)
	}
	if len(p.TrackItems) != currentLength {
		s.playlists[p.ID] = p
		if err := s.repositories.playlists.Update(s.ctx, p); err != nil {
			return err
		}
	}

	if enabled {
		oppositeKind := playlistKindFavorites
		if kind == playlistKindFavorites {
			oppositeKind = playlistKindDislikes
		}
		if opposite, ok := s.findPlaylistByKindLocked(userID, oppositeKind); ok {
			updated := removePlaylistTrack(opposite.TrackItems, trackID)
			if len(updated) != len(opposite.TrackItems) {
				opposite.TrackItems = updated
				s.playlists[opposite.ID] = opposite
				if err := s.repositories.playlists.Update(s.ctx, opposite); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *domainState) nextAutoplayTracks(userID int64, req autoplayNextRequest) (autoplayNextResponse, error) {
	songsMutationMu.RLock()
	defer songsMutationMu.RUnlock()

	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, ok := s.users[userID]; !ok {
		return autoplayNextResponse{}, errInvalidAutoplayRequest
	}

	switch req.SourceType {
	case autoplaySourceMyVibe:
	case autoplaySourcePlaylist:
		p, ok := s.playlists[*req.SourceID]
		if !ok || p.UserID != userID {
			return autoplayNextResponse{}, errPlaylistNotFound
		}
	case autoplaySourceAlbum:
		if _, ok := s.albums[*req.SourceID]; !ok {
			return autoplayNextResponse{}, errAlbumNotFound
		}
	case autoplaySourceTrack:
		if _, ok := s.tracks[*req.SourceID]; !ok {
			return autoplayNextResponse{}, errTrackNotFound
		}
	case autoplaySourceAuthor:
		if _, ok := s.authors[*req.SourceID]; !ok {
			return autoplayNextResponse{}, errAuthorNotFound
		}
	default:
		return autoplayNextResponse{}, fmt.Errorf("%w: unsupported sourceType", errInvalidAutoplayRequest)
	}

	excluded, dislikedIDs := s.automaticPlaybackExcludedTrackSetLocked(userID, req.RecentTrackIDs, req.ExcludedTrackIDs)
	if req.SourceType == autoplaySourceAuthor {
		return s.nextAuthorAutoplayTracksLocked(userID, req, excluded, dislikedIDs)
	}

	candidates := make([]track, 0, len(s.tracks))
	for _, t := range s.tracks {
		if _, skip := excluded[t.ID]; skip {
			continue
		}
		candidates = append(candidates, t)
	}

	chosen := make([]trackResponse, 0, min(req.Count, len(candidates)))
	favoriteIDs := s.favoriteTrackSetLocked(userID)
	for len(candidates) > 0 && len(chosen) < req.Count {
		index, err := randomInt(len(candidates))
		if err != nil {
			return autoplayNextResponse{}, err
		}
		selected := candidates[index]
		_, isFavorite := favoriteIDs[selected.ID]
		_, isDisliked := dislikedIDs[selected.ID]
		chosen = append(chosen, s.toTrackResponseLocked(selected, isFavorite, isDisliked, true))
		candidates = append(candidates[:index], candidates[index+1:]...)
	}

	return autoplayNextResponse{
		SourceType: req.SourceType,
		SourceID:   req.SourceID,
		Profile:    req.Profile,
		Strategy:   "random_stub_v1",
		Tracks:     chosen,
	}, nil
}

func (s *domainState) nextAuthorAutoplayTracksLocked(
	userID int64,
	req autoplayNextRequest,
	excluded, dislikedIDs map[int64]struct{},
) (autoplayNextResponse, error) {
	authorID := *req.SourceID
	albums := make([]album, 0, len(s.albums))
	now := time.Now().UTC()
	for _, albumItem := range s.albums {
		if !albumItem.IsPublished || albumItem.ReleaseDate.After(now) {
			continue
		}
		albums = append(albums, albumItem)
	}
	sort.Slice(albums, func(i, j int) bool {
		if !albums[i].ReleaseDate.Equal(albums[j].ReleaseDate) {
			return albums[i].ReleaseDate.After(albums[j].ReleaseDate)
		}
		return albums[i].ID > albums[j].ID
	})

	chosen := make([]trackResponse, 0, req.Count)
	favoriteIDs := s.favoriteTrackSetLocked(userID)
	for _, albumItem := range albums {
		for _, trackID := range albumItem.TrackIDs {
			if len(chosen) >= req.Count {
				break
			}
			if _, skip := excluded[trackID]; skip {
				continue
			}
			trackItem, ok := s.tracks[trackID]
			if !ok || trackItem.AlbumID != albumItem.ID || !containsInt64(trackItem.AuthorIDs, authorID) {
				continue
			}
			playable, err := s.isTrackAutomaticallyPlayableLocked(trackItem)
			if err != nil {
				return autoplayNextResponse{}, err
			}
			if !playable {
				continue
			}
			_, isFavorite := favoriteIDs[trackID]
			_, isDisliked := dislikedIDs[trackID]
			chosen = append(chosen, s.toTrackResponseLocked(trackItem, isFavorite, isDisliked, true))
		}
		if len(chosen) >= req.Count {
			break
		}
	}

	return autoplayNextResponse{
		SourceType: req.SourceType,
		SourceID:   req.SourceID,
		Profile:    req.Profile,
		Strategy:   "author_release_desc_v1",
		Tracks:     chosen,
	}, nil
}

func (s *domainState) isTrackAutomaticallyPlayableLocked(trackItem track) (bool, error) {
	if trackItem.AudioFilePath == "" {
		return false, nil
	}
	fileName, isLocalSong := extractReferencedSongFileName(trackItem.AudioFilePath)
	if !isLocalSong {
		parsed, err := url.Parse(trackItem.AudioFilePath)
		return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "", nil
	}
	if s.songsDir == "" {
		return true, nil
	}
	info, err := os.Stat(filepath.Join(s.songsDir, fileName))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) && pathErr.Err != nil {
			err = pathErr.Err
		}
		return false, fmt.Errorf("%w: %w", errAutoplaySongStorage, err)
	}
	return info.Mode().IsRegular() && info.Size() > 0, nil
}

func (s *domainState) reorderPlaylistTracks(userID, playlistID int64, trackIDs []int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.playlists[playlistID]
	if !ok || p.UserID != userID {
		return false, nil
	}
	if p.System {
		return true, errSystemPlaylistImmutable
	}
	if err := validatePlaylistTrackOrder(trackIDs, p.TrackItems); err != nil {
		return true, err
	}
	p.TrackItems = reorderPlaylistTrackItems(p.TrackItems, trackIDs)
	s.playlists[playlistID] = p
	if err := s.repositories.playlists.Update(s.ctx, p); err != nil {
		return true, err
	}
	return true, nil
}

func (s *domainState) listAuthors(filter authorListFilter) ([]author, error) {
	var rankedAuthorIDs []int64
	if filter.Sort == authorPopularitySort {
		var err error
		rankedAuthorIDs, err = s.repositories.authors.ListPopularityRankedIDs(s.ctx)
		if err != nil {
			return nil, err
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	items := make([]author, 0, len(s.authors))
	seen := make(map[int64]struct{}, len(rankedAuthorIDs))
	for _, authorID := range rankedAuthorIDs {
		authorItem, exists := s.authors[authorID]
		if !exists {
			continue
		}
		items = append(items, cloneAuthor(authorItem))
		seen[authorID] = struct{}{}
	}

	unranked := make([]author, 0, len(s.authors)-len(items))
	for authorID, authorItem := range s.authors {
		if _, exists := seen[authorID]; exists {
			continue
		}
		unranked = append(unranked, cloneAuthor(authorItem))
	}
	sort.Slice(unranked, func(i, j int) bool {
		return unranked[i].ID < unranked[j].ID
	})
	items = append(items, unranked...)
	return items, nil
}

func (s *domainState) search(userID int64, filter searchListFilter) paginatedSearchResults {
	s.mu.RLock()
	defer s.mu.RUnlock()

	items := make([]searchResultItem, 0, len(s.authors)+len(s.albums)+len(s.tracks)+len(s.playlists))
	query := strings.ToLower(strings.TrimSpace(filter.Query))
	favoriteIDs := s.favoriteTrackSetLocked(userID)
	dislikedIDs := s.dislikedTrackSetLocked(userID)

	for _, a := range s.authors {
		if query != "" && !strings.Contains(strings.ToLower(a.CurrentName), query) {
			continue
		}
		authorCopy := cloneAuthor(a)
		items = append(items, searchResultItem{
			Type:   "author",
			Author: &authorCopy,
		})
	}

	for _, a := range s.albums {
		if !filter.IncludeEmpty && len(a.TrackIDs) == 0 {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(a.Title), query) {
			continue
		}
		albumCopy := a
		albumCopy.AuthorIDs = append([]int64(nil), albumCopy.AuthorIDs...)
		albumCopy.TrackIDs = append([]int64(nil), albumCopy.TrackIDs...)
		albumCopy.AdditionalInfo = normalizeAdditionalInfo(albumCopy.AdditionalInfo)
		items = append(items, searchResultItem{
			Type:  "album",
			Album: &albumCopy,
		})
	}

	for _, t := range s.tracks {
		if query != "" && !strings.Contains(strings.ToLower(t.Name), query) {
			continue
		}
		_, isFavorite := favoriteIDs[t.ID]
		_, isDisliked := dislikedIDs[t.ID]
		trackCopy := s.toSearchTrackResponseLocked(t, isFavorite, isDisliked)
		items = append(items, searchResultItem{
			Type:  "track",
			Track: &trackCopy,
		})
	}

	for _, p := range s.playlists {
		if !playlistVisibleInSearch(p, userID) {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(p.Name), query) && !strings.Contains(strings.ToLower(p.Description), query) {
			continue
		}
		playlistCopy := s.buildPlaylistResponseLocked(p)
		if p.UserID != userID {
			playlistCopy = publicPlaylistResponse(playlistCopy)
		}
		items = append(items, searchResultItem{
			Type:     "playlist",
			Playlist: &playlistCopy,
		})
	}

	sort.Slice(items, func(i, j int) bool {
		leftName, leftType, leftID := searchResultSortKey(items[i])
		rightName, rightType, rightID := searchResultSortKey(items[j])
		if leftName != rightName {
			return leftName < rightName
		}
		if leftType != rightType {
			return leftType < rightType
		}
		return leftID < rightID
	})

	page := normalizePage(filter.Page)
	pageSize := normalizePageSize(filter.PageSize)
	totalItems := len(items)
	totalPages := 0
	if totalItems > 0 {
		totalPages = (totalItems + pageSize - 1) / pageSize
	}
	start := (page - 1) * pageSize
	if start > totalItems {
		start = totalItems
	}
	end := start + pageSize
	if end > totalItems {
		end = totalItems
	}

	return paginatedSearchResults{
		Items:      items[start:end],
		Page:       page,
		PageSize:   pageSize,
		TotalItems: totalItems,
		TotalPages: totalPages,
	}
}

func (s *domainState) getAuthor(id int64) (author, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.authors[id]
	return a, ok
}

func (s *domainState) createAuthor(req upsertAuthorRequest) (author, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := s.repositories.metadata.AllocateID(s.ctx, "next_author_id")
	if err != nil {
		return author{}, err
	}
	a := author{
		ID:          id,
		CurrentName: strings.TrimSpace(req.CurrentName),
		Photos:      normalizePhotos(req.Photos),
	}
	if err := validateAuthor(a); err != nil {
		return author{}, err
	}

	s.authors[a.ID] = a
	if err := s.repositories.authors.Insert(s.ctx, a); err != nil {
		return author{}, err
	}
	return a, nil
}

func (s *trackStore) appendAnalyticsEvents(events []analyticsEventRecord) (response analyticsEventsResponse, returnErr error) {
	if s.db == nil {
		return analyticsEventsResponse{}, errors.New("sqlite database is not initialized")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return analyticsEventsResponse{}, err
	}
	committed := false
	defer func() {
		if !committed {
			joinRollbackError(&returnErr, tx, "append analytics events")
		}
	}()

	response = analyticsEventsResponse{}
	for _, event := range events {
		metadataJSON, err := marshalJSONColumn(event.Metadata)
		if err != nil {
			return analyticsEventsResponse{}, err
		}
		result, err := tx.Exec(
			`INSERT OR IGNORE INTO analytics_events (
				event_id,
				user_id,
				client_id,
				session_id,
				event_type,
				track_id,
				playlist_id,
				album_id,
				position_ms,
				duration_ms,
				search_query,
				metadata_json,
				client_time,
				received_at,
				platform,
				app_version
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			event.EventID,
			sqlNullInt64(event.UserID),
			event.ClientID,
			event.SessionID,
			event.EventType,
			sqlNullInt64(event.TrackID),
			sqlNullInt64(event.PlaylistID),
			sqlNullInt64(event.AlbumID),
			sqlNullInt(event.PositionMs),
			sqlNullInt(event.DurationMs),
			sqlNullString(event.SearchQuery),
			metadataJSON,
			formatSQLiteTime(event.ClientTime),
			formatSQLiteTime(event.ReceivedAt),
			event.Platform,
			event.AppVersion,
		)
		if err != nil {
			return analyticsEventsResponse{}, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return analyticsEventsResponse{}, err
		}
		if affected == 0 {
			response.Duplicates++
			continue
		}
		response.Accepted++
	}

	if err := tx.Commit(); err != nil {
		return analyticsEventsResponse{}, err
	}
	committed = true
	return response, nil
}

func (s *domainState) updateAuthor(id int64, req upsertAuthorRequest) (author, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.authors[id]
	if !ok {
		return author{}, false, nil
	}

	a := author{
		ID:          id,
		CurrentName: strings.TrimSpace(req.CurrentName),
		Photos:      normalizePhotos(req.Photos),
	}
	if err := validateAuthor(a); err != nil {
		return author{}, true, err
	}

	s.authors[id] = a
	if err := s.repositories.authors.Update(s.ctx, a); err != nil {
		return author{}, true, err
	}
	return a, true, nil
}

func (s *domainState) deleteAuthor(id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.authors[id]
	if !ok {
		return false, nil
	}
	for _, t := range s.tracks {
		for _, authorID := range t.AuthorIDs {
			if authorID == id {
				return false, errAuthorInUse
			}
		}
	}

	if err := s.repositories.authors.Delete(s.ctx, id); err != nil {
		return false, err
	}
	return true, nil
}

func (s *domainState) createUser(email, passwordHash string) (user, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	email = normalizeEmail(email)
	if _, exists := s.usersByEmail[email]; exists {
		return user{}, errEmailAlreadyExists
	}

	role := roleListener
	if len(s.users) == 0 {
		role = roleAdmin
	}
	userID, err := s.repositories.metadata.AllocateID(s.ctx, "next_user_id")
	if err != nil {
		return user{}, err
	}
	u := user{
		ID:           userID,
		Email:        email,
		Role:         role,
		PasswordHash: passwordHash,
		CreatedAt:    time.Now().UTC(),
	}

	favoritesID, err := s.repositories.metadata.AllocateID(s.ctx, "next_playlist_id")
	if err != nil {
		return user{}, err
	}
	dislikesID, err := s.repositories.metadata.AllocateID(s.ctx, "next_playlist_id")
	if err != nil {
		return user{}, err
	}
	favoritesPlaylist := playlist{ID: favoritesID, UserID: u.ID, Name: "Favorites", Visibility: playlistVisibilityPrivate, TrackItems: []playlistTrack{}, System: true, Kind: playlistKindFavorites}
	dislikesPlaylist := playlist{ID: dislikesID, UserID: u.ID, Name: "Disliked", Visibility: playlistVisibilityPrivate, TrackItems: []playlistTrack{}, System: true, Kind: playlistKindDislikes}
	if err := s.repositories.users.Insert(s.ctx, u); err != nil {
		if errors.Is(err, errRepositoryConflict) {
			return user{}, errEmailAlreadyExists
		}
		return user{}, err
	}
	if err := s.repositories.playlists.Insert(s.ctx, favoritesPlaylist); err != nil {
		return user{}, err
	}
	if err := s.repositories.playlists.Insert(s.ctx, dislikesPlaylist); err != nil {
		return user{}, err
	}
	return u, nil
}

func (s *domainState) getUserByEmail(email string) (user, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	id, ok := s.usersByEmail[normalizeEmail(email)]
	if !ok {
		return user{}, false
	}
	u, ok := s.users[id]
	return u, ok
}

func (s *domainState) getUser(id int64) (user, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	u, ok := s.users[id]
	return u, ok
}

func (s *domainState) createRefreshSession(userID int64, expiresAt time.Time) (refreshSession, string, error) {
	if _, ok, err := s.repositories.users.FindByID(s.ctx, userID); err != nil {
		return refreshSession{}, "", err
	} else if !ok {
		return refreshSession{}, "", errInvalidCredentials
	}
	now := time.Now().UTC()
	if err := s.repositories.sessions.DeleteExpired(s.ctx, now); err != nil {
		return refreshSession{}, "", err
	}

	rawToken, err := randomToken(32)
	if err != nil {
		return refreshSession{}, "", err
	}

	sessionID, err := randomToken(16)
	if err != nil {
		return refreshSession{}, "", err
	}

	session := refreshSession{
		ID:        sessionID,
		UserID:    userID,
		TokenHash: hashToken(rawToken),
		CreatedAt: now,
		ExpiresAt: expiresAt.UTC(),
	}

	if err := s.repositories.sessions.Insert(s.ctx, session); err != nil {
		return refreshSession{}, "", err
	}
	return session, rawToken, nil
}

func (s *domainState) rotateRefreshSession(rawToken string, expiresAt time.Time) (user, refreshSession, string, error) {
	now := time.Now().UTC()
	if err := s.repositories.sessions.DeleteExpired(s.ctx, now); err != nil {
		return user{}, refreshSession{}, "", err
	}
	session, ok, err := s.repositories.sessions.FindByToken(s.ctx, rawToken, now)
	if err != nil {
		return user{}, refreshSession{}, "", err
	}
	if !ok {
		return user{}, refreshSession{}, "", errInvalidRefreshToken
	}
	u, ok, err := s.repositories.users.FindByID(s.ctx, session.UserID)
	if err != nil {
		return user{}, refreshSession{}, "", err
	}
	if !ok {
		if err := s.repositories.sessions.Delete(s.ctx, session.ID); err != nil {
			return user{}, refreshSession{}, "", fmt.Errorf("remove refresh session for missing user: %w", err)
		}
		return user{}, refreshSession{}, "", errInvalidRefreshToken
	}

	newRawToken, err := randomToken(32)
	if err != nil {
		return user{}, refreshSession{}, "", err
	}

	newSessionID, err := randomToken(16)
	if err != nil {
		return user{}, refreshSession{}, "", err
	}

	newSession := refreshSession{
		ID:        newSessionID,
		UserID:    u.ID,
		TokenHash: hashToken(newRawToken),
		CreatedAt: now,
		ExpiresAt: expiresAt.UTC(),
	}

	if err := s.repositories.sessions.Delete(s.ctx, session.ID); err != nil {
		return user{}, refreshSession{}, "", err
	}
	if err := s.repositories.sessions.Insert(s.ctx, newSession); err != nil {
		return user{}, refreshSession{}, "", err
	}

	return u, newSession, newRawToken, nil
}

func (s *domainState) deleteRefreshSession(rawToken string) (bool, error) {
	session, ok, err := s.repositories.sessions.FindByToken(s.ctx, rawToken, time.Time{})
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if err := s.repositories.sessions.Delete(s.ctx, session.ID); err != nil {
		return false, err
	}
	return true, nil
}

func marshalJSONColumn(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func unmarshalJSONColumn(value string, target any) error {
	if strings.TrimSpace(value) == "" {
		value = "null"
	}
	return json.Unmarshal([]byte(value), target)
}

func formatSQLiteTime(value time.Time) string {
	if value.IsZero() {
		return time.Time{}.UTC().Format(time.RFC3339Nano)
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func parseSQLiteTime(value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func boolToSQLiteInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func sqlNullString(value *string) sql.NullString {
	if value == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *value, Valid: true}
}

func sqlNullInt64(value *int64) sql.NullInt64 {
	if value == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *value, Valid: true}
}

func sqlNullInt(value *int) sql.NullInt64 {
	if value == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*value), Valid: true}
}

func nullStringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func listSongsHandler(songsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		songsMutationMu.RLock()
		defer songsMutationMu.RUnlock()

		entries, err := os.ReadDir(songsDir)
		if err != nil {
			writeSentryInternalError(w, r, fmt.Errorf("read songs directory: %w", err), "failed to read songs directory", "storage", "songs.list")
			return
		}

		songs := make([]songInfo, 0, len(entries))
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			if entry.Type()&os.ModeSymlink != 0 {
				writeSentryInternalError(w, r, errors.New("songs directory contains an unsupported symbolic link"), "failed to read songs directory", "storage", "songs.list_entry")
				return
			}

			name := entry.Name()
			info, err := entry.Info()
			if err != nil {
				writeSentryInternalError(w, r, fmt.Errorf("read song directory entry metadata: %w", err), "failed to read songs directory", "storage", "songs.list_entry")
				return
			}

			songs = append(songs, buildSongInfo(name, info))
		}

		sort.Slice(songs, func(i, j int) bool {
			return strings.ToLower(songs[i].Name) < strings.ToLower(songs[j].Name)
		})

		writeJSON(w, http.StatusOK, songs)
	}
}

func uploadSongHandler(songsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info, err := uploadMediaFile(w, r, songsDir, "failed to create song file", "/api/songs/")
		if err != nil {
			writeUploadError(w, r, err, "songs.upload")
			return
		}
		writeJSON(w, http.StatusCreated, info)
	}
}

func getSongHandler(songsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveSongFile(w, r, songsDir)
	}
}

func listUnusedSongsHandler(store *trackStore, songsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		songsMutationMu.RLock()
		defer songsMutationMu.RUnlock()

		entries, err := os.ReadDir(songsDir)
		if err != nil {
			writeSentryInternalError(w, r, fmt.Errorf("read unused songs directory: %w", err), "failed to read songs directory", "storage", "songs.list_unused")
			return
		}

		referenced := store.referencedSongFiles()
		songs := make([]songInfo, 0, len(entries))
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			if entry.Type()&os.ModeSymlink != 0 {
				writeSentryInternalError(w, r, errors.New("songs directory contains an unsupported symbolic link"), "failed to read songs directory", "storage", "songs.list_unused_entry")
				return
			}

			name := entry.Name()
			if _, ok := referenced[name]; ok {
				continue
			}

			info, err := entry.Info()
			if err != nil {
				writeSentryInternalError(w, r, fmt.Errorf("read unused song directory entry metadata: %w", err), "failed to read songs directory", "storage", "songs.list_unused_entry")
				return
			}

			songs = append(songs, buildSongInfo(name, info))
		}

		sort.Slice(songs, func(i, j int) bool {
			return strings.ToLower(songs[i].Name) < strings.ToLower(songs[j].Name)
		})

		writeJSON(w, http.StatusOK, songs)
	}
}

func deleteSongHandler(store *trackStore, songsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name, err := extractMediaFileName(r.URL.Path, "/api/songs/")
		if err != nil {
			http.Error(w, "invalid song name", http.StatusBadRequest)
			return
		}

		songsMutationMu.Lock()
		defer songsMutationMu.Unlock()

		inUse, trackID := store.songFileReferenced(name)
		if inUse {
			http.Error(w, fmt.Sprintf("song is referenced by track %d", trackID), http.StatusConflict)
			return
		}

		fullPath := filepath.Join(songsDir, name)
		if err := os.Remove(fullPath); err != nil {
			if os.IsNotExist(err) {
				http.NotFound(w, r)
				return
			}
			writeSentryInternalError(w, r, fmt.Errorf("delete song file: %w", err), "failed to delete file", "storage", "songs.delete")
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func getAlbumCoverHandler(albumCoversDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveMediaFile(w, r, "/api/album-covers/", albumCoversDir)
	}
}

func uploadAuthorPhotoHandler(authorPhotosDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info, err := uploadMediaFile(w, r, authorPhotosDir, "failed to create author photo file", "/api/author-photos/")
		if err != nil {
			writeUploadError(w, r, err, "author_photos.upload")
			return
		}
		writeJSON(w, http.StatusCreated, info)
	}
}

func getAuthorPhotoHandler(authorPhotosDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveMediaFile(w, r, "/api/author-photos/", authorPhotosDir)
	}
}

func buildSongInfo(name string, info os.FileInfo) songInfo {
	path := "/api/songs/" + url.PathEscape(name)
	return songInfo{
		Name:         name,
		SizeBytes:    info.Size(),
		LastModified: info.ModTime(),
		Path:         path,
		URL:          path,
	}
}

func normalizeAudioFilePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "/api/songs/") {
		return value
	}

	parsed, err := url.Parse(value)
	if err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return value
	}

	name, err := sanitizeSongFileName(value)
	if err != nil {
		return value
	}
	return "/api/songs/" + url.PathEscape(name)
}

func sanitizeSongFileName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("uploaded filename is required")
	}

	name = strings.ReplaceAll(name, "\\", "/")
	cleanName := filepath.Base(filepath.Clean(name))
	if cleanName == "." || cleanName == "/" || cleanName == "" {
		return "", errors.New("invalid song name")
	}

	return cleanName, nil
}

func serveMediaFile(w http.ResponseWriter, r *http.Request, prefix, dir string) {
	name, err := extractMediaFileName(r.URL.Path, prefix)
	if err != nil {
		http.Error(w, "invalid song name", http.StatusBadRequest)
		return
	}

	fullPath := filepath.Join(dir, name)
	pathInfo, err := os.Lstat(fullPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		writeSentryInternalError(w, r, fmt.Errorf("inspect media path: %w", err), "failed to read file", "storage", "media.lstat")
		return
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		http.NotFound(w, r)
		return
	}

	file, err := os.Open(fullPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		writeSentryInternalError(w, r, fmt.Errorf("open media file: %w", err), "failed to read file", "storage", "media.open")
		return
	}
	info, statErr := file.Stat()
	if statErr != nil {
		if closeErr := file.Close(); closeErr != nil {
			statErr = errors.Join(statErr, fmt.Errorf("close media after stat failure: %w", closeErr))
		}
		writeSentryInternalError(w, r, fmt.Errorf("stat opened media file: %w", statErr), "failed to read file", "storage", "media.stat")
		return
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			captureSentryError(r.Context(), fmt.Errorf("close served media file: %w", closeErr), "storage", "media.close")
		}
	}()
	if !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}

	http.ServeContent(w, r, name, info.ModTime(), file)
}

func serveSongFile(w http.ResponseWriter, r *http.Request, songsDir string) {
	name, err := extractMediaFileName(r.URL.Path, "/api/songs/")
	if err != nil {
		http.Error(w, "invalid song name", http.StatusBadRequest)
		return
	}

	songsMutationMu.RLock()
	fullPath := filepath.Join(songsDir, name)
	pathInfo, err := os.Lstat(fullPath)
	if err != nil {
		songsMutationMu.RUnlock()
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		writeSentryInternalError(w, r, fmt.Errorf("inspect song path: %w", err), "failed to read file", "storage", "songs.lstat")
		return
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		songsMutationMu.RUnlock()
		http.NotFound(w, r)
		return
	}
	file, err := os.Open(fullPath)
	if err != nil {
		songsMutationMu.RUnlock()
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		writeSentryInternalError(w, r, fmt.Errorf("open song file: %w", err), "failed to read file", "storage", "songs.open")
		return
	}
	info, statErr := file.Stat()
	songsMutationMu.RUnlock()
	if statErr != nil {
		if closeErr := file.Close(); closeErr != nil {
			statErr = errors.Join(statErr, fmt.Errorf("close song after stat failure: %w", closeErr))
		}
		writeSentryInternalError(w, r, fmt.Errorf("stat opened song file: %w", statErr), "failed to read file", "storage", "songs.stat")
		return
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			captureSentryError(r.Context(), fmt.Errorf("close served song file: %w", closeErr), "storage", "songs.close")
		}
	}()
	if !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}

	http.ServeContent(w, r, name, info.ModTime(), file)
}

func extractMediaFileName(path, prefix string) (string, error) {
	encodedName := strings.TrimPrefix(path, prefix)
	if encodedName == "" {
		return "", errors.New("missing file name")
	}

	name, err := url.PathUnescape(encodedName)
	if err != nil {
		return "", err
	}

	return sanitizeSongFileName(name)
}

func uploadMediaFile(w http.ResponseWriter, r *http.Request, dir, createFailureMessage, urlPrefix string) (albumCoverInfo, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSongUploadSize)
	parseErr := r.ParseMultipartForm(multipartUploadMemoryThreshold)
	if r.MultipartForm != nil {
		defer func() {
			if cleanupErr := r.MultipartForm.RemoveAll(); cleanupErr != nil {
				captureSentryError(r.Context(), fmt.Errorf("remove multipart upload temporary files: %w", cleanupErr), "storage", "upload.cleanup_multipart")
			}
		}()
	}
	if parseErr != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(parseErr, &maxBytesErr) {
			return albumCoverInfo{}, errors.New("uploaded file is too large")
		}
		var pathErr *os.PathError
		if errors.As(parseErr, &pathErr) {
			return albumCoverInfo{}, uploadError{message: "failed to process uploaded file", cause: fmt.Errorf("parse multipart upload: %w", parseErr)}
		}
		return albumCoverInfo{}, errors.New("invalid multipart form")
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			return albumCoverInfo{}, uploadError{message: "failed to process uploaded file", cause: fmt.Errorf("open multipart upload: %w", err)}
		}
		return albumCoverInfo{}, errors.New("file is required")
	}
	uploadSucceeded := false
	defer func() {
		if closeErr := file.Close(); closeErr != nil && uploadSucceeded {
			captureSentryError(r.Context(), fmt.Errorf("close multipart upload: %w", closeErr), "storage", "upload.close_source")
		}
	}()

	name, err := sanitizeSongFileName(header.Filename)
	if err != nil {
		return albumCoverInfo{}, err
	}
	if urlPrefix == "/api/songs/" {
		songsMutationMu.Lock()
		defer songsMutationMu.Unlock()
	}

	var (
		dst      *os.File
		fullPath string
	)
	for attempt := 0; attempt < 10; attempt++ {
		storedName, err := randomizedStoredFileName(name)
		if err != nil {
			return albumCoverInfo{}, uploadError{message: createFailureMessage, cause: fmt.Errorf("generate stored media filename: %w", err)}
		}

		fullPath = filepath.Join(dir, storedName)
		dst, err = os.OpenFile(fullPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			name = storedName
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return albumCoverInfo{}, uploadError{message: createFailureMessage, cause: fmt.Errorf("create uploaded media file: %w", err)}
		}
	}
	if dst == nil {
		return albumCoverInfo{}, uploadError{message: createFailureMessage, cause: errors.New("could not allocate a unique media filename")}
	}

	var saveErr error
	if _, err := io.Copy(dst, file); err != nil {
		saveErr = errors.Join(saveErr, fmt.Errorf("copy uploaded media: %w", err))
	}
	if err := dst.Close(); err != nil {
		saveErr = errors.Join(saveErr, fmt.Errorf("close uploaded media: %w", err))
	}
	if saveErr != nil {
		if cleanupErr := removeFileForCleanup(fullPath); cleanupErr != nil {
			saveErr = errors.Join(saveErr, fmt.Errorf("remove incomplete uploaded media: %w", cleanupErr))
		}
		return albumCoverInfo{}, uploadError{message: "failed to save uploaded file", cause: saveErr}
	}

	info, err := os.Stat(fullPath)
	if err != nil {
		readErr := fmt.Errorf("stat saved media: %w", err)
		if cleanupErr := removeFileForCleanup(fullPath); cleanupErr != nil {
			readErr = errors.Join(readErr, fmt.Errorf("remove unreadable uploaded media: %w", cleanupErr))
		}
		return albumCoverInfo{}, uploadError{message: "failed to read saved file", cause: readErr}
	}
	if info.Size() == 0 {
		if cleanupErr := removeFileForCleanup(fullPath); cleanupErr != nil {
			captureSentryError(r.Context(), fmt.Errorf("remove empty uploaded media: %w", cleanupErr), "storage", "upload.cleanup")
		}
		return albumCoverInfo{}, errors.New("uploaded file is empty")
	}

	path := urlPrefix + url.PathEscape(name)
	uploadSucceeded = true
	return albumCoverInfo{
		Name:         name,
		SizeBytes:    info.Size(),
		LastModified: info.ModTime(),
		Path:         path,
		URL:          path,
	}, nil
}

type uploadError struct {
	message string
	cause   error
}

func (e uploadError) Error() string {
	return e.message
}

func (e uploadError) Unwrap() error {
	return e.cause
}

func removeFileForCleanup(path string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func removeUploadedMediaFile(dir, name string) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	return removeFileForCleanup(filepath.Join(dir, name))
}

func randomizedStoredFileName(name string) (string, error) {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	if base == "" {
		base = "file"
	}

	suffix, err := randomFileNameToken(6)
	if err != nil {
		return "", err
	}

	return base + "-" + suffix + ext, nil
}

func randomFileNameToken(size int) (string, error) {
	bytes := make([]byte, size)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func writeUploadError(w http.ResponseWriter, r *http.Request, err error, operation string) {
	switch err.Error() {
	case "invalid multipart form", "file is required", "uploaded filename is required", "invalid song name", "uploaded file is empty":
		http.Error(w, err.Error(), http.StatusBadRequest)
	case "uploaded file is too large":
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
	case "song already exists", "album cover already exists":
		http.Error(w, err.Error(), http.StatusConflict)
	case "failed to create song file", "failed to create album cover file", "failed to create author photo file", "failed to process uploaded file", "failed to save uploaded file", "failed to read saved file":
		writeSentryInternalError(w, r, err, err.Error(), "storage", operation)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

func listAlbumsHandler(store *trackStore, auth *authManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filter, err := parseAlbumListFilter(r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		filter.IncludeEmpty = false
		if auth != nil {
			userID, err := auth.authenticateRequest(r)
			if err == nil {
				setSentryUser(r.Context(), userID)
				if user, ok := store.getUser(userID); ok && user.Role == roleAdmin {
					filter.IncludeEmpty = true
				}
			}
		}
		writeJSON(w, http.StatusOK, store.listAlbums(filter))
	}
}

func createAlbumHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := decodeUpsertAlbumRequest(r)
		if err != nil {
			writeRequestDecodeError(w, err)
			return
		}

		a, err := store.createAlbum(req)
		if err != nil {
			if errors.Is(err, errInvalidAlbum) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeSentryInternalError(w, r, err, "failed to create album", "database", "albums.create")
			return
		}
		writeJSON(w, http.StatusCreated, a)
	}
}

func getAlbumByRouteHandler(store *trackStore, auth *authManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tracks") {
			getAlbumTracksHandler(store, auth).ServeHTTP(w, r)
			return
		}
		getAlbumByIDHandler(store).ServeHTTP(w, r)
	}
}

func getAlbumByIDHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseAlbumID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid album id", http.StatusBadRequest)
			return
		}
		a, ok := store.getAlbum(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, a)
	}
}

func getAlbumTracksHandler(store *trackStore, auth *authManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseAlbumTracksID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid album id", http.StatusBadRequest)
			return
		}
		tracks, ok := store.getAlbumTracks(id, optionalUserIDFromRequest(r, auth))
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, tracks)
	}
}

func updateAlbumByRouteHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseAlbumID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid album id", http.StatusBadRequest)
			return
		}
		req, err := decodeUpsertAlbumRequest(r)
		if err != nil {
			writeRequestDecodeError(w, err)
			return
		}
		a, exists, err := store.updateAlbum(id, req)
		if !exists {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			if errors.Is(err, errInvalidAlbum) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeSentryInternalError(w, r, err, "failed to update album", "database", "albums.update")
			return
		}
		writeJSON(w, http.StatusOK, a)
	}
}

func deleteAlbumByRouteHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseAlbumID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid album id", http.StatusBadRequest)
			return
		}
		deleted, err := store.deleteAlbum(id)
		if err != nil {
			if errors.Is(err, errAlbumInUse) {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			writeSentryInternalError(w, r, err, "failed to delete album", "database", "albums.delete")
			return
		}
		if !deleted {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func uploadAlbumCoverHandler(albumCoversDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info, err := uploadMediaFile(w, r, albumCoversDir, "failed to create album cover file", "/api/album-covers/")
		if err != nil {
			writeUploadError(w, r, err, "album_covers.upload")
			return
		}
		writeJSON(w, http.StatusCreated, info)
	}
}

func listPlaylistsHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		filter, err := parsePlaylistListFilter(r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, store.listPlaylists(userID, filter))
	}
}

func createPlaylistHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		req, err := decodeUpsertPlaylistRequest(r)
		if err != nil {
			writeRequestDecodeError(w, err)
			return
		}
		p, err := store.createPlaylist(userID, req)
		if err != nil {
			if errors.Is(err, errInvalidPlaylistPayload) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeSentryInternalError(w, r, err, "failed to create playlist", "database", "playlists.create")
			return
		}
		writeJSON(w, http.StatusCreated, p)
	}
}

func getPlaylistByRouteHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tracks") {
			getPlaylistTracksHandler(store).ServeHTTP(w, r)
			return
		}
		getPlaylistByIDHandler(store).ServeHTTP(w, r)
	}
}

func getPlaylistByIDHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		id, err := parsePlaylistID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid playlist id", http.StatusBadRequest)
			return
		}
		p, ok := store.getPlaylist(userID, id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, p)
	}
}

func getPlaylistTracksHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		id, err := parsePlaylistTracksID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid playlist id", http.StatusBadRequest)
			return
		}
		page := int64(parseIntWithDefault(r.URL.Query().Get("page"), 1))
		pageSize := int64(parseIntWithDefault(r.URL.Query().Get("pageSize"), 20))
		items, exists := store.getPlaylistTracks(userID, id, page, pageSize)
		if !exists {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, items)
	}
}

func getPublicPlaylistByRouteHandler(store *trackStore, auth *authManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tracks") {
			getPublicPlaylistTracksHandler(store, auth).ServeHTTP(w, r)
			return
		}
		getPublicPlaylistByIDHandler(store).ServeHTTP(w, r)
	}
}

func getPublicPlaylistByIDHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parsePublicPlaylistID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid playlist id", http.StatusBadRequest)
			return
		}
		p, ok := store.getPublicPlaylist(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, p)
	}
}

func getPublicPlaylistTracksHandler(store *trackStore, auth *authManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parsePublicPlaylistTracksID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid playlist id", http.StatusBadRequest)
			return
		}
		page := int64(parseIntWithDefault(r.URL.Query().Get("page"), 1))
		pageSize := int64(parseIntWithDefault(r.URL.Query().Get("pageSize"), 20))
		items, exists := store.getPublicPlaylistTracks(id, optionalUserIDFromRequest(r, auth), page, pageSize)
		if !exists {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, items)
	}
}

func getSharedPlaylistByRouteHandler(store *trackStore, auth *authManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tracks") {
			getSharedPlaylistTracksHandler(store, auth).ServeHTTP(w, r)
			return
		}
		getSharedPlaylistByTokenHandler(store).ServeHTTP(w, r)
	}
}

func getSharedPlaylistByTokenHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		shareToken, err := parseSharedPlaylistToken(r.URL.Path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		p, ok := store.getSharedPlaylist(shareToken)
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, p)
	}
}

func getSharedPlaylistTracksHandler(store *trackStore, auth *authManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		shareToken, err := parseSharedPlaylistTracksToken(r.URL.Path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		page := int64(parseIntWithDefault(r.URL.Query().Get("page"), 1))
		pageSize := int64(parseIntWithDefault(r.URL.Query().Get("pageSize"), 20))
		items, exists := store.getSharedPlaylistTracks(shareToken, optionalUserIDFromRequest(r, auth), page, pageSize)
		if !exists {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, items)
	}
}

func uploadPlaylistCoverByRouteHandler(store *trackStore, albumCoversDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		id, err := parsePlaylistCoverID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid playlist id", http.StatusBadRequest)
			return
		}

		exists, err := store.validatePlaylistCoverUploadTarget(userID, id)
		if !exists {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			if errors.Is(err, errSystemPlaylistImmutable) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeSentryInternalError(w, r, err, "failed to update playlist cover", "database", "playlists.cover_validate")
			return
		}

		info, err := uploadMediaFile(w, r, albumCoversDir, "failed to create album cover file", "/api/album-covers/")
		if err != nil {
			writeUploadError(w, r, err, "playlists.cover_upload")
			return
		}

		p, exists, err := store.updatePlaylistCoverImage(userID, id, info.URL)
		if !exists {
			if cleanupErr := removeUploadedMediaFile(albumCoversDir, info.Name); cleanupErr != nil {
				captureSentryError(r.Context(), fmt.Errorf("remove orphaned playlist cover: %w", cleanupErr), "storage", "playlists.cover_cleanup")
			}
			http.NotFound(w, r)
			return
		}
		if err != nil {
			cleanupErr := removeUploadedMediaFile(albumCoversDir, info.Name)
			if errors.Is(err, errSystemPlaylistImmutable) {
				if cleanupErr != nil {
					captureSentryError(r.Context(), fmt.Errorf("remove rejected playlist cover upload: %w", cleanupErr), "storage", "playlists.cover_cleanup")
				}
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("remove failed playlist cover upload: %w", cleanupErr))
			}
			writeSentryInternalError(w, r, err, "failed to update playlist cover", "database", "playlists.cover_update")
			return
		}

		writeJSON(w, http.StatusOK, p)
	}
}

func updatePlaylistByRouteHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tracks/order") {
			reorderPlaylistTracksHandler(store).ServeHTTP(w, r)
			return
		}
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		id, err := parsePlaylistID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid playlist id", http.StatusBadRequest)
			return
		}
		req, err := decodeUpsertPlaylistRequest(r)
		if err != nil {
			writeRequestDecodeError(w, err)
			return
		}
		p, exists, err := store.updatePlaylist(userID, id, req)
		if !exists {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			if errors.Is(err, errInvalidPlaylistPayload) || errors.Is(err, errSystemPlaylistImmutable) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeSentryInternalError(w, r, err, "failed to update playlist", "database", "playlists.update")
			return
		}
		writeJSON(w, http.StatusOK, p)
	}
}

func deletePlaylistByRouteHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		id, err := parsePlaylistID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid playlist id", http.StatusBadRequest)
			return
		}
		deleted, err := store.deletePlaylist(userID, id)
		if err != nil {
			if errors.Is(err, errSystemPlaylistImmutable) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeSentryInternalError(w, r, err, "failed to delete playlist", "database", "playlists.delete")
			return
		}
		if !deleted {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func reorderPlaylistTracksHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		id, err := parsePlaylistTrackOrderID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid playlist id", http.StatusBadRequest)
			return
		}
		req, err := decodeReorderPlaylistTracksRequest(r)
		if err != nil {
			writeRequestDecodeError(w, err)
			return
		}
		exists, err := store.reorderPlaylistTracks(userID, id, req.TrackIDs)
		if !exists {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			if errors.Is(err, errInvalidPlaylistPayload) || errors.Is(err, errSystemPlaylistImmutable) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeSentryInternalError(w, r, err, "failed to reorder playlist tracks", "database", "playlists.reorder_tracks")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func listTracksHandler(store *trackStore, auth *authManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filter, err := parseTrackListFilter(r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, store.listTrackResponses(optionalUserIDFromRequest(r, auth), filter))
	}
}

func createTrackHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := decodeUpsertTrackRequest(r)
		if err != nil {
			writeRequestDecodeError(w, err)
			return
		}

		t, err := store.create(req)
		if err != nil {
			if errors.Is(err, errInvalidTrack) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeSentryInternalError(w, r, err, "failed to create track", "database", "tracks.create")
			return
		}
		writeJSON(w, http.StatusCreated, store.toTrackResponse(t, false, false, true))
	}
}

func getTrackByRouteHandler(store *trackStore, auth *authManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/lyrics") {
			getTrackLyricsHandler(store).ServeHTTP(w, r)
			return
		}
		getTrackByIDHandler(store, auth).ServeHTTP(w, r)
	}
}

func getTrackByIDHandler(store *trackStore, auth *authManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseTrackID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid track id", http.StatusBadRequest)
			return
		}
		t, ok := store.getTrackResponse(id, optionalUserIDFromRequest(r, auth))
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, t)
	}
}

func updateTrackHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseTrackID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid track id", http.StatusBadRequest)
			return
		}

		req, err := decodeUpsertTrackRequest(r)
		if err != nil {
			writeRequestDecodeError(w, err)
			return
		}

		t, exists, err := store.update(id, req)
		if !exists {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			if errors.Is(err, errInvalidTrack) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeSentryInternalError(w, r, err, "failed to update track", "database", "tracks.update")
			return
		}
		writeJSON(w, http.StatusOK, store.toTrackResponse(t, false, false, true))
	}
}

func deleteTrackHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseTrackID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid track id", http.StatusBadRequest)
			return
		}

		deleted, err := store.delete(id)
		if !deleted {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			writeSentryInternalError(w, r, err, "failed to delete track", "database", "tracks.delete")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func postTrackByRouteHandler(store *trackStore, auth *authManager, lyricsSearch *lyricsSearchService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/lyrics/search") {
			requireRole(auth, store, roleAdmin, lyricsSearchHandler(store, lyricsSearch)).ServeHTTP(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/playlists") {
			requireAuth(auth, store, addTrackToPlaylistsHandler(store)).ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}
}

func putTrackByRouteHandler(store *trackStore, auth *authManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/lyrics"):
			requireRole(auth, store, roleAdmin, putTrackLyricsHandler(store)).ServeHTTP(w, r)
		case strings.HasSuffix(r.URL.Path, "/favorite"):
			requireAuth(auth, store, favoriteTrackHandler(store, true)).ServeHTTP(w, r)
		case strings.HasSuffix(r.URL.Path, "/dislike"):
			requireAuth(auth, store, dislikeTrackHandler(store, true)).ServeHTTP(w, r)
		default:
			requireRole(auth, store, roleAdmin, updateTrackHandler(store)).ServeHTTP(w, r)
		}
	}
}

func deleteTrackByRouteHandler(store *trackStore, auth *authManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/lyrics"):
			requireRole(auth, store, roleAdmin, deleteTrackLyricsHandler(store)).ServeHTTP(w, r)
		case strings.HasSuffix(r.URL.Path, "/favorite"):
			requireAuth(auth, store, favoriteTrackHandler(store, false)).ServeHTTP(w, r)
		case strings.HasSuffix(r.URL.Path, "/dislike"):
			requireAuth(auth, store, dislikeTrackHandler(store, false)).ServeHTTP(w, r)
		case strings.Contains(r.URL.Path, "/playlists/"):
			requireAuth(auth, store, removeTrackFromPlaylistHandler(store)).ServeHTTP(w, r)
		default:
			requireRole(auth, store, roleAdmin, deleteTrackHandler(store)).ServeHTTP(w, r)
		}
	}
}

func addTrackToPlaylistsHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		trackID, err := parseTrackPlaylistsTrackID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid track id", http.StatusBadRequest)
			return
		}
		req, err := decodeAddTrackToPlaylistsRequest(r)
		if err != nil {
			writeRequestDecodeError(w, err)
			return
		}
		if err := store.addTrackToPlaylists(userID, trackID, req.PlaylistIDs); err != nil {
			switch {
			case errors.Is(err, errInvalidPlaylistPayload), errors.Is(err, errSystemPlaylistImmutable):
				http.Error(w, err.Error(), http.StatusBadRequest)
			case errors.Is(err, errTrackNotFound), errors.Is(err, errPlaylistNotFound):
				http.NotFound(w, r)
			default:
				writeSentryInternalError(w, r, err, "failed to add track to playlists", "database", "playlists.add_track")
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func removeTrackFromPlaylistHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		trackID, playlistID, err := parseTrackPlaylistMembershipIDs(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid track or playlist id", http.StatusBadRequest)
			return
		}
		deleted, err := store.removeTrackFromPlaylist(userID, playlistID, trackID)
		if err != nil {
			if errors.Is(err, errSystemPlaylistImmutable) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeSentryInternalError(w, r, err, "failed to remove track from playlist", "database", "playlists.remove_track")
			return
		}
		if !deleted {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func favoriteTrackHandler(store *trackStore, favorite bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		trackID, err := parseTrackFavoriteID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid track id", http.StatusBadRequest)
			return
		}
		if err := store.setFavoriteTrack(userID, trackID, favorite); err != nil {
			switch {
			case errors.Is(err, errTrackNotFound), errors.Is(err, errPlaylistNotFound):
				http.NotFound(w, r)
			default:
				writeSentryInternalError(w, r, err, "failed to update favorite track", "database", "tracks.favorite")
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func dislikeTrackHandler(store *trackStore, disliked bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		trackID, err := parseTrackDislikeID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid track id", http.StatusBadRequest)
			return
		}
		if err := store.setDislikedTrack(userID, trackID, disliked); err != nil {
			switch {
			case errors.Is(err, errTrackNotFound), errors.Is(err, errPlaylistNotFound):
				http.NotFound(w, r)
			default:
				writeSentryInternalError(w, r, err, "failed to update disliked track", "database", "tracks.dislike")
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func autoplayNextHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}

		req, err := decodeAutoplayNextRequest(r)
		if err != nil {
			writeRequestDecodeError(w, err)
			return
		}

		response, err := store.nextAutoplayTracks(userID, req)
		if err != nil {
			switch {
			case errors.Is(err, errInvalidAutoplayRequest):
				http.Error(w, err.Error(), http.StatusBadRequest)
			case errors.Is(err, errTrackNotFound), errors.Is(err, errPlaylistNotFound), errors.Is(err, errAlbumNotFound), errors.Is(err, errAuthorNotFound):
				http.NotFound(w, r)
			case errors.Is(err, errAutoplaySongStorage):
				writeSentryInternalError(w, r, err, "failed to inspect local song", "storage", "autoplay.inspect_song")
			default:
				writeSentryInternalError(w, r, err, "failed to build autoplay continuation", "database", "autoplay.next")
			}
			return
		}

		writeJSON(w, http.StatusOK, response)
	}
}

func analyticsEventsHandler(store *trackStore, auth *authManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, err := analyticsUserIDFromRequest(r, auth, store)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}

		req, err := decodeAnalyticsEventsRequest(r)
		if err != nil {
			writeRequestDecodeError(w, err)
			return
		}

		receivedAt := time.Now().UTC()
		records := make([]analyticsEventRecord, 0, len(req.Events))
		for _, event := range req.Events {
			records = append(records, analyticsEventRecord{
				EventID:     event.EventID,
				UserID:      userID,
				ClientID:    req.ClientID,
				SessionID:   req.SessionID,
				EventType:   event.Type,
				TrackID:     event.TrackID,
				PlaylistID:  event.PlaylistID,
				AlbumID:     event.AlbumID,
				PositionMs:  event.PositionMs,
				DurationMs:  event.DurationMs,
				SearchQuery: event.SearchQuery,
				Metadata:    event.Metadata,
				ClientTime:  event.ClientTime.UTC(),
				ReceivedAt:  receivedAt,
				Platform:    req.Platform,
				AppVersion:  req.AppVersion,
			})
		}

		response, err := store.appendAnalyticsEvents(records)
		if err != nil {
			writeSentryInternalError(w, r, err, "failed to store analytics events", "database", "analytics.append")
			return
		}

		writeJSON(w, http.StatusAccepted, response)
	}
}

func listAuthorsHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filter, err := parseAuthorListFilter(r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		items, err := store.listAuthors(filter)
		if err != nil {
			writeSentryInternalError(w, r, err, "failed to list authors", "database", "authors.list")
			return
		}
		writeJSON(w, http.StatusOK, items)
	}
}

func searchHandler(store *trackStore, auth *authManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filter, err := parseSearchListFilter(r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if auth != nil {
			userID, err := auth.authenticateRequest(r)
			if err == nil {
				setSentryUser(r.Context(), userID)
				if user, ok := store.getUser(userID); ok && user.Role == roleAdmin {
					filter.IncludeEmpty = true
				}
			}
		}
		writeJSON(w, http.StatusOK, store.search(optionalUserIDFromRequest(r, auth), filter))
	}
}

func createAuthorHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := decodeUpsertAuthorRequest(r)
		if err != nil {
			writeRequestDecodeError(w, err)
			return
		}
		a, err := store.createAuthor(req)
		if err != nil {
			if errors.Is(err, errInvalidAuthor) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeSentryInternalError(w, r, err, "failed to create author", "database", "authors.create")
			return
		}
		writeJSON(w, http.StatusCreated, a)
	}
}

func getAuthorByIDHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseAuthorID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid author id", http.StatusBadRequest)
			return
		}
		a, ok := store.getAuthor(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, a)
	}
}

func updateAuthorHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseAuthorID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid author id", http.StatusBadRequest)
			return
		}

		req, err := decodeUpsertAuthorRequest(r)
		if err != nil {
			writeRequestDecodeError(w, err)
			return
		}

		a, exists, err := store.updateAuthor(id, req)
		if !exists {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			if errors.Is(err, errInvalidAuthor) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeSentryInternalError(w, r, err, "failed to update author", "database", "authors.update")
			return
		}
		writeJSON(w, http.StatusOK, a)
	}
}

func deleteAuthorHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseAuthorID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid author id", http.StatusBadRequest)
			return
		}

		deleted, err := store.deleteAuthor(id)
		if err != nil {
			if errors.Is(err, errAuthorInUse) {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			writeSentryInternalError(w, r, err, "failed to delete author", "database", "authors.delete")
			return
		}
		if !deleted {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func registerHandler(store *trackStore, auth *authManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := decodeRegisterRequest(r)
		if err != nil {
			writeRequestDecodeError(w, err)
			return
		}

		passwordHash, err := hashPassword(req.Password)
		if err != nil {
			writeSentryInternalError(w, r, fmt.Errorf("hash registration password: %w", err), "failed to hash password", "auth", "register.hash_password")
			return
		}

		u, err := store.createUser(req.Email, passwordHash)
		if err != nil {
			if errors.Is(err, errEmailAlreadyExists) {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			writeSentryInternalError(w, r, err, "failed to create user", "database", "users.create")
			return
		}

		setSentryUser(r.Context(), u.ID)
		writeAuthResponse(w, r, http.StatusCreated, auth, store, u)
	}
}

func loginHandler(store *trackStore, auth *authManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := decodeLoginRequest(r)
		if err != nil {
			writeRequestDecodeError(w, err)
			return
		}

		u, ok := store.getUserByEmail(req.Email)
		if !ok || !verifyPassword(req.Password, u.PasswordHash) {
			http.Error(w, errInvalidCredentials.Error(), http.StatusUnauthorized)
			return
		}

		setSentryUser(r.Context(), u.ID)
		writeAuthResponse(w, r, http.StatusOK, auth, store, u)
	}
}

func refreshHandler(store *trackStore, auth *authManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := decodeRefreshRequest(r)
		if err != nil {
			writeRequestDecodeError(w, err)
			return
		}

		refreshExpiresAt := time.Now().UTC().Add(auth.refreshTokenTTL)
		u, session, rawToken, err := store.rotateRefreshSession(req.RefreshToken, refreshExpiresAt)
		if err != nil {
			if errors.Is(err, errInvalidRefreshToken) {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			writeSentryInternalError(w, r, err, "failed to refresh token", "auth", "refresh.rotate_session")
			return
		}
		setSentryUser(r.Context(), u.ID)

		accessToken, accessExpiresAt, err := auth.createAccessToken(u.ID)
		if err != nil {
			writeSentryInternalError(w, r, fmt.Errorf("create access token: %w", err), "failed to issue access token", "auth", "refresh.issue_access_token")
			return
		}

		writeJSON(w, http.StatusOK, authResponse{
			User:                  toPublicUser(u),
			AccessToken:           accessToken,
			AccessTokenExpiresAt:  accessExpiresAt,
			RefreshToken:          rawToken,
			RefreshTokenExpiresAt: session.ExpiresAt,
		})
	}
}

func logoutHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := decodeRefreshRequest(r)
		if err != nil {
			writeRequestDecodeError(w, err)
			return
		}

		_, err = store.deleteRefreshSession(req.RefreshToken)
		if err != nil {
			writeSentryInternalError(w, r, err, "failed to logout", "auth", "logout.delete_session")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func meHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}

		u, ok := store.getUser(userID)
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}

		writeJSON(w, http.StatusOK, toPublicUser(u))
	}
}

func requireAuth(auth *authManager, store *trackStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, err := auth.authenticateRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		if _, ok := store.getUser(userID); !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}

		setSentryUser(r.Context(), userID)
		ctx := context.WithValue(r.Context(), userContextKey, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requireRole(auth *authManager, store *trackStore, role string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, err := auth.authenticateRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}

		u, ok := store.getUser(userID)
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if u.Role != role {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		setSentryUser(r.Context(), userID)
		ctx := context.WithValue(r.Context(), userContextKey, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func writeAuthResponse(w http.ResponseWriter, r *http.Request, status int, auth *authManager, store *trackStore, u user) {
	accessToken, accessExpiresAt, err := auth.createAccessToken(u.ID)
	if err != nil {
		writeSentryInternalError(w, r, fmt.Errorf("create access token: %w", err), "failed to issue access token", "auth", "login.issue_access_token")
		return
	}

	refreshSession, refreshToken, err := store.createRefreshSession(u.ID, time.Now().UTC().Add(auth.refreshTokenTTL))
	if err != nil {
		writeSentryInternalError(w, r, fmt.Errorf("create refresh session: %w", err), "failed to issue refresh token", "auth", "login.issue_refresh_token")
		return
	}

	writeJSON(w, status, authResponse{
		User:                  toPublicUser(u),
		AccessToken:           accessToken,
		AccessTokenExpiresAt:  accessExpiresAt,
		RefreshToken:          refreshToken,
		RefreshTokenExpiresAt: refreshSession.ExpiresAt,
	})
}

func (a *authManager) createAccessToken(userID int64) (string, time.Time, error) {
	now := time.Now().UTC()
	expiresAt := now.Add(a.accessTokenTTL)

	headerJSON, err := json.Marshal(map[string]string{
		"alg": "HS256",
		"typ": "JWT",
	})
	if err != nil {
		return "", time.Time{}, err
	}

	payloadJSON, err := json.Marshal(accessTokenClaims{
		Sub: userID,
		Exp: expiresAt.Unix(),
		Iat: now.Unix(),
	})
	if err != nil {
		return "", time.Time{}, err
	}

	headerPart := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadPart := base64.RawURLEncoding.EncodeToString(payloadJSON)
	unsigned := headerPart + "." + payloadPart

	mac := hmac.New(sha256.New, a.secret)
	_, _ = mac.Write([]byte(unsigned))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return unsigned + "." + signature, expiresAt, nil
}

func (a *authManager) authenticateRequest(r *http.Request) (int64, error) {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if authHeader == "" {
		return 0, errors.New("authentication required")
	}

	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return 0, errors.New("invalid authorization header")
	}

	return a.parseAccessToken(strings.TrimSpace(strings.TrimPrefix(authHeader, prefix)))
}

func (a *authManager) parseAccessToken(token string) (int64, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0, errors.New("invalid access token")
	}

	unsigned := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, a.secret)
	_, _ = mac.Write([]byte(unsigned))
	expectedSignature := mac.Sum(nil)

	receivedSignature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return 0, errors.New("invalid access token")
	}
	if subtle.ConstantTimeCompare(receivedSignature, expectedSignature) != 1 {
		return 0, errors.New("invalid access token")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, errors.New("invalid access token")
	}

	var claims accessTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return 0, errors.New("invalid access token")
	}
	if claims.Sub <= 0 || claims.Exp <= 0 {
		return 0, errors.New("invalid access token")
	}
	if time.Now().UTC().Unix() >= claims.Exp {
		return 0, errors.New("access token expired")
	}

	return claims.Sub, nil
}

func decodeUpsertTrackRequest(r *http.Request) (upsertTrackRequest, error) {
	var req upsertTrackRequest
	if err := decodeJSON(r, &req); err != nil {
		return upsertTrackRequest{}, err
	}
	return req, nil
}

func decodeUpsertAlbumRequest(r *http.Request) (upsertAlbumRequest, error) {
	var req upsertAlbumRequest
	if err := decodeJSON(r, &req); err != nil {
		return upsertAlbumRequest{}, err
	}
	return req, nil
}

func decodeUpsertAuthorRequest(r *http.Request) (upsertAuthorRequest, error) {
	var req upsertAuthorRequest
	if err := decodeJSON(r, &req); err != nil {
		return upsertAuthorRequest{}, err
	}
	return req, nil
}

func decodeUpsertPlaylistRequest(r *http.Request) (upsertPlaylistRequest, error) {
	var req upsertPlaylistRequest
	if err := decodeJSON(r, &req); err != nil {
		return upsertPlaylistRequest{}, err
	}
	return req, nil
}

func decodeAddTrackToPlaylistsRequest(r *http.Request) (addTrackToPlaylistsRequest, error) {
	var req addTrackToPlaylistsRequest
	if err := decodeJSON(r, &req); err != nil {
		return addTrackToPlaylistsRequest{}, err
	}
	return req, nil
}

func decodeReorderPlaylistTracksRequest(r *http.Request) (reorderPlaylistTracksRequest, error) {
	var req reorderPlaylistTracksRequest
	if err := decodeJSON(r, &req); err != nil {
		return reorderPlaylistTracksRequest{}, err
	}
	return req, nil
}

func decodeAutoplayNextRequest(r *http.Request) (autoplayNextRequest, error) {
	var req autoplayNextRequest
	if err := decodeJSON(r, &req); err != nil {
		return autoplayNextRequest{}, err
	}

	req.SourceType = normalizeAutoplaySourceType(req.SourceType)
	req.Profile = normalizeAutoplayProfile(req.Profile)
	req.RecentTrackIDs = normalizeTrackIDs(req.RecentTrackIDs)
	req.ExcludedTrackIDs = normalizeTrackIDs(req.ExcludedTrackIDs)

	switch req.SourceType {
	case autoplaySourceMyVibe:
		if req.SourceID != nil {
			return autoplayNextRequest{}, fmt.Errorf("%w: sourceId must be omitted for sourceType my_vibe", errInvalidAutoplayRequest)
		}
	case autoplaySourcePlaylist, autoplaySourceAlbum, autoplaySourceTrack, autoplaySourceAuthor:
		if req.SourceID == nil || *req.SourceID <= 0 {
			return autoplayNextRequest{}, fmt.Errorf("%w: sourceId is required for sourceType %s", errInvalidAutoplayRequest, req.SourceType)
		}
	default:
		return autoplayNextRequest{}, fmt.Errorf("%w: sourceType must be one of my_vibe, playlist, album, track, author", errInvalidAutoplayRequest)
	}

	if req.Count == 0 {
		req.Count = defaultAutoplayCount
	}
	if req.Count < 0 || req.Count > maxAutoplayCount {
		return autoplayNextRequest{}, fmt.Errorf("%w: count must be between 1 and %d", errInvalidAutoplayRequest, maxAutoplayCount)
	}

	return req, nil
}

func decodeAnalyticsEventsRequest(r *http.Request) (analyticsEventsRequest, error) {
	var req analyticsEventsRequest
	if err := decodeJSON(r, &req); err != nil {
		return analyticsEventsRequest{}, err
	}

	req.ClientID = strings.TrimSpace(req.ClientID)
	req.SessionID = strings.TrimSpace(req.SessionID)
	var err error
	req.Platform, err = normalizeOptionalAnalyticsValue(req.Platform, maxAnalyticsPlatformLen, "unknown", "platform")
	if err != nil {
		return analyticsEventsRequest{}, err
	}
	req.AppVersion, err = normalizeOptionalAnalyticsValue(req.AppVersion, maxAnalyticsAppVersionLen, "unknown", "appVersion")
	if err != nil {
		return analyticsEventsRequest{}, err
	}

	switch {
	case req.ClientID == "":
		return analyticsEventsRequest{}, errors.New("clientId is required")
	case len(req.ClientID) > maxAnalyticsIDLength:
		return analyticsEventsRequest{}, fmt.Errorf("clientId must be at most %d characters", maxAnalyticsIDLength)
	case req.SessionID == "":
		return analyticsEventsRequest{}, errors.New("sessionId is required")
	case len(req.SessionID) > maxAnalyticsIDLength:
		return analyticsEventsRequest{}, fmt.Errorf("sessionId must be at most %d characters", maxAnalyticsIDLength)
	case len(req.Events) == 0:
		return analyticsEventsRequest{}, errors.New("events must contain at least one item")
	case len(req.Events) > maxAnalyticsBatchSize:
		return analyticsEventsRequest{}, fmt.Errorf("events must contain at most %d items", maxAnalyticsBatchSize)
	}

	for index := range req.Events {
		normalized, err := normalizeAnalyticsEventRequest(req.Events[index], index)
		if err != nil {
			return analyticsEventsRequest{}, err
		}
		req.Events[index] = normalized
	}

	return req, nil
}

func normalizeAnalyticsEventRequest(event analyticsEventRequest, index int) (analyticsEventRequest, error) {
	event.EventID = strings.TrimSpace(event.EventID)
	event.Type = strings.ToLower(strings.TrimSpace(event.Type))
	if event.Metadata == nil {
		event.Metadata = map[string]any{}
	}

	switch {
	case event.EventID == "":
		return analyticsEventRequest{}, fmt.Errorf("events[%d].eventId is required", index)
	case len(event.EventID) > maxAnalyticsIDLength:
		return analyticsEventRequest{}, fmt.Errorf("events[%d].eventId must be at most %d characters", index, maxAnalyticsIDLength)
	case event.Type == "":
		return analyticsEventRequest{}, fmt.Errorf("events[%d].type is required", index)
	case len(event.Type) > maxAnalyticsEventTypeLen:
		return analyticsEventRequest{}, fmt.Errorf("events[%d].type must be at most %d characters", index, maxAnalyticsEventTypeLen)
	case !isAnalyticsEventType(event.Type):
		return analyticsEventRequest{}, fmt.Errorf("events[%d].type must be one of play, pause, resume, seek, track_change, track_complete, track_skip, search, search_result_click, playback_error, track_dislike, track_undislike", index)
	case event.ClientTime.IsZero():
		return analyticsEventRequest{}, fmt.Errorf("events[%d].clientTime is required", index)
	}

	if err := validatePositiveOptionalInt64(event.TrackID, fmt.Sprintf("events[%d].trackId", index)); err != nil {
		return analyticsEventRequest{}, err
	}
	if err := validatePositiveOptionalInt64(event.PlaylistID, fmt.Sprintf("events[%d].playlistId", index)); err != nil {
		return analyticsEventRequest{}, err
	}
	if err := validatePositiveOptionalInt64(event.AlbumID, fmt.Sprintf("events[%d].albumId", index)); err != nil {
		return analyticsEventRequest{}, err
	}
	if err := validateNonNegativeOptionalInt(event.PositionMs, fmt.Sprintf("events[%d].positionMs", index)); err != nil {
		return analyticsEventRequest{}, err
	}
	if err := validateNonNegativeOptionalInt(event.DurationMs, fmt.Sprintf("events[%d].durationMs", index)); err != nil {
		return analyticsEventRequest{}, err
	}

	if event.SearchQuery != nil {
		trimmed := strings.TrimSpace(*event.SearchQuery)
		event.SearchQuery = &trimmed
		if len(trimmed) > maxAnalyticsSearchQueryLen {
			return analyticsEventRequest{}, fmt.Errorf("events[%d].searchQuery must be at most %d characters", index, maxAnalyticsSearchQueryLen)
		}
	}

	if analyticsEventRequiresTrackID(event.Type) && event.TrackID == nil {
		return analyticsEventRequest{}, fmt.Errorf("events[%d].trackId is required for type %s", index, event.Type)
	}
	if analyticsEventRequiresSearchQuery(event.Type) {
		if event.SearchQuery == nil || *event.SearchQuery == "" {
			return analyticsEventRequest{}, fmt.Errorf("events[%d].searchQuery is required for type %s", index, event.Type)
		}
	}

	return event, nil
}

func decodeRegisterRequest(r *http.Request) (registerRequest, error) {
	var req registerRequest
	if err := decodeJSON(r, &req); err != nil {
		return registerRequest{}, err
	}

	req.Email = normalizeEmail(req.Email)
	if err := validateCredentials(req.Email, req.Password); err != nil {
		return registerRequest{}, err
	}
	return req, nil
}

func decodeLoginRequest(r *http.Request) (loginRequest, error) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		return loginRequest{}, err
	}
	req.Email = normalizeEmail(req.Email)
	if req.Email == "" || req.Password == "" {
		return loginRequest{}, errors.New("email and password are required")
	}
	return req, nil
}

func decodeRefreshRequest(r *http.Request) (refreshRequest, error) {
	var req refreshRequest
	if err := decodeJSON(r, &req); err != nil {
		return refreshRequest{}, err
	}
	req.RefreshToken = strings.TrimSpace(req.RefreshToken)
	if req.RefreshToken == "" {
		return refreshRequest{}, errors.New("refreshToken is required")
	}
	return req, nil
}

func decodeJSON(r *http.Request, dst any) error {
	limited := &io.LimitedReader{R: r.Body, N: maxJSONRequestBodySize + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		if limited.N == 0 {
			return errRequestBodyTooLarge
		}
		return errors.New("invalid JSON body")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if limited.N == 0 {
			return errRequestBodyTooLarge
		}
		return errors.New("invalid JSON body")
	}
	if limited.N == 0 {
		return errRequestBodyTooLarge
	}
	return nil
}

func writeRequestDecodeError(w http.ResponseWriter, err error) {
	if errors.Is(err, errRequestBodyTooLarge) {
		http.Error(w, errRequestBodyTooLarge.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, err.Error(), http.StatusBadRequest)
}

func validateCredentials(email, password string) error {
	if !looksLikeEmail(email) {
		return errors.New("valid email is required")
	}
	if len(password) < minPasswordLength {
		return fmt.Errorf("password must be at least %d characters", minPasswordLength)
	}
	return nil
}

func looksLikeEmail(email string) bool {
	return strings.Count(email, "@") == 1 && !strings.Contains(email, " ")
}

func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}

	derivedKey := pbkdf2SHA256([]byte(password), salt, passwordHashIterations, passwordHashKeyLength)
	return fmt.Sprintf(
		"pbkdf2-sha256$%d$%s$%s",
		passwordHashIterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(derivedKey),
	), nil
}

func verifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}

	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations <= 0 {
		return false
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}

	expectedKey, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}

	actualKey := pbkdf2SHA256([]byte(password), salt, iterations, len(expectedKey))
	return subtle.ConstantTimeCompare(actualKey, expectedKey) == 1
}

func pbkdf2SHA256(password, salt []byte, iterations, keyLength int) []byte {
	hashLength := sha256.Size
	blockCount := (keyLength + hashLength - 1) / hashLength
	derivedKey := make([]byte, 0, blockCount*hashLength)

	for block := 1; block <= blockCount; block++ {
		mac := hmac.New(sha256.New, password)
		_, _ = mac.Write(salt)

		blockIndex := make([]byte, 4)
		binary.BigEndian.PutUint32(blockIndex, uint32(block))
		_, _ = mac.Write(blockIndex)
		u := mac.Sum(nil)

		t := make([]byte, len(u))
		copy(t, u)

		for i := 1; i < iterations; i++ {
			mac = hmac.New(sha256.New, password)
			_, _ = mac.Write(u)
			u = mac.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}

		derivedKey = append(derivedKey, t...)
	}

	return derivedKey[:keyLength]
}

func randomToken(length int) (string, error) {
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func randomInt(max int) (int, error) {
	if max <= 0 {
		return 0, errors.New("max must be positive")
	}
	value, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return 0, err
	}
	return int(value.Int64()), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawStdEncoding.EncodeToString(sum[:])
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func toPublicUser(u user) publicUser {
	return publicUser{
		ID:        u.ID,
		Email:     u.Email,
		Role:      u.Role,
		CreatedAt: u.CreatedAt,
	}
}

func normalizeRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case roleAdmin:
		return roleAdmin
	case roleListener:
		return roleListener
	default:
		return ""
	}
}

func normalizeAutoplaySourceType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case autoplaySourceMyVibe:
		return autoplaySourceMyVibe
	case autoplaySourcePlaylist:
		return autoplaySourcePlaylist
	case autoplaySourceAlbum:
		return autoplaySourceAlbum
	case autoplaySourceTrack:
		return autoplaySourceTrack
	case autoplaySourceAuthor:
		return autoplaySourceAuthor
	default:
		return ""
	}
}

func normalizeAutoplayProfile(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return defaultAutoplayProfile
	}
	return value
}

func normalizeOptionalAnalyticsValue(value string, maxLength int, fallback, field string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	if len(value) > maxLength {
		return "", fmt.Errorf("%s must be at most %d characters", field, maxLength)
	}
	return value, nil
}

func isAnalyticsEventType(value string) bool {
	switch value {
	case analyticsEventPlay,
		analyticsEventPause,
		analyticsEventResume,
		analyticsEventSeek,
		analyticsEventTrackChange,
		analyticsEventTrackComplete,
		analyticsEventTrackSkip,
		analyticsEventSearch,
		analyticsEventSearchResultClick,
		analyticsEventPlaybackError,
		analyticsEventTrackDislike,
		analyticsEventTrackUndislike:
		return true
	default:
		return false
	}
}

func analyticsEventRequiresTrackID(value string) bool {
	switch value {
	case analyticsEventPlay,
		analyticsEventPause,
		analyticsEventResume,
		analyticsEventSeek,
		analyticsEventTrackChange,
		analyticsEventTrackComplete,
		analyticsEventTrackSkip,
		analyticsEventPlaybackError,
		analyticsEventTrackDislike,
		analyticsEventTrackUndislike:
		return true
	default:
		return false
	}
}

func analyticsEventRequiresSearchQuery(value string) bool {
	return value == analyticsEventSearch || value == analyticsEventSearchResultClick
}

func validatePositiveOptionalInt64(value *int64, field string) error {
	if value == nil {
		return nil
	}
	if *value <= 0 {
		return fmt.Errorf("%s must be a positive integer", field)
	}
	return nil
}

func validateNonNegativeOptionalInt(value *int, field string) error {
	if value == nil {
		return nil
	}
	if *value < 0 {
		return fmt.Errorf("%s must be greater than or equal to 0", field)
	}
	return nil
}

func analyticsUserIDFromRequest(r *http.Request, auth *authManager, store *trackStore) (*int64, error) {
	if auth == nil {
		return nil, nil
	}
	if strings.TrimSpace(r.Header.Get("Authorization")) == "" {
		return nil, nil
	}
	userID, err := auth.authenticateRequest(r)
	if err != nil {
		return nil, err
	}
	if _, ok := store.getUser(userID); !ok {
		return nil, errors.New("authentication required")
	}
	setSentryUser(r.Context(), userID)
	return &userID, nil
}

func optionalUserIDFromRequest(r *http.Request, auth *authManager) int64 {
	if auth == nil {
		return 0
	}
	userID, err := auth.authenticateRequest(r)
	if err != nil {
		return 0
	}
	setSentryUser(r.Context(), userID)
	return userID
}

func userIDFromContext(ctx context.Context) (int64, bool) {
	userID, ok := ctx.Value(userContextKey).(int64)
	return userID, ok
}

func parseTrackID(path string) (int64, error) {
	return parseResourceID(path, "/api/tracks/")
}

func parseTrackFavoriteID(path string) (int64, error) {
	return parseResourceID(strings.TrimSuffix(path, "/favorite"), "/api/tracks/")
}

func parseTrackDislikeID(path string) (int64, error) {
	return parseResourceID(strings.TrimSuffix(path, "/dislike"), "/api/tracks/")
}

func parseTrackPlaylistsTrackID(path string) (int64, error) {
	return parseResourceID(strings.TrimSuffix(path, "/playlists"), "/api/tracks/")
}

func parseTrackPlaylistMembershipIDs(path string) (int64, int64, error) {
	const marker = "/playlists/"
	parts := strings.SplitN(path, marker, 2)
	if len(parts) != 2 {
		return 0, 0, errors.New("invalid membership route")
	}
	trackID, err := parseResourceID(parts[0], "/api/tracks/")
	if err != nil {
		return 0, 0, err
	}
	playlistID, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil || playlistID <= 0 {
		return 0, 0, errors.New("invalid playlist id")
	}
	return trackID, playlistID, nil
}

func parseAlbumID(path string) (int64, error) {
	return parseResourceID(path, "/api/albums/")
}

func parseAlbumTracksID(path string) (int64, error) {
	const suffix = "/tracks"
	if !strings.HasSuffix(path, suffix) {
		return 0, errors.New("invalid id")
	}
	return parseResourceID(strings.TrimSuffix(path, suffix), "/api/albums/")
}

func parseAuthorID(path string) (int64, error) {
	return parseResourceID(path, "/api/authors/")
}

func parsePlaylistID(path string) (int64, error) {
	return parseResourceID(path, "/api/playlists/")
}

func parsePlaylistTracksID(path string) (int64, error) {
	return parseResourceID(strings.TrimSuffix(path, "/tracks"), "/api/playlists/")
}

func parsePlaylistCoverID(path string) (int64, error) {
	const suffix = "/cover"
	if !strings.HasSuffix(path, suffix) {
		return 0, errors.New("invalid playlist cover route")
	}
	return parseResourceID(strings.TrimSuffix(path, suffix), "/api/playlists/")
}

func parsePublicPlaylistID(path string) (int64, error) {
	return parseResourceID(path, "/api/public/playlists/")
}

func parsePublicPlaylistTracksID(path string) (int64, error) {
	return parseResourceID(strings.TrimSuffix(path, "/tracks"), "/api/public/playlists/")
}

func parseSharedPlaylistToken(path string) (string, error) {
	return parseTokenResource(path, "/api/shared/playlists/")
}

func parseSharedPlaylistTracksToken(path string) (string, error) {
	return parseTokenResource(strings.TrimSuffix(path, "/tracks"), "/api/shared/playlists/")
}

func parsePlaylistTrackOrderID(path string) (int64, error) {
	return parseResourceID(strings.TrimSuffix(path, "/tracks/order"), "/api/playlists/")
}

func parseResourceID(path, prefix string) (int64, error) {
	idPart := strings.TrimPrefix(path, prefix)
	if idPart == "" || strings.Contains(idPart, "/") {
		return 0, errors.New("invalid id")
	}
	id, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

func parseTokenResource(path, prefix string) (string, error) {
	token := strings.TrimSpace(strings.TrimPrefix(path, prefix))
	if token == "" || strings.Contains(token, "/") {
		return "", errors.New("invalid token")
	}
	return token, nil
}

func normalizeNames(names []string) []string {
	normalized := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		normalized = append(normalized, name)
	}
	return normalized
}

func normalizePhotos(photos []string) []string {
	return normalizeNames(photos)
}

func normalizeTrackIDs(trackIDs []int64) []int64 {
	normalized := make([]int64, 0, len(trackIDs))
	seen := make(map[int64]struct{}, len(trackIDs))
	for _, id := range trackIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		normalized = append(normalized, id)
	}
	return normalized
}

func normalizeAuthorIDs(authorIDs []int64) []int64 {
	normalized := make([]int64, 0, len(authorIDs))
	seen := make(map[int64]struct{}, len(authorIDs))
	for _, id := range authorIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		normalized = append(normalized, id)
	}
	return normalized
}

func normalizeAdditionalInfo(items []additionalInfo) []additionalInfo {
	if len(items) == 0 {
		return []additionalInfo{}
	}
	normalized := make([]additionalInfo, 0, len(items))
	for _, item := range items {
		copied := make(additionalInfo, len(item))
		for key, value := range item {
			if text, ok := value.(string); ok {
				value = strings.TrimSpace(text)
			}
			copied[key] = value
		}
		normalized = append(normalized, copied)
	}
	return normalized
}

func normalizeSourceMetadata(items []sourceMetadata) []sourceMetadata {
	if len(items) == 0 {
		return []sourceMetadata{}
	}
	normalized := make([]sourceMetadata, 0, len(items))
	for _, item := range items {
		copied := make(sourceMetadata, len(item))
		for key, value := range item {
			copied[key] = normalizeMetadataValue(value)
		}
		normalized = append(normalized, copied)
	}
	return normalized
}

func normalizeMetadataValue(value any) any {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		normalized := make(map[string]any, len(typed))
		for key, nested := range typed {
			normalized[key] = normalizeMetadataValue(nested)
		}
		return normalized
	case []any:
		normalized := make([]any, len(typed))
		for index, nested := range typed {
			normalized[index] = normalizeMetadataValue(nested)
		}
		return normalized
	default:
		return value
	}
}

func normalizePlaylistTrackItems(items []playlistTrack) []playlistTrack {
	normalized := make([]playlistTrack, 0, len(items))
	seen := make(map[int64]struct{}, len(items))
	for _, item := range items {
		if item.TrackID <= 0 {
			continue
		}
		if _, ok := seen[item.TrackID]; ok {
			continue
		}
		seen[item.TrackID] = struct{}{}
		normalized = append(normalized, playlistTrack{
			TrackID:          item.TrackID,
			UnavailableTrack: cloneTrackPointer(item.UnavailableTrack),
		})
	}
	return normalized
}

func normalizePlaylistVisibility(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case playlistVisibilityPrivate:
		return playlistVisibilityPrivate
	case playlistVisibilityPublic:
		return playlistVisibilityPublic
	case playlistVisibilityShared:
		return playlistVisibilityShared
	default:
		return ""
	}
}

func (s *domainState) normalizePlaylistSharingLocked(p *playlist) error {
	if p.Visibility != playlistVisibilityShared {
		p.ShareToken = ""
		return nil
	}
	p.ShareToken = strings.TrimSpace(p.ShareToken)
	if p.ShareToken != "" && !s.playlistShareTokenInUseLocked(p.ShareToken, p.ID) {
		return nil
	}
	token, err := s.newPlaylistShareTokenLocked(p.ID)
	if err != nil {
		return err
	}
	p.ShareToken = token
	return nil
}

func (s *domainState) newPlaylistShareTokenLocked(excludePlaylistID int64) (string, error) {
	for attempts := 0; attempts < 10; attempts++ {
		token, err := randomURLToken(playlistShareTokenBytes)
		if err != nil {
			return "", err
		}
		if !s.playlistShareTokenInUseLocked(token, excludePlaylistID) {
			return token, nil
		}
	}
	return "", errors.New("failed to generate unique playlist share token")
}

func (s *domainState) playlistShareTokenInUseLocked(token string, excludePlaylistID int64) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	for _, p := range s.playlists {
		if p.ID != excludePlaylistID && p.ShareToken == token {
			return true
		}
	}
	return false
}

func (s *domainState) findSharedPlaylistByTokenLocked(shareToken string) (playlist, bool) {
	shareToken = strings.TrimSpace(shareToken)
	if shareToken == "" {
		return playlist{}, false
	}
	for _, p := range s.playlists {
		if p.Visibility == playlistVisibilityShared && p.ShareToken == shareToken {
			return p, true
		}
	}
	return playlist{}, false
}

func playlistVisibleInSearch(p playlist, userID int64) bool {
	return p.Visibility == playlistVisibilityPublic || (userID > 0 && p.UserID == userID)
}

func randomURLToken(byteCount int) (string, error) {
	tokenBytes := make([]byte, byteCount)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(tokenBytes), nil
}

func normalizePlaylistKind(value string, system bool) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case playlistKindFavorites:
		return playlistKindFavorites
	case playlistKindDislikes:
		return playlistKindDislikes
	case playlistKindCustom:
		return playlistKindCustom
	case "":
		if system {
			return playlistKindFavorites
		}
		return playlistKindCustom
	default:
		return ""
	}
}

func playlistKindSortRank(kind string) int {
	switch kind {
	case playlistKindFavorites:
		return 0
	case playlistKindDislikes:
		return 1
	default:
		return 2
	}
}

func (s *domainState) validateTrackLocked(t track) error {
	switch {
	case t.Name == "":
		return fmt.Errorf("%w: name is required", errInvalidTrack)
	case len(t.AuthorIDs) == 0:
		return fmt.Errorf("%w: at least one authorId is required", errInvalidTrack)
	case t.AlbumID <= 0:
		return fmt.Errorf("%w: albumId is required", errInvalidTrack)
	case t.AudioFilePath == "":
		return fmt.Errorf("%w: audioFilePath is required", errInvalidTrack)
	default:
		for _, authorID := range t.AuthorIDs {
			if _, ok := s.authors[authorID]; !ok {
				return fmt.Errorf("%w: authorId %d does not exist", errInvalidTrack, authorID)
			}
		}
		if _, ok := s.albums[t.AlbumID]; !ok {
			return fmt.Errorf("%w: albumId %d does not exist", errInvalidTrack, t.AlbumID)
		}
		if err := validateAdditionalInfo(t.AdditionalInfo); err != nil {
			return fmt.Errorf("%w: %v", errInvalidTrack, err)
		}
		if err := validateSourceMetadata(t.SourceMetadata); err != nil {
			return fmt.Errorf("%w: %v", errInvalidTrack, err)
		}
		return nil
	}
}

func (s *domainState) songFileReferenced(fileName string) (bool, int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, t := range s.tracks {
		if trackReferencesSongFile(t.AudioFilePath, fileName) {
			return true, t.ID
		}
	}

	return false, 0
}

func (s *domainState) referencedSongFiles() map[string]struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	referenced := make(map[string]struct{}, len(s.tracks))
	for _, t := range s.tracks {
		fileName, ok := extractReferencedSongFileName(t.AudioFilePath)
		if !ok {
			continue
		}
		referenced[fileName] = struct{}{}
	}

	return referenced
}

func validatePlaylist(p playlist) error {
	switch {
	case p.UserID <= 0:
		return fmt.Errorf("%w: userId is required", errInvalidPlaylistPayload)
	case p.Name == "":
		return fmt.Errorf("%w: name is required", errInvalidPlaylistPayload)
	case len(p.Name) > maxPlaylistNameLength:
		return fmt.Errorf("%w: name must be at most %d characters", errInvalidPlaylistPayload, maxPlaylistNameLength)
	case len(p.Description) > maxPlaylistDescLength:
		return fmt.Errorf("%w: description must be at most %d characters", errInvalidPlaylistPayload, maxPlaylistDescLength)
	case normalizePlaylistVisibility(p.Visibility) == "":
		return fmt.Errorf("%w: visibility must be one of private, public, shared", errInvalidPlaylistPayload)
	case normalizePlaylistKind(p.Kind, p.System) == "":
		return fmt.Errorf("%w: invalid playlist kind", errInvalidPlaylistPayload)
	}

	normalizedTrackIDs := make([]int64, 0, len(p.TrackItems))
	for _, item := range p.TrackItems {
		if item.TrackID <= 0 {
			return fmt.Errorf("%w: trackId must be positive", errInvalidPlaylistPayload)
		}
		normalizedTrackIDs = append(normalizedTrackIDs, item.TrackID)
	}
	if len(normalizedTrackIDs) != len(normalizeTrackIDs(normalizedTrackIDs)) {
		return fmt.Errorf("%w: duplicate trackId is not allowed", errInvalidPlaylistPayload)
	}
	if (p.Kind == playlistKindFavorites || p.Kind == playlistKindDislikes) && !p.System {
		return fmt.Errorf("%w: %s playlist must be system managed", errInvalidPlaylistPayload, p.Kind)
	}
	if p.System && p.Kind == playlistKindCustom {
		return fmt.Errorf("%w: custom playlist cannot be system managed", errInvalidPlaylistPayload)
	}
	if p.System && p.Visibility != playlistVisibilityPrivate {
		return fmt.Errorf("%w: system playlist must be private", errInvalidPlaylistPayload)
	}
	if p.System && p.ShareToken != "" {
		return fmt.Errorf("%w: system playlist cannot have a share token", errInvalidPlaylistPayload)
	}
	return nil
}

func validatePlaylistTrackOrder(trackIDs []int64, existing []playlistTrack) error {
	if len(trackIDs) != len(existing) {
		return fmt.Errorf("%w: trackIds must contain exactly %d items", errInvalidPlaylistPayload, len(existing))
	}
	if len(trackIDs) != len(normalizeTrackIDs(trackIDs)) {
		return fmt.Errorf("%w: duplicate trackId is not allowed", errInvalidPlaylistPayload)
	}
	existingSet := make(map[int64]struct{}, len(existing))
	for _, item := range existing {
		existingSet[item.TrackID] = struct{}{}
	}
	for _, trackID := range trackIDs {
		if _, ok := existingSet[trackID]; !ok {
			return fmt.Errorf("%w: trackId %d is not part of the playlist", errInvalidPlaylistPayload, trackID)
		}
	}
	return nil
}

func (s *domainState) validateAlbumLocked(a album) error {
	switch {
	case a.Title == "":
		return fmt.Errorf("%w: title is required", errInvalidAlbum)
	case a.ReleaseDate.IsZero():
		return fmt.Errorf("%w: releaseDate is required", errInvalidAlbum)
	}
	for _, trackID := range a.TrackIDs {
		if _, ok := s.tracks[trackID]; !ok {
			return fmt.Errorf("%w: trackId %d does not exist", errInvalidAlbum, trackID)
		}
	}
	if err := validateAdditionalInfo(a.AdditionalInfo); err != nil {
		return fmt.Errorf("%w: %v", errInvalidAlbum, err)
	}
	return nil
}

func validateAdditionalInfo(items []additionalInfo) error {
	for index, item := range items {
		typeValue, ok := item["type"].(string)
		if !ok || strings.TrimSpace(typeValue) == "" {
			return fmt.Errorf("additionalInfo[%d].type is required", index)
		}
		switch strings.TrimSpace(typeValue) {
		case "text":
			title, ok := item["title"].(string)
			if !ok || strings.TrimSpace(title) == "" {
				return fmt.Errorf("additionalInfo[%d].title is required for type text", index)
			}
			text, ok := item["text"].(string)
			if !ok || strings.TrimSpace(text) == "" {
				return fmt.Errorf("additionalInfo[%d].text is required for type text", index)
			}
		case "external_link":
			provider, ok := item["provider"].(string)
			if !ok || strings.TrimSpace(provider) == "" {
				return fmt.Errorf("additionalInfo[%d].provider is required for type external_link", index)
			}
			urlValue, ok := item["url"].(string)
			if !ok || strings.TrimSpace(urlValue) == "" {
				return fmt.Errorf("additionalInfo[%d].url is required for type external_link", index)
			}
		}
	}
	return nil
}

func validateSourceMetadata(items []sourceMetadata) error {
	seen := make(map[string]struct{}, len(items))
	for index, item := range items {
		provider, ok := item["provider"].(string)
		if !ok || strings.TrimSpace(provider) == "" {
			return fmt.Errorf("sourceMetadata[%d].provider is required", index)
		}
		identity, ok := item["identity"].(map[string]any)
		if !ok || len(identity) == 0 {
			return fmt.Errorf("sourceMetadata[%d].identity is required", index)
		}
		identityKey, err := sourceMetadataIdentityKey(identity)
		if err != nil {
			return fmt.Errorf("sourceMetadata[%d].identity is invalid", index)
		}
		if urlValue, ok := item["url"]; ok {
			urlString, ok := urlValue.(string)
			if !ok || strings.TrimSpace(urlString) == "" {
				return fmt.Errorf("sourceMetadata[%d].url must be a non-empty string when present", index)
			}
		}
		key := strings.ToLower(strings.TrimSpace(provider)) + "\x00" + identityKey
		if _, ok := seen[key]; ok {
			return fmt.Errorf("sourceMetadata[%d] duplicates provider/identity pair", index)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func sourceMetadataIdentityKey(identity map[string]any) (string, error) {
	normalized := normalizeMetadataValue(identity)
	identityMap, ok := normalized.(map[string]any)
	if !ok || len(identityMap) == 0 {
		return "", errors.New("identity must be an object")
	}
	encoded, err := json.Marshal(identityMap)
	if err != nil {
		return "", err
	}
	if string(encoded) == "{}" {
		return "", errors.New("identity must not be empty")
	}
	return string(encoded), nil
}

func (s *domainState) findPlaylistByKindLocked(userID int64, kind string) (playlist, bool) {
	for _, p := range s.playlists {
		if p.UserID == userID && p.Kind == kind {
			return p, true
		}
	}
	return playlist{}, false
}

func (s *domainState) findFavoritesPlaylistLocked(userID int64) (playlist, bool) {
	return s.findPlaylistByKindLocked(userID, playlistKindFavorites)
}

func (s *domainState) favoriteTrackSetLocked(userID int64) map[int64]struct{} {
	favorites, ok := s.findFavoritesPlaylistLocked(userID)
	if !ok {
		return map[int64]struct{}{}
	}
	items := make(map[int64]struct{}, len(favorites.TrackItems))
	for _, item := range favorites.TrackItems {
		items[item.TrackID] = struct{}{}
	}
	return items
}

func (s *domainState) dislikedTrackSetLocked(userID int64) map[int64]struct{} {
	dislikes, ok := s.findPlaylistByKindLocked(userID, playlistKindDislikes)
	if !ok {
		return map[int64]struct{}{}
	}
	items := make(map[int64]struct{}, len(dislikes.TrackItems))
	for _, item := range dislikes.TrackItems {
		items[item.TrackID] = struct{}{}
	}
	return items
}

func (s *domainState) automaticPlaybackExcludedTrackSetLocked(userID int64, recentTrackIDs, requestedExcludedTrackIDs []int64) (map[int64]struct{}, map[int64]struct{}) {
	dislikedIDs := s.dislikedTrackSetLocked(userID)
	excluded := make(map[int64]struct{}, len(recentTrackIDs)+len(requestedExcludedTrackIDs)+len(dislikedIDs))
	for _, trackID := range recentTrackIDs {
		excluded[trackID] = struct{}{}
	}
	for _, trackID := range requestedExcludedTrackIDs {
		excluded[trackID] = struct{}{}
	}
	for trackID := range dislikedIDs {
		excluded[trackID] = struct{}{}
	}
	return excluded, dislikedIDs
}

func (s *domainState) buildPlaylistResponseLocked(p playlist) playlistResponse {
	return playlistResponse{
		ID:             p.ID,
		UserID:         p.UserID,
		Name:           p.Name,
		Description:    p.Description,
		CoverImagePath: p.CoverImagePath,
		Visibility:     p.Visibility,
		TrackCount:     len(p.TrackItems),
		System:         p.System,
		Kind:           p.Kind,
		IsFavorites:    p.Kind == playlistKindFavorites,
		ShareToken:     p.ShareToken,
	}
}

func publicPlaylistResponse(p playlistResponse) playlistResponse {
	p.ShareToken = ""
	return p
}

func (s *domainState) buildPlaylistTrackResponseLocked(item playlistTrack, favoriteIDs, dislikedIDs map[int64]struct{}) trackResponse {
	if current, ok := s.tracks[item.TrackID]; ok {
		_, isFavorite := favoriteIDs[item.TrackID]
		_, isDisliked := dislikedIDs[item.TrackID]
		return s.toTrackResponseLocked(current, isFavorite, isDisliked, true)
	}
	if item.UnavailableTrack != nil {
		_, isFavorite := favoriteIDs[item.TrackID]
		_, isDisliked := dislikedIDs[item.TrackID]
		return s.toTrackResponseLocked(*item.UnavailableTrack, isFavorite, isDisliked, false)
	}
	return trackResponse{
		ID:          item.TrackID,
		IsFavorite:  false,
		IsAvailable: false,
	}
}

func (s *domainState) listTrackResponses(userID int64, filter trackListFilter) paginatedTracks {
	s.mu.RLock()
	defer s.mu.RUnlock()

	items := make([]trackResponse, 0, len(s.tracks))
	query := strings.ToLower(strings.TrimSpace(filter.Query))
	favoriteIDs := s.favoriteTrackSetLocked(userID)
	dislikedIDs := s.dislikedTrackSetLocked(userID)
	for _, t := range s.tracks {
		if filter.AuthorID > 0 && !containsInt64(t.AuthorIDs, filter.AuthorID) {
			continue
		}
		if filter.AlbumID > 0 && t.AlbumID != filter.AlbumID {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(t.Name), query) {
			continue
		}
		_, isFavorite := favoriteIDs[t.ID]
		_, isDisliked := dislikedIDs[t.ID]
		items = append(items, s.toTrackResponseLocked(t, isFavorite, isDisliked, true))
	}
	sort.Slice(items, func(i, j int) bool {
		switch filter.Sort {
		case trackListSortCreatedAt:
			if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
				if filter.Order == sortOrderDesc {
					return items[i].CreatedAt.After(items[j].CreatedAt)
				}
				return items[i].CreatedAt.Before(items[j].CreatedAt)
			}
			if filter.Order == sortOrderDesc {
				return items[i].ID > items[j].ID
			}
			return items[i].ID < items[j].ID
		default:
			if filter.Order == sortOrderDesc {
				return items[i].ID > items[j].ID
			}
			return items[i].ID < items[j].ID
		}
	})

	page := normalizePage(filter.Page)
	pageSize := normalizePageSize(filter.PageSize)
	totalItems := len(items)
	totalPages := 0
	if totalItems > 0 {
		totalPages = (totalItems + pageSize - 1) / pageSize
	}
	start := (page - 1) * pageSize
	if start > totalItems {
		start = totalItems
	}
	end := start + pageSize
	if end > totalItems {
		end = totalItems
	}

	return paginatedTracks{
		Items:      items[start:end],
		Page:       page,
		PageSize:   pageSize,
		TotalItems: totalItems,
		TotalPages: totalPages,
	}
}

func (s *domainState) getTrackResponse(trackID, userID int64) (trackResponse, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	t, ok := s.tracks[trackID]
	if !ok {
		return trackResponse{}, false
	}
	_, isFavorite := s.favoriteTrackSetLocked(userID)[trackID]
	_, isDisliked := s.dislikedTrackSetLocked(userID)[trackID]
	return s.toTrackResponseLocked(t, isFavorite, isDisliked, true), true
}

func (s *domainState) markTrackUnavailableLocked(t track) {
	snapshot := cloneTrack(t)
	for playlistID, p := range s.playlists {
		changed := false
		for index, item := range p.TrackItems {
			if item.TrackID != t.ID {
				continue
			}
			p.TrackItems[index].UnavailableTrack = &snapshot
			changed = true
		}
		if changed {
			s.playlists[playlistID] = p
		}
	}
}

func (s *domainState) rebuildAlbumDerivedDataLocked() error {
	for albumID, albumItem := range s.albums {
		normalizedTrackIDs := make([]int64, 0, len(albumItem.TrackIDs))
		authorSet := make(map[int64]struct{})
		for _, trackID := range normalizeTrackIDs(albumItem.TrackIDs) {
			t, ok := s.tracks[trackID]
			if !ok {
				return fmt.Errorf("%w: trackId %d does not exist", errInvalidAlbum, trackID)
			}
			if t.AlbumID != albumID {
				return fmt.Errorf("%w: trackId %d does not belong to albumId %d", errInvalidAlbum, trackID, albumID)
			}
			normalizedTrackIDs = append(normalizedTrackIDs, trackID)
			for _, authorID := range t.AuthorIDs {
				authorSet[authorID] = struct{}{}
			}
		}

		albumItem.TrackIDs = normalizedTrackIDs
		albumItem.AuthorIDs = setToSortedIDs(authorSet)
		if err := s.validateAlbumLocked(albumItem); err != nil {
			return err
		}
		s.albums[albumID] = albumItem
	}
	return nil
}

func (s *domainState) applyAlbumTrackIDsLocked(albumID int64, trackIDs []int64, allowRemoval bool) error {
	albumItem, ok := s.albums[albumID]
	if !ok {
		return fmt.Errorf("%w: albumId %d does not exist", errInvalidAlbum, albumID)
	}

	if !allowRemoval {
		for _, existingTrackID := range albumItem.TrackIDs {
			if !containsInt64(trackIDs, existingTrackID) {
				return fmt.Errorf("%w: cannot remove tracks from album using album update", errInvalidAlbum)
			}
		}
	}

	for _, trackID := range trackIDs {
		t, ok := s.tracks[trackID]
		if !ok {
			return fmt.Errorf("%w: trackId %d does not exist", errInvalidAlbum, trackID)
		}
		if t.AlbumID > 0 && t.AlbumID != albumID {
			s.removeTrackFromAlbumLocked(t.AlbumID, trackID)
		}
		t.AlbumID = albumID
		s.tracks[trackID] = t
	}

	albumItem.TrackIDs = normalizeTrackIDs(trackIDs)
	s.albums[albumID] = albumItem
	return nil
}

func (s *domainState) removeTrackFromAlbumLocked(albumID, trackID int64) {
	albumItem, ok := s.albums[albumID]
	if !ok {
		return
	}
	removeTrackFromAlbumLocked(&albumItem, trackID)
	s.albums[albumID] = albumItem
}

func removeTrackFromAlbumLocked(albumItem *album, trackID int64) {
	filtered := make([]int64, 0, len(albumItem.TrackIDs))
	for _, existingID := range albumItem.TrackIDs {
		if existingID == trackID {
			continue
		}
		filtered = append(filtered, existingID)
	}
	albumItem.TrackIDs = filtered
}

func insertTrackIntoAlbumLocked(albumItem *album, trackID int64, index int) {
	filtered := make([]int64, 0, len(albumItem.TrackIDs)+1)
	for _, existingID := range albumItem.TrackIDs {
		if existingID == trackID {
			continue
		}
		filtered = append(filtered, existingID)
	}
	if index < 0 {
		index = 0
	}
	if index > len(filtered) {
		index = len(filtered)
	}
	filtered = append(filtered[:index], append([]int64{trackID}, filtered[index:]...)...)
	albumItem.TrackIDs = filtered
}

func setToSortedIDs(items map[int64]struct{}) []int64 {
	ids := make([]int64, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return ids[i] < ids[j]
	})
	return ids
}

func containsInt64(items []int64, target int64) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func normalizeAlbumOrder(order, maxIndex int, allowAppend bool) (int, error) {
	if order < 0 {
		return 0, errors.New("albumOrder must be greater than or equal to 0")
	}
	limit := maxIndex
	if allowAppend {
		limit = maxIndex
	}
	if order > limit {
		return 0, fmt.Errorf("albumOrder must be less than or equal to %d", limit)
	}
	return order, nil
}

func parsePlaylistListFilter(values url.Values) (playlistListFilter, error) {
	filter := playlistListFilter{
		Page:       parseIntWithDefault(values.Get("page"), 1),
		PageSize:   parseIntWithDefault(values.Get("pageSize"), 20),
		Query:      strings.TrimSpace(values.Get("query")),
		Visibility: strings.TrimSpace(values.Get("visibility")),
	}
	if filter.Visibility != "" && normalizePlaylistVisibility(filter.Visibility) == "" {
		return playlistListFilter{}, errors.New("visibility must be one of private, public, shared")
	}
	return filter, nil
}

func parseAuthorListFilter(values url.Values) (authorListFilter, error) {
	filter := authorListFilter{Sort: authorPopularitySort}
	if rawSort := strings.ToLower(strings.TrimSpace(values.Get("sort"))); rawSort != "" {
		switch rawSort {
		case authorPopularitySort, authorIDSort:
			filter.Sort = rawSort
		default:
			return authorListFilter{}, errors.New("sort must be one of popularity, id")
		}
	}
	return filter, nil
}

func normalizePage(page int) int {
	if page <= 0 {
		return 1
	}
	return page
}

func normalizePageSize(pageSize int) int {
	if pageSize <= 0 {
		return 20
	}
	if pageSize > 100 {
		return 100
	}
	return pageSize
}

func parseAlbumListFilter(values url.Values) (albumListFilter, error) {
	filter := albumListFilter{
		Page:     parseIntWithDefault(values.Get("page"), 1),
		PageSize: parseIntWithDefault(values.Get("pageSize"), 20),
		Query:    strings.TrimSpace(values.Get("query")),
	}
	if rawAuthorID := strings.TrimSpace(values.Get("authorId")); rawAuthorID != "" {
		authorID, err := strconv.ParseInt(rawAuthorID, 10, 64)
		if err != nil || authorID <= 0 {
			return albumListFilter{}, errors.New("authorId must be a positive integer")
		}
		filter.AuthorID = authorID
	}
	if rawPublished := strings.TrimSpace(values.Get("isPublished")); rawPublished != "" {
		parsed, err := strconv.ParseBool(rawPublished)
		if err != nil {
			return albumListFilter{}, errors.New("isPublished must be true or false")
		}
		filter.IsPublished = &parsed
	}
	return filter, nil
}

func parseTrackListFilter(values url.Values) (trackListFilter, error) {
	filter := trackListFilter{
		Page:     parseIntWithDefault(values.Get("page"), 1),
		PageSize: parseIntWithDefault(values.Get("pageSize"), 20),
		Query:    strings.TrimSpace(values.Get("query")),
		Sort:     trackListSortID,
		Order:    sortOrderAsc,
	}
	if rawSort := strings.TrimSpace(values.Get("sort")); rawSort != "" {
		switch rawSort {
		case trackListSortID, trackListSortCreatedAt:
			filter.Sort = rawSort
		default:
			return trackListFilter{}, errors.New("sort must be one of id, createdAt")
		}
	}
	if rawOrder := strings.ToLower(strings.TrimSpace(values.Get("order"))); rawOrder != "" {
		switch rawOrder {
		case sortOrderAsc, sortOrderDesc:
			filter.Order = rawOrder
		default:
			return trackListFilter{}, errors.New("order must be one of asc, desc")
		}
	}
	if rawAuthorID := strings.TrimSpace(values.Get("authorId")); rawAuthorID != "" {
		authorID, err := strconv.ParseInt(rawAuthorID, 10, 64)
		if err != nil || authorID <= 0 {
			return trackListFilter{}, errors.New("authorId must be a positive integer")
		}
		filter.AuthorID = authorID
	}
	if rawAlbumID := strings.TrimSpace(values.Get("albumId")); rawAlbumID != "" {
		albumID, err := strconv.ParseInt(rawAlbumID, 10, 64)
		if err != nil || albumID <= 0 {
			return trackListFilter{}, errors.New("albumId must be a positive integer")
		}
		filter.AlbumID = albumID
	}
	return filter, nil
}

func parseSearchListFilter(values url.Values) (searchListFilter, error) {
	return searchListFilter{
		Page:     parseIntWithDefault(values.Get("page"), 1),
		PageSize: parseIntWithDefault(values.Get("pageSize"), 20),
		Query:    strings.TrimSpace(values.Get("query")),
	}, nil
}

func parseIntWithDefault(raw string, fallback int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func cloneTrack(t track) track {
	t.AuthorIDs = append([]int64(nil), t.AuthorIDs...)
	t.AdditionalInfo = normalizeAdditionalInfo(t.AdditionalInfo)
	t.SourceMetadata = normalizeSourceMetadata(t.SourceMetadata)
	return t
}

func cloneTrackPointer(t *track) *track {
	if t == nil {
		return nil
	}
	cloned := cloneTrack(*t)
	return &cloned
}

func cloneAuthor(a author) author {
	a.Photos = append([]string(nil), a.Photos...)
	return a
}

func clonePlaylist(p playlist) playlist {
	p.TrackItems = normalizePlaylistTrackItems(p.TrackItems)
	return p
}

func appendPlaylistTrack(items []playlistTrack, item playlistTrack) []playlistTrack {
	for _, existing := range items {
		if existing.TrackID == item.TrackID {
			return items
		}
	}
	return append(items, playlistTrack{TrackID: item.TrackID, UnavailableTrack: cloneTrackPointer(item.UnavailableTrack)})
}

func removePlaylistTrack(items []playlistTrack, trackID int64) []playlistTrack {
	filtered := make([]playlistTrack, 0, len(items))
	for _, item := range items {
		if item.TrackID == trackID {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func reorderPlaylistTrackItems(items []playlistTrack, trackIDs []int64) []playlistTrack {
	byID := make(map[int64]playlistTrack, len(items))
	for _, item := range items {
		byID[item.TrackID] = item
	}
	reordered := make([]playlistTrack, 0, len(trackIDs))
	for _, trackID := range trackIDs {
		reordered = append(reordered, byID[trackID])
	}
	return reordered
}

func subtractTrackIDs(left, right []int64) []int64 {
	missing := make([]int64, 0)
	for _, id := range left {
		if !containsInt64(right, id) {
			missing = append(missing, id)
		}
	}
	return missing
}

func toTrackResponse(t track, isFavorite, isAvailable bool) trackResponse {
	return trackResponse{
		ID:             t.ID,
		Name:           t.Name,
		AuthorIDs:      append([]int64(nil), t.AuthorIDs...),
		AlbumID:        t.AlbumID,
		AudioFilePath:  normalizeAudioFilePath(t.AudioFilePath),
		AdditionalInfo: normalizeAdditionalInfo(t.AdditionalInfo),
		SourceMetadata: normalizeSourceMetadata(t.SourceMetadata),
		CreatedAt:      t.CreatedAt,
		IsFavorite:     isFavorite,
		IsDisliked:     false,
		IsAvailable:    isAvailable,
	}
}

func (s *domainState) toTrackResponse(t track, isFavorite, isDisliked, isAvailable bool) trackResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.toTrackResponseLocked(t, isFavorite, isDisliked, isAvailable)
}

func (s *domainState) toTrackResponseLocked(t track, isFavorite, isDisliked, isAvailable bool) trackResponse {
	response := toTrackResponse(t, isFavorite, isAvailable)
	response.IsDisliked = isDisliked
	if albumItem, ok := s.albums[t.AlbumID]; ok {
		response.CoverImagePath = albumItem.CoverImagePath
	}
	return response
}

func (s *domainState) toSearchTrackResponseLocked(t track, isFavorite, isDisliked bool) trackResponse {
	response := s.toTrackResponseLocked(t, isFavorite, isDisliked, true)
	response.Authors = make([]author, 0, len(t.AuthorIDs))
	for _, authorID := range t.AuthorIDs {
		a, ok := s.authors[authorID]
		if !ok {
			continue
		}
		response.Authors = append(response.Authors, cloneAuthor(a))
	}
	return response
}

func searchResultSortKey(item searchResultItem) (string, string, int64) {
	switch item.Type {
	case "author":
		if item.Author != nil {
			return strings.ToLower(item.Author.CurrentName), item.Type, item.Author.ID
		}
	case "album":
		if item.Album != nil {
			return strings.ToLower(item.Album.Title), item.Type, item.Album.ID
		}
	case "track":
		if item.Track != nil {
			return strings.ToLower(item.Track.Name), item.Type, item.Track.ID
		}
	case "playlist":
		if item.Playlist != nil {
			return strings.ToLower(item.Playlist.Name), item.Type, item.Playlist.ID
		}
	}
	return "", item.Type, 0
}

func validateTrack(t track, authors map[int64]author) error {
	switch {
	case t.Name == "":
		return fmt.Errorf("%w: name is required", errInvalidTrack)
	case len(t.AuthorIDs) == 0:
		return fmt.Errorf("%w: at least one authorId is required", errInvalidTrack)
	case t.AudioFilePath == "":
		return fmt.Errorf("%w: audioFilePath is required", errInvalidTrack)
	default:
		for _, authorID := range t.AuthorIDs {
			if _, ok := authors[authorID]; !ok {
				return fmt.Errorf("%w: authorId %d does not exist", errInvalidTrack, authorID)
			}
		}
		return nil
	}
}

func trackReferencesSongFile(audioFilePath, fileName string) bool {
	name, ok := extractReferencedSongFileName(audioFilePath)
	if !ok {
		return false
	}

	return name == fileName
}

func extractReferencedSongFileName(audioFilePath string) (string, bool) {
	audioFilePath = strings.TrimSpace(audioFilePath)
	if audioFilePath == "" {
		return "", false
	}

	parsed, err := url.Parse(audioFilePath)
	if err == nil && parsed.Path != "" {
		audioFilePath = parsed.Path
	}

	name, err := extractMediaFileName(audioFilePath, "/api/songs/")
	if err != nil {
		return "", false
	}

	return name, true
}

func validateAuthor(a author) error {
	switch {
	case a.CurrentName == "":
		return fmt.Errorf("%w: currentName is required", errInvalidAuthor)
	default:
		return nil
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		if reporter, ok := w.(interface {
			reportResponseError(error, string)
		}); ok {
			reporter.reportResponseError(fmt.Errorf("encode JSON response: %w", err), "encode_json_response")
		} else {
			log.Printf("failed to encode JSON response: %s", safeOperationalError(err))
		}
	}
}

func healthzHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, struct {
			Status string `json:"status"`
		}{Status: "ok"})
	}
}

func serveOpenAPIHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		_, _ = io.WriteString(w, openAPISpec)
	}
}

func swaggerUIHandler() http.HandlerFunc {
	const page = `<!doctype html>
<html>
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Esketit Music API Docs</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css" />
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script>
    window.ui = SwaggerUIBundle({
      url: '/api/openapi.yaml',
      dom_id: '#swagger-ui'
    });
  </script>
</body>
</html>`

	tmpl := template.Must(template.New("swagger").Parse(page))
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, nil); err != nil {
			writeSentryInternalError(w, r, fmt.Errorf("render Swagger UI: %w", err), "failed to render docs page", "docs", "swagger.render")
		}
	}
}

func redocHandler() http.HandlerFunc {
	const page = `<!doctype html>
<html>
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Esketit Music API ReDoc</title>
</head>
<body>
  <redoc spec-url="/api/openapi.yaml"></redoc>
  <script src="https://cdn.jsdelivr.net/npm/redoc@2.1.3/bundles/redoc.standalone.js"></script>
</body>
</html>`

	tmpl := template.Must(template.New("redoc").Parse(page))
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, nil); err != nil {
			writeSentryInternalError(w, r, fmt.Errorf("render ReDoc UI: %w", err), "failed to render docs page", "docs", "redoc.render")
		}
	}
}

var errInvalidTrack = errors.New("invalid track payload")
var errHTTPServerPanic = errors.New("HTTP server panicked")
var errRequestBodyTooLarge = errors.New("request body is too large")
var errInvalidAlbum = errors.New("invalid album payload")
var errAlbumNotFound = errors.New("album not found")
var errInvalidAuthor = errors.New("invalid author payload")
var errAuthorNotFound = errors.New("author not found")
var errInvalidLyricsPayload = errors.New("invalid lyrics payload")
var errInvalidPlaylistPayload = errors.New("invalid playlist payload")
var errInvalidAutoplayRequest = errors.New("invalid autoplay request")
var errAutoplaySongStorage = errors.New("failed to inspect local song for autoplay")
var errAlbumInUse = errors.New("album is used by one or more tracks")
var errAuthorInUse = errors.New("author is used by one or more tracks")
var errEmailAlreadyExists = errors.New("user with this email already exists")
var errInvalidCredentials = errors.New("invalid email or password")
var errInvalidRefreshToken = errors.New("invalid refresh token")
var errTrackNotFound = errors.New("track not found")
var errLyricsNotFound = errors.New("lyrics not found")
var errPlaylistNotFound = errors.New("playlist not found")
var errSystemPlaylistImmutable = errors.New("system playlist cannot be modified directly")
