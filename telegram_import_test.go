package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeTelegramGateway struct {
	status       telegramAuthStatus
	scannedItems []telegramScannedTrack
	downloadData []byte
	passwordAuth bool
}

func (f *fakeTelegramGateway) Status(context.Context) (telegramAuthStatus, error) {
	return f.status, nil
}

func (f *fakeTelegramGateway) BeginLogin(context.Context, string) (string, error) {
	return "code-hash", nil
}

func (f *fakeTelegramGateway) ConfirmLogin(context.Context, string, string, string) (telegramAuthStatus, error) {
	if f.passwordAuth {
		result := f.status
		result.PasswordRequired = true
		result.Authorized = false
		return result, nil
	}
	return f.status, nil
}

func (f *fakeTelegramGateway) PasswordLogin(context.Context, string) (telegramAuthStatus, error) {
	result := f.status
	result.PasswordRequired = false
	result.Authorized = true
	return result, nil
}

func (f *fakeTelegramGateway) ScanPublicChannel(context.Context, string) ([]telegramScannedTrack, error) {
	if len(f.scannedItems) == 0 {
		return nil, errTelegramNoAudioTracks
	}
	items := make([]telegramScannedTrack, len(f.scannedItems))
	copy(items, f.scannedItems)
	return items, nil
}

func (f *fakeTelegramGateway) DownloadTrack(_ context.Context, _ telegramScannedTrack, destinationPath string) error {
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
	if !strings.HasPrefix(createdTrack.AudioFilePath, "/songs/") {
		t.Fatalf("created track audio path = %q", createdTrack.AudioFilePath)
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
	if len(sessionTempEntries) != 1 {
		t.Fatalf("temp entries = %#v", sessionTempEntries)
	}
	sessionTempDir := filepath.Join(tempRoot, sessionTempEntries[0].Name())
	remaining, err := os.ReadDir(sessionTempDir)
	if err != nil {
		t.Fatalf("ReadDir(%s) error = %v", sessionTempDir, err)
	}
	if len(remaining) != 0 {
		t.Fatalf("remaining temp files = %#v", remaining)
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
	})

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
