package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestYouTubeCloseWaitsForMutationsAndRejectsNewOnes(t *testing.T) {
	service := newYouTubeImportService(
		youtubeImportConfig{},
		&fakeYouTubeGateway{},
		nil,
		t.TempDir(),
	)
	finishOperation, err := service.beginOperation()
	if err != nil {
		t.Fatalf("beginOperation() error = %v", err)
	}

	closeResult := make(chan error, 1)
	go func() {
		closeResult <- service.Close()
	}()
	deadline := time.Now().Add(time.Second)
	for {
		service.lifecycleMu.Lock()
		closing := service.closing
		service.lifecycleMu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close() did not mark the service as closing")
		}
		time.Sleep(time.Millisecond)
	}

	if _, err := service.StartSession(context.Background(), 1, "https://www.youtube.com/watch?v=closing", nil, false, ""); !errors.Is(err, errYouTubeShuttingDown) {
		t.Fatalf("StartSession() error = %v, want %v", err, errYouTubeShuttingDown)
	}
	select {
	case err := <-closeResult:
		t.Fatalf("Close() returned before the active mutation finished: %v", err)
	default:
	}

	finishOperation()
	select {
	case err := <-closeResult:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close() did not finish after the active mutation ended")
	}
}

func TestYouTubeOperationLockWaitIsContextAwareAndReleasesReference(t *testing.T) {
	service := &youtubeImportService{operationLocks: make(map[int64]*youtubeImportUserLock)}
	unlock, err := service.lockUserOperation(context.Background(), 42)
	if err != nil {
		t.Fatalf("lockUserOperation() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := service.lockUserOperation(ctx, 42)
		result <- err
	}()
	waitForYouTubeOperationRefs(t, service, 42, 2)
	cancel()
	if err := receiveYouTubeOperationResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting lock error = %v, want context cancellation", err)
	}
	waitForYouTubeOperationRefs(t, service, 42, 1)
	unlock()
	waitForYouTubeOperationRefs(t, service, 42, 0)
}

type cancelingYouTubeScanGateway struct {
	cancel context.CancelFunc
	item   youtubeImportItem
}

func (g *cancelingYouTubeScanGateway) Scan(context.Context, string, *time.Time, string) ([]youtubeImportItem, youtubeImportScanSource, error) {
	g.cancel()
	return []youtubeImportItem{g.item}, youtubeImportScanSource{SourceType: youtubeImportSourceTrack, CanonicalURL: g.item.SourceURL}, nil
}

func (*cancelingYouTubeScanGateway) DownloadAudio(context.Context, youtubeImportItem, string, string) error {
	return errors.New("unexpected download")
}

func TestYouTubeStartRechecksContextBeforeSessionCommit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	item := youtubeImportItem{
		VideoID:           "canceled-start",
		SourceURL:         "https://www.youtube.com/watch?v=canceled-start",
		OriginalSourceURL: "https://www.youtube.com/watch?v=canceled-start",
		LinkProvider:      "youtube",
		ParsedTitle:       "Canceled",
		ParsedAuthorNames: []string{"Artist"},
	}
	service, _ := newYouTubeSentryTestService(t, &cancelingYouTubeScanGateway{cancel: cancel, item: item})
	if _, err := service.StartSession(ctx, 1, item.SourceURL, nil, false, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("StartSession() error = %v, want context cancellation", err)
	}
	if _, err := service.CurrentSession(1); !errors.Is(err, errYouTubeSessionNotFound) {
		t.Fatalf("CurrentSession() error = %v, want no committed session", err)
	}
}

type snapshottingYouTubeGateway struct {
	*fakeYouTubeGateway
	store *youtubeCookieStore
}

