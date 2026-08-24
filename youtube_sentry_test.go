package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	youtube "github.com/kkdai/youtube/v2"
)

func TestClassifyYouTubeScanError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{name: "rate limited", err: youtube.ErrUnexpectedStatusCode(http.StatusTooManyRequests), want: errYouTubeUpstreamFailed},
		{name: "upstream server error", err: youtube.ErrUnexpectedStatusCode(http.StatusServiceUnavailable), want: errYouTubeUpstreamFailed},
		{name: "upstream gateway timeout", err: youtube.ErrUnexpectedStatusCode(http.StatusGatewayTimeout), want: errYouTubeUpstreamTimeout},
		{name: "upstream request timeout", err: youtube.ErrUnexpectedStatusCode(http.StatusRequestTimeout), want: errYouTubeUpstreamTimeout},
		{name: "upstream redirect", err: youtube.ErrUnexpectedStatusCode(http.StatusTemporaryRedirect), want: errYouTubeUpstreamFailed},
		{name: "missing source", err: youtube.ErrUnexpectedStatusCode(http.StatusNotFound), want: errYouTubeInvalidURL},
		{name: "request timeout", err: context.DeadlineExceeded, want: errYouTubeUpstreamTimeout},
		{name: "invalid video id", err: youtube.ErrVideoIDMinLength, want: errYouTubeInvalidURL},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classifyYouTubeScanError(test.err)
			if !errors.Is(got, test.want) {
				t.Fatalf("classifyYouTubeScanError(%v) = %v, want errors.Is(_, %v)", test.err, got, test.want)
			}
			if !errors.Is(got, test.err) {
				t.Fatalf("classifyYouTubeScanError(%v) lost original cause: %v", test.err, got)
			}
		})
	}
}

func TestCombineYouTubeScanErrorsKeepsOperationalFailure(t *testing.T) {
	root := errors.New("metadata parser failed")
	err := combineYouTubeScanErrors(errYouTubeNoTracks, root)
	if !errors.Is(err, errYouTubeNoTracks) {
		t.Fatalf("combined error = %v, want no-tracks cause", err)
	}
	if !errors.Is(err, errYouTubeUpstreamFailed) {
		t.Fatalf("combined error = %v, want upstream failure classification", err)
	}
	if !errors.Is(err, root) {
		t.Fatalf("combined error = %v, want original cause", err)
	}
}

func TestYouTubeStageErrorsPreserveRootCause(t *testing.T) {
	root := errors.New("process exited unexpectedly")
	downloadErr := youtubeDownloadError(youtubeImportItem{VideoID: "video-1"}, "ytdlp_run", nil, root)
	if !errors.Is(downloadErr, errYouTubeDownloadFailed) || !errors.Is(downloadErr, root) {
		t.Fatalf("youtubeDownloadError() = %v, want download sentinel and root cause", downloadErr)
	}

	transcodeErr := youtubeStageError(errYouTubeTranscodeFailed, "ffmpeg run", root)
	if !errors.Is(transcodeErr, errYouTubeTranscodeFailed) || !errors.Is(transcodeErr, root) {
		t.Fatalf("youtubeStageError() = %v, want transcode sentinel and root cause", transcodeErr)
	}
}

func TestYouTubeTranscodeErrorDoesNotIncludeCommandOutput(t *testing.T) {
	root := t.TempDir()
	binaryPath := filepath.Join(root, "fake-ffmpeg.sh")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\nprintf 'do-not-capture'\nexit 7\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", binaryPath, err)
	}

	var logs bytes.Buffer
	previousLogWriter := log.Writer()
	previousLogFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousLogWriter)
		log.SetFlags(previousLogFlags)
	})

	service := &youtubeImportService{cfg: youtubeImportConfig{FFmpegBinary: binaryPath}}
	sourcePath := filepath.Join(root, "secret-source-path")
	targetPath := filepath.Join(root, "secret-target-path")
	err := service.transcodeToMP3(context.Background(), sourcePath, targetPath)
	if !errors.Is(err, errYouTubeTranscodeFailed) {
		t.Fatalf("transcodeToMP3() error = %v, want %v", err, errYouTubeTranscodeFailed)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("transcodeToMP3() error = %v, want preserved *exec.ExitError", err)
	}
	if strings.Contains(err.Error(), "do-not-capture") {
		t.Fatalf("transcodeToMP3() error leaked command output: %v", err)
	}
	for _, secret := range []string{"do-not-capture", sourcePath, targetPath} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("transcode log leaked %q: %s", secret, logs.String())
		}
	}
}

