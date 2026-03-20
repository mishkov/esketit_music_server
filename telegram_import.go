package main

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
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

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	tauth "github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
)

const (
	telegramImportStatusActive    = "active"
	telegramImportStatusCompleted = "completed"
	telegramScanBatchSize         = 100
)

var (
	errTelegramNotConfigured      = errors.New("telegram is not configured")
	errTelegramNotAuthorized      = errors.New("telegram is not authorized")
	errTelegramInvalidChannel     = errors.New("invalid or non-public telegram channel")
	errTelegramNoAudioTracks      = errors.New("no audio tracks found in telegram channel")
	errTelegramScanStalled        = errors.New("telegram channel scan did not make progress")
	errTelegramSessionActive      = errors.New("telegram import session is already active")
	errTelegramSessionNotFound    = errors.New("telegram import session not found")
	errTelegramTempFileMissing    = errors.New("telegram import temp file is missing")
	errTelegramAuthPendingMissing = errors.New("telegram login code was not requested")
	errTelegramAuthPasswordNeeded = errors.New("telegram password is required")
	errTelegramPasswordNotPending = errors.New("telegram password was not requested")
)

type telegramConfig struct {
	APIID           int
	APIHash         string
	StateDir        string
	SessionFile     string
	ImportTempDir   string
	RequestTimeout  time.Duration
	ScanTimeout     time.Duration
	DownloadTimeout time.Duration
}

func loadTelegramConfig(stateDir, tempDir string) (telegramConfig, error) {
	cfg := telegramConfig{
		APIHash:         strings.TrimSpace(os.Getenv("TELEGRAM_API_HASH")),
		StateDir:        stateDir,
		SessionFile:     filepath.Join(stateDir, "session.json"),
		ImportTempDir:   tempDir,
		RequestTimeout:  15 * time.Minute,
		ScanTimeout:     15 * time.Minute,
		DownloadTimeout: 15 * time.Minute,
	}

	rawAPIID := strings.TrimSpace(os.Getenv("TELEGRAM_API_ID"))
	if rawAPIID == "" {
		return cfg, nil
	}

	apiID, err := strconv.Atoi(rawAPIID)
	if err != nil || apiID <= 0 {
		return telegramConfig{}, errors.New("TELEGRAM_API_ID must be a positive integer")
	}
	cfg.APIID = apiID
	return cfg, nil
}

func (c telegramConfig) Configured() bool {
	return c.APIID > 0 && c.APIHash != ""
}

type telegramAuthStatus struct {
	Configured         bool   `json:"configured"`
	Authorized         bool   `json:"authorized"`
	PasswordRequired   bool   `json:"passwordRequired"`
	AccountIdentifier  string `json:"accountIdentifier,omitempty"`
	ImportTempDir      string `json:"importTempDir,omitempty"`
	SessionStorageFile string `json:"sessionStorageFile,omitempty"`
}

type telegramScannedTrack struct {
	MessageID   int
	MessageLink string
	FileName    string
	MimeType    string
	SizeBytes   int64
	ParsedTitle string
	DocumentID  int64
	AccessHash  int64
	FileRef     []byte
	DCID        int
}

type telegramGateway interface {
	Status(ctx context.Context) (telegramAuthStatus, error)
	BeginLogin(ctx context.Context, phoneNumber string) (string, error)
	ConfirmLogin(ctx context.Context, phoneNumber, code, codeHash string) (telegramAuthStatus, error)
	PasswordLogin(ctx context.Context, password string) (telegramAuthStatus, error)
	ScanPublicChannel(ctx context.Context, channelUsername string) ([]telegramScannedTrack, error)
	DownloadTrack(ctx context.Context, item telegramScannedTrack, destinationPath string) error
}

type gotdTelegramGateway struct {
	cfg telegramConfig
}

func newGotdTelegramGateway(cfg telegramConfig) *gotdTelegramGateway {
	return &gotdTelegramGateway{cfg: cfg}
}

func (g *gotdTelegramGateway) newClient() *telegram.Client {
	return telegram.NewClient(g.cfg.APIID, g.cfg.APIHash, telegram.Options{
		SessionStorage: &session.FileStorage{
			Path: g.cfg.SessionFile,
		},
	})
}

func (g *gotdTelegramGateway) run(ctx context.Context, fn func(context.Context, *telegram.Client) error) error {
	return g.runWithTimeout(ctx, g.cfg.RequestTimeout, fn)
}

