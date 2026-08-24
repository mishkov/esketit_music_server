package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func TestTelegramAuthStatusJSONDoesNotExposeServerPaths(t *testing.T) {
	encoded, err := json.Marshal(telegramAuthStatus{
		Configured:         true,
		ImportTempDir:      "/sensitive/import/path",
		SessionStorageFile: "/sensitive/session.json",
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(encoded), "sensitive") || strings.Contains(string(encoded), "importTempDir") || strings.Contains(string(encoded), "sessionStorageFile") {
		t.Fatalf("telegram auth status leaked server paths: %s", encoded)
	}
}

type fakeTelegramGateway struct {
	status             telegramAuthStatus
	scannedItems       []telegramScannedTrack
	downloadData       []byte
	passwordAuth       bool
	scanStartMessageID int
	statusErr          error
	beginLoginErr      error
	confirmLoginErr    error
	passwordLoginErr   error
	scanErr            error
	downloadErr        error
}

func (f *fakeTelegramGateway) Status(context.Context) (telegramAuthStatus, error) {
	return f.status, f.statusErr
}

func (f *fakeTelegramGateway) BeginLogin(context.Context, string) (string, error) {
	return "code-hash", f.beginLoginErr
}

func (f *fakeTelegramGateway) ConfirmLogin(context.Context, string, string, string) (telegramAuthStatus, error) {
	if f.confirmLoginErr != nil {
		return telegramAuthStatus{}, f.confirmLoginErr
	}
	if f.passwordAuth {
		result := f.status
		result.PasswordRequired = true
		result.Authorized = false
		return result, nil
	}
	return f.status, nil
}

func (f *fakeTelegramGateway) PasswordLogin(context.Context, string) (telegramAuthStatus, error) {
	if f.passwordLoginErr != nil {
		return telegramAuthStatus{}, f.passwordLoginErr
	}
	result := f.status
	result.PasswordRequired = false
	result.Authorized = true
	return result, nil
}

func (f *fakeTelegramGateway) ScanPublicChannel(_ context.Context, _ string, startMessageID int) ([]telegramScannedTrack, error) {
	f.scanStartMessageID = startMessageID
	if f.scanErr != nil {
		return nil, f.scanErr
	}
	if len(f.scannedItems) == 0 {
		return nil, errTelegramNoAudioTracks
	}
	items := make([]telegramScannedTrack, len(f.scannedItems))
	copy(items, f.scannedItems)
	return items, nil
}

func (f *fakeTelegramGateway) DownloadTrack(_ context.Context, _ telegramScannedTrack, destinationPath string) error {
	if f.downloadErr != nil {
		return f.downloadErr
	}
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(destinationPath, f.downloadData, 0o644)
}

func TestTelegramImportSessionConflictAndSkipReport(t *testing.T) {
	service, _, _, _ := newTelegramImportTestService(t, &fakeTelegramGateway{
		status: telegramAuthStatus{Configured: true, Authorized: true},
		scannedItems: []telegramScannedTrack{
			{
				MessageID:   11,
				MessageLink: "https://t.me/test_channel/11",
				FileName:    "Artist - First.mp3",
				MimeType:    "audio/mpeg",
				SizeBytes:   123,
				ParsedTitle: "Artist - First",
			},
			{
				MessageID:   12,
				MessageLink: "https://t.me/test_channel/12",
				FileName:    "Artist - Second.mp3",
				MimeType:    "audio/mpeg",
				SizeBytes:   456,
				ParsedTitle: "Artist - Second",
			},
		},
		downloadData: []byte("mp3-bytes"),
	})

	ctx := context.Background()
	_, err := service.StartSession(ctx, 1, "@test_channel", 0, false)
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	_, err = service.StartSession(ctx, 1, "@test_channel", 0, false)
	if !errors.Is(err, errTelegramSessionActive) {
		t.Fatalf("StartSession() conflict error = %v, want %v", err, errTelegramSessionActive)
	}

	current, err := service.CurrentSession(ctx, 1)
	if err != nil {
		t.Fatalf("CurrentSession() error = %v", err)
	}
	if current.CurrentTrack == nil || current.CurrentTrack.MessageID != 11 {
		t.Fatalf("CurrentSession() current track = %#v", current.CurrentTrack)
	}

	current, err = service.SkipCurrent(1)
	if err != nil {
		t.Fatalf("SkipCurrent() error = %v", err)
	}
	if current.Progress.Skipped != 1 || current.Progress.Processed != 1 || current.Status != telegramImportStatusActive {
		t.Fatalf("SkipCurrent() progress = %#v status=%s", current.Progress, current.Status)
	}

	report, err := service.SkippedReport(1)
	if err != nil {
		t.Fatalf("SkippedReport() error = %v", err)
	}
	reportText := string(report)
	if !strings.Contains(reportText, "parsed_title,telegram_message_link") || !strings.Contains(reportText, "Artist - First,https://t.me/test_channel/11") {
		t.Fatalf("SkippedReport() = %q", reportText)
	}

	current, err = service.SkipCurrent(1)
	if err != nil {
		t.Fatalf("SkipCurrent() second error = %v", err)
	}
	if current.Status != telegramImportStatusCompleted || current.CurrentTrack != nil {
		t.Fatalf("completed session = %#v", current)
	}
}

func TestTelegramImportSaveCurrentPromotesFileAndCreatesTrack(t *testing.T) {
	service, store, songsDir, tempRoot := newTelegramImportTestService(t, &fakeTelegramGateway{
		status: telegramAuthStatus{Configured: true, Authorized: true},
		scannedItems: []telegramScannedTrack{
			{
				MessageID:   22,
				MessageLink: "https://t.me/test_channel/22",
				FileName:    "save-me.mp3",
				MimeType:    "audio/mpeg",
				SizeBytes:   777,
				ParsedTitle: "Save Me",
			},
		},
		downloadData: []byte("audio-data"),
	})

	artist, album := seedTrackDependencies(t, store)

	ctx := context.Background()
	if _, err := service.StartSession(ctx, 1, "test_channel", 0, false); err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	dto, createdTrack, err := service.SaveCurrent(ctx, 1, telegramSaveTrackRequest{
		Name:       "Final Track",
		AuthorIDs:  []int64{artist.ID},
		AlbumID:    album.ID,
		AlbumOrder: 0,
		AdditionalInfo: []additionalInfo{
			{
				"type":     "external_link",
				"provider": "youtube_music",
				"url":      "https://music.youtube.com/watch?v=save-me",
			},
		},
		SourceMetadata: []sourceMetadata{
			{
				"provider": "telegram",
				"identity": map[string]any{
					"chatId":    "test_channel",
					"messageId": "10",
				},
				"url": "https://t.me/test_channel/10",
			},
		},
	})
	if err != nil {
		t.Fatalf("SaveCurrent() error = %v", err)
	}
	if dto.Status != telegramImportStatusCompleted || dto.Progress.Saved != 1 {
		t.Fatalf("SaveCurrent() dto = %#v", dto)
	}
	if createdTrack.Name != "Final Track" {
		t.Fatalf("created track = %#v", createdTrack)
	}
	if !strings.HasPrefix(createdTrack.AudioFilePath, "/api/songs/") {
		t.Fatalf("created track audio path = %q", createdTrack.AudioFilePath)
	}
	if len(createdTrack.SourceMetadata) != 1 {
		t.Fatalf("created track source metadata = %#v", createdTrack.SourceMetadata)
	}
	identity, ok := createdTrack.SourceMetadata[0]["identity"].(map[string]any)
	if !ok {
		t.Fatalf("created track sourceMetadata identity = %#v, want object", createdTrack.SourceMetadata[0]["identity"])
	}
	if got := identity["messageId"]; got != "10" {
		t.Fatalf("created track sourceMetadata identity.messageId = %#v, want 10", got)
	}

	savedFiles, err := os.ReadDir(songsDir)
	if err != nil {
		t.Fatalf("ReadDir(%s) error = %v", songsDir, err)
	}
	if len(savedFiles) != 1 || savedFiles[0].Name() != "save-me.mp3" {
		t.Fatalf("saved files = %#v", savedFiles)
	}

	sessionTempEntries, err := os.ReadDir(tempRoot)
	if err != nil {
		t.Fatalf("ReadDir(%s) error = %v", tempRoot, err)
	}
	if len(sessionTempEntries) != 0 {
		t.Fatalf("temp entries after completed session = %#v, want none", sessionTempEntries)
	}
}

func TestCreateUniqueFileConcurrentReservations(t *testing.T) {
	dir := t.TempDir()
	const reservations = 16
	start := make(chan struct{})
	names := make(chan string, reservations)
	errs := make(chan error, reservations)
	var wait sync.WaitGroup

	for range reservations {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			name, file, err := createUniqueFile(dir, "shared-track.mp3")
			if err != nil {
				errs <- err
				return
			}
			_, writeErr := file.WriteString(name)
			if closeErr := file.Close(); writeErr != nil || closeErr != nil {
				errs <- errors.Join(writeErr, closeErr)
				return
			}
			names <- name
		}()
	}

	close(start)
	wait.Wait()
	close(names)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("createUniqueFile() error = %v", err)
		}
	}

	seen := make(map[string]struct{}, reservations)
	for name := range names {
		if _, exists := seen[name]; exists {
			t.Fatalf("duplicate reserved filename %q", name)
		}
		seen[name] = struct{}{}
	}
	if len(seen) != reservations {
		t.Fatalf("reserved filenames = %d, want %d", len(seen), reservations)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s) error = %v", dir, err)
	}
	if len(entries) != reservations {
		t.Fatalf("reserved files = %d, want %d", len(entries), reservations)
	}
}