func TestYouTubeErrorSanitizationRemovesUpstreamURL(t *testing.T) {
	const sensitiveURL = "https://www.youtube.com/watch?v=video&credential=secret"
	root := errors.New("connection reset")
	raw := &url.Error{Op: "Get", URL: sensitiveURL, Err: root}
	err := classifyYouTubeScanError(raw)
	if strings.Contains(err.Error(), sensitiveURL) || strings.Contains(err.Error(), "credential=secret") {
		t.Fatalf("classified error leaked URL: %v", err)
	}
	if !errors.Is(err, errYouTubeUpstreamFailed) || !errors.Is(err, root) {
		t.Fatalf("classified error = %v, want upstream and root causes", err)
	}
	if diagnostic := youtubeErrorDiagnostic(raw); strings.Contains(diagnostic, sensitiveURL) || strings.Contains(diagnostic, "credential=secret") {
		t.Fatalf("diagnostic leaked URL: %q", diagnostic)
	}
}

func TestSanitizeYouTubeCapturedErrorPreservesJoinedCauses(t *testing.T) {
	const sensitiveURL = "https://www.youtube.com/watch?v=video&credential=secret"
	const sensitivePath = "/private/cookies/user-secret.txt"
	upstreamRoot := errors.New("connection reset")
	primary := youtubeStageError(errYouTubeDownloadFailed, "download", &url.Error{
		Op:  "Get",
		URL: sensitiveURL,
		Err: upstreamRoot,
	})
	cleanup := sanitizeYouTubeFilesystemError("remove session cookies file", &os.PathError{
		Op:   "remove",
		Path: sensitivePath,
		Err:  os.ErrPermission,
	})

	err := sanitizeYouTubeCapturedError(errors.Join(primary, cleanup))
	for _, secret := range []string{sensitiveURL, "credential=secret", sensitivePath} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("sanitized joined error leaked %q: %v", secret, err)
		}
	}
	for _, cause := range []error{errYouTubeDownloadFailed, errYouTubeStorageFailed, upstreamRoot, os.ErrPermission} {
		if !errors.Is(err, cause) {
			t.Fatalf("sanitized joined error = %v, want cause %v", err, cause)
		}
	}
}

func TestWithYouTubeDownloadTimeoutRespectsEarlierParentDeadline(t *testing.T) {
	parentDeadline := time.Now().Add(time.Minute)
	parent, cancelParent := context.WithDeadline(context.Background(), parentDeadline)
	defer cancelParent()

	ctx, cancel := withYouTubeDownloadTimeout(parent, time.Hour)
	defer cancel()
	gotDeadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("download context has no deadline")
	}
	if !gotDeadline.Equal(parentDeadline) {
		t.Fatalf("download deadline = %v, want parent deadline %v", gotDeadline, parentDeadline)
	}
}

type blockingYouTubeDownloadGateway struct {
	item youtubeImportItem
}

func (g *blockingYouTubeDownloadGateway) Scan(context.Context, string, *time.Time, string) ([]youtubeImportItem, youtubeImportScanSource, error) {
	return []youtubeImportItem{g.item}, youtubeImportScanSource{
		SourceType:   youtubeImportSourceTrack,
		CanonicalURL: g.item.SourceURL,
	}, nil
}

