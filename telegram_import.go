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

	"github.com/getsentry/sentry-go"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	tauth "github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

const (
	telegramImportStatusActive    = "active"
	telegramImportStatusCompleted = "completed"
	telegramScanBatchSize         = 100
	telegramScanMaxPages          = 100
	telegramScanMaxTracks         = 5000
	telegramPendingLoginTTL       = 10 * time.Minute
	telegramCompletedSessionTTL   = time.Hour
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
	errTelegramInvalidPhoneNumber = errors.New("invalid telegram phone number")
	errTelegramInvalidLoginCode   = errors.New("invalid or expired telegram login code")
	errTelegramInvalidRequest     = errors.New("invalid telegram request")
	errTelegramSessionChanged     = errors.New("telegram import session changed")
	errTelegramStorageFailure     = errors.New("telegram storage operation failed")
	errTelegramTrackTooLarge      = errors.New("telegram track exceeds the upload size limit")
	errTelegramScanLimitExceeded  = errors.New("telegram channel scan exceeds the supported limit")
	errTelegramShuttingDown       = errors.New("telegram import service is shutting down")
)

type telegramUpstreamError struct {
	operation string
	err       error
}

func (e *telegramUpstreamError) Error() string {
	return fmt.Sprintf("telegram %s failed: %v", e.operation, e.err)
}

func (e *telegramUpstreamError) Unwrap() error {
	return e.err
}

func wrapTelegramUpstreamError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errTelegramNotConfigured) ||
		errors.Is(err, errTelegramNotAuthorized) ||
		errors.Is(err, errTelegramInvalidChannel) ||
		errors.Is(err, errTelegramNoAudioTracks) ||
		errors.Is(err, errTelegramInvalidPhoneNumber) ||
		errors.Is(err, errTelegramInvalidLoginCode) ||
		errors.Is(err, errTelegramStorageFailure) ||
		errors.Is(err, errTelegramTrackTooLarge) ||
		errors.Is(err, errTelegramScanLimitExceeded) ||
		errors.Is(err, errTelegramShuttingDown) {
		return err
	}
	return &telegramUpstreamError{operation: operation, err: err}
}

func isTelegramUpstreamError(err error) bool {
	var upstreamErr *telegramUpstreamError
	return errors.As(err, &upstreamErr)
}

type telegramCleanupError struct {
	operation string
	err       error
}

func newTelegramCleanupError(operation string, err error) error {
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return &telegramCleanupError{operation: operation, err: err}
}

func (e *telegramCleanupError) Error() string {
	return fmt.Sprintf("telegram cleanup failed during %s: %v", e.operation, e.err)
}

func (e *telegramCleanupError) Unwrap() error {
	return e.err
}

func hasTelegramCleanupError(err error) bool {
	var cleanupErr *telegramCleanupError
	return errors.As(err, &cleanupErr)
}

func sanitizeTelegramCapturedError(err error) error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		sanitized := make([]error, 0, len(causes))
		for _, cause := range causes {
			if cause != nil {
				sanitized = append(sanitized, sanitizeTelegramCapturedError(cause))
			}
		}
		return errors.Join(sanitized...)
	}
	if cleanupErr, ok := err.(*telegramCleanupError); ok {
		return &telegramCleanupError{
			operation: cleanupErr.operation,
			err:       sanitizeTelegramCapturedError(cleanupErr.err),
		}
	}
	if upstreamErr, ok := err.(*telegramUpstreamError); ok {
		return &telegramUpstreamError{
			operation: upstreamErr.operation,
			err:       sanitizeTelegramCapturedError(upstreamErr.err),
		}
	}
	return sanitizeTelegramErrorCause(err)
}

func sanitizeTelegramErrorCause(err error) error {
	if err == nil {
		return nil
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return fmt.Errorf("filesystem %s failed: %w", pathErr.Op, pathErr.Err)
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return fmt.Errorf("filesystem %s failed: %w", linkErr.Op, linkErr.Err)
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("telegram network %s failed: %w", urlErr.Op, sanitizeTelegramErrorCause(urlErr.Err))
	}
	return err
}

func removeFileIfExists(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func classifyTelegramAuthError(operation string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case tg.IsPhoneNumberInvalid(err), tg.IsPhoneNumberBanned(err), tg.IsPhoneNumberAppSignupForbidden(err):
		return errTelegramInvalidPhoneNumber
	case tg.IsPhoneCodeEmpty(err), tg.IsPhoneCodeExpired(err), tg.IsPhoneCodeHashEmpty(err), tg.IsPhoneCodeInvalid(err), tg.IsPhoneHashExpired(err):
		return errTelegramInvalidLoginCode
	default:
		return wrapTelegramUpstreamError(operation, err)
	}
}

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
	ImportTempDir      string `json:"-"`
	SessionStorageFile string `json:"-"`
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
	ScanPublicChannel(ctx context.Context, channelUsername string, startMessageID int) ([]telegramScannedTrack, error)
	DownloadTrack(ctx context.Context, item telegramScannedTrack, destinationPath string) error
}

type telegramHistoryClient interface {
	MessagesGetHistory(ctx context.Context, request *tg.MessagesGetHistoryRequest) (tg.MessagesMessagesClass, error)
}

type telegramUsernameResolver interface {
	ContactsResolveUsername(ctx context.Context, request *tg.ContactsResolveUsernameRequest) (*tg.ContactsResolvedPeer, error)
}

type gotdTelegramGateway struct {
	cfg       telegramConfig
	runGateMu sync.Mutex
	runGate   chan struct{}
}

func newGotdTelegramGateway(cfg telegramConfig) *gotdTelegramGateway {
	return &gotdTelegramGateway{
		cfg:     cfg,
		runGate: makeTelegramRunGate(),
	}
}

func makeTelegramRunGate() chan struct{} {
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return gate
}

func (g *gotdTelegramGateway) acquireRun(ctx context.Context) (func(), error) {
	g.runGateMu.Lock()
	if g.runGate == nil {
		g.runGate = makeTelegramRunGate()
	}
	gate := g.runGate
	g.runGateMu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-gate:
		return func() { gate <- struct{}{} }, nil
	}
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
	release, err := g.acquireRun(ctx)
	if err != nil {
		return err
	}
	defer release()

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
	return status, wrapTelegramUpstreamError("read authorization status", err)
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
	return codeHash, classifyTelegramAuthError("request login code", err)
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
	return result, classifyTelegramAuthError("confirm login code", err)
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
	return result, wrapTelegramUpstreamError("submit login password", err)
}

func (g *gotdTelegramGateway) ScanPublicChannel(ctx context.Context, channelUsername string, startMessageID int) ([]telegramScannedTrack, error) {
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

		items, err = scanPublicChannelHistory(runCtx, api, channel.Username, inputPeer, startMessageID)
		return err
	})
	if err != nil {
		return nil, wrapTelegramUpstreamError("scan public channel", err)
	}
	if len(items) == 0 {
		return nil, errTelegramNoAudioTracks
	}

	return items, nil
}