func TestTelegramGatewayRunGateSerializesAndHonorsCancellation(t *testing.T) {
	gateway := &gotdTelegramGateway{}
	releaseFirst, err := gateway.acquireRun(context.Background())
	if err != nil {
		t.Fatalf("first acquireRun() error = %v", err)
	}

	secondAcquired := make(chan func(), 1)
	secondErr := make(chan error, 1)
	go func() {
		release, err := gateway.acquireRun(context.Background())
		if err != nil {
			secondErr <- err
			return
		}
		secondAcquired <- release
	}()
	select {
	case release := <-secondAcquired:
		release()
		t.Fatal("second acquireRun() was not serialized")
	case err := <-secondErr:
		t.Fatalf("second acquireRun() error = %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	canceledContext, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gateway.acquireRun(canceledContext); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquireRun() error = %v, want %v", err, context.Canceled)
	}

	releaseFirst()
	select {
	case release := <-secondAcquired:
		release()
	case err := <-secondErr:
		t.Fatalf("second acquireRun() error = %v", err)
	case <-time.After(time.Second):
		t.Fatal("second acquireRun() did not resume")
	}
}

func TestTelegramImportCancelCleansTempFiles(t *testing.T) {
	service, _, _, tempRoot := newTelegramImportTestService(t, &fakeTelegramGateway{
		status: telegramAuthStatus{Configured: true, Authorized: true},
		scannedItems: []telegramScannedTrack{
			{
				MessageID:   33,
				MessageLink: "https://t.me/test_channel/33",
				FileName:    "cancel.mp3",
				MimeType:    "audio/mpeg",
				SizeBytes:   999,
				ParsedTitle: "Cancel",
			},
		},
		downloadData: []byte("audio-data"),
	})

	ctx := context.Background()
	session, err := service.StartSession(ctx, 1, "test_channel", 0, false)
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	if _, err := service.CurrentSession(ctx, 1); err != nil {
		t.Fatalf("CurrentSession() error = %v", err)
	}

	sessionDir := filepath.Join(tempRoot, session.SessionID)
	if _, err := os.Stat(sessionDir); err != nil {
		t.Fatalf("session temp dir stat error = %v", err)
	}

	if err := service.CancelSession(1); err != nil {
		t.Fatalf("CancelSession() error = %v", err)
	}
	if _, err := os.Stat(sessionDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session temp dir still exists, stat err = %v", err)
	}
}

func TestTelegramAuthPasswordFlow(t *testing.T) {
	service, _, _, _ := newTelegramImportTestService(t, &fakeTelegramGateway{
		status: telegramAuthStatus{
			Configured:        true,
			Authorized:        true,
			AccountIdentifier: "@me",
		},
		passwordAuth: true,
	})

	ctx := context.Background()
	if err := service.BeginLogin(ctx, "+1234567890"); err != nil {
		t.Fatalf("BeginLogin() error = %v", err)
	}

	status, err := service.ConfirmLogin(ctx, "+1234567890", "12345")
	if err != nil {
		t.Fatalf("ConfirmLogin() error = %v", err)
	}
	if status.Authorized || !status.PasswordRequired {
		t.Fatalf("ConfirmLogin() status = %#v", status)
	}

	currentStatus, err := service.Status(ctx)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if !currentStatus.PasswordRequired {
		t.Fatalf("Status() = %#v", currentStatus)
	}

	status, err = service.SubmitPassword(ctx, "secret")
	if err != nil {
		t.Fatalf("SubmitPassword() error = %v", err)
	}
	if !status.Authorized || status.PasswordRequired {
		t.Fatalf("SubmitPassword() status = %#v", status)
	}
}

