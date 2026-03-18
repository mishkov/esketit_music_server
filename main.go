package main

import (
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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultAccessTokenTTL  = 15 * time.Minute
	defaultRefreshTokenTTL = 30 * 24 * time.Hour
	passwordHashIterations = 310000
	passwordHashKeyLength  = 32
	minPasswordLength      = 8
	maxSongUploadSize      = 512 << 20
	roleAdmin              = "admin"
	roleListener           = "listener"
)

type contextKey string

const userContextKey contextKey = "authenticated-user-id"

type songInfo struct {
	Name         string    `json:"name"`
	SizeBytes    int64     `json:"sizeBytes"`
	LastModified time.Time `json:"lastModified"`
	URL          string    `json:"url"`
}

type track struct {
	ID             int64   `json:"id"`
	Name           string  `json:"name"`
	AuthorIDs      []int64 `json:"authorIds"`
	AlbumImagePath string  `json:"albumImagePath"`
	AudioFilePath  string  `json:"audioFilePath"`
}

type upsertTrackRequest struct {
	Name           string  `json:"name"`
	AuthorIDs      []int64 `json:"authorIds"`
	AlbumImagePath string  `json:"albumImagePath"`
	AudioFilePath  string  `json:"audioFilePath"`
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

type dbFile struct {
	NextTrackID  int64            `json:"nextTrackId"`
	NextAuthorID int64            `json:"nextAuthorId"`
	NextUserID   int64            `json:"nextUserId"`
	Tracks       []track          `json:"tracks"`
	Authors      []author         `json:"authors"`
	Users        []user           `json:"users"`
	Sessions     []refreshSession `json:"sessions"`
}

type diskDBFile struct {
	NextTrackID  int64             `json:"nextTrackId"`
	NextAuthorID int64             `json:"nextAuthorId"`
	NextUserID   int64             `json:"nextUserId"`
	NextID       int64             `json:"nextId"`
	Tracks       []json.RawMessage `json:"tracks"`
	Authors      []author          `json:"authors"`
	Users        []user            `json:"users"`
	Sessions     []refreshSession  `json:"sessions"`
}

type legacyTrack struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	Authors        []string `json:"authors"`
	AlbumImagePath string   `json:"albumImagePath"`
	AudioFilePath  string   `json:"audioFilePath"`
}