func scanPublicChannelHistory(ctx context.Context, api telegramHistoryClient, channelUsername string, inputPeer tg.InputPeerClass, startMessageID int) ([]telegramScannedTrack, error) {
	items := make([]telegramScannedTrack, 0)
	offsetID := 0
	pageCount := 0
	minMessageID := 0
	if startMessageID > 0 {
		// Telegram treats MinID as exclusive, so subtract one to include the
		// requested starting message itself.
		minMessageID = startMessageID - 1
	}
	seenMessageIDs := make(map[int]struct{})
	for {
		if pageCount >= telegramScanMaxPages {
			return nil, fmt.Errorf("%w: at most %d history pages can be scanned", errTelegramScanLimitExceeded, telegramScanMaxPages)
		}
		previousOffsetID := offsetID
		var history tg.MessagesMessagesClass
		for {
			var err error
			history, err = api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
				Peer:     inputPeer,
				OffsetID: offsetID,
				Limit:    telegramScanBatchSize,
				MinID:    minMessageID,
			})
			if flood, waitErr := tgerr.FloodWait(ctx, err); waitErr != nil {
				if flood {
					continue
				}
				return nil, waitErr
			}
			break
		}

		messages := extractMessages(history)
		if len(messages) == 0 {
			break
		}
		pageCount++

		batchProgressed := false
		for _, message := range messages {
			if message.ID < startMessageID {
				continue
			}
			if _, seen := seenMessageIDs[message.ID]; seen {
				continue
			}
			seenMessageIDs[message.ID] = struct{}{}
			offsetID = message.ID
			batchProgressed = true
			item, ok := buildScannedTrack(channelUsername, message)
			if !ok {
				continue
			}
			if len(items) >= telegramScanMaxTracks {
				return nil, fmt.Errorf("%w: at most %d audio tracks can be imported at once", errTelegramScanLimitExceeded, telegramScanMaxTracks)
			}
			items = append(items, item)
		}

		if !batchProgressed || offsetID == previousOffsetID {
			return nil, errTelegramScanStalled
		}
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].MessageID < items[j].MessageID
	})
	return items, nil
}

func (g *gotdTelegramGateway) DownloadTrack(ctx context.Context, item telegramScannedTrack, destinationPath string) error {
	if item.SizeBytes > maxSongUploadSize {
		return errTelegramTrackTooLarge
	}
	err := g.runWithTimeout(ctx, g.cfg.DownloadTimeout, func(runCtx context.Context, client *telegram.Client) error {
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
			return fmt.Errorf("%w: create download directory: %w", errTelegramStorageFailure, err)
		}
		output, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("%w: create download file: %w", errTelegramStorageFailure, err)
		}
		limitedOutput := &telegramDownloadLimitWriter{
			writer:    output,
			remaining: maxSongUploadSize,
		}
		_, downloadErr := downloader.NewDownloader().Download(client.API(), location).Stream(runCtx, limitedOutput)
		closeErr := output.Close()
		if downloadErr != nil || closeErr != nil {
			if closeErr != nil {
				closeErr = fmt.Errorf("%w: close download file: %w", errTelegramStorageFailure, closeErr)
			}
			err := errors.Join(downloadErr, closeErr)
			if cleanupErr := removeFileIfExists(destinationPath); cleanupErr != nil {
				err = errors.Join(err, newTelegramCleanupError("remove partial bounded download", cleanupErr))
			}
			return err
		}

		info, err := os.Stat(destinationPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return errTelegramTempFileMissing
			}
			return fmt.Errorf("%w: inspect downloaded track: %w", errTelegramStorageFailure, err)
		}
		if info.Size() == 0 {
			removeErr := removeFileIfExists(destinationPath)
			if removeErr != nil {
				return errors.Join(errTelegramTempFileMissing, newTelegramCleanupError("remove empty download", removeErr))
			}
			return errTelegramTempFileMissing
		}
		if info.Size() > maxSongUploadSize {
			err := error(errTelegramTrackTooLarge)
			if removeErr := removeFileIfExists(destinationPath); removeErr != nil {
				err = errors.Join(err, newTelegramCleanupError("remove oversized download", removeErr))
			}
			return err
		}
		return nil
	})
	return wrapTelegramUpstreamError("download track", err)
}

type telegramDownloadLimitWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *telegramDownloadLimitWriter) Write(data []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, errTelegramTrackTooLarge
	}

	writable := data
	exceedsLimit := int64(len(data)) > w.remaining
	if exceedsLimit {
		writable = data[:int(w.remaining)]
	}

	written, err := w.writer.Write(writable)
	w.remaining -= int64(written)
	if err != nil {
		return written, fmt.Errorf("%w: write download file: %w", errTelegramStorageFailure, err)
	}
	if written != len(writable) {
		return written, fmt.Errorf("%w: write download file: %w", errTelegramStorageFailure, io.ErrShortWrite)
	}
	if exceedsLimit {
		return written, errTelegramTrackTooLarge
	}
	return written, nil
}

type telegramPendingLogin struct {
	PhoneNumber      string
	CodeHash         string
	RequestedAt      time.Time
	PasswordRequired bool
}

type telegramImportSession struct {
	downloadMu      sync.Mutex
	ID              string
	UserID          int64
	Status          string
	ChannelUsername string
	Items           []telegramScannedTrack
	TotalItems      int
	CurrentIndex    int
	SkippedItems    []telegramSkippedItem
	SavedCount      int
	TempFiles       map[int]string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type telegramUserOperationLock struct {
	gate chan struct{}
	refs int
}

func newTelegramUserOperationLock() *telegramUserOperationLock {
	lock := &telegramUserOperationLock{gate: make(chan struct{}, 1)}
	lock.gate <- struct{}{}
	return lock
}

type telegramAudioLease struct {
	File     *os.File
	FileName string
	ModTime  time.Time
}

func (l *telegramAudioLease) Close() error {
	if l == nil || l.File == nil {
		return nil
	}
	return l.File.Close()
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
	StartMessageID  int    `json:"startMessageId"`
	ReplaceExisting bool   `json:"replaceExisting"`
}

type telegramSaveTrackRequest struct {
	Name           string           `json:"name"`
	AuthorIDs      []int64          `json:"authorIds"`
	AlbumID        int64            `json:"albumId"`
	AlbumOrder     int              `json:"albumOrder"`
	AdditionalInfo []additionalInfo `json:"additionalInfo"`
	SourceMetadata []sourceMetadata `json:"sourceMetadata"`
}

type telegramImportService struct {
	authMu        sync.Mutex
	mu            sync.Mutex
	cfg           telegramConfig
	gateway       telegramGateway
	store         *trackStore
	songsDir      string
	pendingLogins map[int64]*telegramPendingLogin
	sessions      map[int64]*telegramImportSession
	operationMu   map[int64]*telegramUserOperationLock
	now           func() time.Time
	closing       bool
}

func newTelegramImportService(cfg telegramConfig, gateway telegramGateway, store *trackStore, songsDir string) *telegramImportService {
	return &telegramImportService{
		cfg:           cfg,
		gateway:       gateway,
		store:         store,
		songsDir:      songsDir,
		pendingLogins: make(map[int64]*telegramPendingLogin),
		sessions:      make(map[int64]*telegramImportSession),
		operationMu:   make(map[int64]*telegramUserOperationLock),
		now:           time.Now,
	}
}

func (s *telegramImportService) lockUserOperation(ctx context.Context, userID int64) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	operationMu := s.operationMu[userID]
	if operationMu == nil {
		operationMu = newTelegramUserOperationLock()
		s.operationMu[userID] = operationMu
	}
	operationMu.refs++
	s.mu.Unlock()

	select {
	case <-ctx.Done():
		s.releaseUserOperationReference(userID, operationMu, false)
		return nil, ctx.Err()
	case <-operationMu.gate:
		if err := ctx.Err(); err != nil {
			s.releaseUserOperationReference(userID, operationMu, true)
			return nil, err
		}
	}

	var releaseOnce sync.Once
	return func() {
		releaseOnce.Do(func() {
			s.releaseUserOperationReference(userID, operationMu, true)
		})
	}, nil
}