func (g *gotdTelegramGateway) runWithTimeout(ctx context.Context, timeout time.Duration, fn func(context.Context, *telegram.Client) error) error {
	if !g.cfg.Configured() {
		return errTelegramNotConfigured
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := g.newClient()
	return client.Run(ctx, func(runCtx context.Context) error {
		return fn(runCtx, client)
	})
}

func (g *gotdTelegramGateway) Status(ctx context.Context) (telegramAuthStatus, error) {
	status := telegramAuthStatus{
		Configured:         g.cfg.Configured(),
		ImportTempDir:      g.cfg.ImportTempDir,
		SessionStorageFile: g.cfg.SessionFile,
	}
	if !g.cfg.Configured() {
		return status, nil
	}

	err := g.run(ctx, func(runCtx context.Context, client *telegram.Client) error {
		authStatus, err := client.Auth().Status(runCtx)
		if err != nil {
			return err
		}
		status.Authorized = authStatus.Authorized
		status.AccountIdentifier = formatTelegramUserIdentifier(authStatus.User)
		return nil
	})
	return status, err
}

func (g *gotdTelegramGateway) BeginLogin(ctx context.Context, phoneNumber string) (string, error) {
	var codeHash string
	err := g.run(ctx, func(runCtx context.Context, client *telegram.Client) error {
		authStatus, err := client.Auth().Status(runCtx)
		if err != nil {
			return err
		}
		if authStatus.Authorized {
			return nil
		}

		sentCode, err := client.Auth().SendCode(runCtx, phoneNumber, tauth.SendCodeOptions{})
		if err != nil {
			return err
		}

		switch sent := sentCode.(type) {
		case *tg.AuthSentCode:
			codeHash = sent.PhoneCodeHash
			return nil
		default:
			return errors.New("unsupported telegram auth response")
		}
	})
	return codeHash, err
}

func (g *gotdTelegramGateway) ConfirmLogin(ctx context.Context, phoneNumber, code, codeHash string) (telegramAuthStatus, error) {
	var result telegramAuthStatus
	err := g.run(ctx, func(runCtx context.Context, client *telegram.Client) error {
		if _, err := client.Auth().SignIn(runCtx, phoneNumber, code, codeHash); err != nil {
			if errors.Is(err, tauth.ErrPasswordAuthNeeded) {
				result = telegramAuthStatus{
					Configured:         true,
					Authorized:         false,
					PasswordRequired:   true,
					ImportTempDir:      g.cfg.ImportTempDir,
					SessionStorageFile: g.cfg.SessionFile,
				}
				return nil
			}
			return err
		}
		authStatus, err := client.Auth().Status(runCtx)
		if err != nil {
			return err
		}
		result = telegramAuthStatus{
			Configured:         true,
			Authorized:         authStatus.Authorized,
			AccountIdentifier:  formatTelegramUserIdentifier(authStatus.User),
			ImportTempDir:      g.cfg.ImportTempDir,
			SessionStorageFile: g.cfg.SessionFile,
		}
		return nil
	})
	return result, err
}

func (g *gotdTelegramGateway) PasswordLogin(ctx context.Context, password string) (telegramAuthStatus, error) {
	var result telegramAuthStatus
	err := g.run(ctx, func(runCtx context.Context, client *telegram.Client) error {
		if _, err := client.Auth().Password(runCtx, password); err != nil {
			if errors.Is(err, tauth.ErrPasswordInvalid) {
				return errTelegramNotAuthorized
			}
			return err
		}
		authStatus, err := client.Auth().Status(runCtx)
		if err != nil {
			return err
		}
		result = telegramAuthStatus{
			Configured:         true,
			Authorized:         authStatus.Authorized,
			PasswordRequired:   false,
			AccountIdentifier:  formatTelegramUserIdentifier(authStatus.User),
			ImportTempDir:      g.cfg.ImportTempDir,
			SessionStorageFile: g.cfg.SessionFile,
		}
		return nil
	})
	return result, err
}

func (g *gotdTelegramGateway) ScanPublicChannel(ctx context.Context, channelUsername string) ([]telegramScannedTrack, error) {
	items := make([]telegramScannedTrack, 0)
	err := g.runWithTimeout(ctx, g.cfg.ScanTimeout, func(runCtx context.Context, client *telegram.Client) error {
		authStatus, err := client.Auth().Status(runCtx)
		if err != nil {
			return err
		}
		if !authStatus.Authorized {
			return errTelegramNotAuthorized
		}

		api := client.API()
		channel, inputPeer, err := resolvePublicChannel(runCtx, api, channelUsername)
		if err != nil {
			return err
		}

		offsetID := 0
		seenMessageIDs := make(map[int]struct{})
		for {
			previousOffsetID := offsetID
			history, err := api.MessagesGetHistory(runCtx, &tg.MessagesGetHistoryRequest{
				Peer:     inputPeer,
				OffsetID: offsetID,
				Limit:    telegramScanBatchSize,
			})
			if err != nil {
				return err
			}

			messages := extractMessages(history)
			if len(messages) == 0 {
				break
			}

			batchProgressed := false
			for _, message := range messages {
				if _, seen := seenMessageIDs[message.ID]; seen {
					continue
				}
				seenMessageIDs[message.ID] = struct{}{}
				offsetID = message.ID
				batchProgressed = true
				item, ok := buildScannedTrack(channel.Username, message)
				if !ok {
					continue
				}
				items = append(items, item)
			}

			if !batchProgressed || offsetID == previousOffsetID {
				return errTelegramScanStalled
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, errTelegramNoAudioTracks
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].MessageID < items[j].MessageID
	})
	return items, nil
}

func (g *gotdTelegramGateway) DownloadTrack(ctx context.Context, item telegramScannedTrack, destinationPath string) error {
	return g.runWithTimeout(ctx, g.cfg.DownloadTimeout, func(runCtx context.Context, client *telegram.Client) error {
		authStatus, err := client.Auth().Status(runCtx)
		if err != nil {
			return err
		}
		if !authStatus.Authorized {
			return errTelegramNotAuthorized
		}

		location := &tg.InputDocumentFileLocation{
			ID:            item.DocumentID,
			AccessHash:    item.AccessHash,
			FileReference: item.FileRef,
			ThumbSize:     "",
		}

		if err := os.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
			return err
		}
		_, err = downloader.NewDownloader().Download(client.API(), location).ToPath(runCtx, destinationPath)
		if err != nil {
			return err
		}

		info, err := os.Stat(destinationPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return errTelegramTempFileMissing
			}
			return err
		}
		if info.Size() == 0 {
			_ = os.Remove(destinationPath)
			return errTelegramTempFileMissing
		}
		return nil
	})
}