func TestTelegramImportStartSessionFiltersFromMessageIDInclusive(t *testing.T) {
	gateway := &fakeTelegramGateway{
		status: telegramAuthStatus{Configured: true, Authorized: true},
		scannedItems: []telegramScannedTrack{
			{
				MessageID:   10,
				MessageLink: "https://t.me/test_channel/10",
				FileName:    "first.mp3",
				MimeType:    "audio/mpeg",
				SizeBytes:   100,
				ParsedTitle: "First",
			},
			{
				MessageID:   20,
				MessageLink: "https://t.me/test_channel/20",
				FileName:    "second.mp3",
				MimeType:    "audio/mpeg",
				SizeBytes:   200,
				ParsedTitle: "Second",
			},
			{
				MessageID:   30,
				MessageLink: "https://t.me/test_channel/30",
				FileName:    "third.mp3",
				MimeType:    "audio/mpeg",
				SizeBytes:   300,
				ParsedTitle: "Third",
			},
		},
		downloadData: []byte("mp3-bytes"),
	}
	service, _, _, _ := newTelegramImportTestService(t, gateway)

	ctx := context.Background()
	current, err := service.StartSession(ctx, 1, "test_channel", 20, false)
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	if current.CurrentTrack == nil || current.CurrentTrack.MessageID != 20 {
		t.Fatalf("CurrentSession() current track = %#v", current.CurrentTrack)
	}
	if current.Progress.Total != 2 || current.Progress.Remaining != 2 {
		t.Fatalf("CurrentSession() progress = %#v", current.Progress)
	}
	if gateway.scanStartMessageID != 20 {
		t.Fatalf("ScanPublicChannel() startMessageID = %d, want 20", gateway.scanStartMessageID)
	}
}

type fakeTelegramHistoryClient struct {
	requests  []*tg.MessagesGetHistoryRequest
	responses []tg.MessagesMessagesClass
}

func (f *fakeTelegramHistoryClient) MessagesGetHistory(_ context.Context, request *tg.MessagesGetHistoryRequest) (tg.MessagesMessagesClass, error) {
	requestCopy := *request
	f.requests = append(f.requests, &requestCopy)
	if len(f.responses) == 0 {
		return &tg.MessagesMessages{}, nil
	}
	response := f.responses[0]
	f.responses = f.responses[1:]
	return response, nil
}

func TestScanPublicChannelHistoryStartsAtMessageIDInclusive(t *testing.T) {
	peer := &tg.InputPeerChannel{ChannelID: 1, AccessHash: 2}
	api := &fakeTelegramHistoryClient{
		responses: []tg.MessagesMessagesClass{
			&tg.MessagesMessages{Messages: []tg.MessageClass{
				telegramAudioMessage(30),
				telegramAudioMessage(20),
			}},
			&tg.MessagesMessages{},
		},
	}

	items, err := scanPublicChannelHistory(context.Background(), api, "test_channel", peer, 20)
	if err != nil {
		t.Fatalf("scanPublicChannelHistory() error = %v", err)
	}
	if len(items) != 2 || items[0].MessageID != 20 || items[1].MessageID != 30 {
		t.Fatalf("scanPublicChannelHistory() items = %#v", items)
	}
	if len(api.requests) != 2 {
		t.Fatalf("MessagesGetHistory() request count = %d, want 2", len(api.requests))
	}
	if api.requests[0].MinID != 19 || api.requests[0].OffsetID != 0 {
		t.Fatalf("first MessagesGetHistory() request = %#v", api.requests[0])
	}
	if api.requests[1].MinID != 19 || api.requests[1].OffsetID != 20 {
		t.Fatalf("second MessagesGetHistory() request = %#v", api.requests[1])
	}
}

func telegramAudioMessage(messageID int) *tg.Message {
	return &tg.Message{
		ID: messageID,
		Media: &tg.MessageMediaDocument{Document: &tg.Document{
			ID:         int64(messageID),
			AccessHash: int64(messageID),
			MimeType:   "audio/mpeg",
			Size:       100,
			Attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeFilename{FileName: "track.mp3"},
			},
		}},
	}
}

func TestTelegramImportStartSessionReturnsNoTracksWhenStartMessageIDAfterLast(t *testing.T) {
	service, _, _, _ := newTelegramImportTestService(t, &fakeTelegramGateway{
		status: telegramAuthStatus{Configured: true, Authorized: true},
		scannedItems: []telegramScannedTrack{
			{
				MessageID:   10,
				MessageLink: "https://t.me/test_channel/10",
				FileName:    "first.mp3",
				MimeType:    "audio/mpeg",
				SizeBytes:   100,
				ParsedTitle: "First",
			},
		},
	})

	ctx := context.Background()
	_, err := service.StartSession(ctx, 1, "test_channel", 11, false)
	if !errors.Is(err, errTelegramNoAudioTracks) {
		t.Fatalf("StartSession() error = %v, want %v", err, errTelegramNoAudioTracks)
	}
}

func TestWriteTelegramErrorCapturesUnexpectedFailureOnce(t *testing.T) {
	service, _, _, _ := newTelegramImportTestService(t, &fakeTelegramGateway{
		statusErr: &telegramUpstreamError{
			operation: "read authorization status",
			err:       errors.New("upstream unavailable"),
		},
	})
	request, transport := newTelegramSentryRequest(t, http.MethodGet, "/api/telegram/status", nil, 42)
	response := httptest.NewRecorder()

	telegramStatusHandler(service).ServeHTTP(response, request)

	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadGateway)
	}
	if got := response.Body.String(); got != "telegram service is temporarily unavailable\n" {
		t.Fatalf("body = %q", got)
	}
	events := transport.Events()
	if len(events) != 1 {
		t.Fatalf("captured events = %d, want 1", len(events))
	}
	if events[0].Tags["component"] != "telegram" || events[0].Tags["operation"] != "status" {
		t.Fatalf("event tags = %#v", events[0].Tags)
	}
	if events[0].Tags["http.status_code"] != "502" {
		t.Fatalf("event status tag = %q, want 502", events[0].Tags["http.status_code"])
	}
}

func TestWriteTelegramErrorDoesNotCaptureExpectedFailure(t *testing.T) {
	service, _, _, _ := newTelegramImportTestService(t, &fakeTelegramGateway{
		scanErr: errTelegramInvalidChannel,
	})
	body := bytes.NewBufferString(`{"channelUsername":"missing"}`)
	request, transport := newTelegramSentryRequest(t, http.MethodPost, "/api/telegram/import-sessions", body, 42)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	telegramStartImportHandler(service).ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	if events := transport.Events(); len(events) != 0 {
		t.Fatalf("captured events = %d, want 0", len(events))
	}
}