func (s *telegramImportService) releaseUserOperationReference(userID int64, operationMu *telegramUserOperationLock, held bool) {
	s.mu.Lock()
	if held {
		operationMu.gate <- struct{}{}
	}
	operationMu.refs--
	if operationMu.refs == 0 && s.operationMu[userID] == operationMu {
		delete(s.operationMu, userID)
	}
	s.mu.Unlock()
}

func (s *telegramImportService) nowUTC() time.Time {
	return s.now().UTC()
}

func (s *telegramImportService) checkContextAndAvailabilityLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closing {
		return errTelegramShuttingDown
	}
	return nil
}

func (s *telegramImportService) evictExpiredCompletedSessionLocked(userID int64, now time.Time) *telegramImportSession {
	session := s.sessions[userID]
	if session == nil || session.Status != telegramImportStatusCompleted || now.Sub(session.UpdatedAt) < telegramCompletedSessionTTL {
		return nil
	}
	delete(s.sessions, userID)
	return session
}

func (s *telegramImportService) compactCompletedSessionLocked(session *telegramImportSession) {
	if session == nil || session.Status != telegramImportStatusCompleted {
		return
	}
	if session.TotalItems == 0 {
		session.TotalItems = len(session.Items)
	}
	session.CurrentIndex = session.TotalItems
	session.Items = nil
	session.TempFiles = nil
}

func reportTelegramCleanupError(ctx context.Context, operation string, err error) {
	cleanupErr := err
	if !hasTelegramCleanupError(cleanupErr) {
		cleanupErr = newTelegramCleanupError(operation, err)
	}
	if cleanupErr == nil {
		return
	}
	log.Printf("telegram cleanup failed operation=%s error_type=%T", operation, err)
	captureSentryError(ctx, sanitizeTelegramCapturedError(cleanupErr), "telegram", operation)
}

func startTelegramSpan(ctx context.Context, operation, description string) (context.Context, *sentry.Span) {
	parent := sentry.SpanFromContext(ctx)
	if parent == nil {
		return ctx, nil
	}
	span := parent.StartChild(operation, sentry.WithDescription(description))
	span.SetTag("component", "telegram")
	return span.Context(), span
}

func finishTelegramSpan(span *sentry.Span, err error) {
	if span == nil {
		return
	}
	switch {
	case errors.Is(err, context.Canceled):
		span.Status = sentry.SpanStatusCanceled
	case errors.Is(err, context.DeadlineExceeded):
		span.Status = sentry.SpanStatusDeadlineExceeded
	case err != nil:
		span.Status = sentry.SpanStatusInternalError
	default:
		span.Status = sentry.SpanStatusOK
	}
	span.Finish()
}

func telegramPendingLoginUserID(ctx context.Context) int64 {
	userID, _ := userIDFromContext(ctx)
	return userID
}

func (s *telegramImportService) pendingLoginLocked(userID int64) (*telegramPendingLogin, bool) {
	pending := s.pendingLogins[userID]
	if pending == nil {
		return nil, false
	}
	if s.nowUTC().Sub(pending.RequestedAt) < telegramPendingLoginTTL {
		return pending, false
	}
	delete(s.pendingLogins, userID)
	return nil, true
}

func (s *telegramImportService) checkContextAndAvailability(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkContextAndAvailabilityLocked(ctx)
}