func (*blockingYouTubeDownloadGateway) DownloadAudio(ctx context.Context, _ youtubeImportItem, _ string, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestYouTubeImportAddCurrentEnforcesDownloadTimeout(t *testing.T) {
	item := youtubeImportItem{
		VideoID:           "timeout-video",
		SourceURL:         "https://www.youtube.com/watch?v=timeout-video",
		OriginalSourceURL: "https://www.youtube.com/watch?v=timeout-video",
		LinkProvider:      "youtube",
		ParsedTitle:       "Timeout Track",
		ParsedAuthorNames: []string{"Artist"},
	}
	service, store := newYouTubeSentryTestService(t, &blockingYouTubeDownloadGateway{item: item})
	service.cfg.DownloadTimeout = 20 * time.Millisecond
	artist, album := seedYouTubeSentryDependencies(t, store)
	if _, err := service.StartSession(context.Background(), 1, item.SourceURL, nil, false, ""); err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	started := time.Now()
	_, _, err := service.AddCurrent(context.Background(), 1, youtubeAddCurrentRequest{
		Mode:      youtubeAddModeCreate,
		Name:      item.ParsedTitle,
		AuthorIDs: []int64{artist.ID},
		AlbumID:   album.ID,
	})
	if !errors.Is(err, errYouTubeDownloadFailed) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AddCurrent() error = %v, want download failure with deadline cause", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("AddCurrent() took %s, download timeout was not enforced", elapsed)
	}
}

func TestLiveYouTubeImportGatewayEnforcesScanTimeout(t *testing.T) {
	root := t.TempDir()
	binaryPath := filepath.Join(root, "fake-yt-dlp.sh")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\nexec sleep 5\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", binaryPath, err)
	}
	gateway := newLiveYouTubeImportGateway(youtubeImportConfig{
		RequestTimeout: 20 * time.Millisecond,
		YTDLPBinary:    binaryPath,
	}, newYouTubeCookieStore(""))

	started := time.Now()
	_, _, err := gateway.Scan(context.Background(), "https://www.youtube.com/@artist", nil, "")
	if !errors.Is(err, errYouTubeUpstreamTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Scan() error = %v, want upstream timeout with deadline cause", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Scan() took %s, request timeout was not enforced", elapsed)
	}
}

func TestRunYouTubeCommandWithOutputLimitRejectsOverflow(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "fake-yt-dlp.sh")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\nprintf '0123456789abcdef'\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", binaryPath, err)
	}

	output, err := runYouTubeCommandWithOutputLimit(context.Background(), binaryPath, nil, 8)
	if !errors.Is(err, errYouTubeResponseTooLarge) {
		t.Fatalf("runYouTubeCommandWithOutputLimit() error = %v, want %v", err, errYouTubeResponseTooLarge)
	}
	if output != nil {
		t.Fatalf("runYouTubeCommandWithOutputLimit() output length = %d, want no oversized output returned", len(output))
	}
}

type youtubeRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn youtubeRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestFetchYouTubeHTMLRejectsOversizedResponse(t *testing.T) {
	body := strings.Repeat("x", maxYouTubeArtistHTMLBytes+1)
	gateway := &liveYouTubeImportGateway{
		httpClient: &http.Client{Transport: youtubeRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Body:          io.NopCloser(strings.NewReader(body)),
				ContentLength: int64(len(body)),
				Header:        make(http.Header),
			}, nil
		})},
	}

	result, err := gateway.fetchHTML(context.Background(), "https://www.youtube.com/@artist")
	if result != "" {
		t.Fatalf("fetchHTML() returned %d bytes, want none", len(result))
	}
	if !errors.Is(err, errYouTubeResponseTooLarge) || !errors.Is(err, errYouTubeUpstreamFailed) {
		t.Fatalf("fetchHTML() error = %v, want response-too-large upstream failure", err)
	}
}

type blockingYouTubeMutationGateway struct {
	downloadStarted chan struct{}
	downloadRelease chan struct{}
	startOnce       sync.Once
}

func newBlockingYouTubeMutationGateway() *blockingYouTubeMutationGateway {
	return &blockingYouTubeMutationGateway{
		downloadStarted: make(chan struct{}),
		downloadRelease: make(chan struct{}),
	}
}