type telegramPendingLogin struct {
	PhoneNumber      string
	CodeHash         string
	RequestedAt      time.Time
	PasswordRequired bool
}

type telegramImportSession struct {
	ID              string
	UserID          int64
	Status          string
	ChannelUsername string
	Items           []telegramScannedTrack
	CurrentIndex    int
	SkippedItems    []telegramSkippedItem
	SavedCount      int
	TempFiles       map[int]string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type telegramSkippedItem struct {
	ParsedTitle string `json:"parsedTitle"`
	MessageLink string `json:"telegramMessageLink"`
}

type telegramCurrentTrackDTO struct {
	MessageID           int    `json:"messageId"`
	MessageLink         string `json:"telegramMessageLink"`
	ParsedTitle         string `json:"parsedTitle"`
	FileName            string `json:"fileName"`
	MimeType            string `json:"mimeType"`
	SizeBytes           int64  `json:"sizeBytes"`
	TempFileDownloadURL string `json:"tempFileDownloadUrl"`
}

type telegramImportProgressDTO struct {
	Total     int `json:"total"`
	Processed int `json:"processed"`
	Remaining int `json:"remaining"`
	Skipped   int `json:"skipped"`
	Saved     int `json:"saved"`
}

type telegramImportSessionDTO struct {
	SessionID       string                    `json:"sessionId"`
	Status          string                    `json:"status"`
	ChannelUsername string                    `json:"channelUsername"`
	CurrentTrack    *telegramCurrentTrackDTO  `json:"currentTrack,omitempty"`
	Progress        telegramImportProgressDTO `json:"progress"`
	CreatedAt       time.Time                 `json:"createdAt"`
	UpdatedAt       time.Time                 `json:"updatedAt"`
}

type telegramAuthRequest struct {
	PhoneNumber string `json:"phoneNumber"`
}

type telegramAuthConfirmRequest struct {
	PhoneNumber string `json:"phoneNumber"`
	Code        string `json:"code"`
}

type telegramAuthPasswordRequest struct {
	Password string `json:"password"`
}

type telegramStartImportRequest struct {
	ChannelUsername string `json:"channelUsername"`
	ReplaceExisting bool   `json:"replaceExisting"`
}

type telegramSaveTrackRequest struct {
	Name           string           `json:"name"`
	AuthorIDs      []int64          `json:"authorIds"`
	AlbumID        int64            `json:"albumId"`
	AlbumOrder     int              `json:"albumOrder"`
	AdditionalInfo []additionalInfo `json:"additionalInfo"`
}

type telegramImportService struct {
	mu           sync.Mutex
	cfg          telegramConfig
	gateway      telegramGateway
	store        *trackStore
	songsDir     string
	pendingLogin *telegramPendingLogin
	sessions     map[int64]*telegramImportSession
}

func newTelegramImportService(cfg telegramConfig, gateway telegramGateway, store *trackStore, songsDir string) *telegramImportService {
	return &telegramImportService{
		cfg:      cfg,
		gateway:  gateway,
		store:    store,
		songsDir: songsDir,
		sessions: make(map[int64]*telegramImportSession),
	}
}

func (s *telegramImportService) Status(ctx context.Context) (telegramAuthStatus, error) {
	status, err := s.gateway.Status(ctx)
	if err != nil {
		return telegramAuthStatus{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingLogin != nil && s.pendingLogin.PasswordRequired {
		status.PasswordRequired = true
	}
	return status, nil
}

func (s *telegramImportService) BeginLogin(ctx context.Context, phoneNumber string) error {
	phoneNumber = strings.TrimSpace(phoneNumber)
	if phoneNumber == "" {
		return errors.New("phoneNumber is required")
	}

	codeHash, err := s.gateway.BeginLogin(ctx, phoneNumber)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingLogin = &telegramPendingLogin{
		PhoneNumber: phoneNumber,
		CodeHash:    codeHash,
		RequestedAt: time.Now().UTC(),
	}
	return nil
}

func (s *telegramImportService) ConfirmLogin(ctx context.Context, phoneNumber, code string) (telegramAuthStatus, error) {
	phoneNumber = strings.TrimSpace(phoneNumber)
	code = strings.TrimSpace(code)
	if phoneNumber == "" || code == "" {
		return telegramAuthStatus{}, errors.New("phoneNumber and code are required")
	}

	s.mu.Lock()
	pending := s.pendingLogin
	s.mu.Unlock()
	if pending == nil {
		return telegramAuthStatus{}, errTelegramAuthPendingMissing
	}
	if pending.PhoneNumber != phoneNumber {
		return telegramAuthStatus{}, errors.New("phoneNumber does not match pending telegram login request")
	}

	status, err := s.gateway.ConfirmLogin(ctx, phoneNumber, code, pending.CodeHash)
	if err != nil {
		return telegramAuthStatus{}, err
	}

	s.mu.Lock()
	if status.PasswordRequired {
		s.pendingLogin.PasswordRequired = true
	} else {
		s.pendingLogin = nil
	}
	s.mu.Unlock()
	return status, nil
}

func (s *telegramImportService) SubmitPassword(ctx context.Context, password string) (telegramAuthStatus, error) {
	password = strings.TrimSpace(password)
	if password == "" {
		return telegramAuthStatus{}, errors.New("password is required")
	}

	s.mu.Lock()
	pending := s.pendingLogin
	s.mu.Unlock()
	if pending == nil {
		return telegramAuthStatus{}, errTelegramAuthPendingMissing
	}
	if !pending.PasswordRequired {
		return telegramAuthStatus{}, errTelegramPasswordNotPending
	}

	status, err := s.gateway.PasswordLogin(ctx, password)
	if err != nil {
		return telegramAuthStatus{}, err
	}

	s.mu.Lock()
	s.pendingLogin = nil
	s.mu.Unlock()
	return status, nil
}

func (s *telegramImportService) StartSession(ctx context.Context, userID int64, channelUsername string, replaceExisting bool) (telegramImportSessionDTO, error) {
	channelUsername = normalizeTelegramChannelUsername(channelUsername)
	if channelUsername == "" {
		return telegramImportSessionDTO{}, errors.New("channelUsername is required")
	}

	s.mu.Lock()
	if existing, ok := s.sessions[userID]; ok && existing.Status == telegramImportStatusActive && !replaceExisting {
		s.mu.Unlock()
		return telegramImportSessionDTO{}, errTelegramSessionActive
	}
	existing := s.sessions[userID]
	s.mu.Unlock()

	if existing != nil {
		_ = s.cleanupSessionFiles(existing)
	}

	items, err := s.gateway.ScanPublicChannel(ctx, channelUsername)
	if err != nil {
		return telegramImportSessionDTO{}, err
	}

	sessionID, err := randomToken(16)
	if err != nil {
		return telegramImportSessionDTO{}, err
	}

	now := time.Now().UTC()
	session := &telegramImportSession{
		ID:              sessionID,
		UserID:          userID,
		Status:          telegramImportStatusActive,
		ChannelUsername: channelUsername,
		Items:           items,
		CurrentIndex:    0,
		SkippedItems:    []telegramSkippedItem{},
		SavedCount:      0,
		TempFiles:       make(map[int]string),
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	s.mu.Lock()
	s.sessions[userID] = session
	s.mu.Unlock()

	return s.CurrentSession(ctx, userID)
}

func (s *telegramImportService) CurrentSession(ctx context.Context, userID int64) (telegramImportSessionDTO, error) {
	s.mu.Lock()
	session, ok := s.sessions[userID]
	if !ok {
		s.mu.Unlock()
		return telegramImportSessionDTO{}, errTelegramSessionNotFound
	}

	currentItem, hasCurrent := session.currentItem()
	currentIndex := session.CurrentIndex
	s.mu.Unlock()

	if hasCurrent {
		if err := s.ensureTempFile(ctx, session, currentIndex, currentItem); err != nil {
			return telegramImportSessionDTO{}, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok = s.sessions[userID]
	if !ok {
		return telegramImportSessionDTO{}, errTelegramSessionNotFound
	}
	return s.buildSessionDTO(session), nil
}

func (s *telegramImportService) SkipCurrent(userID int64) (telegramImportSessionDTO, error) {
	s.mu.Lock()
	session, ok := s.sessions[userID]
	if !ok {
		s.mu.Unlock()
		return telegramImportSessionDTO{}, errTelegramSessionNotFound
	}

	item, ok := session.currentItem()
	if !ok {
		s.mu.Unlock()
		return s.buildSessionDTO(session), nil
	}

	session.SkippedItems = append(session.SkippedItems, telegramSkippedItem{
		ParsedTitle: item.ParsedTitle,
		MessageLink: item.MessageLink,
	})
	if path := session.TempFiles[session.CurrentIndex]; path != "" {
		delete(session.TempFiles, session.CurrentIndex)
		_ = os.Remove(path)
	}
	session.CurrentIndex++
	session.UpdatedAt = time.Now().UTC()
	if session.CurrentIndex >= len(session.Items) {
		session.Status = telegramImportStatusCompleted
	}
	dto := s.buildSessionDTO(session)
	s.mu.Unlock()
	return dto, nil
}

func (s *telegramImportService) SaveCurrent(ctx context.Context, userID int64, req telegramSaveTrackRequest) (telegramImportSessionDTO, track, error) {
	s.mu.Lock()
	session, ok := s.sessions[userID]
	if !ok {
		s.mu.Unlock()
		return telegramImportSessionDTO{}, track{}, errTelegramSessionNotFound
	}

	itemIndex := session.CurrentIndex
	item, ok := session.currentItem()
	s.mu.Unlock()
	if !ok {
		current, currentErr := s.CurrentSession(ctx, userID)
		return current, track{}, currentErr
	}

	tempPath, err := s.ensureTempFilePath(ctx, session, itemIndex, item)
	if err != nil {
		return telegramImportSessionDTO{}, track{}, err
	}

	finalName, audioPath, err := s.promoteTempFile(tempPath, item.FileName)
	if err != nil {
		return telegramImportSessionDTO{}, track{}, err
	}

	createdTrack, err := s.store.create(upsertTrackRequest{
		Name:           strings.TrimSpace(req.Name),
		AuthorIDs:      req.AuthorIDs,
		AlbumID:        req.AlbumID,
		AlbumOrder:     req.AlbumOrder,
		AudioFilePath:  audioPath,
		AdditionalInfo: req.AdditionalInfo,
	})
	if err != nil {
		_ = os.Remove(filepath.Join(s.songsDir, finalName))
		return telegramImportSessionDTO{}, track{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok = s.sessions[userID]
	if !ok {
		return telegramImportSessionDTO{}, track{}, errTelegramSessionNotFound
	}
	delete(session.TempFiles, itemIndex)
	session.CurrentIndex++
	session.SavedCount++
	session.UpdatedAt = time.Now().UTC()
	if session.CurrentIndex >= len(session.Items) {
		session.Status = telegramImportStatusCompleted
	}
	return s.buildSessionDTO(session), createdTrack, nil
}

func (s *telegramImportService) CancelSession(userID int64) error {
	s.mu.Lock()
	session, ok := s.sessions[userID]
	if !ok {
		s.mu.Unlock()
		return errTelegramSessionNotFound
	}
	delete(s.sessions, userID)
	s.mu.Unlock()

	return s.cleanupSessionFiles(session)
}

func (s *telegramImportService) CurrentAudioPath(ctx context.Context, userID int64) (string, string, error) {
	s.mu.Lock()
	session, ok := s.sessions[userID]
	if !ok {
		s.mu.Unlock()
		return "", "", errTelegramSessionNotFound
	}
	index := session.CurrentIndex
	item, ok := session.currentItem()
	s.mu.Unlock()
	if !ok {
		return "", "", errTelegramSessionNotFound
	}

	path, err := s.ensureTempFilePath(ctx, session, index, item)
	if err != nil {
		return "", "", err
	}
	return path, item.FileName, nil
}

func (s *telegramImportService) SkippedReport(userID int64) ([]byte, error) {
	s.mu.Lock()
	session, ok := s.sessions[userID]
	if !ok {
		s.mu.Unlock()
		return nil, errTelegramSessionNotFound
	}
	skippedItems := append([]telegramSkippedItem(nil), session.SkippedItems...)
	s.mu.Unlock()

	buffer := &strings.Builder{}
	writer := csv.NewWriter(buffer)
	if err := writer.Write([]string{"parsed_title", "telegram_message_link"}); err != nil {
		return nil, err
	}
	for _, item := range skippedItems {
		if err := writer.Write([]string{item.ParsedTitle, item.MessageLink}); err != nil {
			return nil, err
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, err
	}
	return []byte(buffer.String()), nil
}

func (s *telegramImportService) ensureTempFile(ctx context.Context, session *telegramImportSession, index int, item telegramScannedTrack) error {
	_, err := s.ensureTempFilePath(ctx, session, index, item)
	return err
}

func (s *telegramImportService) ensureTempFilePath(ctx context.Context, session *telegramImportSession, index int, item telegramScannedTrack) (string, error) {
	s.mu.Lock()
	if path := session.TempFiles[index]; path != "" {
		if _, err := os.Stat(path); err == nil {
			s.mu.Unlock()
			return path, nil
		}
		delete(session.TempFiles, index)
	}
	sessionDir := filepath.Join(s.cfg.ImportTempDir, session.ID)
	targetPath := filepath.Join(sessionDir, buildTempFileName(index, item.FileName))
	session.TempFiles[index] = targetPath
	session.UpdatedAt = time.Now().UTC()
	s.mu.Unlock()

	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return "", err
	}
	if err := s.gateway.DownloadTrack(ctx, item, targetPath); err != nil {
		_ = os.Remove(targetPath)
		s.mu.Lock()
		delete(session.TempFiles, index)
		s.mu.Unlock()
		return "", err
	}
	return targetPath, nil
}

func (s *telegramImportService) promoteTempFile(tempPath, originalFileName string) (string, string, error) {
	if _, err := os.Stat(tempPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", errTelegramTempFileMissing
		}
		return "", "", err
	}

	fileName, err := sanitizeSongFileName(originalFileName)
	if err != nil {
		fileName = "telegram-track.bin"
	}
	finalName, err := uniqueFileName(s.songsDir, fileName)
	if err != nil {
		return "", "", err
	}

	finalPath := filepath.Join(s.songsDir, finalName)
	if err := moveFile(tempPath, finalPath); err != nil {
		return "", "", err
	}
	return finalName, "/songs/" + url.PathEscape(finalName), nil
}

func (s *telegramImportService) cleanupSessionFiles(session *telegramImportSession) error {
	if session == nil {
		return nil
	}
	sessionDir := filepath.Join(s.cfg.ImportTempDir, session.ID)
	if err := os.RemoveAll(sessionDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *telegramImportService) buildSessionDTO(session *telegramImportSession) telegramImportSessionDTO {
	progress := telegramImportProgressDTO{
		Total:     len(session.Items),
		Processed: session.CurrentIndex,
		Remaining: max(0, len(session.Items)-session.CurrentIndex),
		Skipped:   len(session.SkippedItems),
		Saved:     session.SavedCount,
	}

	dto := telegramImportSessionDTO{
		SessionID:       session.ID,
		Status:          session.Status,
		ChannelUsername: session.ChannelUsername,
		Progress:        progress,
		CreatedAt:       session.CreatedAt,
		UpdatedAt:       session.UpdatedAt,
	}

	item, ok := session.currentItem()
	if !ok {
		return dto
	}

	dto.CurrentTrack = &telegramCurrentTrackDTO{
		MessageID:           item.MessageID,
		MessageLink:         item.MessageLink,
		ParsedTitle:         item.ParsedTitle,
		FileName:            item.FileName,
		MimeType:            item.MimeType,
		SizeBytes:           item.SizeBytes,
		TempFileDownloadURL: "/telegram/import-sessions/current/audio",
	}
	return dto
}

func (s *telegramImportSession) currentItem() (telegramScannedTrack, bool) {
	if s.CurrentIndex < 0 || s.CurrentIndex >= len(s.Items) {
		return telegramScannedTrack{}, false
	}
	return s.Items[s.CurrentIndex], true
}

func telegramStatusHandler(service *telegramImportService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, err := service.Status(r.Context())
		if err != nil {
			log.Printf("telegram status error: %v", err)
			http.Error(w, "failed to read telegram status", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, status)
	})
}

func telegramAuthRequestHandler(service *telegramImportService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req telegramAuthRequest
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := service.BeginLogin(r.Context(), req.PhoneNumber); err != nil {
			writeTelegramError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func telegramAuthConfirmHandler(service *telegramImportService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req telegramAuthConfirmRequest
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		status, err := service.ConfirmLogin(r.Context(), req.PhoneNumber, req.Code)
		if err != nil {
			writeTelegramError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, status)
	})
}

func telegramAuthPasswordHandler(service *telegramImportService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req telegramAuthPasswordRequest
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		status, err := service.SubmitPassword(r.Context(), req.Password)
		if err != nil {
			writeTelegramError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, status)
	})
}

func telegramStartImportHandler(service *telegramImportService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}

		var req telegramStartImportRequest
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		session, err := service.StartSession(r.Context(), userID, req.ChannelUsername, req.ReplaceExisting)
		if err != nil {
			writeTelegramError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, session)
	})
}

func telegramCurrentImportHandler(service *telegramImportService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		session, err := service.CurrentSession(r.Context(), userID)
		if err != nil {
			writeTelegramError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, session)
	})
}