func (s *telegramImportService) Status(ctx context.Context) (telegramAuthStatus, error) {
	if err := s.checkContextAndAvailability(ctx); err != nil {
		return telegramAuthStatus{}, err
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()

	gatewayCtx, span := startTelegramSpan(ctx, "telegram.auth", "status")
	status, err := s.gateway.Status(gatewayCtx)
	finishTelegramSpan(span, err)
	if err != nil {
		return telegramAuthStatus{}, err
	}
	userID := telegramPendingLoginUserID(ctx)
	if pending, _ := s.pendingLoginLocked(userID); pending != nil && pending.PasswordRequired {
		status.PasswordRequired = true
	}
	return status, nil
}

func (s *telegramImportService) BeginLogin(ctx context.Context, phoneNumber string) error {
	phoneNumber = strings.TrimSpace(phoneNumber)
	if phoneNumber == "" {
		return fmt.Errorf("%w: phoneNumber is required", errTelegramInvalidRequest)
	}
	if err := s.checkContextAndAvailability(ctx); err != nil {
		return err
	}

	s.authMu.Lock()
	defer s.authMu.Unlock()

	gatewayCtx, span := startTelegramSpan(ctx, "telegram.auth", "request_code")
	codeHash, err := s.gateway.BeginLogin(gatewayCtx, phoneNumber)
	finishTelegramSpan(span, err)
	if err != nil {
		return err
	}
	if err := s.checkContextAndAvailability(ctx); err != nil {
		return err
	}
	userID := telegramPendingLoginUserID(ctx)
	if codeHash == "" {
		delete(s.pendingLogins, userID)
		return nil
	}
	s.pendingLogins[userID] = &telegramPendingLogin{
		PhoneNumber: phoneNumber,
		CodeHash:    codeHash,
		RequestedAt: s.nowUTC(),
	}
	return nil
}

func (s *telegramImportService) ConfirmLogin(ctx context.Context, phoneNumber, code string) (telegramAuthStatus, error) {
	phoneNumber = strings.TrimSpace(phoneNumber)
	code = strings.TrimSpace(code)
	if phoneNumber == "" || code == "" {
		return telegramAuthStatus{}, fmt.Errorf("%w: phoneNumber and code are required", errTelegramInvalidRequest)
	}
	if err := s.checkContextAndAvailability(ctx); err != nil {
		return telegramAuthStatus{}, err
	}

	s.authMu.Lock()
	defer s.authMu.Unlock()

	userID := telegramPendingLoginUserID(ctx)
	pending, expired := s.pendingLoginLocked(userID)
	if pending == nil {
		if expired {
			return telegramAuthStatus{}, errTelegramInvalidLoginCode
		}
		return telegramAuthStatus{}, errTelegramAuthPendingMissing
	}
	if pending.PhoneNumber != phoneNumber {
		return telegramAuthStatus{}, fmt.Errorf("%w: phoneNumber does not match pending telegram login request", errTelegramInvalidRequest)
	}

	gatewayCtx, span := startTelegramSpan(ctx, "telegram.auth", "confirm_code")
	status, err := s.gateway.ConfirmLogin(gatewayCtx, phoneNumber, code, pending.CodeHash)
	finishTelegramSpan(span, err)
	if err != nil {
		return telegramAuthStatus{}, err
	}
	if err := s.checkContextAndAvailability(ctx); err != nil {
		return telegramAuthStatus{}, err
	}

	if status.PasswordRequired {
		pending.PasswordRequired = true
	} else {
		delete(s.pendingLogins, userID)
	}
	return status, nil
}

func (s *telegramImportService) SubmitPassword(ctx context.Context, password string) (telegramAuthStatus, error) {
	password = strings.TrimSpace(password)
	if password == "" {
		return telegramAuthStatus{}, fmt.Errorf("%w: password is required", errTelegramInvalidRequest)
	}
	if err := s.checkContextAndAvailability(ctx); err != nil {
		return telegramAuthStatus{}, err
	}

	s.authMu.Lock()
	defer s.authMu.Unlock()

	userID := telegramPendingLoginUserID(ctx)
	pending, expired := s.pendingLoginLocked(userID)
	if pending == nil {
		if expired {
			return telegramAuthStatus{}, errTelegramAuthPendingMissing
		}
		return telegramAuthStatus{}, errTelegramAuthPendingMissing
	}
	if !pending.PasswordRequired {
		return telegramAuthStatus{}, errTelegramPasswordNotPending
	}

	gatewayCtx, span := startTelegramSpan(ctx, "telegram.auth", "submit_password")
	status, err := s.gateway.PasswordLogin(gatewayCtx, password)
	finishTelegramSpan(span, err)
	if err != nil {
		return telegramAuthStatus{}, err
	}
	if err := s.checkContextAndAvailability(ctx); err != nil {
		return telegramAuthStatus{}, err
	}

	delete(s.pendingLogins, userID)
	return status, nil
}

func (s *telegramImportService) StartSession(ctx context.Context, userID int64, channelUsername string, startMessageID int, replaceExisting bool) (telegramImportSessionDTO, error) {
	channelUsername = normalizeTelegramChannelUsername(channelUsername)
	if channelUsername == "" {
		return telegramImportSessionDTO{}, fmt.Errorf("%w: channelUsername is required", errTelegramInvalidRequest)
	}
	if startMessageID < 0 {
		return telegramImportSessionDTO{}, fmt.Errorf("%w: startMessageId must be greater than or equal to 0", errTelegramInvalidRequest)
	}
	unlockOperation, err := s.lockUserOperation(ctx, userID)
	if err != nil {
		return telegramImportSessionDTO{}, err
	}
	defer unlockOperation()

	s.mu.Lock()
	if err := s.checkContextAndAvailabilityLocked(ctx); err != nil {
		s.mu.Unlock()
		return telegramImportSessionDTO{}, err
	}
	expired := s.evictExpiredCompletedSessionLocked(userID, s.nowUTC())
	if existing, ok := s.sessions[userID]; ok && existing.Status == telegramImportStatusActive && !replaceExisting {
		s.mu.Unlock()
		reportTelegramCleanupError(ctx, "import.start.expired_cleanup", s.cleanupSessionFiles(expired))
		return telegramImportSessionDTO{}, errTelegramSessionActive
	}
	existing := s.sessions[userID]
	s.mu.Unlock()
	reportTelegramCleanupError(ctx, "import.start.expired_cleanup", s.cleanupSessionFiles(expired))

	gatewayCtx, span := startTelegramSpan(ctx, "telegram.scan", "public_channel")
	items, err := s.gateway.ScanPublicChannel(gatewayCtx, channelUsername, startMessageID)
	finishTelegramSpan(span, err)
	if err != nil {
		return telegramImportSessionDTO{}, err
	}
	if startMessageID > 0 {
		filtered := items[:0]
		for _, item := range items {
			if item.MessageID < startMessageID {
				continue
			}
			filtered = append(filtered, item)
		}
		items = filtered
	}
	if len(items) == 0 {
		return telegramImportSessionDTO{}, errTelegramNoAudioTracks
	}
	if len(items) > telegramScanMaxTracks {
		return telegramImportSessionDTO{}, fmt.Errorf("%w: at most %d audio tracks can be imported at once", errTelegramScanLimitExceeded, telegramScanMaxTracks)
	}
	if err := ctx.Err(); err != nil {
		return telegramImportSessionDTO{}, err
	}

	sessionID, err := randomToken(16)
	if err != nil {
		return telegramImportSessionDTO{}, err
	}

	now := s.nowUTC()
	session := &telegramImportSession{
		ID:              sessionID,
		UserID:          userID,
		Status:          telegramImportStatusActive,
		ChannelUsername: channelUsername,
		Items:           items,
		TotalItems:      len(items),
		CurrentIndex:    0,
		SkippedItems:    []telegramSkippedItem{},
		SavedCount:      0,
		TempFiles:       make(map[int]string),
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	initialPath, err := s.downloadTempFile(ctx, session, 0, items[0])
	if err != nil {
		if cleanupErr := s.cleanupSessionFiles(session); cleanupErr != nil {
			err = errors.Join(err, newTelegramCleanupError("cleanup failed session start", cleanupErr))
		}
		return telegramImportSessionDTO{}, err
	}
	session.TempFiles[0] = initialPath

	s.mu.Lock()
	if err := s.checkContextAndAvailabilityLocked(ctx); err != nil {
		s.mu.Unlock()
		if cleanupErr := s.cleanupSessionFiles(session); cleanupErr != nil {
			err = errors.Join(err, newTelegramCleanupError("cleanup canceled session start", cleanupErr))
		}
		return telegramImportSessionDTO{}, err
	}
	if s.sessions[userID] != existing {
		s.mu.Unlock()
		err := error(errTelegramSessionChanged)
		if cleanupErr := s.cleanupSessionFiles(session); cleanupErr != nil {
			err = errors.Join(err, newTelegramCleanupError("cleanup stale session start", cleanupErr))
		}
		return telegramImportSessionDTO{}, err
	}
	s.sessions[userID] = session
	dto := s.buildSessionDTO(session)
	s.mu.Unlock()

	reportTelegramCleanupError(ctx, "import.start.cleanup", s.cleanupSessionFiles(existing))
	return dto, nil
}

func (s *telegramImportService) CurrentSession(ctx context.Context, userID int64) (telegramImportSessionDTO, error) {
	unlockOperation, err := s.lockUserOperation(ctx, userID)
	if err != nil {
		return telegramImportSessionDTO{}, err
	}
	defer unlockOperation()
	return s.currentSession(ctx, userID)
}

func (s *telegramImportService) currentSession(ctx context.Context, userID int64) (telegramImportSessionDTO, error) {
	s.mu.Lock()
	if err := s.checkContextAndAvailabilityLocked(ctx); err != nil {
		s.mu.Unlock()
		return telegramImportSessionDTO{}, err
	}
	expired := s.evictExpiredCompletedSessionLocked(userID, s.nowUTC())
	session, ok := s.sessions[userID]
	if !ok {
		s.mu.Unlock()
		reportTelegramCleanupError(ctx, "import.current.expired_cleanup", s.cleanupSessionFiles(expired))
		return telegramImportSessionDTO{}, errTelegramSessionNotFound
	}

	currentItem, hasCurrent := session.currentItem()
	currentIndex := session.CurrentIndex
	s.mu.Unlock()
	reportTelegramCleanupError(ctx, "import.current.expired_cleanup", s.cleanupSessionFiles(expired))

	if hasCurrent {
		if err := s.ensureTempFile(ctx, session, currentIndex, currentItem); err != nil {
			return telegramImportSessionDTO{}, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkContextAndAvailabilityLocked(ctx); err != nil {
		return telegramImportSessionDTO{}, err
	}
	currentSession, ok := s.sessions[userID]
	if !ok || currentSession != session {
		return telegramImportSessionDTO{}, errTelegramSessionChanged
	}
	return s.buildSessionDTO(currentSession), nil
}

func (s *telegramImportService) SkipCurrent(userID int64) (telegramImportSessionDTO, error) {
	return s.skipCurrentWithContext(context.Background(), userID)
}

func (s *telegramImportService) skipCurrentWithContext(ctx context.Context, userID int64) (telegramImportSessionDTO, error) {
	unlockOperation, err := s.lockUserOperation(ctx, userID)
	if err != nil {
		return telegramImportSessionDTO{}, err
	}
	defer unlockOperation()

	s.mu.Lock()
	if err := s.checkContextAndAvailabilityLocked(ctx); err != nil {
		s.mu.Unlock()
		return telegramImportSessionDTO{}, err
	}
	expired := s.evictExpiredCompletedSessionLocked(userID, s.nowUTC())
	session, ok := s.sessions[userID]
	if !ok {
		s.mu.Unlock()
		reportTelegramCleanupError(ctx, "import.skip.expired_cleanup", s.cleanupSessionFiles(expired))
		return telegramImportSessionDTO{}, errTelegramSessionNotFound
	}
	item, ok := session.currentItem()
	if !ok {
		dto := s.buildSessionDTO(session)
		s.mu.Unlock()
		return dto, nil
	}

	session.SkippedItems = append(session.SkippedItems, telegramSkippedItem{
		ParsedTitle: item.ParsedTitle,
		MessageLink: item.MessageLink,
	})
	tempPath := session.TempFiles[session.CurrentIndex]
	if tempPath != "" {
		delete(session.TempFiles, session.CurrentIndex)
	}
	session.CurrentIndex++
	session.UpdatedAt = s.nowUTC()
	completed := false
	if session.CurrentIndex >= len(session.Items) {
		session.Status = telegramImportStatusCompleted
		completed = true
	}
	if completed {
		s.compactCompletedSessionLocked(session)
	}
	dto := s.buildSessionDTO(session)
	s.mu.Unlock()
	var cleanupErr error
	if tempPath != "" {
		cleanupErr = newTelegramCleanupError("remove skipped track", removeFileIfExists(tempPath))
	}
	if completed {
		cleanupErr = errors.Join(cleanupErr, newTelegramCleanupError("remove completed session directory", s.cleanupSessionFiles(session)))
	}
	reportTelegramCleanupError(ctx, "import.skip.cleanup", cleanupErr)
	return dto, nil
}

func (s *telegramImportService) SaveCurrent(ctx context.Context, userID int64, req telegramSaveTrackRequest) (telegramImportSessionDTO, track, error) {
	unlockOperation, err := s.lockUserOperation(ctx, userID)
	if err != nil {
		return telegramImportSessionDTO{}, track{}, err
	}
	defer unlockOperation()

	s.mu.Lock()
	if err := s.checkContextAndAvailabilityLocked(ctx); err != nil {
		s.mu.Unlock()
		return telegramImportSessionDTO{}, track{}, err
	}
	expired := s.evictExpiredCompletedSessionLocked(userID, s.nowUTC())
	session, ok := s.sessions[userID]
	if !ok {
		s.mu.Unlock()
		reportTelegramCleanupError(ctx, "import.save.expired_cleanup", s.cleanupSessionFiles(expired))
		return telegramImportSessionDTO{}, track{}, errTelegramSessionNotFound
	}

	itemIndex := session.CurrentIndex
	item, ok := session.currentItem()
	s.mu.Unlock()
	if !ok {
		current, currentErr := s.currentSession(ctx, userID)
		return current, track{}, currentErr
	}

	tempPath, err := s.ensureTempFilePath(ctx, session, itemIndex, item)
	if err != nil {
		return telegramImportSessionDTO{}, track{}, err
	}
	s.mu.Lock()
	currentSession, current := s.sessions[userID]
	stillCurrent := current && currentSession == session && session.CurrentIndex == itemIndex
	s.mu.Unlock()
	if !stillCurrent {
		return telegramImportSessionDTO{}, track{}, errTelegramSessionChanged
	}
	if err := ctx.Err(); err != nil {
		return telegramImportSessionDTO{}, track{}, err
	}

	songsMutationMu.Lock()
	s.mu.Lock()
	currentSession, current = s.sessions[userID]
	stillCurrent = current && currentSession == session && session.CurrentIndex == itemIndex
	availabilityErr := s.checkContextAndAvailabilityLocked(ctx)
	s.mu.Unlock()
	if availabilityErr != nil {
		songsMutationMu.Unlock()
		return telegramImportSessionDTO{}, track{}, availabilityErr
	}
	if !stillCurrent {
		songsMutationMu.Unlock()
		return telegramImportSessionDTO{}, track{}, errTelegramSessionChanged
	}
	finalName, audioPath, err := s.promoteTempFile(tempPath, item.FileName)
	if err != nil {
		songsMutationMu.Unlock()
		return telegramImportSessionDTO{}, track{}, err
	}

	createdTrack, err := s.store.create(upsertTrackRequest{
		Name:           strings.TrimSpace(req.Name),
		AuthorIDs:      req.AuthorIDs,
		AlbumID:        req.AlbumID,
		AlbumOrder:     req.AlbumOrder,
		AudioFilePath:  audioPath,
		AdditionalInfo: req.AdditionalInfo,
		SourceMetadata: req.SourceMetadata,
	})
	if err != nil {
		if rollbackErr := removeFileIfExists(filepath.Join(s.songsDir, finalName)); rollbackErr != nil {
			err = errors.Join(err, newTelegramCleanupError("rollback promoted track", rollbackErr))
		}
		songsMutationMu.Unlock()
		return telegramImportSessionDTO{}, track{}, err
	}

	s.mu.Lock()
	currentSession, ok = s.sessions[userID]
	if !ok || currentSession != session || session.CurrentIndex != itemIndex {
		s.mu.Unlock()
		songsMutationMu.Unlock()
		return telegramImportSessionDTO{}, createdTrack, errTelegramSessionChanged
	}
	delete(session.TempFiles, itemIndex)
	session.CurrentIndex++
	session.SavedCount++
	session.UpdatedAt = s.nowUTC()
	completed := false
	if session.CurrentIndex >= len(session.Items) {
		session.Status = telegramImportStatusCompleted
		completed = true
	}
	if completed {
		s.compactCompletedSessionLocked(session)
	}
	dto := s.buildSessionDTO(session)
	s.mu.Unlock()
	songsMutationMu.Unlock()
	if completed {
		reportTelegramCleanupError(ctx, "import.save.cleanup", s.cleanupSessionFiles(session))
	}
	return dto, createdTrack, nil
}

func (s *telegramImportService) CancelSession(userID int64) error {
	return s.cancelSessionWithContext(context.Background(), userID)
}

func (s *telegramImportService) cancelSessionWithContext(ctx context.Context, userID int64) error {
	unlockOperation, err := s.lockUserOperation(ctx, userID)
	if err != nil {
		return err
	}
	defer unlockOperation()

	s.mu.Lock()
	if err := s.checkContextAndAvailabilityLocked(ctx); err != nil {
		s.mu.Unlock()
		return err
	}
	expired := s.evictExpiredCompletedSessionLocked(userID, s.nowUTC())
	session, ok := s.sessions[userID]
	if !ok {
		s.mu.Unlock()
		reportTelegramCleanupError(ctx, "import.cancel.expired_cleanup", s.cleanupSessionFiles(expired))
		return errTelegramSessionNotFound
	}
	delete(s.sessions, userID)
	s.mu.Unlock()

	reportTelegramCleanupError(ctx, "import.cancel.cleanup", s.cleanupSessionFiles(session))
	return nil
}

func (s *telegramImportService) CurrentAudioPath(ctx context.Context, userID int64) (string, string, error) {
	unlockOperation, err := s.lockUserOperation(ctx, userID)
	if err != nil {
		return "", "", err
	}
	defer unlockOperation()
	return s.currentAudioPath(ctx, userID)
}

func (s *telegramImportService) OpenCurrentAudio(ctx context.Context, userID int64) (*telegramAudioLease, error) {
	unlockOperation, err := s.lockUserOperation(ctx, userID)
	if err != nil {
		return nil, err
	}
	defer unlockOperation()

	path, fileName, err := s.currentAudioPath(ctx, userID)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errTelegramTempFileMissing
		}
		return nil, fmt.Errorf("open telegram audio lease: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("inspect telegram audio lease: %w", err),
			newTelegramCleanupError("close failed audio lease", file.Close()),
		)
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, newTelegramCleanupError("close canceled audio lease", file.Close()))
	}
	return &telegramAudioLease{File: file, FileName: fileName, ModTime: info.ModTime()}, nil
}

func (s *telegramImportService) currentAudioPath(ctx context.Context, userID int64) (string, string, error) {

	s.mu.Lock()
	if err := s.checkContextAndAvailabilityLocked(ctx); err != nil {
		s.mu.Unlock()
		return "", "", err
	}
	expired := s.evictExpiredCompletedSessionLocked(userID, s.nowUTC())
	session, ok := s.sessions[userID]
	if !ok {
		s.mu.Unlock()
		reportTelegramCleanupError(ctx, "import.audio.expired_cleanup", s.cleanupSessionFiles(expired))
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
	return s.skippedReportWithContext(context.Background(), userID)
}

func (s *telegramImportService) skippedReportWithContext(ctx context.Context, userID int64) ([]byte, error) {
	unlockOperation, err := s.lockUserOperation(ctx, userID)
	if err != nil {
		return nil, err
	}
	defer unlockOperation()

	s.mu.Lock()
	if err := s.checkContextAndAvailabilityLocked(ctx); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	expired := s.evictExpiredCompletedSessionLocked(userID, s.nowUTC())
	session, ok := s.sessions[userID]
	if !ok {
		s.mu.Unlock()
		reportTelegramCleanupError(ctx, "import.skipped_report.expired_cleanup", s.cleanupSessionFiles(expired))
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []byte(buffer.String()), nil
}

func (s *telegramImportService) evictCompletedSessionAfterReport(ctx context.Context, userID int64) error {
	unlockOperation, err := s.lockUserOperation(ctx, userID)
	if err != nil {
		return err
	}
	defer unlockOperation()

	s.mu.Lock()
	session := s.sessions[userID]
	if session == nil || session.Status != telegramImportStatusCompleted {
		s.mu.Unlock()
		return nil
	}
	delete(s.sessions, userID)
	s.mu.Unlock()
	if err := s.cleanupSessionFiles(session); err != nil {
		return newTelegramCleanupError("evict completed reported session", err)
	}
	return nil
}

func (s *telegramImportService) ensureTempFile(ctx context.Context, session *telegramImportSession, index int, item telegramScannedTrack) error {
	_, err := s.ensureTempFilePath(ctx, session, index, item)
	return err
}

func (s *telegramImportService) ensureTempFilePath(ctx context.Context, session *telegramImportSession, index int, item telegramScannedTrack) (string, error) {
	session.downloadMu.Lock()
	defer session.downloadMu.Unlock()

	s.mu.Lock()
	if err := s.checkContextAndAvailabilityLocked(ctx); err != nil {
		s.mu.Unlock()
		return "", err
	}
	currentSession, ok := s.sessions[session.UserID]
	if !ok || currentSession != session {
		s.mu.Unlock()
		return "", errTelegramSessionChanged
	}
	if path := session.TempFiles[index]; path != "" {
		if err := validateTelegramDownloadedFile(path); err == nil {
			s.mu.Unlock()
			return path, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			if errors.Is(err, errTelegramTrackTooLarge) || errors.Is(err, errTelegramTempFileMissing) {
				delete(session.TempFiles, index)
				s.mu.Unlock()
				if cleanupErr := removeFileIfExists(path); cleanupErr != nil {
					err = errors.Join(err, newTelegramCleanupError("remove invalid cached download", cleanupErr))
				}
				return "", err
			}
			s.mu.Unlock()
			return "", fmt.Errorf("inspect telegram temp file: %w", err)
		}
		delete(session.TempFiles, index)
	}
	s.mu.Unlock()

	targetPath, err := s.downloadTempFile(ctx, session, index, item)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	currentSession, ok = s.sessions[session.UserID]
	availabilityErr := s.checkContextAndAvailabilityLocked(ctx)
	if availabilityErr != nil || !ok || currentSession != session || session.CurrentIndex != index {
		s.mu.Unlock()
		staleErr := availabilityErr
		if staleErr == nil {
			staleErr = errTelegramSessionChanged
		}
		if cleanupErr := removeFileIfExists(targetPath); cleanupErr != nil {
			staleErr = errors.Join(staleErr, newTelegramCleanupError("remove stale download", cleanupErr))
		}
		return "", staleErr
	}
	session.TempFiles[index] = targetPath
	session.UpdatedAt = s.nowUTC()
	s.mu.Unlock()
	return targetPath, nil
}

func (s *telegramImportService) downloadTempFile(ctx context.Context, session *telegramImportSession, index int, item telegramScannedTrack) (string, error) {
	if item.SizeBytes > maxSongUploadSize {
		return "", errTelegramTrackTooLarge
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	sessionDir := filepath.Join(s.cfg.ImportTempDir, session.ID)
	targetPath := filepath.Join(sessionDir, buildTempFileName(index, item.FileName))
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return "", fmt.Errorf("%w: create telegram import temp directory: %w", errTelegramStorageFailure, err)
	}
	gatewayCtx, span := startTelegramSpan(ctx, "telegram.download", "audio")
	downloadErr := s.gateway.DownloadTrack(gatewayCtx, item, targetPath)
	finishTelegramSpan(span, downloadErr)
	if downloadErr == nil {
		downloadErr = validateTelegramDownloadedFile(targetPath)
	}
	if downloadErr != nil {
		err := downloadErr
		if cleanupErr := removeFileIfExists(targetPath); cleanupErr != nil {
			err = errors.Join(err, newTelegramCleanupError("remove invalid or partial download", cleanupErr))
		}
		return "", err
	}
	if err := ctx.Err(); err != nil {
		if cleanupErr := removeFileIfExists(targetPath); cleanupErr != nil {
			err = errors.Join(err, newTelegramCleanupError("remove canceled download", cleanupErr))
		}
		return "", err
	}
	return targetPath, nil
}

func validateTelegramDownloadedFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.Join(errTelegramTempFileMissing, err)
		}
		return fmt.Errorf("%w: inspect downloaded track: %w", errTelegramStorageFailure, err)
	}
	if info.Size() == 0 {
		return errTelegramTempFileMissing
	}
	if info.Size() > maxSongUploadSize {
		return errTelegramTrackTooLarge
	}
	return nil
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

	input, err := os.Open(tempPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", errTelegramTempFileMissing
		}
		return "", "", fmt.Errorf("open telegram temp file: %w", err)
	}

	finalName, output, err := createUniqueFile(s.songsDir, fileName)
	if err != nil {
		return "", "", errors.Join(
			fmt.Errorf("create promoted telegram track: %w", err),
			newTelegramCleanupError("close temp file after failed promotion", input.Close()),
		)
	}
	finalPath := output.Name()

	_, copyErr := io.Copy(output, input)
	copyErr = errors.Join(copyErr, output.Close(), input.Close())
	if copyErr != nil {
		return "", "", errors.Join(
			fmt.Errorf("copy telegram temp file: %w", copyErr),
			newTelegramCleanupError("remove incomplete promoted track", removeFileIfExists(finalPath)),
		)
	}

	if err := os.Remove(tempPath); err != nil {
		return "", "", errors.Join(
			fmt.Errorf("remove copied telegram temp file: %w", err),
			newTelegramCleanupError("rollback copied promoted track", removeFileIfExists(finalPath)),
		)
	}
	return finalName, "/api/songs/" + url.PathEscape(finalName), nil
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

