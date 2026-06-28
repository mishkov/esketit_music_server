package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultAccessTokenTTL     = 15 * time.Minute
	defaultRefreshTokenTTL    = 30 * 24 * time.Hour
	passwordHashIterations    = 310000
	passwordHashKeyLength     = 32
	minPasswordLength         = 8
	maxSongUploadSize         = 512 << 20
	maxPlaylistNameLength     = 200
	maxPlaylistDescLength     = 1000
	playlistShareTokenBytes   = 32
	defaultAutoplayCount      = 1
	maxAutoplayCount          = 50
	roleAdmin                 = "admin"
	roleListener              = "listener"
	logModeVerbose            = "verbose"
	logModeErrorOnly          = "error-only"
	playlistVisibilityPrivate = "private"
	playlistVisibilityPublic  = "public"
	playlistVisibilityShared  = "shared"
	playlistKindCustom        = "custom"
	playlistKindFavorites     = "favorites"
	autoplaySourceMyVibe      = "my_vibe"
	autoplaySourcePlaylist    = "playlist"
	autoplaySourceAlbum       = "album"
	autoplaySourceTrack       = "track"
	defaultAutoplayProfile    = "default"
)

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
	IsFavorite     bool             `json:"isFavorite"`
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
	IsFavorites    bool   `json:"isFavorites"`
	ShareToken     string `json:"shareToken,omitempty"`
}

type dbFile struct {
	NextTrackID      int64            `json:"nextTrackId"`
	NextAlbumID      int64            `json:"nextAlbumId"`
	NextAuthorID     int64            `json:"nextAuthorId"`
	NextUserID       int64            `json:"nextUserId"`
	NextPlaylistID   int64            `json:"nextPlaylistId"`
	NextLyricsID     int64            `json:"nextLyricsId"`
	NextLyricsLineID int64            `json:"nextLyricsLineId"`
	Tracks           []track          `json:"tracks"`
	Albums           []album          `json:"albums"`
	Authors          []author         `json:"authors"`
	Users            []user           `json:"users"`
	Sessions         []refreshSession `json:"sessions"`
	Playlists        []playlist       `json:"playlists"`
	Lyrics           []lyrics         `json:"lyrics"`
}

type diskDBFile struct {
	NextTrackID      int64             `json:"nextTrackId"`
	NextAlbumID      int64             `json:"nextAlbumId"`
	NextAuthorID     int64             `json:"nextAuthorId"`
	NextUserID       int64             `json:"nextUserId"`
	NextPlaylistID   int64             `json:"nextPlaylistId"`
	NextLyricsID     int64             `json:"nextLyricsId"`
	NextLyricsLineID int64             `json:"nextLyricsLineId"`
	NextID           int64             `json:"nextId"`
	Tracks           []json.RawMessage `json:"tracks"`
	Albums           []album           `json:"albums"`
	Authors          []author          `json:"authors"`
	Users            []user            `json:"users"`
	Sessions         []refreshSession  `json:"sessions"`
	Playlists        []playlist        `json:"playlists"`
	Lyrics           []lyrics          `json:"lyrics"`
}

type legacyTrack struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	Authors        []string `json:"authors"`
	AlbumImagePath string   `json:"albumImagePath"`
	AudioFilePath  string   `json:"audioFilePath"`
}