func (g *blockingYouTubeMutationGateway) Scan(_ context.Context, rawURL string, _ *time.Time, _ string) ([]youtubeImportItem, youtubeImportScanSource, error) {
	videoID := "first-video"
	title := "First Track"
	if strings.Contains(rawURL, "replacement") {
		videoID = "replacement-video"
		title = "Replacement Track"
	}
	item := youtubeImportItem{
		VideoID:           videoID,
		SourceURL:         "https://www.youtube.com/watch?v=" + videoID,
		OriginalSourceURL: rawURL,
		LinkProvider:      "youtube",
		ParsedTitle:       title,
		ParsedAuthorNames: []string{"Artist"},
	}
	return []youtubeImportItem{item}, youtubeImportScanSource{
		SourceType:   youtubeImportSourceTrack,
		CanonicalURL: item.SourceURL,
	}, nil
}

func (g *blockingYouTubeMutationGateway) DownloadAudio(ctx context.Context, _ youtubeImportItem, destinationPath string, _ string) error {
	g.startOnce.Do(func() { close(g.downloadStarted) })
	select {
	case <-g.downloadRelease:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(destinationPath, []byte("source audio"), 0o644)
}

func installPassingYouTubeFFmpeg(t *testing.T, service *youtubeImportService) {
	t.Helper()
	binaryPath := filepath.Join(t.TempDir(), "fake-ffmpeg.sh")
	script := "#!/bin/sh\nfor arg in \"$@\"; do target=$arg; done\nprintf 'mp3 audio' > \"$target\"\n"
	if err := os.WriteFile(binaryPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", binaryPath, err)
	}
	service.cfg.FFmpegBinary = binaryPath
}

func youtubeCreateRequest(artist author, albumItem album, name string) youtubeAddCurrentRequest {
	return youtubeAddCurrentRequest{
		Mode:      youtubeAddModeCreate,
		Name:      name,
		AuthorIDs: []int64{artist.ID},
		AlbumID:   albumItem.ID,
	}
}

func TestYouTubeImportSerializesAddAndCancel(t *testing.T) {
	gateway := newBlockingYouTubeMutationGateway()
	service, store := newYouTubeSentryTestService(t, gateway)
	installPassingYouTubeFFmpeg(t, service)
	artist, albumItem := seedYouTubeSentryDependencies(t, store)
	if _, err := service.StartSession(context.Background(), 1, "https://www.youtube.com/watch?v=first-video", nil, false, ""); err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	addResult := make(chan error, 1)
	go func() {
		_, _, err := service.AddCurrent(context.Background(), 1, youtubeCreateRequest(artist, albumItem, "First Track"))
		addResult <- err
	}()
	<-gateway.downloadStarted

	cancelStarted := make(chan struct{})
	cancelResult := make(chan error, 1)
	go func() {
		close(cancelStarted)
		cancelResult <- service.CancelSession(1)
	}()
	<-cancelStarted
	waitForYouTubeOperationRefs(t, service, 1, 2)
	select {
	case err := <-cancelResult:
		t.Fatalf("CancelSession() completed during AddCurrent(): %v", err)
	default:
	}
	if current, err := service.CurrentSession(1); err != nil || current.CurrentItem == nil || current.CurrentItem.VideoID != "first-video" {
		t.Fatalf("CurrentSession() while download runs = %#v, %v", current, err)
	}

	close(gateway.downloadRelease)
	if err := receiveYouTubeOperationResult(t, addResult); err != nil {
		t.Fatalf("AddCurrent() error = %v", err)
	}
	if err := receiveYouTubeOperationResult(t, cancelResult); err != nil {
		t.Fatalf("CancelSession() error = %v", err)
	}
	if _, err := service.CurrentSession(1); !errors.Is(err, errYouTubeSessionNotFound) {
		t.Fatalf("CurrentSession() after cancel error = %v, want %v", err, errYouTubeSessionNotFound)
	}
	waitForYouTubeOperationRefs(t, service, 1, 0)
}

func TestYouTubeImportSerializesAddAndReplacement(t *testing.T) {
	gateway := newBlockingYouTubeMutationGateway()
	service, store := newYouTubeSentryTestService(t, gateway)
	installPassingYouTubeFFmpeg(t, service)
	artist, albumItem := seedYouTubeSentryDependencies(t, store)
	if _, err := service.StartSession(context.Background(), 1, "https://www.youtube.com/watch?v=first-video", nil, false, ""); err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	addResult := make(chan error, 1)
	go func() {
		dto, _, err := service.AddCurrent(context.Background(), 1, youtubeCreateRequest(artist, albumItem, "First Track"))
		if err == nil && (dto.Status != youtubeImportStatusCompleted || dto.Progress.Saved != 1) {
			err = fmt.Errorf("unexpected completed session: %#v", dto)
		}
		addResult <- err
	}()
	<-gateway.downloadStarted

	replaceStarted := make(chan struct{})
	replaceResult := make(chan error, 1)
	go func() {
		close(replaceStarted)
		_, err := service.StartSession(context.Background(), 1, "https://www.youtube.com/watch?v=replacement", nil, true, "")
		replaceResult <- err
	}()
	<-replaceStarted
	waitForYouTubeOperationRefs(t, service, 1, 2)
	select {
	case err := <-replaceResult:
		t.Fatalf("replacement StartSession() completed during AddCurrent(): %v", err)
	default:
	}

	close(gateway.downloadRelease)
	if err := receiveYouTubeOperationResult(t, addResult); err != nil {
		t.Fatalf("AddCurrent() error = %v", err)
	}
	if err := receiveYouTubeOperationResult(t, replaceResult); err != nil {
		t.Fatalf("replacement StartSession() error = %v", err)
	}
	current, err := service.CurrentSession(1)
	if err != nil {
		t.Fatalf("CurrentSession() error = %v", err)
	}
	if current.CurrentItem == nil || current.CurrentItem.VideoID != "replacement-video" || current.Progress.Processed != 0 {
		t.Fatalf("replacement session was mutated by prior add: %#v", current)
	}
	waitForYouTubeOperationRefs(t, service, 1, 0)
}

type blockingYouTubeStartGateway struct {
	scanCalls chan struct{}
	release   chan struct{}
	item      youtubeImportItem
}

func (g *blockingYouTubeStartGateway) Scan(ctx context.Context, _ string, _ *time.Time, _ string) ([]youtubeImportItem, youtubeImportScanSource, error) {
	g.scanCalls <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, youtubeImportScanSource{}, ctx.Err()
	}
	return []youtubeImportItem{g.item}, youtubeImportScanSource{SourceType: youtubeImportSourceTrack, CanonicalURL: g.item.SourceURL}, nil
}