func (g *snapshottingYouTubeGateway) SnapshotCookies(destinationPath string) (bool, error) {
	err := g.store.snapshotTo(destinationPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func TestYouTubeSessionSnapshotsConfiguredCookies(t *testing.T) {
	root := t.TempDir()
	configured := newYouTubeCookieStore(filepath.Join(root, "configured-cookies.txt"))
	if err := configured.Replace(strings.NewReader("original-cookie")); err != nil {
		t.Fatalf("Replace(original) error = %v", err)
	}
	fake := &fakeYouTubeGateway{
		scanSource: youtubeImportScanSource{SourceType: youtubeImportSourceTrack, CanonicalURL: "https://www.youtube.com/watch?v=cookie-snapshot"},
		scanItems: []youtubeImportItem{{
			VideoID:           "cookie-snapshot",
			SourceURL:         "https://www.youtube.com/watch?v=cookie-snapshot",
			OriginalSourceURL: "https://www.youtube.com/watch?v=cookie-snapshot",
			LinkProvider:      "youtube",
			ParsedTitle:       "Cookie Snapshot",
			ParsedAuthorNames: []string{"Artist"},
		}},
		downloadData: []byte("audio"),
	}
	gateway := &snapshottingYouTubeGateway{fakeYouTubeGateway: fake, store: configured}
	service, store, _, _ := newYouTubeImportTestService(t, gateway)
	artist, albumItem := seedTrackDependencies(t, store)
	if _, err := service.StartSession(context.Background(), 1, fake.scanItems[0].SourceURL, nil, false, ""); err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	snapshotPath := fake.scanCookiesPath
	if snapshotPath == "" || snapshotPath == configured.path {
		t.Fatalf("scan cookies path = %q, want independent snapshot", snapshotPath)
	}
	if err := configured.Replace(strings.NewReader("replacement-cookie")); err != nil {
		t.Fatalf("Replace(replacement) error = %v", err)
	}
	if err := configured.Delete(); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	data, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("ReadFile(snapshot) error = %v", err)
	}
	if string(data) != "original-cookie" {
		t.Fatalf("snapshot = %q, want original cookie data", data)
	}
	if _, _, err := service.AddCurrent(context.Background(), 1, youtubeCreateRequest(artist, albumItem, "Cookie Snapshot")); err != nil {
		t.Fatalf("AddCurrent() error = %v", err)
	}
	if fake.downloadCookiesPath != snapshotPath {
		t.Fatalf("download cookies path = %q, want snapshot %q", fake.downloadCookiesPath, snapshotPath)
	}
}

func TestYouTubeMetadataTransportCapsMetadataButNotMedia(t *testing.T) {
	const payload = "0123456789abcdef"
	transport := youtubeMetadataLimitTransport{
		limit: 8,
		base: youtubeRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Body:          io.NopCloser(strings.NewReader(payload)),
				ContentLength: -1,
				Header:        make(http.Header),
				Request:       req,
			}, nil
		}),
	}

	metadataReq, _ := http.NewRequest(http.MethodGet, "https://www.youtube.com/watch?v=test", nil)
	metadataResponse, err := transport.RoundTrip(metadataReq)
	if err != nil {
		t.Fatalf("metadata RoundTrip() error = %v", err)
	}
	metadata, err := io.ReadAll(metadataResponse.Body)
	_ = metadataResponse.Body.Close()
	if !errors.Is(err, errYouTubeResponseTooLarge) || len(metadata) > 8 {
		t.Fatalf("metadata read length=%d error=%v, want bounded response error", len(metadata), err)
	}

	mediaReq, _ := http.NewRequest(http.MethodGet, "https://r1---sn.example.googlevideo.com/videoplayback", nil)
	mediaResponse, err := transport.RoundTrip(mediaReq)
	if err != nil {
		t.Fatalf("media RoundTrip() error = %v", err)
	}
	media, err := io.ReadAll(mediaResponse.Body)
	_ = mediaResponse.Body.Close()
	if err != nil || string(media) != payload {
		t.Fatalf("media read = %q error=%v, want uncapped payload", media, err)
	}
}

func TestYouTubeAudioWritersEnforceHardLimitAndCleanup(t *testing.T) {
	var copied bytes.Buffer
	written, err := copyYouTubeAudio(&copied, strings.NewReader("0123456789abcdef"), 8)
	if !errors.Is(err, errYouTubeAudioTooLarge) || written != 8 || copied.Len() != 8 {
		t.Fatalf("copyYouTubeAudio() written=%d retained=%d error=%v", written, copied.Len(), err)
	}

	root := t.TempDir()
	binaryPath := filepath.Join(root, "fake-yt-dlp.sh")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\nprintf '0123456789abcdef'\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(binary) error = %v", err)
	}
	destination := filepath.Join(root, "download.bin")
	if err := runYTDLPDownload(context.Background(), binaryPath, nil, destination, 8); !errors.Is(err, errYouTubeAudioTooLarge) {
		t.Fatalf("runYTDLPDownload() error = %v, want size error", err)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized destination stat error = %v, want removed file", err)
	}
}

type parallelDuplicateYouTubeGateway struct {
	item    youtubeImportItem
	started chan struct{}
	release chan struct{}
}

func (g *parallelDuplicateYouTubeGateway) Scan(context.Context, string, *time.Time, string) ([]youtubeImportItem, youtubeImportScanSource, error) {
	return []youtubeImportItem{g.item}, youtubeImportScanSource{SourceType: youtubeImportSourceTrack, CanonicalURL: g.item.SourceURL}, nil
}

func (g *parallelDuplicateYouTubeGateway) DownloadAudio(ctx context.Context, _ youtubeImportItem, destinationPath string, _ string) error {
	g.started <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return os.WriteFile(destinationPath, []byte("audio"), 0o644)
}