func TestWriteTelegramErrorMapsTimeoutToGatewayTimeout(t *testing.T) {
	service, _, _, _ := newTelegramImportTestService(t, &fakeTelegramGateway{
		statusErr: context.DeadlineExceeded,
	})
	request, transport := newTelegramSentryRequest(t, http.MethodGet, "/api/telegram/status", nil, 42)
	response := httptest.NewRecorder()

	telegramStatusHandler(service).ServeHTTP(response, request)

	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusGatewayTimeout)
	}
	if events := transport.Events(); len(events) != 1 {
		t.Fatalf("captured events = %d, want 1", len(events))
	}
}

type fakeTelegramUsernameResolver struct {
	resolved *tg.ContactsResolvedPeer
	err      error
}

func (f *fakeTelegramUsernameResolver) ContactsResolveUsername(context.Context, *tg.ContactsResolveUsernameRequest) (*tg.ContactsResolvedPeer, error) {
	return f.resolved, f.err
}

func TestResolvePublicChannelClassifiesOnlyKnownUsernameErrorsAsInvalid(t *testing.T) {
	invalidErr := tgerr.New(400, tg.ErrUsernameNotOccupied)
	_, _, err := resolvePublicChannel(context.Background(), &fakeTelegramUsernameResolver{err: invalidErr}, "missing")
	if !errors.Is(err, errTelegramInvalidChannel) {
		t.Fatalf("invalid username error = %v, want %v", err, errTelegramInvalidChannel)
	}

	upstreamErr := tgerr.New(500, "INTERNAL")
	_, _, err = resolvePublicChannel(context.Background(), &fakeTelegramUsernameResolver{err: upstreamErr}, "channel")
	if !errors.Is(err, upstreamErr) {
		t.Fatalf("upstream error = %v, want preserved cause %v", err, upstreamErr)
	}
	if errors.Is(err, errTelegramInvalidChannel) {
		t.Fatalf("upstream error was misclassified as invalid channel: %v", err)
	}
}

func TestClassifyTelegramAuthError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		want       error
		upstream   bool
		causeIsErr bool
	}{
		{name: "invalid phone", err: tgerr.New(400, tg.ErrPhoneNumberInvalid), want: errTelegramInvalidPhoneNumber},
		{name: "expired code", err: tgerr.New(400, tg.ErrPhoneCodeExpired), want: errTelegramInvalidLoginCode},
		{name: "rate limited", err: tgerr.New(420, "FLOOD_WAIT_30"), upstream: true, causeIsErr: true},
		{name: "timeout", err: context.DeadlineExceeded, upstream: true, causeIsErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classifyTelegramAuthError("authenticate", test.err)
			if test.want != nil && !errors.Is(got, test.want) {
				t.Fatalf("classifyTelegramAuthError() = %v, want %v", got, test.want)
			}
			if isTelegramUpstreamError(got) != test.upstream {
				t.Fatalf("isTelegramUpstreamError(%v) = %t, want %t", got, isTelegramUpstreamError(got), test.upstream)
			}
			if test.causeIsErr && !errors.Is(got, test.err) {
				t.Fatalf("classifyTelegramAuthError() = %v, want preserved cause %v", got, test.err)
			}
		})
	}
}

func TestSanitizeTelegramCapturedErrorRemovesSessionPath(t *testing.T) {
	err := &telegramUpstreamError{
		operation: "download track",
		err: &os.PathError{
			Op:   "open",
			Path: "/tmp/telegram/session-sensitive-id/track.mp3",
			Err:  os.ErrPermission,
		},
	}

	sanitized := sanitizeTelegramCapturedError(err)
	if strings.Contains(sanitized.Error(), "session-sensitive-id") || strings.Contains(sanitized.Error(), "track.mp3") {
		t.Fatalf("sanitized error leaked path: %v", sanitized)
	}
	if !errors.Is(sanitized, os.ErrPermission) {
		t.Fatalf("sanitized error lost cause: %v", sanitized)
	}
	if !isTelegramUpstreamError(sanitized) {
		t.Fatalf("sanitized error lost upstream classification: %v", sanitized)
	}
}

func TestSanitizeTelegramCapturedErrorPreservesPrimaryAndCleanupCauses(t *testing.T) {
	primary := errors.New("database unavailable")
	joined := errors.Join(
		primary,
		newTelegramCleanupError("rollback track", &os.PathError{
			Op:   "remove",
			Path: "/tmp/telegram/session-sensitive-id/track.mp3",
			Err:  os.ErrPermission,
		}),
	)

	sanitized := sanitizeTelegramCapturedError(joined)
	if !errors.Is(sanitized, primary) || !errors.Is(sanitized, os.ErrPermission) {
		t.Fatalf("sanitized joined error lost a cause: %v", sanitized)
	}
	if strings.Contains(sanitized.Error(), "session-sensitive-id") || strings.Contains(sanitized.Error(), "track.mp3") {
		t.Fatalf("sanitized joined error leaked path: %v", sanitized)
	}
}

type blockingConfirmTelegramGateway struct {
	*fakeTelegramGateway
	confirmStarted chan struct{}
	releaseConfirm chan struct{}
}

func (g *blockingConfirmTelegramGateway) ConfirmLogin(ctx context.Context, _, _, _ string) (telegramAuthStatus, error) {
	close(g.confirmStarted)
	select {
	case <-g.releaseConfirm:
		status := g.status
		status.Authorized = false
		status.PasswordRequired = true
		return status, nil
	case <-ctx.Done():
		return telegramAuthStatus{}, ctx.Err()
	}
}