func (*blockingYouTubeStartGateway) DownloadAudio(context.Context, youtubeImportItem, string, string) error {
	return errors.New("unexpected download")
}

func TestYouTubeImportSerializesConcurrentStarts(t *testing.T) {
	item := youtubeImportItem{
		VideoID:           "start-video",
		SourceURL:         "https://www.youtube.com/watch?v=start-video",
		OriginalSourceURL: "https://www.youtube.com/watch?v=start-video",
		LinkProvider:      "youtube",
		ParsedTitle:       "Start Track",
		ParsedAuthorNames: []string{"Artist"},
	}
	gateway := &blockingYouTubeStartGateway{
		scanCalls: make(chan struct{}, 2),
		release:   make(chan struct{}),
		item:      item,
	}
	service, _ := newYouTubeSentryTestService(t, gateway)
	firstResult := make(chan error, 1)
	secondResult := make(chan error, 1)
	go func() {
		_, err := service.StartSession(context.Background(), 1, item.SourceURL, nil, false, "")
		firstResult <- err
	}()
	<-gateway.scanCalls
	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		_, err := service.StartSession(context.Background(), 1, item.SourceURL, nil, false, "")
		secondResult <- err
	}()
	<-secondStarted
	waitForYouTubeOperationRefs(t, service, 1, 2)
	select {
	case <-gateway.scanCalls:
		t.Fatal("concurrent StartSession() calls both scanned before the first committed")
	default:
	}

	close(gateway.release)
	if err := receiveYouTubeOperationResult(t, firstResult); err != nil {
		t.Fatalf("first StartSession() error = %v", err)
	}
	if err := receiveYouTubeOperationResult(t, secondResult); !errors.Is(err, errYouTubeSessionActive) {
		t.Fatalf("second StartSession() error = %v, want %v", err, errYouTubeSessionActive)
	}
	select {
	case <-gateway.scanCalls:
		t.Fatal("conflicting StartSession() unexpectedly scanned after the first committed")
	default:
	}
	waitForYouTubeOperationRefs(t, service, 1, 0)
}