type trackStore struct {
	mu             sync.RWMutex
	path           string
	nextTrackID    int64
	nextAuthorID   int64
	nextUserID     int64
	tracks         map[int64]track
	authors        map[int64]author
	users          map[int64]user
	usersByEmail   map[string]int64
	refreshSession map[string]refreshSession
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

	mux := http.NewServeMux()
	mux.HandleFunc("GET /songs", listSongsHandler(songsDir))
	mux.Handle("POST /songs", requireRole(auth, store, roleAdmin, uploadSongHandler(songsDir)))
	mux.HandleFunc("GET /songs/", getSongHandler(songsDir))
	mux.HandleFunc("GET /tracks", listTracksHandler(store))
	mux.Handle("POST /tracks", requireRole(auth, store, roleAdmin, createTrackHandler(store)))
	mux.HandleFunc("GET /tracks/", getTrackByIDHandler(store))
	mux.Handle("PUT /tracks/", requireRole(auth, store, roleAdmin, updateTrackHandler(store)))
	mux.Handle("DELETE /tracks/", requireRole(auth, store, roleAdmin, deleteTrackHandler(store)))
	mux.HandleFunc("GET /authors", listAuthorsHandler(store))
	mux.Handle("POST /authors", requireRole(auth, store, roleAdmin, createAuthorHandler(store)))
	mux.HandleFunc("GET /authors/", getAuthorByIDHandler(store))
	mux.Handle("PUT /authors/", requireRole(auth, store, roleAdmin, updateAuthorHandler(store)))
	mux.Handle("DELETE /authors/", requireRole(auth, store, roleAdmin, deleteAuthorHandler(store)))
	mux.HandleFunc("POST /auth/register", registerHandler(store, auth))
	mux.HandleFunc("POST /auth/login", loginHandler(store, auth))
	mux.HandleFunc("POST /auth/refresh", refreshHandler(store, auth))
	mux.HandleFunc("POST /auth/logout", logoutHandler(store))
	mux.Handle("GET /auth/me", requireAuth(auth, store, meHandler(store)))
	mux.HandleFunc("GET /openapi.yaml", serveOpenAPIHandler("openapi.yaml"))
	mux.HandleFunc("GET /docs", swaggerUIHandler())
	mux.HandleFunc("GET /redoc", redocHandler())

	addr := ":8080"
	log.Printf("server listening on %s", addr)
	log.Printf("serving songs from %s", songsDir)
	log.Printf("using tracks database %s", tracksDBPath)
	log.Printf("swagger docs available at http://localhost%s/docs", addr)
	log.Printf("redoc available at http://localhost%s/redoc", addr)
	log.Fatal(http.ListenAndServe(addr, withCORS(mux)))
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
		path:           path,
		nextTrackID:    1,
		nextAuthorID:   1,
		nextUserID:     1,
		tracks:         make(map[int64]track),
		authors:        make(map[int64]author),
		users:          make(map[int64]user),
		usersByEmail:   make(map[string]int64),
		refreshSession: make(map[string]refreshSession),
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

	for _, rawTrack := range file.Tracks {
		var t track
		if err := json.Unmarshal(rawTrack, &t); err == nil {
			t.Name = strings.TrimSpace(t.Name)
			t.AuthorIDs = normalizeAuthorIDs(t.AuthorIDs)
			t.AlbumImagePath = strings.TrimSpace(t.AlbumImagePath)
			t.AudioFilePath = strings.TrimSpace(t.AudioFilePath)
			if t.ID <= 0 {
				continue
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
			ID:             legacy.ID,
			Name:           strings.TrimSpace(legacy.Name),
			AuthorIDs:      normalizeAuthorIDs(authorIDs),
			AlbumImagePath: strings.TrimSpace(legacy.AlbumImagePath),
			AudioFilePath:  strings.TrimSpace(legacy.AudioFilePath),
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
	if file.NextID > s.nextTrackID {
		s.nextTrackID = file.NextID
	}
	if file.NextAuthorID > s.nextAuthorID {
		s.nextAuthorID = file.NextAuthorID
	}
	if file.NextUserID > s.nextUserID {
		s.nextUserID = file.NextUserID
	}
	if s.nextTrackID < 1 {
		s.nextTrackID = 1
	}
	if s.nextAuthorID < 1 {
		s.nextAuthorID = 1
	}
	if s.nextUserID < 1 {
		s.nextUserID = 1
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

func (s *trackStore) get(id int64) (track, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tracks[id]
	return t, ok
}

func (s *trackStore) create(req upsertTrackRequest) (track, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t := track{
		ID:             s.nextTrackID,
		Name:           strings.TrimSpace(req.Name),
		AuthorIDs:      normalizeAuthorIDs(req.AuthorIDs),
		AlbumImagePath: strings.TrimSpace(req.AlbumImagePath),
		AudioFilePath:  strings.TrimSpace(req.AudioFilePath),
	}
	if err := validateTrack(t, s.authors); err != nil {
		return track{}, err
	}

	s.nextTrackID++
	s.tracks[t.ID] = t
	if err := s.persistLocked(); err != nil {
		delete(s.tracks, t.ID)
		s.nextTrackID--
		return track{}, err
	}
	return t, nil
}

func (s *trackStore) update(id int64, req upsertTrackRequest) (track, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tracks[id]; !ok {
		return track{}, false, nil
	}

	t := track{
		ID:             id,
		Name:           strings.TrimSpace(req.Name),
		AuthorIDs:      normalizeAuthorIDs(req.AuthorIDs),
		AlbumImagePath: strings.TrimSpace(req.AlbumImagePath),
		AudioFilePath:  strings.TrimSpace(req.AudioFilePath),
	}
	if err := validateTrack(t, s.authors); err != nil {
		return track{}, true, err
	}

	s.tracks[id] = t
	if err := s.persistLocked(); err != nil {
		return track{}, true, err
	}
	return t, true, nil
}

func (s *trackStore) delete(id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tracks[id]; !ok {
		return false, nil
	}
	delete(s.tracks, id)
	if err := s.persistLocked(); err != nil {
		return false, err
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
	if err := s.persistLocked(); err != nil {
		delete(s.users, u.ID)
		delete(s.usersByEmail, email)
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

	payload, err := json.MarshalIndent(dbFile{
		NextTrackID:  s.nextTrackID,
		NextAuthorID: s.nextAuthorID,
		NextUserID:   s.nextUserID,
		Tracks:       trackItems,
		Authors:      authorItems,
		Users:        userItems,
		Sessions:     sessionItems,
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
		r.Body = http.MaxBytesReader(w, r.Body, maxSongUploadSize)
		if err := r.ParseMultipartForm(maxSongUploadSize); err != nil {
			http.Error(w, "invalid multipart form", http.StatusBadRequest)
			return
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "file is required", http.StatusBadRequest)
			return
		}
		defer file.Close()

		name, err := sanitizeSongFileName(header.Filename)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		fullPath := filepath.Join(songsDir, name)
		dst, err := os.OpenFile(fullPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				http.Error(w, "song already exists", http.StatusConflict)
				return
			}
			http.Error(w, "failed to create song file", http.StatusInternalServerError)
			return
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
			http.Error(w, "failed to save song", http.StatusInternalServerError)
			return
		}

		info, err := os.Stat(fullPath)
		if err != nil {
			_ = os.Remove(fullPath)
			http.Error(w, "failed to read saved song", http.StatusInternalServerError)
			return
		}
		if info.Size() == 0 {
			_ = os.Remove(fullPath)
			http.Error(w, "uploaded file is empty", http.StatusBadRequest)
			return
		}

		writeJSON(w, http.StatusCreated, buildSongInfo(name, info))
	}
}

func getSongHandler(songsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		encodedName := strings.TrimPrefix(r.URL.Path, "/songs/")
		if encodedName == "" {
			http.NotFound(w, r)
			return
		}

		name, err := url.PathUnescape(encodedName)
		if err != nil {
			http.Error(w, "invalid song name", http.StatusBadRequest)
			return
		}

		cleanName := filepath.Base(filepath.Clean(name))
		if cleanName == "." || cleanName == "/" || cleanName == "" {
			http.Error(w, "invalid song name", http.StatusBadRequest)
			return
		}

		fullPath := filepath.Join(songsDir, cleanName)
		if _, err := os.Stat(fullPath); err != nil {
			if os.IsNotExist(err) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "failed to read song", http.StatusInternalServerError)
			return
		}

		http.ServeFile(w, r, fullPath)
	}
}

func buildSongInfo(name string, info os.FileInfo) songInfo {
	return songInfo{
		Name:         name,
		SizeBytes:    info.Size(),
		LastModified: info.ModTime(),
		URL:          "/songs/" + url.PathEscape(name),
	}
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

func listTracksHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, store.list())
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
		writeJSON(w, http.StatusCreated, t)
	}
}

func getTrackByIDHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseTrackID(r.URL.Path)
		if err != nil {
			http.Error(w, "invalid track id", http.StatusBadRequest)
			return
		}
		t, ok := store.get(id)
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
		writeJSON(w, http.StatusOK, t)
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

func listAuthorsHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, store.listAuthors())
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

func decodeUpsertAuthorRequest(r *http.Request) (upsertAuthorRequest, error) {
	var req upsertAuthorRequest
	if err := decodeJSON(r, &req); err != nil {
		return upsertAuthorRequest{}, err
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

func userIDFromContext(ctx context.Context) (int64, bool) {
	userID, ok := ctx.Value(userContextKey).(int64)
	return userID, ok
}

func parseTrackID(path string) (int64, error) {
	return parseResourceID(path, "/tracks/")
}

func parseAuthorID(path string) (int64, error) {
	return parseResourceID(path, "/authors/")
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

func validateTrack(t track, authors map[int64]author) error {
	switch {
	case t.Name == "":
		return fmt.Errorf("%w: name is required", errInvalidTrack)
	case len(t.AuthorIDs) == 0:
		return fmt.Errorf("%w: at least one authorId is required", errInvalidTrack)
	case t.AlbumImagePath == "":
		return fmt.Errorf("%w: albumImagePath is required", errInvalidTrack)
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
      url: '/openapi.yaml',
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
  <redoc spec-url="/openapi.yaml"></redoc>
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
var errInvalidAuthor = errors.New("invalid author payload")
var errAuthorInUse = errors.New("author is used by one or more tracks")
var errEmailAlreadyExists = errors.New("user with this email already exists")
var errInvalidCredentials = errors.New("invalid email or password")
var errInvalidRefreshToken = errors.New("invalid refresh token")