type trackStore struct {
	mu               sync.RWMutex
	path             string
	nextTrackID      int64
	nextAlbumID      int64
	nextAuthorID     int64
	nextUserID       int64
	nextPlaylistID   int64
	nextLyricsID     int64
	nextLyricsLineID int64
	tracks           map[int64]track
	albums           map[int64]album
	authors          map[int64]author
	users            map[int64]user
	usersByEmail     map[string]int64
	refreshSession   map[string]refreshSession
	playlists        map[int64]playlist
	lyricsByTrack    map[int64]lyrics
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

type legacyTrackV1 struct {
	ID             int64            `json:"id"`
	Name           string           `json:"name"`
	AuthorIDs      []int64          `json:"authorIds"`
	AlbumImagePath string           `json:"albumImagePath"`
	AudioFilePath  string           `json:"audioFilePath"`
	AdditionalInfo []additionalInfo `json:"additionalInfo"`
	SourceMetadata []sourceMetadata `json:"sourceMetadata"`
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	bytes       int
	body        bytes.Buffer
}

func (w *loggingResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		w.ResponseWriter.WriteHeader(status)
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
	if shouldLogBodyContentType(w.Header().Get("Content-Type")) {
		_, _ = w.body.Write(p[:n])
	}
	return n, err
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
	if err := loadDotEnv(".env"); err != nil {
		log.Fatalf("failed to load .env: %v", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("cannot resolve home directory: %v", err)
	}

	defaultSongsDir := filepath.Join(home, "Projects", "esketit_music", "media_storage", "songs")
	songsDir := os.Getenv("SONGS_DIR")
	if songsDir == "" {
		songsDir = defaultSongsDir
	}

	if err := ensureDir(songsDir); err != nil {
		log.Fatalf("invalid songs directory %q: %v", songsDir, err)
	}

	defaultAlbumCoversDir := filepath.Join(home, "Projects", "esketit_music", "media_storage", "album_covers")
	albumCoversDir := os.Getenv("ALBUM_COVERS_DIR")
	if albumCoversDir == "" {
		albumCoversDir = defaultAlbumCoversDir
	}

	if err := ensureDirOrCreate(albumCoversDir); err != nil {
		log.Fatalf("invalid album covers directory %q: %v", albumCoversDir, err)
	}

	telegramStateDir := os.Getenv("TELEGRAM_STATE_DIR")
	if telegramStateDir == "" {
		telegramStateDir = "telegram_state"
	}
	if err := ensureDirOrCreate(telegramStateDir); err != nil {
		log.Fatalf("invalid telegram state directory %q: %v", telegramStateDir, err)
	}

	telegramImportTempDir := os.Getenv("TELEGRAM_IMPORT_TEMP_DIR")
	if telegramImportTempDir == "" {
		telegramImportTempDir = "telegram_import_tmp"
	}
	if err := ensureDirOrCreate(telegramImportTempDir); err != nil {
		log.Fatalf("invalid telegram import temp directory %q: %v", telegramImportTempDir, err)
	}

	youtubeImportTempDir := os.Getenv("YOUTUBE_IMPORT_TEMP_DIR")
	if youtubeImportTempDir == "" {
		youtubeImportTempDir = "youtube_import_tmp"
	}
	if err := ensureDirOrCreate(youtubeImportTempDir); err != nil {
		log.Fatalf("invalid youtube import temp directory %q: %v", youtubeImportTempDir, err)
	}

	youtubeCookiesFile := os.Getenv("YOUTUBE_COOKIES_FILE")
	if youtubeCookiesFile == "" {
		youtubeCookiesFile = "youtube_cookies.txt"
	}
	youtubeCookiesDir := filepath.Dir(youtubeCookiesFile)
	if youtubeCookiesDir != "." {
		if err := ensureDirOrCreate(youtubeCookiesDir); err != nil {
			log.Fatalf("invalid youtube cookies directory %q: %v", youtubeCookiesDir, err)
		}
	}
	ytdlpBinary := strings.TrimSpace(os.Getenv("YTDLP_BINARY"))
	if ytdlpBinary == "" {
		ytdlpBinary = "yt-dlp"
	}
	ffmpegBinary := strings.TrimSpace(os.Getenv("FFMPEG_BINARY"))
	if ffmpegBinary == "" {
		ffmpegBinary = "ffmpeg"
	}

	tracksDBPath := os.Getenv("TRACKS_DB_PATH")
	if tracksDBPath == "" {
		tracksDBPath = "tracks_db.json"
	}

	authSecret := os.Getenv("AUTH_SECRET")
	if len(authSecret) < 32 {
		log.Fatal("AUTH_SECRET must be set and contain at least 32 characters")
	}

	store, err := newTrackStore(tracksDBPath)
	if err != nil {
		log.Fatalf("failed to initialize tracks database at %q: %v", tracksDBPath, err)
	}

	auth := newAuthManager([]byte(authSecret), defaultAccessTokenTTL, defaultRefreshTokenTTL)
	logMode := resolveLogMode(os.Getenv("LOG_MODE"))
	albumCoverService := newAlbumCoverServiceFromEnv(albumCoversDir)
	telegramConfig, err := loadTelegramConfig(telegramStateDir, telegramImportTempDir)
	if err != nil {
		log.Fatalf("failed to initialize telegram configuration: %v", err)
	}
	telegramGateway := newGotdTelegramGateway(telegramConfig)
	telegramImport := newTelegramImportService(telegramConfig, telegramGateway, store, songsDir)
	youtubeCookieStore := newYouTubeCookieStore(youtubeCookiesFile)
	youtubeImportConfig := youtubeImportConfig{
		ImportTempDir:   youtubeImportTempDir,
		RequestTimeout:  2 * time.Minute,
		DownloadTimeout: 15 * time.Minute,
		YTDLPBinary:     ytdlpBinary,
		FFmpegBinary:    ffmpegBinary,
	}
	youtubeImportGateway := newLiveYouTubeImportGateway(youtubeImportConfig, youtubeCookieStore)
	youtubeImport := newYouTubeImportService(youtubeImportConfig, youtubeImportGateway, store, songsDir)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/songs", listSongsHandler(songsDir))
	mux.Handle("POST /api/songs", requireRole(auth, store, roleAdmin, uploadSongHandler(songsDir)))
	mux.Handle("GET /api/songs/unused", requireRole(auth, store, roleAdmin, listUnusedSongsHandler(store, songsDir)))
	mux.HandleFunc("GET /api/songs/", getSongHandler(songsDir))
	mux.Handle("DELETE /api/songs/", requireRole(auth, store, roleAdmin, deleteSongHandler(store, songsDir)))
	mux.HandleFunc("GET /api/album-covers/", getAlbumCoverHandler(albumCoversDir))
	mux.HandleFunc("GET /api/albums", listAlbumsHandler(store, auth))
	mux.HandleFunc("GET /api/search", searchHandler(store, auth))
	mux.Handle("POST /api/albums", requireRole(auth, store, roleAdmin, createAlbumHandler(store)))
	mux.HandleFunc("GET /api/albums/", getAlbumByRouteHandler(store))
	mux.Handle("PUT /api/albums/", requireRole(auth, store, roleAdmin, updateAlbumByRouteHandler(store)))
	mux.Handle("DELETE /api/albums/", requireRole(auth, store, roleAdmin, deleteAlbumByRouteHandler(store)))
	mux.Handle("POST /api/album-covers", requireRole(auth, store, roleAdmin, uploadAlbumCoverHandler(albumCoversDir)))
	mux.Handle("GET /api/album-covers/suggestions", requireRole(auth, store, roleAdmin, albumCoverSuggestionsHandler(albumCoverService)))
	mux.Handle("POST /api/album-covers/import", requireRole(auth, store, roleAdmin, importAlbumCoverHandler(albumCoverService)))
	mux.Handle("GET /api/playlists", requireAuth(auth, store, listPlaylistsHandler(store)))
	mux.Handle("POST /api/playlists", requireAuth(auth, store, createPlaylistHandler(store)))
	mux.Handle("GET /api/playlists/", requireAuth(auth, store, getPlaylistByRouteHandler(store)))
	mux.Handle("PUT /api/playlists/", requireAuth(auth, store, updatePlaylistByRouteHandler(store)))
	mux.Handle("DELETE /api/playlists/", requireAuth(auth, store, deletePlaylistByRouteHandler(store)))
	mux.HandleFunc("GET /api/public/playlists/", getPublicPlaylistByRouteHandler(store))
	mux.HandleFunc("GET /api/shared/playlists/", getSharedPlaylistByRouteHandler(store))
	mux.Handle("POST /api/autoplay/next", requireAuth(auth, store, autoplayNextHandler(store)))
	mux.HandleFunc("GET /api/tracks", listTracksHandler(store, auth))
	mux.Handle("POST /api/tracks", requireRole(auth, store, roleAdmin, createTrackHandler(store)))
	mux.Handle("GET /api/tracks/", getTrackByRouteHandler(store, auth))
	mux.Handle("POST /api/tracks/", postTrackByRouteHandler(store, auth))
	mux.Handle("PUT /api/tracks/", putTrackByRouteHandler(store, auth))
	mux.Handle("DELETE /api/tracks/", deleteTrackByRouteHandler(store, auth))
	mux.HandleFunc("GET /api/authors", listAuthorsHandler(store))
	mux.Handle("POST /api/authors", requireRole(auth, store, roleAdmin, createAuthorHandler(store)))
	mux.HandleFunc("GET /api/authors/", getAuthorByIDHandler(store))
	mux.Handle("PUT /api/authors/", requireRole(auth, store, roleAdmin, updateAuthorHandler(store)))
	mux.Handle("DELETE /api/authors/", requireRole(auth, store, roleAdmin, deleteAuthorHandler(store)))
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
	mux.HandleFunc("GET /api/openapi.yaml", serveOpenAPIHandler("openapi.yaml"))
	mux.HandleFunc("GET /api/docs", swaggerUIHandler())
	mux.HandleFunc("GET /api/redoc", redocHandler())

	addr := ":8080"
	log.Printf("server listening on %s", addr)
	log.Printf("serving songs from %s", songsDir)
	log.Printf("serving album covers from %s", albumCoversDir)
	log.Printf("using tracks database %s", tracksDBPath)
	log.Printf("telegram state directory %s", telegramStateDir)
	log.Printf("telegram import temp directory %s", telegramImportTempDir)
	log.Printf("youtube import temp directory %s", youtubeImportTempDir)
	log.Printf("youtube cookies file %s", youtubeCookiesFile)
	log.Printf("yt-dlp binary %s", ytdlpBinary)
	log.Printf("ffmpeg binary %s", ffmpegBinary)
	log.Printf("http logging mode %s", logMode)
	log.Printf("swagger docs available at http://localhost%s/api/docs", addr)
	log.Printf("redoc available at http://localhost%s/api/redoc", addr)
	handler := withRequestLogging(withRecovery(withCORS(mux)), logMode)
	log.Fatal(http.ListenAndServe(addr, handler))
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
				log.Printf(
					"panic method=%s path=%s remote=%s err=%v\n%s",
					r.Method,
					r.URL.RequestURI(),
					r.RemoteAddr,
					rec,
					debug.Stack(),
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()

		next.ServeHTTP(w, r)
	})
}

func withRequestLogging(next http.Handler, mode string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestBody, restoreBody, err := captureRequestBody(r)
		if err != nil {
			log.Printf("failed to capture request body method=%s path=%s err=%v", r.Method, r.URL.RequestURI(), err)
			http.Error(w, "failed to read request body", http.StatusInternalServerError)
			return
		}
		if restoreBody != nil {
			defer restoreBody()
		}

		lw := &loggingResponseWriter{
			ResponseWriter: w,
			status:         http.StatusOK,
		}

		next.ServeHTTP(lw, r)

		if mode == logModeErrorOnly && lw.status < http.StatusInternalServerError {
			return
		}

		log.Printf(
			"http request method=%s path=%s status=%d duration=%s request_bytes=%d request_body=%q response_bytes=%d response_body=%q remote=%s user_agent=%q",
			r.Method,
			r.URL.RequestURI(),
			lw.status,
			time.Since(start).Round(time.Millisecond),
			r.ContentLength,
			requestBody,
			lw.bytes,
			responseBodyForLog(lw),
			r.RemoteAddr,
			r.UserAgent(),
		)
	})
}

func captureRequestBody(r *http.Request) (string, func(), error) {
	if r.Body == nil {
		return "", nil, nil
	}

	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "multipart/form-data") {
		return fmt.Sprintf("[multipart body omitted content_type=%q size=%d]", contentType, r.ContentLength), nil, nil
	}
	if !shouldLogBodyContentType(contentType) {
		return fmt.Sprintf("[body omitted content_type=%q size=%d]", contentType, r.ContentLength), nil, nil
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", nil, err
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	return string(body), func() {
		r.Body = io.NopCloser(bytes.NewReader(body))
	}, nil
}

func responseBodyForLog(w *loggingResponseWriter) string {
	contentType := w.Header().Get("Content-Type")
	if !shouldLogBodyContentType(contentType) {
		return fmt.Sprintf("[body omitted content_type=%q size=%d]", contentType, w.bytes)
	}
	return w.body.String()
}