func receiveYouTubeOperationResult(t *testing.T, results <-chan error) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for youtube operation")
		return nil
	}
}

func waitForYouTubeOperationRefs(t *testing.T, service *youtubeImportService, userID int64, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		service.operationLocksMu.Lock()
		operationLock := service.operationLocks[userID]
		got := 0
		if operationLock != nil {
			got = operationLock.refs
		}
		service.operationLocksMu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("youtube operation lock refs = %d, want %d", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStartYouTubeImportEmptyURLIsExpectedBadRequest(t *testing.T) {
	service := &youtubeImportService{}
	_, err := service.StartSession(context.Background(), 1, "  ", nil, false, "")
	if !errors.Is(err, errYouTubeInvalidURL) {
		t.Fatalf("StartSession() error = %v, want %v", err, errYouTubeInvalidURL)
	}
}

func TestWriteYouTubeImportErrorCapturesUnexpectedFailureOnce(t *testing.T) {
	transport := &sentry.MockTransport{}
	client, err := sentry.NewClient(sentry.ClientOptions{Transport: transport})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	hub := sentry.NewHub(client, sentry.NewScope())
	req := httptest.NewRequest(http.MethodPost, "/api/youtube/import-sessions/current/add", nil)
	req = req.WithContext(sentry.SetHubOnContext(req.Context(), hub))
	req = withSentryRequestCaptureState(req)
	rec := httptest.NewRecorder()
	root := errors.New("database unavailable")

	writeYouTubeImportError(rec, req, root, "import.add")
	captureUnhandledHTTPStatus(req, rec.Code)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if got, want := rec.Body.String(), "internal server error\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	events := transport.Events()
	if len(events) != 1 {
		t.Fatalf("captured events = %d, want 1", len(events))
	}
	if events[0].Tags["component"] != "youtube" || events[0].Tags["operation"] != "import.add" {
		t.Fatalf("event tags = %#v", events[0].Tags)
	}
	if events[0].Tags["http.status_code"] != "500" {
		t.Fatalf("event HTTP status tag = %q, want 500", events[0].Tags["http.status_code"])
	}
}

func TestWriteYouTubeImportErrorDoesNotCaptureExpectedBadRequest(t *testing.T) {
	transport := &sentry.MockTransport{}
	client, err := sentry.NewClient(sentry.ClientOptions{Transport: transport})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	hub := sentry.NewHub(client, sentry.NewScope())
	req := httptest.NewRequest(http.MethodPost, "/api/youtube/import-sessions", nil)
	req = req.WithContext(sentry.SetHubOnContext(req.Context(), hub))
	req = withSentryRequestCaptureState(req)
	rec := httptest.NewRecorder()

	writeYouTubeImportError(rec, req, errYouTubeInvalidURL, "import.start")
	captureUnhandledHTTPStatus(req, rec.Code)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if events := transport.Events(); len(events) != 0 {
		t.Fatalf("captured events = %d, want 0", len(events))
	}
}

func TestWriteYouTubeImportErrorCapturesCleanupFailureJoinedWithCancellation(t *testing.T) {
	transport := &sentry.MockTransport{}
	client, err := sentry.NewClient(sentry.ClientOptions{Transport: transport})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	hub := sentry.NewHub(client, sentry.NewScope())
	req := httptest.NewRequest(http.MethodPost, "/api/youtube/import-sessions/current/add", nil)
	req = req.WithContext(sentry.SetHubOnContext(req.Context(), hub))
	req = withSentryRequestCaptureState(req)
	rec := httptest.NewRecorder()

	writeYouTubeImportError(rec, req, errors.Join(
		context.Canceled,
		newYouTubeCleanupError("remove staged audio", errors.New("cleanup failed")),
	), "import.add")

	if rec.Code != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestTimeout)
	}
	if events := transport.Events(); len(events) != 1 {
		t.Fatalf("captured events = %d, want cleanup failure captured once", len(events))
	}
}