// Close prevents new Telegram operations, waits for in-flight per-user imports,
// removes their temporary files, and forgets authentication secrets and session
// state. The server should call it after HTTP handlers have drained.
func (s *telegramImportService) Close() error {
	s.mu.Lock()
	s.closing = true
	userIDs := make(map[int64]struct{}, len(s.sessions)+len(s.operationMu))
	for userID := range s.sessions {
		userIDs[userID] = struct{}{}
	}
	for userID := range s.operationMu {
		userIDs[userID] = struct{}{}
	}
	s.mu.Unlock()

	orderedUserIDs := make([]int64, 0, len(userIDs))
	for userID := range userIDs {
		orderedUserIDs = append(orderedUserIDs, userID)
	}
	sort.Slice(orderedUserIDs, func(i, j int) bool { return orderedUserIDs[i] < orderedUserIDs[j] })

	var cleanupErrs []error
	for _, userID := range orderedUserIDs {
		unlockOperation, err := s.lockUserOperation(context.Background(), userID)
		if err != nil {
			cleanupErrs = append(cleanupErrs, err)
			continue
		}
		s.mu.Lock()
		session := s.sessions[userID]
		delete(s.sessions, userID)
		s.mu.Unlock()
		if err := s.cleanupSessionFiles(session); err != nil {
			cleanupErrs = append(cleanupErrs, newTelegramCleanupError("close active session", err))
		}
		unlockOperation()
	}

	// Catch any already-completed state which could have been installed before
	// Close marked the service unavailable but was not present in the snapshot.
	s.mu.Lock()
	remainingSessions := make([]*telegramImportSession, 0, len(s.sessions))
	for userID, session := range s.sessions {
		remainingSessions = append(remainingSessions, session)
		delete(s.sessions, userID)
	}
	s.mu.Unlock()
	for _, session := range remainingSessions {
		if err := s.cleanupSessionFiles(session); err != nil {
			cleanupErrs = append(cleanupErrs, newTelegramCleanupError("close remaining session", err))
		}
	}

	s.authMu.Lock()
	for userID := range s.pendingLogins {
		delete(s.pendingLogins, userID)
	}
	s.authMu.Unlock()

	return sanitizeTelegramCapturedError(errors.Join(cleanupErrs...))
}