func TestTelegramAuthSerializesPendingLoginReplacement(t *testing.T) {
	releaseConfirm := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseConfirm) }) }
	defer release()

	gateway := &blockingConfirmTelegramGateway{
		fakeTelegramGateway: &fakeTelegramGateway{status: telegramAuthStatus{Configured: true}},
		confirmStarted:      make(chan struct{}),
		releaseConfirm:      releaseConfirm,
	}
	service, _, _, _ := newTelegramImportTestService(t, gateway)
	ctx := telegramTestUserContext(1)
	if err := service.BeginLogin(ctx, "+111111111"); err != nil {
		t.Fatalf("BeginLogin() error = %v", err)
	}

	confirmDone := make(chan error, 1)
	go func() {
		_, err := service.ConfirmLogin(ctx, "+111111111", "11111")
		confirmDone <- err
	}()
	select {
	case <-gateway.confirmStarted:
	case <-time.After(time.Second):
		t.Fatal("ConfirmLogin did not reach gateway")
	}

	beginAttempted := make(chan struct{})
	beginDone := make(chan error, 1)
	go func() {
		close(beginAttempted)
		beginDone <- service.BeginLogin(ctx, "+222222222")
	}()
	<-beginAttempted
	select {
	case err := <-beginDone:
		t.Fatalf("replacement BeginLogin completed before ConfirmLogin: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	release()
	if err := <-confirmDone; err != nil {
		t.Fatalf("ConfirmLogin() error = %v", err)
	}
	if err := <-beginDone; err != nil {
		t.Fatalf("replacement BeginLogin() error = %v", err)
	}

	service.authMu.Lock()
	pending := service.pendingLogins[1]
	service.authMu.Unlock()
	if pending == nil || pending.PhoneNumber != "+222222222" || pending.PasswordRequired {
		t.Fatalf("pending login = %#v, want replacement request", pending)
	}
}

func TestTelegramPendingLoginsAreScopedByUser(t *testing.T) {
	service, _, _, _ := newTelegramImportTestService(t, &fakeTelegramGateway{
		status:       telegramAuthStatus{Configured: true},
		passwordAuth: true,
	})
	ctx1 := telegramTestUserContext(1)
	ctx2 := telegramTestUserContext(2)
	if err := service.BeginLogin(ctx1, "+111111111"); err != nil {
		t.Fatalf("BeginLogin(user 1) error = %v", err)
	}
	if err := service.BeginLogin(ctx2, "+222222222"); err != nil {
		t.Fatalf("BeginLogin(user 2) error = %v", err)
	}
	if _, err := service.ConfirmLogin(ctx1, "+111111111", "11111"); err != nil {
		t.Fatalf("ConfirmLogin(user 1) error = %v", err)
	}

	status1, err := service.Status(ctx1)
	if err != nil {
		t.Fatalf("Status(user 1) error = %v", err)
	}
	status2, err := service.Status(ctx2)
	if err != nil {
		t.Fatalf("Status(user 2) error = %v", err)
	}
	if !status1.PasswordRequired {
		t.Fatalf("Status(user 1) = %#v, want password required", status1)
	}
	if status2.PasswordRequired {
		t.Fatalf("Status(user 2) = %#v, want independent pending login", status2)
	}
}

type controlledTelegramGateway struct {
	*fakeTelegramGateway
	mu                sync.Mutex
	scanBatches       [][]telegramScannedTrack
	scanCalls         int
	downloadCalls     int
	blockDownloadCall int
	downloadStarted   chan struct{}
	releaseDownload   chan struct{}
	secondScanStarted chan struct{}
}

func (g *controlledTelegramGateway) ScanPublicChannel(_ context.Context, _ string, _ int) ([]telegramScannedTrack, error) {
	g.mu.Lock()
	g.scanCalls++
	call := g.scanCalls
	var items []telegramScannedTrack
	if call <= len(g.scanBatches) {
		items = append(items, g.scanBatches[call-1]...)
	}
	secondScanStarted := g.secondScanStarted
	g.mu.Unlock()
	if call == 2 && secondScanStarted != nil {
		close(secondScanStarted)
	}
	if len(items) == 0 {
		return nil, errTelegramNoAudioTracks
	}
	return items, nil
}

func (g *controlledTelegramGateway) DownloadTrack(ctx context.Context, _ telegramScannedTrack, destinationPath string) error {
	g.mu.Lock()
	g.downloadCalls++
	call := g.downloadCalls
	shouldBlock := call == g.blockDownloadCall
	started := g.downloadStarted
	release := g.releaseDownload
	data := append([]byte(nil), g.downloadData...)
	g.mu.Unlock()

	if shouldBlock {
		if started != nil {
			close(started)
		}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(destinationPath, data, 0o644)
}

func (g *controlledTelegramGateway) downloadCallCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.downloadCalls
}

func TestTelegramConcurrentCurrentSessionDownloadsTrackOnce(t *testing.T) {
	item := telegramScannedTrack{MessageID: 1, FileName: "once.mp3", MimeType: "audio/mpeg", ParsedTitle: "Once"}
	releaseDownload := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseDownload) }) }
	defer release()
	gateway := &controlledTelegramGateway{
		fakeTelegramGateway: &fakeTelegramGateway{downloadData: []byte("audio")},
		scanBatches:         [][]telegramScannedTrack{{item}},
		blockDownloadCall:   2,
		downloadStarted:     make(chan struct{}),
		releaseDownload:     releaseDownload,
	}
	service, _, _, _ := newTelegramImportTestService(t, gateway)
	ctx := context.Background()
	if _, err := service.StartSession(ctx, 1, "channel", 0, false); err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	service.mu.Lock()
	tempPath := service.sessions[1].TempFiles[0]
	service.mu.Unlock()
	if err := os.Remove(tempPath); err != nil {
		t.Fatalf("Remove(%s) error = %v", tempPath, err)
	}

	results := make(chan error, 2)
	go func() {
		_, err := service.CurrentSession(ctx, 1)
		results <- err
	}()
	select {
	case <-gateway.downloadStarted:
	case <-time.After(time.Second):
		t.Fatal("first CurrentSession did not start download")
	}
	secondAttempted := make(chan struct{})
	go func() {
		close(secondAttempted)
		_, err := service.CurrentSession(ctx, 1)
		results <- err
	}()
	<-secondAttempted
	time.Sleep(50 * time.Millisecond)
	if calls := gateway.downloadCallCount(); calls != 2 {
		t.Fatalf("downloads while first request blocked = %d, want 2 total including initial download", calls)
	}

	release()
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("CurrentSession() error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("CurrentSession calls did not finish")
		}
	}
	if calls := gateway.downloadCallCount(); calls != 2 {
		t.Fatalf("download calls = %d, want 2 total including initial download", calls)
	}
}