func TestWriteYouTubeImportErrorSuppressesCancellationOnly(t *testing.T) {
	transport := &sentry.MockTransport{}
	client, err := sentry.NewClient(sentry.ClientOptions{Transport: transport})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	hub := sentry.NewHub(client, sentry.NewScope())
	req := httptest.NewRequest(http.MethodPost, "/api/youtube/import-sessions/current/add", nil)
	req = req.WithContext(sentry.SetHubOnContext(req.Context(), hub))
	req = withSentryRequestCaptureState(req)
	rec := httptest.NewRecorder()

	writeYouTubeImportError(rec, req, context.Canceled, "import.add")

	if rec.Code != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestTimeout)
	}
	if events := transport.Events(); len(events) != 0 {
		t.Fatalf("captured events = %d, want 0", len(events))
	}
}

func TestWriteYouTubeImportErrorMapsUpstreamFailures(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantBody   string
	}{
		{
			name:       "rate limited",
			err:        classifyYouTubeScanError(youtube.ErrUnexpectedStatusCode(http.StatusTooManyRequests)),
			wantStatus: http.StatusBadGateway,
			wantBody:   "youtube is temporarily unavailable\n",
		},
		{
			name:       "timeout",
			err:        classifyYouTubeScanError(context.DeadlineExceeded),
			wantStatus: http.StatusGatewayTimeout,
			wantBody:   "youtube request timed out\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := &sentry.MockTransport{}
			client, err := sentry.NewClient(sentry.ClientOptions{Transport: transport})
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			hub := sentry.NewHub(client, sentry.NewScope())
			req := httptest.NewRequest(http.MethodPost, "/api/youtube/import-sessions", nil)
			req = req.WithContext(sentry.SetHubOnContext(req.Context(), hub))
			req = withSentryRequestCaptureState(req)
			rec := httptest.NewRecorder()

			writeYouTubeImportError(rec, req, test.err, "import.start")
			captureUnhandledHTTPStatus(req, rec.Code)

			if rec.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, test.wantStatus)
			}
			if rec.Body.String() != test.wantBody {
				t.Fatalf("body = %q, want %q", rec.Body.String(), test.wantBody)
			}
			if events := transport.Events(); len(events) != 1 {
				t.Fatalf("captured events = %d, want 1", len(events))
			}
		})
	}
}

func TestSanitizeYouTubeFilesystemErrorRemovesPath(t *testing.T) {
	const secretPath = "/private/cookies/user-secret.txt"
	err := sanitizeYouTubeFilesystemError("remove session cookies file", &os.PathError{
		Op:   "remove",
		Path: secretPath,
		Err:  os.ErrPermission,
	})
	if strings.Contains(err.Error(), secretPath) {
		t.Fatalf("sanitized error contains path: %v", err)
	}
	if !errors.Is(err, errYouTubeStorageFailed) || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("sanitized error = %v, want storage and permission causes", err)
	}
}

func seedYouTubeSentryDependencies(t *testing.T, store *trackStore) (author, album) {
	t.Helper()
	artist, err := store.createAuthor(upsertAuthorRequest{CurrentName: "Artist"})
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

func newYouTubeSentryTestService(t *testing.T, gateway youtubeImportGateway) (*youtubeImportService, *trackStore) {
	t.Helper()
	root := t.TempDir()
	songsDir := filepath.Join(root, "songs")
	importDir := filepath.Join(root, "youtube_import_tmp")
	for _, dir := range []string{songsDir, importDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", dir, err)
		}
	}
	store, err := newTrackStore(filepath.Join(root, "tracks.db"))
	if err != nil {
		t.Fatalf("newTrackStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.db.Close() })
	return newYouTubeImportService(youtubeImportConfig{
		ImportTempDir:   importDir,
		RequestTimeout:  time.Second,
		DownloadTimeout: time.Second,
	}, gateway, store, songsDir), store
}