func shouldLogBodyContentType(contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	if contentType == "" {
		return true
	}
	if strings.HasPrefix(contentType, "text/") {
		return true
	}
	if strings.Contains(contentType, "json") || strings.Contains(contentType, "xml") || strings.Contains(contentType, "yaml") || strings.Contains(contentType, "form-urlencoded") {
		return true
	}
	return false
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

func loadDotEnv(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("invalid line %q", line)
		}

		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("invalid line %q", line)
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}

		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}

	return scanner.Err()
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
	s := &trackStore{
		path:             path,
		nextTrackID:      1,
		nextAlbumID:      1,
		nextAuthorID:     1,
		nextUserID:       1,
		nextPlaylistID:   1,
		nextLyricsID:     1,
		nextLyricsLineID: 1,
		tracks:           make(map[int64]track),
		albums:           make(map[int64]album),
		authors:          make(map[int64]author),
		users:            make(map[int64]user),
		usersByEmail:     make(map[string]int64),
		refreshSession:   make(map[string]refreshSession),
		playlists:        make(map[int64]playlist),
		lyricsByTrack:    make(map[int64]lyrics),
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if err := s.persistLocked(); err != nil {
				return nil, err
			}
			return s, nil
		}
		return nil, err
	}

	var file diskDBFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("invalid tracks db format: %w", err)
	}

	for _, a := range file.Authors {
		a.CurrentName = strings.TrimSpace(a.CurrentName)
		a.Photos = normalizePhotos(a.Photos)
		if a.ID <= 0 || a.CurrentName == "" {
			continue
		}
		s.authors[a.ID] = a
		if a.ID >= s.nextAuthorID {
			s.nextAuthorID = a.ID + 1
		}
	}

	for _, albumItem := range file.Albums {
		albumItem.Title = strings.TrimSpace(albumItem.Title)
		albumItem.CoverImagePath = strings.TrimSpace(albumItem.CoverImagePath)
		albumItem.AuthorIDs = normalizeAuthorIDs(albumItem.AuthorIDs)
		albumItem.TrackIDs = normalizeTrackIDs(albumItem.TrackIDs)
		albumItem.AdditionalInfo = normalizeAdditionalInfo(albumItem.AdditionalInfo)
		if albumItem.ID <= 0 {
			continue
		}
		s.albums[albumItem.ID] = albumItem
		if albumItem.ID >= s.nextAlbumID {
			s.nextAlbumID = albumItem.ID + 1
		}
	}

	for _, rawTrack := range file.Tracks {
		var t track
		if err := json.Unmarshal(rawTrack, &t); err == nil {
			t.Name = strings.TrimSpace(t.Name)
			t.AuthorIDs = normalizeAuthorIDs(t.AuthorIDs)
			t.AudioFilePath = normalizeAudioFilePath(t.AudioFilePath)
			t.AdditionalInfo = normalizeAdditionalInfo(t.AdditionalInfo)
			t.SourceMetadata = normalizeSourceMetadata(t.SourceMetadata)
			if t.ID <= 0 || t.AlbumID <= 0 {
				continue
			}
			s.tracks[t.ID] = t
			if t.ID >= s.nextTrackID {
				s.nextTrackID = t.ID + 1
			}
			continue
		}

		var previous legacyTrackV1
		if err := json.Unmarshal(rawTrack, &previous); err == nil && previous.ID > 0 {
			t = track{
				ID:             previous.ID,
				Name:           strings.TrimSpace(previous.Name),
				AuthorIDs:      normalizeAuthorIDs(previous.AuthorIDs),
				AudioFilePath:  normalizeAudioFilePath(previous.AudioFilePath),
				AdditionalInfo: normalizeAdditionalInfo(previous.AdditionalInfo),
				SourceMetadata: normalizeSourceMetadata(previous.SourceMetadata),
			}
			s.tracks[t.ID] = t
			if t.ID >= s.nextTrackID {
				s.nextTrackID = t.ID + 1
			}
			continue
		}

		var legacy legacyTrack
		if err := json.Unmarshal(rawTrack, &legacy); err != nil {
			continue
		}
		if legacy.ID <= 0 {
			continue
		}

		authorIDs := make([]int64, 0, len(legacy.Authors))
		for _, name := range normalizeNames(legacy.Authors) {
			authorIDs = append(authorIDs, s.getOrCreateAuthorIDLocked(name))
		}

		t = track{
			ID:            legacy.ID,
			Name:          strings.TrimSpace(legacy.Name),
			AuthorIDs:     normalizeAuthorIDs(authorIDs),
			AudioFilePath: normalizeAudioFilePath(legacy.AudioFilePath),
		}
		s.tracks[t.ID] = t
		if t.ID >= s.nextTrackID {
			s.nextTrackID = t.ID + 1
		}
	}

	for _, u := range file.Users {
		u.Email = normalizeEmail(u.Email)
		u.Role = normalizeRole(u.Role)
		if u.ID <= 0 || u.Email == "" || u.PasswordHash == "" || u.Role == "" {
			continue
		}
		s.users[u.ID] = u
		s.usersByEmail[u.Email] = u.ID
		if u.ID >= s.nextUserID {
			s.nextUserID = u.ID + 1
		}
	}

	for _, playlistItem := range file.Playlists {
		playlistItem.Name = strings.TrimSpace(playlistItem.Name)
		playlistItem.Description = strings.TrimSpace(playlistItem.Description)
		playlistItem.CoverImagePath = strings.TrimSpace(playlistItem.CoverImagePath)
		playlistItem.Visibility = normalizePlaylistVisibility(playlistItem.Visibility)
		playlistItem.ShareToken = strings.TrimSpace(playlistItem.ShareToken)
		playlistItem.Kind = normalizePlaylistKind(playlistItem.Kind, playlistItem.System)
		playlistItem.TrackItems = normalizePlaylistTrackItems(playlistItem.TrackItems)
		if playlistItem.ID <= 0 || playlistItem.UserID <= 0 || playlistItem.Name == "" || playlistItem.Visibility == "" || playlistItem.Kind == "" {
			continue
		}
		if err := s.normalizePlaylistSharingLocked(&playlistItem); err != nil {
			return nil, err
		}
		s.playlists[playlistItem.ID] = playlistItem
		if playlistItem.ID >= s.nextPlaylistID {
			s.nextPlaylistID = playlistItem.ID + 1
		}
	}

	for _, lyricsItem := range file.Lyrics {
		lyricsItem, ok := normalizeLyrics(lyricsItem)
		if !ok {
			continue
		}
		if _, exists := s.tracks[lyricsItem.TrackID]; !exists {
			continue
		}
		s.lyricsByTrack[lyricsItem.TrackID] = lyricsItem
		if lyricsItem.ID >= s.nextLyricsID {
			s.nextLyricsID = lyricsItem.ID + 1
		}
		for _, line := range lyricsItem.Lines {
			if line.ID >= s.nextLyricsLineID {
				s.nextLyricsLineID = line.ID + 1
			}
		}
	}

	now := time.Now()
	for _, session := range file.Sessions {
		if session.ID == "" || session.UserID <= 0 || session.TokenHash == "" {
			continue
		}
		if session.ExpiresAt.Before(now) {
			continue
		}
		if _, ok := s.users[session.UserID]; !ok {
			continue
		}
		s.refreshSession[session.ID] = session
	}

	if file.NextTrackID > s.nextTrackID {
		s.nextTrackID = file.NextTrackID
	}
	if file.NextAlbumID > s.nextAlbumID {
		s.nextAlbumID = file.NextAlbumID
	}
	if file.NextID > s.nextTrackID {
		s.nextTrackID = file.NextID
	}
	if file.NextAuthorID > s.nextAuthorID {
		s.nextAuthorID = file.NextAuthorID
	}
	if file.NextUserID > s.nextUserID {
		s.nextUserID = file.NextUserID
	}
	if file.NextPlaylistID > s.nextPlaylistID {
		s.nextPlaylistID = file.NextPlaylistID
	}
	if file.NextLyricsID > s.nextLyricsID {
		s.nextLyricsID = file.NextLyricsID
	}
	if file.NextLyricsLineID > s.nextLyricsLineID {
		s.nextLyricsLineID = file.NextLyricsLineID
	}
	if s.nextTrackID < 1 {
		s.nextTrackID = 1
	}
	if s.nextAlbumID < 1 {
		s.nextAlbumID = 1
	}
	if s.nextAuthorID < 1 {
		s.nextAuthorID = 1
	}
	if s.nextUserID < 1 {
		s.nextUserID = 1
	}
	if s.nextPlaylistID < 1 {
		s.nextPlaylistID = 1
	}
	if s.nextLyricsID < 1 {
		s.nextLyricsID = 1
	}
	if s.nextLyricsLineID < 1 {
		s.nextLyricsLineID = 1
	}

	if err := s.migrateLegacyAlbumsLocked(); err != nil {
		return nil, err
	}
	if err := s.rebuildAlbumDerivedDataLocked(); err != nil {
		return nil, err
	}
	if err := s.ensureFavoritesPlaylistsLocked(); err != nil {
		return nil, err
	}
	if err := s.validateLyricsStateLocked(); err != nil {
		return nil, err
	}
	if err := s.persistLocked(); err != nil {
		return nil, err
	}

	return s, nil
}