func TestTelegramSaveSerializesReplacementSession(t *testing.T) {
	oldItem := telegramScannedTrack{MessageID: 1, FileName: "old.mp3", MimeType: "audio/mpeg", ParsedTitle: "Old"}
	newItem := telegramScannedTrack{MessageID: 2, FileName: "new.mp3", MimeType: "audio/mpeg", ParsedTitle: "New"}
	releaseDownload := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseDownload) }) }
	defer release()
	gateway := &controlledTelegramGateway{
		fakeTelegramGateway: &fakeTelegramGateway{downloadData: []byte("audio")},
		scanBatches:         [][]telegramScannedTrack{{oldItem}, {newItem}},
		blockDownloadCall:   2,
		downloadStarted:     make(chan struct{}),
		releaseDownload:     releaseDownload,
		secondScanStarted:   make(chan struct{}),
	}
	service, store, _, _ := newTelegramImportTestService(t, gateway)
	artist, album := seedTrackDependencies(t, store)
	ctx := context.Background()
	if _, err := service.StartSession(ctx, 1, "old_channel", 0, false); err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	service.mu.Lock()
	tempPath := service.sessions[1].TempFiles[0]
	service.mu.Unlock()
	if err := os.Remove(tempPath); err != nil {
		t.Fatalf("Remove(%s) error = %v", tempPath, err)
	}

	saveDone := make(chan error, 1)
	go func() {
		_, _, err := service.SaveCurrent(ctx, 1, telegramSaveTrackRequest{
			Name:      "Old",
			AuthorIDs: []int64{artist.ID},
			AlbumID:   album.ID,
		})
		saveDone <- err
	}()
	select {
	case <-gateway.downloadStarted:
	case <-time.After(time.Second):
		t.Fatal("SaveCurrent did not start replacement download")
	}

	replaceAttempted := make(chan struct{})
	type replacementResult struct {
		dto telegramImportSessionDTO
		err error
	}
	replaceDone := make(chan replacementResult, 1)
	go func() {
		close(replaceAttempted)
		dto, err := service.StartSession(ctx, 1, "new_channel", 0, true)
		replaceDone <- replacementResult{dto: dto, err: err}
	}()
	<-replaceAttempted
	select {
	case <-gateway.secondScanStarted:
		t.Fatal("replacement scan started while SaveCurrent was in progress")
	case <-time.After(50 * time.Millisecond):
	}

	release()
	if err := <-saveDone; err != nil {
		t.Fatalf("SaveCurrent() error = %v", err)
	}
	result := <-replaceDone
	if result.err != nil {
		t.Fatalf("replacement StartSession() error = %v", result.err)
	}
	if result.dto.CurrentTrack == nil || result.dto.CurrentTrack.MessageID != newItem.MessageID {
		t.Fatalf("replacement session current track = %#v", result.dto.CurrentTrack)
	}
	if result.dto.Progress.Processed != 0 || result.dto.Progress.Saved != 0 {
		t.Fatalf("replacement session progress = %#v", result.dto.Progress)
	}
}

func TestTelegramDownloadLimitWriterStopsAtLimit(t *testing.T) {
	var output bytes.Buffer
	writer := &telegramDownloadLimitWriter{writer: &output, remaining: 4}

	written, err := writer.Write([]byte("abcdef"))
	if written != 4 || !errors.Is(err, errTelegramTrackTooLarge) {
		t.Fatalf("Write() = (%d, %v), want (4, %v)", written, err, errTelegramTrackTooLarge)
	}
	if got := output.String(); got != "abcd" {
		t.Fatalf("bounded output = %q, want abcd", got)
	}
	written, err = writer.Write([]byte("z"))
	if written != 0 || !errors.Is(err, errTelegramTrackTooLarge) {
		t.Fatalf("second Write() = (%d, %v), want (0, %v)", written, err, errTelegramTrackTooLarge)
	}
}

func TestTelegramStartRejectsOversizedTrackBeforeDownload(t *testing.T) {
	gateway := &controlledTelegramGateway{
		fakeTelegramGateway: &fakeTelegramGateway{downloadData: []byte("unused")},
		scanBatches: [][]telegramScannedTrack{{{
			MessageID: 1,
			FileName:  "oversized.mp3",
			MimeType:  "audio/mpeg",
			SizeBytes: maxSongUploadSize + 1,
		}}},
	}
	service, _, _, tempRoot := newTelegramImportTestService(t, gateway)

	_, err := service.StartSession(context.Background(), 1, "channel", 0, false)
	if !errors.Is(err, errTelegramTrackTooLarge) {
		t.Fatalf("StartSession() error = %v, want %v", err, errTelegramTrackTooLarge)
	}
	if calls := gateway.downloadCallCount(); calls != 0 {
		t.Fatalf("download calls = %d, want 0", calls)
	}
	entries, readErr := os.ReadDir(tempRoot)
	if readErr != nil {
		t.Fatalf("ReadDir(%s) error = %v", tempRoot, readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("temp entries = %#v, want none", entries)
	}
}

type oversizedTelegramDownloadGateway struct {
	*fakeTelegramGateway
}

func (g *oversizedTelegramDownloadGateway) DownloadTrack(_ context.Context, _ telegramScannedTrack, destinationPath string) error {
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
		return err
	}
	file, err := os.Create(destinationPath)
	if err != nil {
		return err
	}
	truncateErr := file.Truncate(maxSongUploadSize + 1)
	return errors.Join(truncateErr, file.Close())
}

func TestTelegramStartRemovesOversizedDownloadedFile(t *testing.T) {
	gateway := &oversizedTelegramDownloadGateway{fakeTelegramGateway: &fakeTelegramGateway{
		scannedItems: []telegramScannedTrack{{MessageID: 1, FileName: "unknown-size.mp3", MimeType: "audio/mpeg"}},
	}}
	service, _, _, tempRoot := newTelegramImportTestService(t, gateway)

	_, err := service.StartSession(context.Background(), 1, "channel", 0, false)
	if !errors.Is(err, errTelegramTrackTooLarge) {
		t.Fatalf("StartSession() error = %v, want %v", err, errTelegramTrackTooLarge)
	}
	entries, readErr := os.ReadDir(tempRoot)
	if readErr != nil {
		t.Fatalf("ReadDir(%s) error = %v", tempRoot, readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("oversized download left temp entries: %#v", entries)
	}
}

func TestScanPublicChannelHistoryCapsTrackCount(t *testing.T) {
	messages := make([]tg.MessageClass, 0, telegramScanMaxTracks+1)
	for messageID := telegramScanMaxTracks + 1; messageID > 0; messageID-- {
		messages = append(messages, telegramAudioMessage(messageID))
	}
	api := &fakeTelegramHistoryClient{responses: []tg.MessagesMessagesClass{
		&tg.MessagesMessages{Messages: messages},
	}}

	_, err := scanPublicChannelHistory(context.Background(), api, "channel", &tg.InputPeerChannel{}, 0)
	if !errors.Is(err, errTelegramScanLimitExceeded) {
		t.Fatalf("scanPublicChannelHistory() error = %v, want %v", err, errTelegramScanLimitExceeded)
	}
}

func TestScanPublicChannelHistoryCapsPageCount(t *testing.T) {
	responses := make([]tg.MessagesMessagesClass, 0, telegramScanMaxPages)
	for page := 0; page < telegramScanMaxPages; page++ {
		responses = append(responses, &tg.MessagesMessages{Messages: []tg.MessageClass{
			&tg.Message{ID: telegramScanMaxPages - page},
		}})
	}
	api := &fakeTelegramHistoryClient{responses: responses}

	_, err := scanPublicChannelHistory(context.Background(), api, "channel", &tg.InputPeerChannel{}, 0)
	if !errors.Is(err, errTelegramScanLimitExceeded) {
		t.Fatalf("scanPublicChannelHistory() error = %v, want %v", err, errTelegramScanLimitExceeded)
	}
	if len(api.requests) != telegramScanMaxPages {
		t.Fatalf("history requests = %d, want %d", len(api.requests), telegramScanMaxPages)
	}
}

func TestWriteTelegramErrorMapsScanAndDownloadLimitsWithoutSentry(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{name: "scan", err: errTelegramScanLimitExceeded, wantStatus: http.StatusUnprocessableEntity},
		{name: "download", err: errTelegramTrackTooLarge, wantStatus: http.StatusRequestEntityTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, transport := newTelegramSentryRequest(t, http.MethodPost, "/api/telegram/import-sessions", nil, 42)
			response := httptest.NewRecorder()
			writeTelegramError(response, request, test.err, "import.start")
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if events := transport.Events(); len(events) != 0 {
				t.Fatalf("captured events = %d, want 0", len(events))
			}
		})
	}
}