func (s *telegramImportService) buildSessionDTO(session *telegramImportSession) telegramImportSessionDTO {
	total := session.TotalItems
	if total == 0 {
		total = len(session.Items)
	}
	progress := telegramImportProgressDTO{
		Total:     total,
		Processed: session.CurrentIndex,
		Remaining: max(0, total-session.CurrentIndex),
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
		TempFileDownloadURL: "/api/telegram/import-sessions/current/audio",
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
		_, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		status, err := service.Status(r.Context())
		if err != nil {
			writeTelegramError(w, r, err, "status")
			return
		}
		writeJSON(w, http.StatusOK, status)
	})
}

func telegramAuthRequestHandler(service *telegramImportService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		var req telegramAuthRequest
		if err := decodeJSON(r, &req); err != nil {
			writeRequestDecodeError(w, err)
			return
		}
		if err := service.BeginLogin(r.Context(), req.PhoneNumber); err != nil {
			writeTelegramError(w, r, err, "auth.request")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func telegramAuthConfirmHandler(service *telegramImportService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		var req telegramAuthConfirmRequest
		if err := decodeJSON(r, &req); err != nil {
			writeRequestDecodeError(w, err)
			return
		}
		status, err := service.ConfirmLogin(r.Context(), req.PhoneNumber, req.Code)
		if err != nil {
			writeTelegramError(w, r, err, "auth.confirm")
			return
		}
		writeJSON(w, http.StatusOK, status)
	})
}