func (s *trackStore) list() []track {
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

func (s *trackStore) listAlbums(filter albumListFilter) paginatedAlbums {
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

func (s *trackStore) getAlbum(id int64) (album, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.albums[id]
	return a, ok
}

func (s *trackStore) getAlbumTracks(id int64) ([]track, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	a, ok := s.albums[id]
	if !ok {
		return nil, false
	}

	tracks := make([]track, 0, len(a.TrackIDs))
	for _, trackID := range a.TrackIDs {
		t, ok := s.tracks[trackID]
		if !ok {
			continue
		}
		tracks = append(tracks, t)
	}
	return tracks, true
}

func (s *trackStore) get(id int64) (track, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tracks[id]
	return t, ok
}

func (s *trackStore) createAlbum(req upsertAlbumRequest) (album, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	albumsSnapshot := cloneAlbumsMap(s.albums)
	tracksSnapshot := cloneTracksMap(s.tracks)
	nextAlbumIDSnapshot := s.nextAlbumID

	a := album{
		ID:             s.nextAlbumID,
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

	s.nextAlbumID++
	s.albums[a.ID] = a
	if err := s.applyAlbumTrackIDsLocked(a.ID, a.TrackIDs, false); err != nil {
		s.albums = albumsSnapshot
		s.tracks = tracksSnapshot
		s.nextAlbumID = nextAlbumIDSnapshot
		return album{}, err
	}
	if err := s.rebuildAlbumDerivedDataLocked(); err != nil {
		s.albums = albumsSnapshot
		s.tracks = tracksSnapshot
		s.nextAlbumID = nextAlbumIDSnapshot
		return album{}, err
	}
	if err := s.persistLocked(); err != nil {
		s.albums = albumsSnapshot
		s.tracks = tracksSnapshot
		s.nextAlbumID = nextAlbumIDSnapshot
		return album{}, err
	}
	return s.albums[a.ID], nil
}

func (s *trackStore) updateAlbum(id int64, req upsertAlbumRequest) (album, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.albums[id]
	if !ok {
		return album{}, false, nil
	}
	albumsSnapshot := cloneAlbumsMap(s.albums)
	tracksSnapshot := cloneTracksMap(s.tracks)

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

	s.albums[id] = updated
	if err := s.applyAlbumTrackIDsLocked(id, updated.TrackIDs, false); err != nil {
		s.albums = albumsSnapshot
		s.tracks = tracksSnapshot
		return album{}, true, err
	}
	if err := s.rebuildAlbumDerivedDataLocked(); err != nil {
		s.albums = albumsSnapshot
		s.tracks = tracksSnapshot
		return album{}, true, err
	}
	if err := s.persistLocked(); err != nil {
		s.albums = albumsSnapshot
		s.tracks = tracksSnapshot
		return album{}, true, err
	}
	return s.albums[id], true, nil
}

func (s *trackStore) deleteAlbum(id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.albums[id]
	if !ok {
		return false, nil
	}
	if len(a.TrackIDs) > 0 {
		return false, errAlbumInUse
	}
	delete(s.albums, id)
	if err := s.persistLocked(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *trackStore) create(req upsertTrackRequest) (track, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	albumsSnapshot := cloneAlbumsMap(s.albums)
	tracksSnapshot := cloneTracksMap(s.tracks)
	nextTrackIDSnapshot := s.nextTrackID

	if _, ok := s.albums[req.AlbumID]; !ok {
		return track{}, fmt.Errorf("%w: albumId %d does not exist", errInvalidTrack, req.AlbumID)
	}

	t := track{
		ID:             s.nextTrackID,
		Name:           strings.TrimSpace(req.Name),
		AuthorIDs:      normalizeAuthorIDs(req.AuthorIDs),
		AlbumID:        req.AlbumID,
		AudioFilePath:  normalizeAudioFilePath(req.AudioFilePath),
		AdditionalInfo: normalizeAdditionalInfo(req.AdditionalInfo),
		SourceMetadata: normalizeSourceMetadata(req.SourceMetadata),
	}
	if err := s.validateTrackLocked(t); err != nil {
		return track{}, err
	}

	albumOrder, err := normalizeAlbumOrder(req.AlbumOrder, len(s.albums[req.AlbumID].TrackIDs), true)
	if err != nil {
		return track{}, fmt.Errorf("%w: %v", errInvalidTrack, err)
	}

	s.nextTrackID++
	s.tracks[t.ID] = t
	targetAlbum := s.albums[req.AlbumID]
	insertTrackIntoAlbumLocked(&targetAlbum, t.ID, albumOrder)
	s.albums[req.AlbumID] = targetAlbum
	if err := s.rebuildAlbumDerivedDataLocked(); err != nil {
		s.albums = albumsSnapshot
		s.tracks = tracksSnapshot
		s.nextTrackID = nextTrackIDSnapshot
		return track{}, err
	}
	if err := s.persistLocked(); err != nil {
		s.albums = albumsSnapshot
		s.tracks = tracksSnapshot
		s.nextTrackID = nextTrackIDSnapshot
		return track{}, err
	}
	return s.tracks[t.ID], nil
}

func (s *trackStore) update(id int64, req upsertTrackRequest) (track, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	albumsSnapshot := cloneAlbumsMap(s.albums)
	tracksSnapshot := cloneTracksMap(s.tracks)

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
		s.albums = albumsSnapshot
		s.tracks = tracksSnapshot
		return track{}, true, err
	}
	if err := s.persistLocked(); err != nil {
		s.albums = albumsSnapshot
		s.tracks = tracksSnapshot
		return track{}, true, err
	}
	return s.tracks[id], true, nil
}

func (s *trackStore) delete(id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	albumsSnapshot := cloneAlbumsMap(s.albums)
	tracksSnapshot := cloneTracksMap(s.tracks)
	playlistsSnapshot := clonePlaylistsMap(s.playlists)
	lyricsSnapshot := cloneLyricsMap(s.lyricsByTrack)

	t, ok := s.tracks[id]
	if !ok {
		return false, nil
	}
	delete(s.tracks, id)
	delete(s.lyricsByTrack, id)
	if albumItem, ok := s.albums[t.AlbumID]; ok {
		removeTrackFromAlbumLocked(&albumItem, id)
		s.albums[t.AlbumID] = albumItem
	}
	s.markTrackUnavailableLocked(t)
	if err := s.rebuildAlbumDerivedDataLocked(); err != nil {
		s.albums = albumsSnapshot
		s.tracks = tracksSnapshot
		s.playlists = playlistsSnapshot
		s.lyricsByTrack = lyricsSnapshot
		return false, err
	}
	if err := s.persistLocked(); err != nil {
		s.albums = albumsSnapshot
		s.tracks = tracksSnapshot
		s.playlists = playlistsSnapshot
		s.lyricsByTrack = lyricsSnapshot
		return false, err
	}
	return true, nil
}

func (s *trackStore) listPlaylists(userID int64, filter playlistListFilter) paginatedPlaylists {
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
		if items[i].IsFavorites != items[j].IsFavorites {
			return items[i].IsFavorites
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

func (s *trackStore) getPlaylist(userID, playlistID int64) (playlistResponse, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	p, ok := s.playlists[playlistID]
	if !ok || p.UserID != userID {
		return playlistResponse{}, false
	}
	return s.buildPlaylistResponseLocked(p), true
}

func (s *trackStore) createPlaylist(userID int64, req upsertPlaylistRequest) (playlistResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.users[userID]; !ok {
		return playlistResponse{}, errPlaylistNotFound
	}

	p := playlist{
		ID:             s.nextPlaylistID,
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

	s.nextPlaylistID++
	s.playlists[p.ID] = p
	if err := s.persistLocked(); err != nil {
		delete(s.playlists, p.ID)
		s.nextPlaylistID--
		return playlistResponse{}, err
	}
	return s.buildPlaylistResponseLocked(p), nil
}

func (s *trackStore) updatePlaylist(userID, playlistID int64, req upsertPlaylistRequest) (playlistResponse, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.playlists[playlistID]
	if !ok || current.UserID != userID {
		return playlistResponse{}, false, nil
	}
	if current.Kind == playlistKindFavorites {
		return playlistResponse{}, true, errFavoritePlaylistImmutable
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
	if err := s.persistLocked(); err != nil {
		s.playlists[playlistID] = current
		return playlistResponse{}, true, err
	}
	return s.buildPlaylistResponseLocked(updated), true, nil
}

func (s *trackStore) deletePlaylist(userID, playlistID int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.playlists[playlistID]
	if !ok || current.UserID != userID {
		return false, nil
	}
	if current.Kind == playlistKindFavorites {
		return false, errFavoritePlaylistImmutable
	}
	delete(s.playlists, playlistID)
	if err := s.persistLocked(); err != nil {
		s.playlists[playlistID] = current
		return false, err
	}
	return true, nil
}

func (s *trackStore) getPlaylistTracks(userID, playlistID int64, page, pageSize int64) (paginatedTracks, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	p, ok := s.playlists[playlistID]
	if !ok || p.UserID != userID {
		return paginatedTracks{}, false
	}

	items := make([]trackResponse, 0, len(p.TrackItems))
	favoriteIDs := s.favoriteTrackSetLocked(userID)
	for _, item := range p.TrackItems {
		items = append(items, s.buildPlaylistTrackResponseLocked(item, favoriteIDs))
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

func (s *trackStore) getPublicPlaylist(playlistID int64) (playlistResponse, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	p, ok := s.playlists[playlistID]
	if !ok || p.Visibility != playlistVisibilityPublic {
		return playlistResponse{}, false
	}
	return publicPlaylistResponse(s.buildPlaylistResponseLocked(p)), true
}

func (s *trackStore) getPublicPlaylistTracks(playlistID int64, page, pageSize int64) (paginatedTracks, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	p, ok := s.playlists[playlistID]
	if !ok || p.Visibility != playlistVisibilityPublic {
		return paginatedTracks{}, false
	}
	return s.buildPublicPlaylistTracksPageLocked(p, page, pageSize), true
}

func (s *trackStore) getSharedPlaylist(shareToken string) (playlistResponse, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	p, ok := s.findSharedPlaylistByTokenLocked(shareToken)
	if !ok {
		return playlistResponse{}, false
	}
	return publicPlaylistResponse(s.buildPlaylistResponseLocked(p)), true
}

func (s *trackStore) getSharedPlaylistTracks(shareToken string, page, pageSize int64) (paginatedTracks, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	p, ok := s.findSharedPlaylistByTokenLocked(shareToken)
	if !ok {
		return paginatedTracks{}, false
	}
	return s.buildPublicPlaylistTracksPageLocked(p, page, pageSize), true
}

func (s *trackStore) buildPublicPlaylistTracksPageLocked(p playlist, page, pageSize int64) paginatedTracks {
	items := make([]trackResponse, 0, len(p.TrackItems))
	favoriteIDs := map[int64]struct{}{}
	for _, item := range p.TrackItems {
		items = append(items, s.buildPlaylistTrackResponseLocked(item, favoriteIDs))
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

func (s *trackStore) addTrackToPlaylists(userID, trackID int64, playlistIDs []int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tracks[trackID]; !ok {
		return errTrackNotFound
	}
	if len(playlistIDs) == 0 {
		return fmt.Errorf("%w: playlistIds are required", errInvalidPlaylistPayload)
	}

	playlistsSnapshot := clonePlaylistsMap(s.playlists)
	for _, playlistID := range normalizeTrackIDs(playlistIDs) {
		p, ok := s.playlists[playlistID]
		if !ok || p.UserID != userID {
			return errPlaylistNotFound
		}
		p.TrackItems = appendPlaylistTrack(p.TrackItems, playlistTrack{TrackID: trackID})
		s.playlists[playlistID] = p
	}
	if err := s.persistLocked(); err != nil {
		s.playlists = playlistsSnapshot
		return err
	}
	return nil
}

func (s *trackStore) removeTrackFromPlaylist(userID, playlistID, trackID int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.playlists[playlistID]
	if !ok || p.UserID != userID {
		return false, nil
	}
	updated := removePlaylistTrack(p.TrackItems, trackID)
	if len(updated) == len(p.TrackItems) {
		return false, nil
	}
	p.TrackItems = updated
	s.playlists[playlistID] = p
	if err := s.persistLocked(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *trackStore) setFavoriteTrack(userID, trackID int64, favorite bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tracks[trackID]; !ok {
		return errTrackNotFound
	}

	p, ok := s.findFavoritesPlaylistLocked(userID)
	if !ok {
		return errPlaylistNotFound
	}
	if favorite {
		p.TrackItems = appendPlaylistTrack(p.TrackItems, playlistTrack{TrackID: trackID})
	} else {
		p.TrackItems = removePlaylistTrack(p.TrackItems, trackID)
	}
	s.playlists[p.ID] = p
	return s.persistLocked()
}

func (s *trackStore) nextAutoplayTracks(userID int64, req autoplayNextRequest) (autoplayNextResponse, error) {
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
	default:
		return autoplayNextResponse{}, fmt.Errorf("%w: unsupported sourceType", errInvalidAutoplayRequest)
	}

	excluded := make(map[int64]struct{}, len(req.RecentTrackIDs)+len(req.ExcludedTrackIDs))
	for _, trackID := range req.RecentTrackIDs {
		excluded[trackID] = struct{}{}
	}
	for _, trackID := range req.ExcludedTrackIDs {
		excluded[trackID] = struct{}{}
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
		chosen = append(chosen, toTrackResponse(selected, isFavorite, true))
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

func (s *trackStore) reorderPlaylistTracks(userID, playlistID int64, trackIDs []int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.playlists[playlistID]
	if !ok || p.UserID != userID {
		return false, nil
	}
	if err := validatePlaylistTrackOrder(trackIDs, p.TrackItems); err != nil {
		return true, err
	}
	p.TrackItems = reorderPlaylistTrackItems(p.TrackItems, trackIDs)
	s.playlists[playlistID] = p
	if err := s.persistLocked(); err != nil {
		return true, err
	}
	return true, nil
}

func (s *trackStore) listAuthors() []author {
	s.mu.RLock()
	defer s.mu.RUnlock()

	items := make([]author, 0, len(s.authors))
	for _, a := range s.authors {
		items = append(items, a)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].ID < items[j].ID
	})
	return items
}

func (s *trackStore) search(userID int64, filter searchListFilter) paginatedSearchResults {
	s.mu.RLock()
	defer s.mu.RUnlock()

	items := make([]searchResultItem, 0, len(s.authors)+len(s.albums)+len(s.tracks)+len(s.playlists))
	query := strings.ToLower(strings.TrimSpace(filter.Query))
	favoriteIDs := s.favoriteTrackSetLocked(userID)

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
		trackCopy := s.toSearchTrackResponseLocked(t, isFavorite)
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

func (s *trackStore) getAuthor(id int64) (author, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.authors[id]
	return a, ok
}

func (s *trackStore) createAuthor(req upsertAuthorRequest) (author, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	a := author{
		ID:          s.nextAuthorID,
		CurrentName: strings.TrimSpace(req.CurrentName),
		Photos:      normalizePhotos(req.Photos),
	}
	if err := validateAuthor(a); err != nil {
		return author{}, err
	}

	s.nextAuthorID++
	s.authors[a.ID] = a
	if err := s.persistLocked(); err != nil {
		delete(s.authors, a.ID)
		s.nextAuthorID--
		return author{}, err
	}
	return a, nil
}

func (s *trackStore) updateAuthor(id int64, req upsertAuthorRequest) (author, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.authors[id]; !ok {
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
	if err := s.persistLocked(); err != nil {
		return author{}, true, err
	}
	return a, true, nil
}

func (s *trackStore) deleteAuthor(id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.authors[id]; !ok {
		return false, nil
	}
	for _, t := range s.tracks {
		for _, authorID := range t.AuthorIDs {
			if authorID == id {
				return false, errAuthorInUse
			}
		}
	}

	delete(s.authors, id)
	if err := s.persistLocked(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *trackStore) createUser(email, passwordHash string) (user, error) {
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

	u := user{
		ID:           s.nextUserID,
		Email:        email,
		Role:         role,
		PasswordHash: passwordHash,
		CreatedAt:    time.Now().UTC(),
	}

	s.nextUserID++
	s.users[u.ID] = u
	s.usersByEmail[email] = u.ID
	favoritesPlaylist := s.newFavoritesPlaylistLocked(u.ID)
	s.playlists[favoritesPlaylist.ID] = favoritesPlaylist
	if err := s.persistLocked(); err != nil {
		delete(s.users, u.ID)
		delete(s.usersByEmail, email)
		delete(s.playlists, favoritesPlaylist.ID)
		s.nextUserID--
		return user{}, err
	}
	return u, nil
}

func (s *trackStore) getUserByEmail(email string) (user, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	id, ok := s.usersByEmail[normalizeEmail(email)]
	if !ok {
		return user{}, false
	}
	u, ok := s.users[id]
	return u, ok
}

func (s *trackStore) getUser(id int64) (user, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	u, ok := s.users[id]
	return u, ok
}

func (s *trackStore) createRefreshSession(userID int64, expiresAt time.Time) (refreshSession, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.users[userID]; !ok {
		return refreshSession{}, "", errInvalidCredentials
	}

	now := time.Now().UTC()
	s.removeExpiredSessionsLocked(now)

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

	s.refreshSession[session.ID] = session
	if err := s.persistLocked(); err != nil {
		delete(s.refreshSession, session.ID)
		return refreshSession{}, "", err
	}
	return session, rawToken, nil
}

func (s *trackStore) rotateRefreshSession(rawToken string, expiresAt time.Time) (user, refreshSession, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	s.removeExpiredSessionsLocked(now)

	session, ok := s.findRefreshSessionLocked(rawToken)
	if !ok {
		return user{}, refreshSession{}, "", errInvalidRefreshToken
	}

	u, ok := s.users[session.UserID]
	if !ok {
		delete(s.refreshSession, session.ID)
		_ = s.persistLocked()
		return user{}, refreshSession{}, "", errInvalidRefreshToken
	}

	delete(s.refreshSession, session.ID)

	newRawToken, err := randomToken(32)
	if err != nil {
		s.refreshSession[session.ID] = session
		return user{}, refreshSession{}, "", err
	}

	newSessionID, err := randomToken(16)
	if err != nil {
		s.refreshSession[session.ID] = session
		return user{}, refreshSession{}, "", err
	}

	newSession := refreshSession{
		ID:        newSessionID,
		UserID:    u.ID,
		TokenHash: hashToken(newRawToken),
		CreatedAt: now,
		ExpiresAt: expiresAt.UTC(),
	}

	s.refreshSession[newSession.ID] = newSession
	if err := s.persistLocked(); err != nil {
		delete(s.refreshSession, newSession.ID)
		s.refreshSession[session.ID] = session
		return user{}, refreshSession{}, "", err
	}

	return u, newSession, newRawToken, nil
}

func (s *trackStore) deleteRefreshSession(rawToken string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	session, ok := s.findRefreshSessionLocked(rawToken)
	if !ok {
		return false, nil
	}
	delete(s.refreshSession, session.ID)
	if err := s.persistLocked(); err != nil {
		s.refreshSession[session.ID] = session
		return false, err
	}
	return true, nil
}

func (s *trackStore) findRefreshSessionLocked(rawToken string) (refreshSession, bool) {
	hashed := hashToken(rawToken)
	for _, session := range s.refreshSession {
		if subtle.ConstantTimeCompare([]byte(session.TokenHash), []byte(hashed)) == 1 {
			return session, true
		}
	}
	return refreshSession{}, false
}

func (s *trackStore) removeExpiredSessionsLocked(now time.Time) {
	for id, session := range s.refreshSession {
		if session.ExpiresAt.Before(now) {
			delete(s.refreshSession, id)
		}
	}
}

func (s *trackStore) getOrCreateAuthorIDLocked(name string) int64 {
	normalized := strings.ToLower(strings.TrimSpace(name))
	for id, a := range s.authors {
		if strings.ToLower(a.CurrentName) == normalized {
			return id
		}
	}

	id := s.nextAuthorID
	s.nextAuthorID++
	s.authors[id] = author{
		ID:          id,
		CurrentName: strings.TrimSpace(name),
		Photos:      []string{},
	}
	return id
}

func (s *trackStore) persistLocked() error {
	trackItems := make([]track, 0, len(s.tracks))
	for _, t := range s.tracks {
		trackItems = append(trackItems, t)
	}
	sort.Slice(trackItems, func(i, j int) bool {
		return trackItems[i].ID < trackItems[j].ID
	})

	albumItems := make([]album, 0, len(s.albums))
	for _, a := range s.albums {
		albumItems = append(albumItems, a)
	}
	sort.Slice(albumItems, func(i, j int) bool {
		return albumItems[i].ID < albumItems[j].ID
	})

	authorItems := make([]author, 0, len(s.authors))
	for _, a := range s.authors {
		authorItems = append(authorItems, a)
	}
	sort.Slice(authorItems, func(i, j int) bool {
		return authorItems[i].ID < authorItems[j].ID
	})

	userItems := make([]user, 0, len(s.users))
	for _, u := range s.users {
		userItems = append(userItems, u)
	}
	sort.Slice(userItems, func(i, j int) bool {
		return userItems[i].ID < userItems[j].ID
	})

	sessionItems := make([]refreshSession, 0, len(s.refreshSession))
	for _, session := range s.refreshSession {
		sessionItems = append(sessionItems, session)
	}
	sort.Slice(sessionItems, func(i, j int) bool {
		if sessionItems[i].UserID == sessionItems[j].UserID {
			return sessionItems[i].CreatedAt.Before(sessionItems[j].CreatedAt)
		}
		return sessionItems[i].UserID < sessionItems[j].UserID
	})

	playlistItems := make([]playlist, 0, len(s.playlists))
	for _, p := range s.playlists {
		playlistItems = append(playlistItems, clonePlaylist(p))
	}
	sort.Slice(playlistItems, func(i, j int) bool {
		if playlistItems[i].UserID == playlistItems[j].UserID {
			return playlistItems[i].ID < playlistItems[j].ID
		}
		return playlistItems[i].UserID < playlistItems[j].UserID
	})

	lyricsItems := make([]lyrics, 0, len(s.lyricsByTrack))
	for _, item := range s.lyricsByTrack {
		lyricsItems = append(lyricsItems, cloneLyrics(item))
	}
	sort.Slice(lyricsItems, func(i, j int) bool {
		return lyricsItems[i].TrackID < lyricsItems[j].TrackID
	})

	payload, err := json.MarshalIndent(dbFile{
		NextTrackID:      s.nextTrackID,
		NextAlbumID:      s.nextAlbumID,
		NextAuthorID:     s.nextAuthorID,
		NextUserID:       s.nextUserID,
		NextPlaylistID:   s.nextPlaylistID,
		NextLyricsID:     s.nextLyricsID,
		NextLyricsLineID: s.nextLyricsLineID,
		Tracks:           trackItems,
		Albums:           albumItems,
		Authors:          authorItems,
		Users:            userItems,
		Sessions:         sessionItems,
		Playlists:        playlistItems,
		Lyrics:           lyricsItems,
	}, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	tempPath := s.path + ".tmp"
	if err := os.WriteFile(tempPath, payload, 0o644); err != nil {
		return err
	}
	return os.Rename(tempPath, s.path)
}

func listSongsHandler(songsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entries, err := os.ReadDir(songsDir)
		if err != nil {
			http.Error(w, "failed to read songs directory", http.StatusInternalServerError)
			return
		}

		songs := make([]songInfo, 0, len(entries))
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}

			name := entry.Name()
			info, err := entry.Info()
			if err != nil {
				continue
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
			writeUploadError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, info)
	}
}

func getSongHandler(songsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveMediaFile(w, r, "/api/songs/", songsDir)
	}
}

func listUnusedSongsHandler(store *trackStore, songsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entries, err := os.ReadDir(songsDir)
		if err != nil {
			http.Error(w, "failed to read songs directory", http.StatusInternalServerError)
			return
		}

		referenced := store.referencedSongFiles()
		songs := make([]songInfo, 0, len(entries))
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}

			name := entry.Name()
			if _, ok := referenced[name]; ok {
				continue
			}

			info, err := entry.Info()
			if err != nil {
				continue
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
			http.Error(w, "failed to delete file", http.StatusInternalServerError)
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
	if _, err := os.Stat(fullPath); err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "failed to read file", http.StatusInternalServerError)
		return
	}

	http.ServeFile(w, r, fullPath)
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
	if err := r.ParseMultipartForm(maxSongUploadSize); err != nil {
		return albumCoverInfo{}, errors.New("invalid multipart form")
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		return albumCoverInfo{}, errors.New("file is required")
	}
	defer file.Close()

	name, err := sanitizeSongFileName(header.Filename)
	if err != nil {
		return albumCoverInfo{}, err
	}

	var (
		dst      *os.File
		fullPath string
	)
	for attempt := 0; attempt < 10; attempt++ {
		storedName, err := randomizedStoredFileName(name)
		if err != nil {
			return albumCoverInfo{}, errors.New(createFailureMessage)
		}

		fullPath = filepath.Join(dir, storedName)
		dst, err = os.OpenFile(fullPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			name = storedName
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return albumCoverInfo{}, errors.New(createFailureMessage)
		}
	}
	if dst == nil {
		return albumCoverInfo{}, errors.New(createFailureMessage)
	}

	copyErr := false
	if _, err := io.Copy(dst, file); err != nil {
		copyErr = true
	}
	if err := dst.Close(); err != nil {
		copyErr = true
	}
	if copyErr {
		_ = os.Remove(fullPath)
		return albumCoverInfo{}, errors.New("failed to save uploaded file")
	}

	info, err := os.Stat(fullPath)
	if err != nil {
		_ = os.Remove(fullPath)
		return albumCoverInfo{}, errors.New("failed to read saved file")
	}
	if info.Size() == 0 {
		_ = os.Remove(fullPath)
		return albumCoverInfo{}, errors.New("uploaded file is empty")
	}

	return albumCoverInfo{
		Name:         name,
		SizeBytes:    info.Size(),
		LastModified: info.ModTime(),
		URL:          urlPrefix + url.PathEscape(name),
	}, nil
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

func writeUploadError(w http.ResponseWriter, err error) {
	switch err.Error() {
	case "invalid multipart form", "file is required", "uploaded filename is required", "invalid song name", "uploaded file is empty":
		http.Error(w, err.Error(), http.StatusBadRequest)
	case "song already exists", "album cover already exists":
		http.Error(w, err.Error(), http.StatusConflict)
	case "failed to create song file", "failed to create album cover file", "failed to save uploaded file", "failed to read saved file":
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		a, err := store.createAlbum(req)
		if err != nil {
			if errors.Is(err, errInvalidAlbum) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			http.Error(w, "failed to create album", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, a)
	}
}

func getAlbumByRouteHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tracks") {
			getAlbumTracksHandler(store).ServeHTTP(w, r)
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

func getAlbumTracksHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseAlbumTracksID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid album id", http.StatusBadRequest)
			return
		}
		tracks, ok := store.getAlbumTracks(id)
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
			http.Error(w, err.Error(), http.StatusBadRequest)
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
			http.Error(w, "failed to update album", http.StatusInternalServerError)
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
			http.Error(w, "failed to delete album", http.StatusInternalServerError)
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
			writeUploadError(w, err)
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
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p, err := store.createPlaylist(userID, req)
		if err != nil {
			if errors.Is(err, errInvalidPlaylistPayload) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			http.Error(w, "failed to create playlist", http.StatusInternalServerError)
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

func getPublicPlaylistByRouteHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tracks") {
			getPublicPlaylistTracksHandler(store).ServeHTTP(w, r)
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

func getPublicPlaylistTracksHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parsePublicPlaylistTracksID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid playlist id", http.StatusBadRequest)
			return
		}
		page := int64(parseIntWithDefault(r.URL.Query().Get("page"), 1))
		pageSize := int64(parseIntWithDefault(r.URL.Query().Get("pageSize"), 20))
		items, exists := store.getPublicPlaylistTracks(id, page, pageSize)
		if !exists {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, items)
	}
}

func getSharedPlaylistByRouteHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tracks") {
			getSharedPlaylistTracksHandler(store).ServeHTTP(w, r)
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

func getSharedPlaylistTracksHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		shareToken, err := parseSharedPlaylistTracksToken(r.URL.Path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		page := int64(parseIntWithDefault(r.URL.Query().Get("page"), 1))
		pageSize := int64(parseIntWithDefault(r.URL.Query().Get("pageSize"), 20))
		items, exists := store.getSharedPlaylistTracks(shareToken, page, pageSize)
		if !exists {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, items)
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
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p, exists, err := store.updatePlaylist(userID, id, req)
		if !exists {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			if errors.Is(err, errInvalidPlaylistPayload) || errors.Is(err, errFavoritePlaylistImmutable) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			http.Error(w, "failed to update playlist", http.StatusInternalServerError)
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
			if errors.Is(err, errFavoritePlaylistImmutable) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			http.Error(w, "failed to delete playlist", http.StatusInternalServerError)
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
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		exists, err := store.reorderPlaylistTracks(userID, id, req.TrackIDs)
		if !exists {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			if errors.Is(err, errInvalidPlaylistPayload) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			http.Error(w, "failed to reorder playlist tracks", http.StatusInternalServerError)
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
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		t, err := store.create(req)
		if err != nil {
			if errors.Is(err, errInvalidTrack) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			http.Error(w, "failed to create track", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, toTrackResponse(t, false, true))
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
			http.Error(w, err.Error(), http.StatusBadRequest)
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
			http.Error(w, "failed to update track", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, toTrackResponse(t, false, true))
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
			http.Error(w, "failed to delete track", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func postTrackByRouteHandler(store *trackStore, auth *authManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/playlists") {
			requireAuth(auth, store, addTrackToPlaylistsHandler(store)).ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}
}

func putTrackByRouteHandler(store *trackStore, auth *authManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/lyrics") {
			requireRole(auth, store, roleAdmin, putTrackLyricsHandler(store)).ServeHTTP(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/favorite") {
			requireAuth(auth, store, favoriteTrackHandler(store, true)).ServeHTTP(w, r)
			return
		}
		requireRole(auth, store, roleAdmin, updateTrackHandler(store)).ServeHTTP(w, r)
	}
}

func deleteTrackByRouteHandler(store *trackStore, auth *authManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/lyrics"):
			requireRole(auth, store, roleAdmin, deleteTrackLyricsHandler(store)).ServeHTTP(w, r)
		case strings.HasSuffix(r.URL.Path, "/favorite"):
			requireAuth(auth, store, favoriteTrackHandler(store, false)).ServeHTTP(w, r)
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
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := store.addTrackToPlaylists(userID, trackID, req.PlaylistIDs); err != nil {
			switch {
			case errors.Is(err, errInvalidPlaylistPayload):
				http.Error(w, err.Error(), http.StatusBadRequest)
			case errors.Is(err, errTrackNotFound), errors.Is(err, errPlaylistNotFound):
				http.NotFound(w, r)
			default:
				http.Error(w, "failed to add track to playlists", http.StatusInternalServerError)
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
			http.Error(w, "failed to remove track from playlist", http.StatusInternalServerError)
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
				http.Error(w, "failed to update favorite track", http.StatusInternalServerError)
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
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		response, err := store.nextAutoplayTracks(userID, req)
		if err != nil {
			switch {
			case errors.Is(err, errInvalidAutoplayRequest):
				http.Error(w, err.Error(), http.StatusBadRequest)
			case errors.Is(err, errTrackNotFound), errors.Is(err, errPlaylistNotFound), errors.Is(err, errAlbumNotFound):
				http.NotFound(w, r)
			default:
				http.Error(w, "failed to build autoplay continuation", http.StatusInternalServerError)
			}
			return
		}

		writeJSON(w, http.StatusOK, response)
	}
}

func listAuthorsHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, store.listAuthors())
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
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		a, err := store.createAuthor(req)
		if err != nil {
			if errors.Is(err, errInvalidAuthor) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			http.Error(w, "failed to create author", http.StatusInternalServerError)
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
			http.Error(w, err.Error(), http.StatusBadRequest)
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
			http.Error(w, "failed to update author", http.StatusInternalServerError)
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
			http.Error(w, "failed to delete author", http.StatusInternalServerError)
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
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		passwordHash, err := hashPassword(req.Password)
		if err != nil {
			http.Error(w, "failed to hash password", http.StatusInternalServerError)
			return
		}

		u, err := store.createUser(req.Email, passwordHash)
		if err != nil {
			if errors.Is(err, errEmailAlreadyExists) {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			http.Error(w, "failed to create user", http.StatusInternalServerError)
			return
		}

		writeAuthResponse(w, http.StatusCreated, auth, store, u)
	}
}

func loginHandler(store *trackStore, auth *authManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := decodeLoginRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		u, ok := store.getUserByEmail(req.Email)
		if !ok || !verifyPassword(req.Password, u.PasswordHash) {
			http.Error(w, errInvalidCredentials.Error(), http.StatusUnauthorized)
			return
		}

		writeAuthResponse(w, http.StatusOK, auth, store, u)
	}
}

func refreshHandler(store *trackStore, auth *authManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := decodeRefreshRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		refreshExpiresAt := time.Now().UTC().Add(auth.refreshTokenTTL)
		u, session, rawToken, err := store.rotateRefreshSession(req.RefreshToken, refreshExpiresAt)
		if err != nil {
			if errors.Is(err, errInvalidRefreshToken) {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			http.Error(w, "failed to refresh token", http.StatusInternalServerError)
			return
		}

		accessToken, accessExpiresAt, err := auth.createAccessToken(u.ID)
		if err != nil {
			http.Error(w, "failed to issue access token", http.StatusInternalServerError)
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
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		_, err = store.deleteRefreshSession(req.RefreshToken)
		if err != nil {
			http.Error(w, "failed to logout", http.StatusInternalServerError)
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

		ctx := context.WithValue(r.Context(), userContextKey, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func writeAuthResponse(w http.ResponseWriter, status int, auth *authManager, store *trackStore, u user) {
	accessToken, accessExpiresAt, err := auth.createAccessToken(u.ID)
	if err != nil {
		http.Error(w, "failed to issue access token", http.StatusInternalServerError)
		return
	}

	refreshSession, refreshToken, err := store.createRefreshSession(u.ID, time.Now().UTC().Add(auth.refreshTokenTTL))
	if err != nil {
		http.Error(w, "failed to issue refresh token", http.StatusInternalServerError)
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
	case autoplaySourcePlaylist, autoplaySourceAlbum, autoplaySourceTrack:
		if req.SourceID == nil || *req.SourceID <= 0 {
			return autoplayNextRequest{}, fmt.Errorf("%w: sourceId is required for sourceType %s", errInvalidAutoplayRequest, req.SourceType)
		}
	default:
		return autoplayNextRequest{}, fmt.Errorf("%w: sourceType must be one of my_vibe, playlist, album, track", errInvalidAutoplayRequest)
	}

	if req.Count == 0 {
		req.Count = defaultAutoplayCount
	}
	if req.Count < 0 || req.Count > maxAutoplayCount {
		return autoplayNextRequest{}, fmt.Errorf("%w: count must be between 1 and %d", errInvalidAutoplayRequest, maxAutoplayCount)
	}

	return req, nil
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
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return errors.New("invalid JSON body")
	}
	return nil
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

func optionalUserIDFromRequest(r *http.Request, auth *authManager) int64 {
	if auth == nil {
		return 0
	}
	userID, err := auth.authenticateRequest(r)
	if err != nil {
		return 0
	}
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

func (s *trackStore) normalizePlaylistSharingLocked(p *playlist) error {
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

func (s *trackStore) newPlaylistShareTokenLocked(excludePlaylistID int64) (string, error) {
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

func (s *trackStore) playlistShareTokenInUseLocked(token string, excludePlaylistID int64) bool {
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

func (s *trackStore) findSharedPlaylistByTokenLocked(shareToken string) (playlist, bool) {
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

func (s *trackStore) validateTrackLocked(t track) error {
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

func (s *trackStore) songFileReferenced(fileName string) (bool, int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, t := range s.tracks {
		if trackReferencesSongFile(t.AudioFilePath, fileName) {
			return true, t.ID
		}
	}

	return false, 0
}

func (s *trackStore) referencedSongFiles() map[string]struct{} {
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
	case p.Description == "":
		return fmt.Errorf("%w: description is required", errInvalidPlaylistPayload)
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
	if p.Kind == playlistKindFavorites && !p.System {
		return fmt.Errorf("%w: favorites playlist must be system managed", errInvalidPlaylistPayload)
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

func (s *trackStore) validateAlbumLocked(a album) error {
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

func (s *trackStore) migrateLegacyAlbumsLocked() error {
	trackIDs := make([]int64, 0, len(s.tracks))
	for id := range s.tracks {
		trackIDs = append(trackIDs, id)
	}
	sort.Slice(trackIDs, func(i, j int) bool {
		return trackIDs[i] < trackIDs[j]
	})

	for _, trackID := range trackIDs {
		t := s.tracks[trackID]
		if t.AlbumID > 0 {
			continue
		}
		albumID := s.nextAlbumID
		s.nextAlbumID++
		s.albums[albumID] = album{
			ID:             albumID,
			Title:          "STUB ALBUM",
			CoverImagePath: "",
			AuthorIDs:      []int64{},
			ReleaseDate:    time.Now().UTC(),
			IsPublished:    false,
			TrackIDs:       []int64{trackID},
			AdditionalInfo: []additionalInfo{},
		}
		t.AlbumID = albumID
		s.tracks[trackID] = t
	}
	return nil
}

func (s *trackStore) ensureFavoritesPlaylistsLocked() error {
	for userID := range s.users {
		if _, ok := s.findFavoritesPlaylistLocked(userID); ok {
			continue
		}
		p := s.newFavoritesPlaylistLocked(userID)
		s.playlists[p.ID] = p
	}
	return nil
}

func (s *trackStore) newFavoritesPlaylistLocked(userID int64) playlist {
	p := playlist{
		ID:             s.nextPlaylistID,
		UserID:         userID,
		Name:           "Favorites",
		Description:    "Tracks marked as favorite by the user.",
		CoverImagePath: "",
		Visibility:     playlistVisibilityPrivate,
		TrackItems:     []playlistTrack{},
		System:         true,
		Kind:           playlistKindFavorites,
	}
	s.nextPlaylistID++
	return p
}

func (s *trackStore) findFavoritesPlaylistLocked(userID int64) (playlist, bool) {
	for _, p := range s.playlists {
		if p.UserID == userID && p.Kind == playlistKindFavorites {
			return p, true
		}
	}
	return playlist{}, false
}

func (s *trackStore) favoriteTrackSetLocked(userID int64) map[int64]struct{} {
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

func (s *trackStore) buildPlaylistResponseLocked(p playlist) playlistResponse {
	return playlistResponse{
		ID:             p.ID,
		UserID:         p.UserID,
		Name:           p.Name,
		Description:    p.Description,
		CoverImagePath: p.CoverImagePath,
		Visibility:     p.Visibility,
		TrackCount:     len(p.TrackItems),
		System:         p.System,
		IsFavorites:    p.Kind == playlistKindFavorites,
		ShareToken:     p.ShareToken,
	}
}

func publicPlaylistResponse(p playlistResponse) playlistResponse {
	p.ShareToken = ""
	return p
}

func (s *trackStore) buildPlaylistTrackResponseLocked(item playlistTrack, favoriteIDs map[int64]struct{}) trackResponse {
	if current, ok := s.tracks[item.TrackID]; ok {
		_, isFavorite := favoriteIDs[item.TrackID]
		return toTrackResponse(current, isFavorite, true)
	}
	if item.UnavailableTrack != nil {
		_, isFavorite := favoriteIDs[item.TrackID]
		return toTrackResponse(*item.UnavailableTrack, isFavorite, false)
	}
	return trackResponse{
		ID:          item.TrackID,
		IsFavorite:  false,
		IsAvailable: false,
	}
}

func (s *trackStore) listTrackResponses(userID int64, filter trackListFilter) paginatedTracks {
	s.mu.RLock()
	defer s.mu.RUnlock()

	items := make([]trackResponse, 0, len(s.tracks))
	query := strings.ToLower(strings.TrimSpace(filter.Query))
	favoriteIDs := s.favoriteTrackSetLocked(userID)
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
		items = append(items, toTrackResponse(t, isFavorite, true))
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

	return paginatedTracks{
		Items:      items[start:end],
		Page:       page,
		PageSize:   pageSize,
		TotalItems: totalItems,
		TotalPages: totalPages,
	}
}

func (s *trackStore) getTrackResponse(trackID, userID int64) (trackResponse, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	t, ok := s.tracks[trackID]
	if !ok {
		return trackResponse{}, false
	}
	_, isFavorite := s.favoriteTrackSetLocked(userID)[trackID]
	return toTrackResponse(t, isFavorite, true), true
}

func (s *trackStore) markTrackUnavailableLocked(t track) {
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

func (s *trackStore) rebuildAlbumDerivedDataLocked() error {
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

func (s *trackStore) applyAlbumTrackIDsLocked(albumID int64, trackIDs []int64, allowRemoval bool) error {
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

func (s *trackStore) removeTrackFromAlbumLocked(albumID, trackID int64) {
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

func cloneTracksMap(src map[int64]track) map[int64]track {
	cloned := make(map[int64]track, len(src))
	for id, item := range src {
		item.AuthorIDs = append([]int64(nil), item.AuthorIDs...)
		item.AdditionalInfo = normalizeAdditionalInfo(item.AdditionalInfo)
		item.SourceMetadata = normalizeSourceMetadata(item.SourceMetadata)
		cloned[id] = item
	}
	return cloned
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

func cloneAlbumsMap(src map[int64]album) map[int64]album {
	cloned := make(map[int64]album, len(src))
	for id, item := range src {
		item.AuthorIDs = append([]int64(nil), item.AuthorIDs...)
		item.TrackIDs = append([]int64(nil), item.TrackIDs...)
		item.AdditionalInfo = normalizeAdditionalInfo(item.AdditionalInfo)
		cloned[id] = item
	}
	return cloned
}

func clonePlaylist(p playlist) playlist {
	p.TrackItems = normalizePlaylistTrackItems(p.TrackItems)
	return p
}

func clonePlaylistsMap(src map[int64]playlist) map[int64]playlist {
	cloned := make(map[int64]playlist, len(src))
	for id, item := range src {
		cloned[id] = clonePlaylist(item)
	}
	return cloned
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
		IsFavorite:     isFavorite,
		IsAvailable:    isAvailable,
	}
}

func (s *trackStore) toSearchTrackResponseLocked(t track, isFavorite bool) trackResponse {
	response := toTrackResponse(t, isFavorite, true)
	response.Authors = make([]author, 0, len(t.AuthorIDs))
	for _, authorID := range t.AuthorIDs {
		a, ok := s.authors[authorID]
		if !ok {
			continue
		}
		response.Authors = append(response.Authors, cloneAuthor(a))
	}
	if albumItem, ok := s.albums[t.AlbumID]; ok {
		response.CoverImagePath = albumItem.CoverImagePath
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
	_ = json.NewEncoder(w).Encode(v)
}

func serveOpenAPIHandler(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := os.Stat(path); err != nil {
			http.Error(w, "openapi spec file not found", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		http.ServeFile(w, r, path)
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
			http.Error(w, "failed to render docs page", http.StatusInternalServerError)
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
			http.Error(w, "failed to render docs page", http.StatusInternalServerError)
		}
	}
}

var errInvalidTrack = errors.New("invalid track payload")
var errInvalidAlbum = errors.New("invalid album payload")
var errAlbumNotFound = errors.New("album not found")
var errInvalidAuthor = errors.New("invalid author payload")
var errInvalidLyricsPayload = errors.New("invalid lyrics payload")
var errInvalidPlaylistPayload = errors.New("invalid playlist payload")
var errInvalidAutoplayRequest = errors.New("invalid autoplay request")
var errAlbumInUse = errors.New("album is used by one or more tracks")
var errAuthorInUse = errors.New("author is used by one or more tracks")
var errEmailAlreadyExists = errors.New("user with this email already exists")
var errInvalidCredentials = errors.New("invalid email or password")
var errInvalidRefreshToken = errors.New("invalid refresh token")
var errTrackNotFound = errors.New("track not found")
var errLyricsNotFound = errors.New("lyrics not found")
var errPlaylistNotFound = errors.New("playlist not found")
var errFavoritePlaylistImmutable = errors.New("favorites playlist cannot be modified directly")