func TestTelegramUserOperationLockHonorsCancellationAndEvictsEntry(t *testing.T) {
	service, _, _, _ := newTelegramImportTestService(t, &fakeTelegramGateway{})
	release, err := service.lockUserOperation(context.Background(), 7)
	if err != nil {
		t.Fatalf("first lockUserOperation() error = %v", err)
	}

	waiterCtx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		unlock, err := service.lockUserOperation(waiterCtx, 7)
		if unlock != nil {
			unlock()
		}
		waiterDone <- err
	}()
	cancel()
	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting lock error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled operation lock did not return")
	}

	release()
	service.mu.Lock()
	_, retained := service.operationMu[7]
	service.mu.Unlock()
	if retained {
		t.Fatal("idle per-user operation lock was retained")
	}
}

type sequencedTelegramGateway struct {
	*fakeTelegramGateway
	mu            sync.Mutex
	scanBatches   [][]telegramScannedTrack
	scanCalls     int
	downloadCalls int
	failDownload  int
	failure       error
}

func (g *sequencedTelegramGateway) ScanPublicChannel(context.Context, string, int) ([]telegramScannedTrack, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.scanCalls++
	if g.scanCalls > len(g.scanBatches) {
		return nil, errTelegramNoAudioTracks
	}
	return append([]telegramScannedTrack(nil), g.scanBatches[g.scanCalls-1]...), nil
}

func (g *sequencedTelegramGateway) DownloadTrack(_ context.Context, _ telegramScannedTrack, destinationPath string) error {
	g.mu.Lock()
	g.downloadCalls++
	call := g.downloadCalls
	g.mu.Unlock()
	if call == g.failDownload {
		return g.failure
	}
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(destinationPath, []byte("audio"), 0o644)
}

func TestTelegramReplacementIsAtomicWhenInitialDownloadFails(t *testing.T) {
	failure := errors.New("download unavailable")
	oldItem := telegramScannedTrack{MessageID: 1, FileName: "old.mp3", MimeType: "audio/mpeg"}
	newItem := telegramScannedTrack{MessageID: 2, FileName: "new.mp3", MimeType: "audio/mpeg"}
	gateway := &sequencedTelegramGateway{
		fakeTelegramGateway: &fakeTelegramGateway{},
		scanBatches:         [][]telegramScannedTrack{{oldItem}, {newItem}},
		failDownload:        2,
		failure:             failure,
	}
	service, _, _, tempRoot := newTelegramImportTestService(t, gateway)
	ctx := context.Background()
	oldDTO, err := service.StartSession(ctx, 1, "old", 0, false)
	if err != nil {
		t.Fatalf("initial StartSession() error = %v", err)
	}
	oldPath := filepath.Join(tempRoot, oldDTO.SessionID, buildTempFileName(0, oldItem.FileName))

	_, err = service.StartSession(ctx, 1, "new", 0, true)
	if !errors.Is(err, failure) {
		t.Fatalf("replacement StartSession() error = %v, want %v", err, failure)
	}
	current, currentErr := service.CurrentSession(ctx, 1)
	if currentErr != nil {
		t.Fatalf("CurrentSession() after failed replacement error = %v", currentErr)
	}
	if current.SessionID != oldDTO.SessionID || current.ChannelUsername != "old" {
		t.Fatalf("session after failed replacement = %#v, want original", current)
	}
	if _, statErr := os.Stat(oldPath); statErr != nil {
		t.Fatalf("original temp file was not preserved: %v", statErr)
	}
	entries, readErr := os.ReadDir(tempRoot)
	if readErr != nil {
		t.Fatalf("ReadDir(%s) error = %v", tempRoot, readErr)
	}
	if len(entries) != 1 || entries[0].Name() != oldDTO.SessionID {
		t.Fatalf("temp entries after failed replacement = %#v, want original only", entries)
	}
}

func TestTelegramAudioLeaseSurvivesConcurrentCancel(t *testing.T) {
	service, _, _, _ := newTelegramImportTestService(t, &fakeTelegramGateway{
		scannedItems: []telegramScannedTrack{{MessageID: 1, FileName: "leased.mp3", MimeType: "audio/mpeg"}},
		downloadData: []byte("leased audio"),
	})
	ctx := context.Background()
	if _, err := service.StartSession(ctx, 1, "channel", 0, false); err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	lease, err := service.OpenCurrentAudio(ctx, 1)
	if err != nil {
		t.Fatalf("OpenCurrentAudio() error = %v", err)
	}
	defer lease.Close()
	if err := service.cancelSessionWithContext(ctx, 1); err != nil {
		t.Fatalf("CancelSession() error = %v", err)
	}
	data, err := io.ReadAll(lease.File)
	if err != nil {
		t.Fatalf("ReadAll(open lease) error = %v", err)
	}
	if got := string(data); got != "leased audio" {
		t.Fatalf("leased audio = %q", got)
	}
}

func TestTelegramPendingLoginExpires(t *testing.T) {
	service, _, _, _ := newTelegramImportTestService(t, &fakeTelegramGateway{})
	now := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	ctx := telegramTestUserContext(1)
	if err := service.BeginLogin(ctx, "+111111111"); err != nil {
		t.Fatalf("BeginLogin() error = %v", err)
	}
	now = now.Add(telegramPendingLoginTTL + time.Second)
	_, err := service.ConfirmLogin(ctx, "+111111111", "12345")
	if !errors.Is(err, errTelegramInvalidLoginCode) {
		t.Fatalf("ConfirmLogin() expired error = %v, want %v", err, errTelegramInvalidLoginCode)
	}
	service.authMu.Lock()
	_, retained := service.pendingLogins[1]
	service.authMu.Unlock()
	if retained {
		t.Fatal("expired pending login was retained")
	}
}