func telegramSkipImportHandler(service *telegramImportService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		session, err := service.SkipCurrent(userID)
		if err != nil {
			writeTelegramError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, session)
	})
}

func telegramSaveImportHandler(service *telegramImportService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}

		var req telegramSaveTrackRequest
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		session, _, err := service.SaveCurrent(r.Context(), userID, req)
		if err != nil {
			writeTelegramError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, session)
	})
}

func telegramCancelImportHandler(service *telegramImportService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if err := service.CancelSession(userID); err != nil {
			writeTelegramError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func telegramCurrentAudioHandler(service *telegramImportService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		path, fileName, err := service.CurrentAudioPath(r.Context(), userID)
		if err != nil {
			writeTelegramError(w, err)
			return
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fileName))
		http.ServeFile(w, r, path)
	})
}

func telegramSkippedReportHandler(service *telegramImportService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		report, err := service.SkippedReport(userID)
		if err != nil {
			writeTelegramError(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="telegram-skipped-items.csv"`)
		_, _ = w.Write(report)
	})
}

func writeTelegramError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errTelegramNotConfigured):
		http.Error(w, err.Error(), http.StatusFailedDependency)
	case errors.Is(err, errTelegramNotAuthorized), errors.Is(err, errTelegramAuthPendingMissing), errors.Is(err, errTelegramAuthPasswordNeeded):
		http.Error(w, err.Error(), http.StatusUnauthorized)
	case errors.Is(err, errTelegramInvalidChannel), errors.Is(err, errTelegramNoAudioTracks), errors.Is(err, errTelegramPasswordNotPending), errors.Is(err, errTelegramScanStalled):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, errTelegramSessionActive):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, errTelegramSessionNotFound), errors.Is(err, errTelegramTempFileMissing):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errInvalidTrack):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func resolvePublicChannel(ctx context.Context, api *tg.Client, username string) (*tg.Channel, *tg.InputPeerChannel, error) {
	resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{
		Username: username,
	})
	if err != nil {
		return nil, nil, errTelegramInvalidChannel
	}

	peerChannel, ok := resolved.Peer.(*tg.PeerChannel)
	if !ok {
		return nil, nil, errTelegramInvalidChannel
	}

	for _, chat := range resolved.Chats {
		channel, ok := chat.(*tg.Channel)
		if !ok {
			continue
		}
		if channel.ID != peerChannel.ChannelID {
			continue
		}
		if strings.TrimSpace(channel.Username) == "" || !channel.Broadcast {
			return nil, nil, errTelegramInvalidChannel
		}
		return channel, &tg.InputPeerChannel{
			ChannelID:  channel.ID,
			AccessHash: channel.AccessHash,
		}, nil
	}

	return nil, nil, errTelegramInvalidChannel
}

func extractMessages(history tg.MessagesMessagesClass) []*tg.Message {
	var source []tg.MessageClass
	switch h := history.(type) {
	case *tg.MessagesMessages:
		source = h.Messages
	case *tg.MessagesMessagesSlice:
		source = h.Messages
	case *tg.MessagesChannelMessages:
		source = h.Messages
	default:
		return nil
	}

	items := make([]*tg.Message, 0, len(source))
	for _, message := range source {
		plain, ok := message.(*tg.Message)
		if !ok {
			continue
		}
		items = append(items, plain)
	}
	return items
}

func buildScannedTrack(channelUsername string, message *tg.Message) (telegramScannedTrack, bool) {
	media, ok := message.Media.(*tg.MessageMediaDocument)
	if !ok {
		return telegramScannedTrack{}, false
	}
	document, ok := media.Document.(*tg.Document)
	if !ok {
		return telegramScannedTrack{}, false
	}

	fileName := ""
	title := ""
	performer := ""
	audioAttrFound := false
	for _, attribute := range document.Attributes {
		switch attr := attribute.(type) {
		case *tg.DocumentAttributeFilename:
			fileName = strings.TrimSpace(attr.FileName)
		case *tg.DocumentAttributeAudio:
			if attr.Voice {
				return telegramScannedTrack{}, false
			}
			audioAttrFound = true
			title = strings.TrimSpace(attr.Title)
			performer = strings.TrimSpace(attr.Performer)
		}
	}

	if !audioAttrFound && !strings.HasPrefix(strings.ToLower(document.MimeType), "audio/") {
		return telegramScannedTrack{}, false
	}

	if fileName == "" {
		fileName = fmt.Sprintf("telegram-%d.bin", message.ID)
	}

	return telegramScannedTrack{
		MessageID:   message.ID,
		MessageLink: fmt.Sprintf("https://t.me/%s/%d", channelUsername, message.ID),
		FileName:    fileName,
		MimeType:    document.MimeType,
		SizeBytes:   document.Size,
		ParsedTitle: deriveParsedTitle(title, performer, fileName),
		DocumentID:  document.ID,
		AccessHash:  document.AccessHash,
		FileRef:     append([]byte(nil), document.FileReference...),
		DCID:        document.DCID,
	}, true
}

func deriveParsedTitle(title, performer, fileName string) string {
	switch {
	case performer != "" && title != "":
		return performer + " - " + title
	case title != "":
		return title
	default:
		ext := filepath.Ext(fileName)
		return strings.TrimSpace(strings.TrimSuffix(fileName, ext))
	}
}

func formatTelegramUserIdentifier(user *tg.User) string {
	if user == nil {
		return ""
	}
	if username := strings.TrimSpace(user.Username); username != "" {
		return "@" + username
	}
	if phone := strings.TrimSpace(user.Phone); phone != "" {
		return "+" + phone
	}
	return strconv.FormatInt(user.ID, 10)
}

func normalizeTelegramChannelUsername(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "https://t.me/")
	value = strings.TrimPrefix(value, "http://t.me/")
	value = strings.TrimPrefix(value, "@")
	value = strings.Trim(value, "/")
	return value
}

func buildTempFileName(index int, fileName string) string {
	safeName, err := sanitizeSongFileName(fileName)
	if err != nil {
		safeName = fmt.Sprintf("track-%d.bin", index+1)
	}
	return fmt.Sprintf("%03d-%s", index+1, safeName)
}

func uniqueFileName(dir, fileName string) (string, error) {
	base := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	ext := filepath.Ext(fileName)
	candidate := fileName
	for i := 0; ; i++ {
		path := filepath.Join(dir, candidate)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
		candidate = fmt.Sprintf("%s-%d%s", base, i+1, ext)
	}
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}

	input, err := os.Open(src)
	if err != nil {
		return err
	}
	defer input.Close()

	output, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}

	copyErr := false
	if _, err := io.Copy(output, input); err != nil {
		copyErr = true
	}
	if err := output.Close(); err != nil {
		copyErr = true
	}
	if copyErr {
		_ = os.Remove(dst)
		return errors.New("failed to move telegram temp file")
	}

	return os.Remove(src)
}

func max(left, right int) int {
	if left > right {
		return left
	}
	return right
}