func telegramAuthPasswordHandler(service *telegramImportService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		var req telegramAuthPasswordRequest
		if err := decodeJSON(r, &req); err != nil {
			writeRequestDecodeError(w, err)
			return
		}
		status, err := service.SubmitPassword(r.Context(), req.Password)
		if err != nil {
			writeTelegramError(w, r, err, "auth.password")
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
			writeRequestDecodeError(w, err)
			return
		}

		session, err := service.StartSession(r.Context(), userID, req.ChannelUsername, req.StartMessageID, req.ReplaceExisting)
		if err != nil {
			writeTelegramError(w, r, err, "import.start")
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
			writeTelegramError(w, r, err, "import.current")
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
		session, err := service.skipCurrentWithContext(r.Context(), userID)
		if err != nil {
			writeTelegramError(w, r, err, "import.skip")
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
			writeRequestDecodeError(w, err)
			return
		}

		session, _, err := service.SaveCurrent(r.Context(), userID, req)
		if err != nil {
			writeTelegramError(w, r, err, "import.save")
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
		if err := service.cancelSessionWithContext(r.Context(), userID); err != nil {
			writeTelegramError(w, r, err, "import.cancel")
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
		lease, err := service.OpenCurrentAudio(r.Context(), userID)
		if err != nil {
			writeTelegramError(w, r, err, "import.audio")
			return
		}
		defer func() {
			if err := lease.Close(); err != nil {
				reportTelegramCleanupError(r.Context(), "import.audio.close", err)
			}
		}()
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", lease.FileName))
		http.ServeContent(w, r, lease.FileName, lease.ModTime, lease.File)
	})
}

func telegramSkippedReportHandler(service *telegramImportService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		report, err := service.skippedReportWithContext(r.Context(), userID)
		if err != nil {
			writeTelegramError(w, r, err, "import.skipped_report")
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="telegram-skipped-items.csv"`)
		written, err := w.Write(report)
		if err == nil && written != len(report) {
			err = io.ErrShortWrite
		}
		if err != nil {
			captureSentryError(r.Context(), fmt.Errorf("write Telegram skipped report: %w", err), "telegram", "import.skipped_report.write")
			return
		}
		if err := service.evictCompletedSessionAfterReport(r.Context(), userID); err != nil {
			reportTelegramCleanupError(r.Context(), "import.skipped_report.cleanup", err)
		}
	})
}

func writeTelegramError(w http.ResponseWriter, r *http.Request, err error, operation string) {
	captureErr := sanitizeTelegramCapturedError(err)
	if hasTelegramCleanupError(err) {
		captureSentryError(r.Context(), captureErr, "telegram", operation)
	}
	switch {
	case errors.Is(err, context.Canceled):
		markSentryErrorHandled(r.Context())
		http.Error(w, "telegram request canceled", http.StatusRequestTimeout)
	case errors.Is(err, context.DeadlineExceeded):
		writeSentryHTTPError(w, r, captureErr, "telegram request timed out", http.StatusGatewayTimeout, "telegram", operation)
	case errors.Is(err, errTelegramNotConfigured):
		http.Error(w, errTelegramNotConfigured.Error(), http.StatusFailedDependency)
	case errors.Is(err, errTelegramShuttingDown):
		markSentryErrorHandled(r.Context())
		http.Error(w, errTelegramShuttingDown.Error(), http.StatusServiceUnavailable)
	case errors.Is(err, errTelegramNotAuthorized), errors.Is(err, errTelegramAuthPendingMissing), errors.Is(err, errTelegramAuthPasswordNeeded):
		http.Error(w, "telegram authentication is required", http.StatusUnauthorized)
	case errors.Is(err, errTelegramInvalidRequest), errors.Is(err, errTelegramInvalidChannel), errors.Is(err, errTelegramNoAudioTracks), errors.Is(err, errTelegramPasswordNotPending), errors.Is(err, errTelegramInvalidPhoneNumber), errors.Is(err, errTelegramInvalidLoginCode):
		http.Error(w, telegramPublicClientError(err), http.StatusBadRequest)
	case errors.Is(err, errTelegramTrackTooLarge):
		http.Error(w, errTelegramTrackTooLarge.Error(), http.StatusRequestEntityTooLarge)
	case errors.Is(err, errTelegramScanLimitExceeded):
		http.Error(w, errTelegramScanLimitExceeded.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, errTelegramSessionActive):
		http.Error(w, errTelegramSessionActive.Error(), http.StatusConflict)
	case errors.Is(err, errTelegramSessionChanged):
		http.Error(w, errTelegramSessionChanged.Error(), http.StatusConflict)
	case errors.Is(err, errTelegramSessionNotFound):
		http.Error(w, errTelegramSessionNotFound.Error(), http.StatusNotFound)
	case errors.Is(err, errInvalidTrack):
		http.Error(w, errInvalidTrack.Error(), http.StatusBadRequest)
	case isTelegramUpstreamError(err), errors.Is(err, errTelegramScanStalled), errors.Is(err, errTelegramTempFileMissing):
		writeSentryHTTPError(w, r, captureErr, "telegram service is temporarily unavailable", http.StatusBadGateway, "telegram", operation)
	default:
		writeSentryHTTPError(w, r, captureErr, "failed to process telegram request", http.StatusInternalServerError, "telegram", operation)
	}
}

func telegramPublicClientError(err error) string {
	if errors.Is(err, errTelegramInvalidRequest) {
		return err.Error()
	}
	for _, candidate := range []error{
		errTelegramInvalidChannel,
		errTelegramNoAudioTracks,
		errTelegramPasswordNotPending,
		errTelegramInvalidPhoneNumber,
		errTelegramInvalidLoginCode,
	} {
		if errors.Is(err, candidate) {
			return candidate.Error()
		}
	}
	return "invalid telegram request"
}

func resolvePublicChannel(ctx context.Context, api telegramUsernameResolver, username string) (*tg.Channel, *tg.InputPeerChannel, error) {
	resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{
		Username: username,
	})
	if err != nil {
		if tg.IsUsernameInvalid(err) || tg.IsUsernameNotOccupied(err) {
			return nil, nil, errTelegramInvalidChannel
		}
		return nil, nil, fmt.Errorf("resolve public channel: %w", err)
	}
	if resolved == nil {
		return nil, nil, errors.New("resolve public channel: telegram returned an empty response")
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
		if !ok || plain == nil {
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

func createUniqueFile(dir, fileName string) (string, *os.File, error) {
	base := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	ext := filepath.Ext(fileName)
	candidate := fileName
	for i := 0; ; i++ {
		path := filepath.Join(dir, candidate)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
		if err == nil {
			return candidate, file, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", nil, err
		}
		candidate = fmt.Sprintf("%s-%d%s", base, i+1, ext)
	}
}

func max(left, right int) int {
	if left > right {
		return left
	}
	return right
}