func TestYouTubeCrossUserDuplicateSourceHasSingleAtomicWinner(t *testing.T) {
	item := youtubeImportItem{
		VideoID:           "same-source",
		SourceURL:         "https://www.youtube.com/watch?v=same-source",
		OriginalSourceURL: "https://www.youtube.com/watch?v=same-source",
		LinkProvider:      "youtube",
		ParsedTitle:       "Same Source",
		ParsedAuthorNames: []string{"Artist"},
	}
	gateway := &parallelDuplicateYouTubeGateway{item: item, started: make(chan struct{}, 2), release: make(chan struct{})}
	service, store, songsDir, _ := newYouTubeImportTestService(t, gateway)
	artist, albumItem := seedTrackDependencies(t, store)
	for _, userID := range []int64{1, 2} {
		if _, err := service.StartSession(context.Background(), userID, item.SourceURL, nil, false, ""); err != nil {
			t.Fatalf("StartSession(%d) error = %v", userID, err)
		}
	}

	results := make(chan error, 2)
	var start sync.WaitGroup
	start.Add(2)
	for _, userID := range []int64{1, 2} {
		userID := userID
		go func() {
			start.Done()
			start.Wait()
			_, _, err := service.AddCurrent(context.Background(), userID, youtubeCreateRequest(artist, albumItem, "Same Source"))
			results <- err
		}()
	}
	<-gateway.started
	<-gateway.started
	close(gateway.release)
	firstErr := receiveYouTubeOperationResult(t, results)
	secondErr := receiveYouTubeOperationResult(t, results)
	if (firstErr == nil) == (secondErr == nil) {
		t.Fatalf("AddCurrent() errors = (%v, %v), want exactly one success", firstErr, secondErr)
	}
	loserErr := firstErr
	if loserErr == nil {
		loserErr = secondErr
	}
	if !errors.Is(loserErr, errYouTubeCurrentConflict) {
		t.Fatalf("loser error = %v, want duplicate-source conflict", loserErr)
	}
	if tracks := store.list(); len(tracks) != 1 {
		t.Fatalf("stored tracks = %d, want exactly one", len(tracks))
	}
	entries, err := os.ReadDir(songsDir)
	if err != nil {
		t.Fatalf("ReadDir(songs) error = %v", err)
	}
	regularFiles := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			regularFiles++
		}
	}
	if regularFiles != 1 {
		t.Fatalf("published song files = %d entries=%#v, want one", regularFiles, entries)
	}
}

func TestYouTubeAudioStaysHiddenUntilCommitAndCloseCleansActiveFiles(t *testing.T) {
	item := youtubeImportItem{
		VideoID:           "hidden-stage",
		SourceURL:         "https://www.youtube.com/watch?v=hidden-stage",
		OriginalSourceURL: "https://www.youtube.com/watch?v=hidden-stage",
		LinkProvider:      "youtube",
		ParsedTitle:       "Hidden Stage",
		ParsedAuthorNames: []string{"Artist"},
	}
	service, _, songsDir, tempDir := newYouTubeImportTestService(t, &fakeYouTubeGateway{downloadData: []byte("audio")})
	prepared, err := service.downloadCurrentToSongs(context.Background(), 0, item, "")
	if err != nil {
		t.Fatalf("downloadCurrentToSongs() error = %v", err)
	}
	entries, err := os.ReadDir(songsDir)
	if err != nil {
		t.Fatalf("ReadDir(songs) error = %v", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			t.Fatalf("partially prepared song is publicly visible as %q", entry.Name())
		}
	}
	cookiePath := filepath.Join(tempDir, "youtube-cookies-close.txt")
	if err := os.WriteFile(cookiePath, []byte("cookie"), 0o600); err != nil {
		t.Fatalf("WriteFile(cookie) error = %v", err)
	}
	service.registerActiveFile(cookiePath)
	service.mu.Lock()
	service.sessions[7] = &youtubeImportSession{ID: "close-session", UserID: 7, Status: youtubeImportStatusActive, CookiesPath: cookiePath}
	service.mu.Unlock()
	finalPath := filepath.Join(songsDir, "keep.mp3")
	if err := os.WriteFile(finalPath, []byte("published"), 0o644); err != nil {
		t.Fatalf("WriteFile(final) error = %v", err)
	}
	if err := service.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	for _, path := range []string{prepared.sourcePath, prepared.stagingPath, cookiePath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("active path %q stat error = %v, want removed", path, err)
		}
	}
	if _, err := os.Stat(finalPath); err != nil {
		t.Fatalf("published song was removed: %v", err)
	}
	if _, err := service.CurrentSession(7); !errors.Is(err, errYouTubeSessionNotFound) {
		t.Fatalf("CurrentSession() after Close error = %v", err)
	}
}

func TestCleanupStaleYouTubeSongStagingIsScoped(t *testing.T) {
	songsDir := t.TempDir()
	stagingDir := filepath.Join(songsDir, ".youtube-import-staging")
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(staging) error = %v", err)
	}
	stalePath := filepath.Join(stagingDir, "youtube-import-stale.mp3")
	finalPath := filepath.Join(songsDir, "published.mp3")
	for path, value := range map[string]string{stalePath: "partial", finalPath: "published"} {
		if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}
	if err := cleanupStaleYouTubeSongStaging(songsDir); err != nil {
		t.Fatalf("cleanupStaleYouTubeSongStaging() error = %v", err)
	}
	if _, err := os.Stat(stagingDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging directory stat error = %v, want removed", err)
	}
	data, err := os.ReadFile(finalPath)
	if err != nil || string(data) != "published" {
		t.Fatalf("published file data=%q error=%v, want preserved", data, err)
	}
}