func TestTelegramCompletedSessionExpiresAndOperationLockIsEvicted(t *testing.T) {
	service, _, _, _ := newTelegramImportTestService(t, &fakeTelegramGateway{
		scannedItems: []telegramScannedTrack{{MessageID: 1, FileName: "done.mp3", MimeType: "audio/mpeg"}},
		downloadData: []byte("audio"),
	})
	now := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	ctx := context.Background()
	if _, err := service.StartSession(ctx, 1, "channel", 0, false); err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	completed, err := service.skipCurrentWithContext(ctx, 1)
	if err != nil || completed.Status != telegramImportStatusCompleted {
		t.Fatalf("SkipCurrent() = (%#v, %v)", completed, err)
	}
	service.mu.Lock()
	retained := service.sessions[1]
	service.mu.Unlock()
	if retained == nil || len(retained.Items) != 0 || retained.TotalItems != 1 {
		t.Fatalf("completed session was not compacted: %#v", retained)
	}

	now = now.Add(telegramCompletedSessionTTL + time.Second)
	_, err = service.CurrentSession(ctx, 1)
	if !errors.Is(err, errTelegramSessionNotFound) {
		t.Fatalf("CurrentSession() expired error = %v, want %v", err, errTelegramSessionNotFound)
	}
	service.mu.Lock()
	_, sessionRetained := service.sessions[1]
	_, lockRetained := service.operationMu[1]
	service.mu.Unlock()
	if sessionRetained || lockRetained {
		t.Fatalf("expired state retained: session=%t lock=%t", sessionRetained, lockRetained)
	}
}

func TestTelegramSkippedReportEvictsCompletedSession(t *testing.T) {
	service, _, _, _ := newTelegramImportTestService(t, &fakeTelegramGateway{
		scannedItems: []telegramScannedTrack{{MessageID: 1, FileName: "done.mp3", MimeType: "audio/mpeg"}},
		downloadData: []byte("audio"),
	})
	ctx := context.Background()
	if _, err := service.StartSession(ctx, 1, "channel", 0, false); err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	if _, err := service.skipCurrentWithContext(ctx, 1); err != nil {
		t.Fatalf("SkipCurrent() error = %v", err)
	}
	if _, err := service.skippedReportWithContext(ctx, 1); err != nil {
		t.Fatalf("SkippedReport() error = %v", err)
	}
	if err := service.evictCompletedSessionAfterReport(ctx, 1); err != nil {
		t.Fatalf("evictCompletedSessionAfterReport() error = %v", err)
	}
	service.mu.Lock()
	_, sessionRetained := service.sessions[1]
	service.mu.Unlock()
	if sessionRetained {
		t.Fatal("completed session was retained after final report")
	}
}

func TestTelegramCloseCleansSessionsPendingSecretsAndLocks(t *testing.T) {
	service, _, _, tempRoot := newTelegramImportTestService(t, &fakeTelegramGateway{
		scannedItems: []telegramScannedTrack{{MessageID: 1, FileName: "active.mp3", MimeType: "audio/mpeg"}},
		downloadData: []byte("audio"),
	})
	ctx := context.Background()
	for _, userID := range []int64{1, 2} {
		if _, err := service.StartSession(ctx, userID, "channel", 0, false); err != nil {
			t.Fatalf("StartSession(%d) error = %v", userID, err)
		}
	}
	service.authMu.Lock()
	service.pendingLogins[1] = &telegramPendingLogin{PhoneNumber: "+111", CodeHash: "secret", RequestedAt: time.Now()}
	service.authMu.Unlock()

	if err := service.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	service.mu.Lock()
	sessionCount := len(service.sessions)
	lockCount := len(service.operationMu)
	service.mu.Unlock()
	service.authMu.Lock()
	pendingCount := len(service.pendingLogins)
	service.authMu.Unlock()
	if sessionCount != 0 || lockCount != 0 || pendingCount != 0 {
		t.Fatalf("state after Close: sessions=%d locks=%d pending=%d", sessionCount, lockCount, pendingCount)
	}
	entries, readErr := os.ReadDir(tempRoot)
	if readErr != nil {
		t.Fatalf("ReadDir(%s) error = %v", tempRoot, readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("temp entries after Close = %#v", entries)
	}
	if _, err := service.StartSession(ctx, 3, "channel", 0, false); !errors.Is(err, errTelegramShuttingDown) {
		t.Fatalf("StartSession() after Close error = %v, want %v", err, errTelegramShuttingDown)
	}
}

func newTelegramSentryRequest(t *testing.T, method, target string, body *bytes.Buffer, userID int64) (*http.Request, *sentry.MockTransport) {
	t.Helper()
	transport := &sentry.MockTransport{}
	client, err := sentry.NewClient(sentry.ClientOptions{Transport: transport})
	if err != nil {
		t.Fatalf("sentry.NewClient() error = %v", err)
	}
	hub := sentry.NewHub(client, sentry.NewScope())
	var requestBody *bytes.Buffer
	if body != nil {
		requestBody = body
	} else {
		requestBody = &bytes.Buffer{}
	}
	request := httptest.NewRequest(method, target, requestBody)
	ctx := context.WithValue(request.Context(), userContextKey, userID)
	ctx = sentry.SetHubOnContext(ctx, hub)
	request = request.WithContext(ctx)
	return withSentryRequestCaptureState(request), transport
}

func telegramTestUserContext(userID int64) context.Context {
	return context.WithValue(context.Background(), userContextKey, userID)
}

func newTelegramImportTestService(t *testing.T, gateway telegramGateway) (*telegramImportService, *trackStore, string, string) {
	t.Helper()

	root := t.TempDir()
	songsDir := filepath.Join(root, "songs")
	tempRoot := filepath.Join(root, "telegram_import_tmp")
	stateDir := filepath.Join(root, "telegram_state")
	dbPath := filepath.Join(root, "tracks_db.json")

	for _, dir := range []string{songsDir, tempRoot, stateDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", dir, err)
		}
	}

	store, err := newTrackStore(dbPath)
	if err != nil {
		t.Fatalf("newTrackStore() error = %v", err)
	}

	service := newTelegramImportService(telegramConfig{
		APIID:          1,
		APIHash:        "hash",
		StateDir:       stateDir,
		SessionFile:    filepath.Join(stateDir, "session.json"),
		ImportTempDir:  tempRoot,
		RequestTimeout: time.Second,
	}, gateway, store, songsDir)

	return service, store, songsDir, tempRoot
}

func seedTrackDependencies(t *testing.T, store *trackStore) (author, album) {
	t.Helper()

	artist, err := store.createAuthor(upsertAuthorRequest{
		CurrentName: "Artist",
	})
	if err != nil {
		t.Fatalf("createAuthor() error = %v", err)
	}

	albumItem, err := store.createAlbum(upsertAlbumRequest{
		Title:       "Album",
		ReleaseDate: time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC),
		IsPublished: false,
	})
	if err != nil {
		t.Fatalf("createAlbum() error = %v", err)
	}

	return artist, albumItem
}
