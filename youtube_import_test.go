package main

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	youtube "github.com/kkdai/youtube/v2"
)

type fakeYouTubeGateway struct {
	scanItems           []youtubeImportItem
	scanSource          youtubeImportScanSource
	downloadData        []byte
	scanCookiesPath     string
	downloadCookiesPath string
}

func (f *fakeYouTubeGateway) Scan(_ context.Context, _ string, cutoff *time.Time, cookiesPath string) ([]youtubeImportItem, youtubeImportScanSource, error) {
	f.scanCookiesPath = cookiesPath
	if len(f.scanItems) == 0 {
		return nil, youtubeImportScanSource{}, errYouTubeNoTracks
	}
	items := make([]youtubeImportItem, 0, len(f.scanItems))
	for _, item := range f.scanItems {
		if !passesYouTubeCutoff(item, cutoff) {
			continue
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		return nil, youtubeImportScanSource{}, errYouTubeNoTracks
	}
	return items, f.scanSource, nil
}

func (f *fakeYouTubeGateway) DownloadAudio(_ context.Context, _ youtubeImportItem, destinationPath string, cookiesPath string) error {
	f.downloadCookiesPath = cookiesPath
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(destinationPath, f.downloadData, 0o644)
}

func TestYouTubeImportSessionConflictAndSkip(t *testing.T) {
	releaseDate := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)
	service, _, songsDir, _ := newYouTubeImportTestService(t, &fakeYouTubeGateway{
		scanSource: youtubeImportScanSource{
			SourceType:   youtubeImportSourcePlaylist,
			CanonicalURL: "https://music.youtube.com/playlist?list=PL1",
		},
		scanItems: []youtubeImportItem{
			{
				VideoID:           "video-1",
				SourceURL:         "https://music.youtube.com/watch?v=video-1",
				OriginalSourceURL: "https://music.youtube.com/playlist?list=PL1",
				LinkProvider:      "youtube_music",
				ParsedTitle:       "First",
				ParsedAuthorNames: []string{"Artist"},
				ParsedReleaseDate: &releaseDate,
			},
			{
				VideoID:           "video-2",
				SourceURL:         "https://music.youtube.com/watch?v=video-2",
				OriginalSourceURL: "https://music.youtube.com/playlist?list=PL1",
				LinkProvider:      "youtube_music",
				ParsedTitle:       "Second",
				ParsedAuthorNames: []string{"Artist"},
				ParsedReleaseDate: &releaseDate,
			},
		},
		downloadData: []byte("audio"),
	})

	ctx := context.Background()
	if _, err := service.StartSession(ctx, 1, "https://music.youtube.com/playlist?list=PL1", nil, false, ""); err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	if _, err := service.StartSession(ctx, 1, "https://music.youtube.com/playlist?list=PL1", nil, false, ""); !errors.Is(err, errYouTubeSessionActive) {
		t.Fatalf("StartSession() conflict error = %v, want %v", err, errYouTubeSessionActive)
	}

	current, err := service.CurrentSession(1)
	if err != nil {
		t.Fatalf("CurrentSession() error = %v", err)
	}
	if current.CurrentItem == nil || current.CurrentItem.VideoID != "video-1" {
		t.Fatalf("CurrentSession() current item = %#v", current.CurrentItem)
	}

	current, err = service.SkipCurrent(1)
	if err != nil {
		t.Fatalf("SkipCurrent() error = %v", err)
	}
	if current.Progress.Skipped != 1 || current.Progress.Processed != 1 || current.Status != youtubeImportStatusActive {
		t.Fatalf("SkipCurrent() progress = %#v status=%s", current.Progress, current.Status)
	}

	current, err = service.SkipCurrent(1)
	if err != nil {
		t.Fatalf("SkipCurrent() second error = %v", err)
	}
	if current.Status != youtubeImportStatusCompleted || current.CurrentItem != nil {
		t.Fatalf("completed session = %#v", current)
	}

	if entries, err := os.ReadDir(songsDir); err != nil {
		t.Fatalf("ReadDir(%s) error = %v", songsDir, err)
	} else if len(entries) != 0 {
		t.Fatalf("songs unexpectedly created = %#v", entries)
	}
}

func TestYouTubeImportAddCurrentCreateDownloadsAndCreatesTrack(t *testing.T) {
	releaseDate := time.Date(2024, time.February, 1, 0, 0, 0, 0, time.UTC)
	service, store, songsDir, _ := newYouTubeImportTestService(t, &fakeYouTubeGateway{
		scanSource: youtubeImportScanSource{
			SourceType:   youtubeImportSourceTrack,
			CanonicalURL: "https://music.youtube.com/watch?v=abc123",
		},
		scanItems: []youtubeImportItem{
			{
				VideoID:           "abc123",
				SourceURL:         "https://music.youtube.com/watch?v=abc123",
				OriginalSourceURL: "https://music.youtube.com/watch?v=abc123",
				LinkProvider:      "youtube_music",
				ParsedTitle:       "Imported Track",
				ParsedAuthorNames: []string{"Artist"},
				ParsedAlbumTitle:  "Release",
				ParsedReleaseDate: &releaseDate,
			},
		},
		downloadData: []byte("audio-data"),
	})

	artist, album := seedTrackDependencies(t, store)
	ctx := context.Background()
	if _, err := service.StartSession(ctx, 1, "https://music.youtube.com/watch?v=abc123", nil, false, ""); err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	dto, createdTrack, err := service.AddCurrent(ctx, 1, youtubeAddCurrentRequest{
		Mode:       youtubeAddModeCreate,
		Name:       "Imported Track",
		AuthorIDs:  []int64{artist.ID},
		AlbumID:    album.ID,
		AlbumOrder: 0,
	})
	if err != nil {
		t.Fatalf("AddCurrent() error = %v", err)
	}
	if dto.Status != youtubeImportStatusCompleted || dto.Progress.Saved != 1 {
		t.Fatalf("AddCurrent() dto = %#v", dto)
	}
	if !strings.HasPrefix(createdTrack.AudioFilePath, "/api/songs/") {
		t.Fatalf("created track audio path = %q", createdTrack.AudioFilePath)
	}
	if !strings.HasSuffix(createdTrack.AudioFilePath, ".mp3") {
		t.Fatalf("created track audio path = %q, want .mp3 suffix", createdTrack.AudioFilePath)
	}
	if len(createdTrack.AdditionalInfo) != 1 || createdTrack.AdditionalInfo[0]["provider"] != "youtube_music" {
		t.Fatalf("created track additionalInfo = %#v", createdTrack.AdditionalInfo)
	}
	if len(createdTrack.SourceMetadata) != 1 {
		t.Fatalf("created track source metadata = %#v", createdTrack.SourceMetadata)
	}
	identity, ok := createdTrack.SourceMetadata[0]["identity"].(map[string]any)
	if !ok || identity["videoId"] != "abc123" {
		t.Fatalf("created track source metadata identity = %#v", createdTrack.SourceMetadata)
	}

	files, err := os.ReadDir(songsDir)
	if err != nil {
		t.Fatalf("ReadDir(%s) error = %v", songsDir, err)
	}
	regularFiles := 0
	for _, file := range files {
		if !file.IsDir() {
			regularFiles++
		}
	}
	if regularFiles != 1 {
		t.Fatalf("regular songs files = %d entries=%#v, want 1", regularFiles, files)
	}
}

func TestYouTubeImportSessionUsesRequestCookiesAndCleansThemUp(t *testing.T) {
	gateway := &fakeYouTubeGateway{
		scanSource: youtubeImportScanSource{
			SourceType:   youtubeImportSourceTrack,
			CanonicalURL: "https://music.youtube.com/watch?v=with-cookies",
		},
		scanItems: []youtubeImportItem{
			{
				VideoID:           "with-cookies",
				SourceURL:         "https://music.youtube.com/watch?v=with-cookies",
				OriginalSourceURL: "https://music.youtube.com/watch?v=with-cookies",
				LinkProvider:      "youtube_music",
				ParsedTitle:       "Cookie Track",
				ParsedAuthorNames: []string{"Artist"},
			},
		},
		downloadData: []byte("audio-data"),
	}
	service, store, _, _ := newYouTubeImportTestService(t, gateway)
	artist, album := seedTrackDependencies(t, store)

	ctx := context.Background()
	if _, err := service.StartSession(ctx, 1, "https://music.youtube.com/watch?v=with-cookies", nil, false, "cookie-data"); err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	if gateway.scanCookiesPath == "" {
		t.Fatal("Scan() did not receive session cookies path")
	}
	if data, err := os.ReadFile(gateway.scanCookiesPath); err != nil {
		t.Fatalf("ReadFile(%s) error = %v", gateway.scanCookiesPath, err)
	} else if !strings.Contains(string(data), "cookie-data") {
		t.Fatalf("cookies file = %q, want cookie-data", string(data))
	}

	_, _, err := service.AddCurrent(ctx, 1, youtubeAddCurrentRequest{
		Mode:       youtubeAddModeCreate,
		Name:       "Cookie Track",
		AuthorIDs:  []int64{artist.ID},
		AlbumID:    album.ID,
		AlbumOrder: 0,
	})
	if err != nil {
		t.Fatalf("AddCurrent() error = %v", err)
	}
	if gateway.downloadCookiesPath != gateway.scanCookiesPath {
		t.Fatalf("DownloadAudio() cookies path = %q, want %q", gateway.downloadCookiesPath, gateway.scanCookiesPath)
	}
	if _, err := os.Stat(gateway.scanCookiesPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session cookies still exist or unexpected stat error: %v", err)
	}
}

func TestYouTubeImportAddCurrentAttachMergesMetadata(t *testing.T) {
	releaseDate := time.Date(2024, time.March, 1, 0, 0, 0, 0, time.UTC)
	service, store, _, _ := newYouTubeImportTestService(t, &fakeYouTubeGateway{
		scanSource: youtubeImportScanSource{
			SourceType:   youtubeImportSourceTrack,
			CanonicalURL: "https://www.youtube.com/watch?v=video-attach",
		},
		scanItems: []youtubeImportItem{
			{
				VideoID:           "video-attach",
				SourceURL:         "https://www.youtube.com/watch?v=video-attach",
				OriginalSourceURL: "https://www.youtube.com/watch?v=video-attach",
				LinkProvider:      "youtube",
				ParsedTitle:       "Track",
				ParsedAuthorNames: []string{"Artist"},
				ParsedReleaseDate: &releaseDate,
			},
		},
		downloadData: []byte("audio-data"),
	})

	artist, album := seedTrackDependencies(t, store)
	existingTrack, err := store.create(upsertTrackRequest{
		Name:          "Track",
		AuthorIDs:     []int64{artist.ID},
		AlbumID:       album.ID,
		AlbumOrder:    0,
		AudioFilePath: "/api/songs/existing-track.mp3",
	})
	if err != nil {
		t.Fatalf("create() error = %v", err)
	}

	ctx := context.Background()
	if _, err := service.StartSession(ctx, 1, "https://www.youtube.com/watch?v=video-attach", nil, false, ""); err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	dto, updatedTrack, err := service.AddCurrent(ctx, 1, youtubeAddCurrentRequest{
		Mode:    youtubeAddModeAttach,
		TrackID: existingTrack.ID,
	})
	if err != nil {
		t.Fatalf("AddCurrent() attach error = %v", err)
	}
	if dto.Status != youtubeImportStatusCompleted || dto.Progress.Saved != 1 {
		t.Fatalf("AddCurrent() attach dto = %#v", dto)
	}
	if len(updatedTrack.AdditionalInfo) != 1 || updatedTrack.AdditionalInfo[0]["provider"] != "youtube" {
		t.Fatalf("updated track additionalInfo = %#v", updatedTrack.AdditionalInfo)
	}
	if len(updatedTrack.SourceMetadata) != 1 {
		t.Fatalf("updated track source metadata = %#v", updatedTrack.SourceMetadata)
	}
}

func TestYouTubeImportSuggestionsIncludeExactSourceMatch(t *testing.T) {
	releaseDate := time.Date(2024, time.April, 1, 0, 0, 0, 0, time.UTC)
	service, store, _, _ := newYouTubeImportTestService(t, &fakeYouTubeGateway{
		scanSource: youtubeImportScanSource{
			SourceType:   youtubeImportSourceTrack,
			CanonicalURL: "https://music.youtube.com/watch?v=match-me",
		},
		scanItems: []youtubeImportItem{
			{
				VideoID:           "match-me",
				SourceURL:         "https://music.youtube.com/watch?v=match-me",
				OriginalSourceURL: "https://music.youtube.com/watch?v=match-me",
				LinkProvider:      "youtube_music",
				ParsedTitle:       "Matched Track",
				ParsedAuthorNames: []string{"Artist"},
				ParsedReleaseDate: &releaseDate,
			},
		},
		downloadData: []byte("audio-data"),
	})

	artist, album := seedTrackDependencies(t, store)
	if _, err := store.create(upsertTrackRequest{
		Name:          "Matched Track",
		AuthorIDs:     []int64{artist.ID},
		AlbumID:       album.ID,
		AlbumOrder:    0,
		AudioFilePath: "/api/songs/matched-track.mp3",
		SourceMetadata: []sourceMetadata{
			{
				"provider": "youtube",
				"identity": map[string]any{
					"videoId": "match-me",
				},
				"url": "https://music.youtube.com/watch?v=match-me",
			},
		},
	}); err != nil {
		t.Fatalf("create() error = %v", err)
	}

	ctx := context.Background()
	if _, err := service.StartSession(ctx, 1, "https://music.youtube.com/watch?v=match-me", nil, false, ""); err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	current, err := service.CurrentSession(1)
	if err != nil {
		t.Fatalf("CurrentSession() error = %v", err)
	}
	if current.CurrentItem == nil || len(current.CurrentItem.Suggestions) == 0 {
		t.Fatalf("CurrentSession() suggestions = %#v", current.CurrentItem)
	}
	if current.CurrentItem.Suggestions[0].Type != youtubeSuggestionTypeExactSourceMatch {
		t.Fatalf("suggestion type = %q, want %q", current.CurrentItem.Suggestions[0].Type, youtubeSuggestionTypeExactSourceMatch)
	}
}

func TestMergePlaylistEntryFallbackFillsMissingVideoFields(t *testing.T) {
	entry := &youtube.PlaylistEntry{
		ID:       "video-123",
		Title:    "Playlist Track",
		Author:   "Playlist Artist",
		Duration: 215 * time.Second,
		Thumbnails: youtube.Thumbnails{
			{URL: "https://img.example/cover.jpg"},
		},
	}

	item := mergePlaylistEntryFallback(youtubeImportItem{
		OriginalSourceURL: "https://music.youtube.com/playlist?list=OLAK-test",
		LinkProvider:      "youtube_music",
	}, entry, "https://music.youtube.com/playlist?list=OLAK-test", "youtube_music", "Album")

	if item.VideoID != "video-123" {
		t.Fatalf("VideoID = %q, want video-123", item.VideoID)
	}
	if item.ParsedTitle != "Playlist Track" {
		t.Fatalf("ParsedTitle = %q, want Playlist Track", item.ParsedTitle)
	}
	if len(item.ParsedAuthorNames) != 1 || item.ParsedAuthorNames[0] != "Playlist Artist" {
		t.Fatalf("ParsedAuthorNames = %#v", item.ParsedAuthorNames)
	}
	if item.DurationSeconds != 215 {
		t.Fatalf("DurationSeconds = %d, want 215", item.DurationSeconds)
	}
}

func TestYouTubeCookieStoreReplaceStatusAndDelete(t *testing.T) {
	store := newYouTubeCookieStore(filepath.Join(t.TempDir(), "youtube-cookies.txt"))

	status := store.Status()
	if !status.Configured || status.FilePresent {
		t.Fatalf("initial status = %#v", status)
	}

	if err := store.Replace(strings.NewReader("cookie-data")); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	status = store.Status()
	if !status.FilePresent || status.LastModified == nil {
		t.Fatalf("status after replace = %#v", status)
	}

	if err := store.Delete(); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	status = store.Status()
	if status.FilePresent {
		t.Fatalf("status after delete = %#v", status)
	}
}

func TestYouTubeCookiesUploadHandlerStoresFile(t *testing.T) {
	store := newYouTubeCookieStore(filepath.Join(t.TempDir(), "youtube-cookies.txt"))

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	fileWriter, err := writer.CreateFormFile("file", "cookies.txt")
	if err != nil {
		t.Fatalf("CreateFormFile() error = %v", err)
	}
	if _, err := fileWriter.Write([]byte("cookie-data")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/youtube/cookies", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()

	youtubeCookiesUploadHandler(store).ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	status := store.Status()
	if !status.FilePresent {
		t.Fatalf("store status = %#v", status)
	}
}

func TestYouTubeCookiesUploadHandlerRejectsOversizedFile(t *testing.T) {
	root := t.TempDir()
	configuredPath := filepath.Join(root, "youtube-cookies.txt")
	store := newYouTubeCookieStore(configuredPath)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	fileWriter, err := writer.CreateFormFile("file", "cookies.txt")
	if err != nil {
		t.Fatalf("CreateFormFile() error = %v", err)
	}
	if _, err := fileWriter.Write(make([]byte, maxYouTubeCookiesBytes+1)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/youtube/cookies", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()

	youtubeCookiesUploadHandler(store).ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusRequestEntityTooLarge, rec.Body.String())
	}
	if _, err := os.Stat(configuredPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("configured cookies stat error = %v, want no oversized file", err)
	}
	if _, err := os.Stat(configuredPath + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary cookies stat error = %v, want cleanup", err)
	}
}

func TestLiveYouTubeImportGatewayDownloadAudioUsesYTDLPWhenCookiesPresent(t *testing.T) {
	root := t.TempDir()
	cookiesPath := filepath.Join(root, "youtube-cookies.txt")
	if err := os.WriteFile(cookiesPath, []byte("cookie-data"), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", cookiesPath, err)
	}

	binaryPath := filepath.Join(root, "fake-yt-dlp.sh")
	script := "#!/bin/sh\nprintf 'audio'\n"
	if err := os.WriteFile(binaryPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", binaryPath, err)
	}

	cookieStore := newYouTubeCookieStore(cookiesPath)
	gateway := newLiveYouTubeImportGateway(youtubeImportConfig{
		RequestTimeout:  time.Second,
		DownloadTimeout: time.Second,
		YTDLPBinary:     binaryPath,
	}, cookieStore)

	targetPath := filepath.Join(root, "downloaded-audio.bin")
	err := gateway.DownloadAudio(context.Background(), youtubeImportItem{
		VideoID:      "video-1",
		SourceURL:    "https://music.youtube.com/watch?v=video-1",
		LinkProvider: "youtube_music",
	}, targetPath, "")
	if err != nil {
		t.Fatalf("DownloadAudio() error = %v", err)
	}

	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", targetPath, err)
	}
	if string(data) != "audio" {
		t.Fatalf("downloaded data = %q, want audio", string(data))
	}
}

func TestCollectYTDLPArtistLeafEntriesSkipsShorts(t *testing.T) {
	entries := []ytdlpFlatPlaylistEntry{
		{
			Title: "DVRST - Videos",
			Entries: []ytdlpFlatPlaylistEntry{
				{
					Type:     "url",
					ID:       "video-1",
					Title:    "Game Over",
					URL:      "https://www.youtube.com/watch?v=video-1",
					Duration: 128,
				},
			},
		},
		{
			Title: "DVRST - Shorts",
			Entries: []ytdlpFlatPlaylistEntry{
				{
					Type:  "url",
					ID:    "short-1",
					Title: "Game Over",
					URL:   "https://www.youtube.com/shorts/short-1",
				},
			},
		},
	}

	flat := collectYTDLPArtistLeafEntries(entries)
	if len(flat) != 1 {
		t.Fatalf("collectYTDLPArtistLeafEntries() count = %d, want 1", len(flat))
	}
	if flat[0].ID != "video-1" {
		t.Fatalf("collectYTDLPArtistLeafEntries() first id = %q, want video-1", flat[0].ID)
	}
}

func TestLiveYouTubeImportGatewayScanArtistUsesYTDLPDump(t *testing.T) {
	root := t.TempDir()
	binaryPath := filepath.Join(root, "fake-yt-dlp.sh")
	dumpJSON := `{"title":"DVRST","channel":"DVRST","entries":[{"title":"DVRST - Videos","entries":[{"_type":"url","id":"video-1","url":"https://www.youtube.com/watch?v=video-1","title":"Game Over","duration":128,"thumbnails":[{"url":"https://img.example/game-over.jpg"}]}]},{"title":"DVRST - Shorts","entries":[{"_type":"url","id":"short-1","url":"https://www.youtube.com/shorts/short-1","title":"Short clip","duration":15}]}]}`
	script := "#!/bin/sh\necho 'WARNING: redirected' \ncat <<'EOF'\n" + dumpJSON + "\nEOF\n"
	if err := os.WriteFile(binaryPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", binaryPath, err)
	}

	gateway := newLiveYouTubeImportGateway(youtubeImportConfig{
		RequestTimeout: time.Second,
		YTDLPBinary:    binaryPath,
	}, newYouTubeCookieStore(""))

	items, err := gateway.scanArtist(context.Background(), gateway.newClient(), "https://music.youtube.com/channel/example", nil, "")
	if err != nil {
		t.Fatalf("scanArtist() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("scanArtist() item count = %d, want 1", len(items))
	}
	if items[0].VideoID != "video-1" {
		t.Fatalf("scanArtist() video id = %q, want video-1", items[0].VideoID)
	}
	if items[0].SourceURL != "https://music.youtube.com/watch?v=video-1" {
		t.Fatalf("scanArtist() source url = %q", items[0].SourceURL)
	}
	if len(items[0].ParsedAuthorNames) != 1 || items[0].ParsedAuthorNames[0] != "DVRST" {
		t.Fatalf("scanArtist() author names = %#v", items[0].ParsedAuthorNames)
	}
	if items[0].CoverImageURL != "https://img.example/game-over.jpg" {
		t.Fatalf("scanArtist() cover image = %q", items[0].CoverImageURL)
	}
}

func TestLiveYouTubeImportGatewayScanTrackUsesYTDLPCookies(t *testing.T) {
	root := t.TempDir()
	cookiesPath := filepath.Join(root, "youtube-cookies.txt")
	if err := os.WriteFile(cookiesPath, []byte("cookie-data"), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", cookiesPath, err)
	}

	argsPath := filepath.Join(root, "args.txt")
	binaryPath := filepath.Join(root, "fake-yt-dlp.sh")
	dumpJSON := `{"id":"4c94WwxWm78","title":"Max Korzh - Official audio","channel":"Max Korzh","duration":228,"timestamp":1714079251,"thumbnails":[{"url":"https://img.example/max-korzh.jpg"}]}`
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsPath + "'\necho 'WARNING: redirected'\ncat <<'EOF'\n" + dumpJSON + "\nEOF\n"
	if err := os.WriteFile(binaryPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", binaryPath, err)
	}

	gateway := newLiveYouTubeImportGateway(youtubeImportConfig{
		RequestTimeout: time.Second,
		YTDLPBinary:    binaryPath,
	}, newYouTubeCookieStore(cookiesPath))

	item, err := gateway.scanTrackWithYTDLP(
		context.Background(),
		"https://www.youtube.com/watch?v=4c94WwxWm78",
		"https://www.youtube.com/watch?v=4c94WwxWm78",
		"",
	)
	if err != nil {
		t.Fatalf("scanTrackWithYTDLP() error = %v", err)
	}
	if item.VideoID != "4c94WwxWm78" {
		t.Fatalf("scanTrackWithYTDLP() video id = %q", item.VideoID)
	}
	if item.SourceURL != "https://www.youtube.com/watch?v=4c94WwxWm78" {
		t.Fatalf("scanTrackWithYTDLP() source url = %q", item.SourceURL)
	}
	if item.ParsedTitle != "Max Korzh - Official audio" {
		t.Fatalf("scanTrackWithYTDLP() title = %q", item.ParsedTitle)
	}
	if len(item.ParsedAuthorNames) != 1 || item.ParsedAuthorNames[0] != "Max Korzh" {
		t.Fatalf("scanTrackWithYTDLP() author names = %#v", item.ParsedAuthorNames)
	}
	if item.DurationSeconds != 228 {
		t.Fatalf("scanTrackWithYTDLP() duration = %d", item.DurationSeconds)
	}
	if item.CoverImageURL != "https://img.example/max-korzh.jpg" {
		t.Fatalf("scanTrackWithYTDLP() cover image = %q", item.CoverImageURL)
	}
	if item.ParsedReleaseDate == nil || item.ParsedReleaseDate.Format("2006-01-02") != "2024-04-25" {
		t.Fatalf("scanTrackWithYTDLP() release date = %#v", item.ParsedReleaseDate)
	}

	argsData, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", argsPath, err)
	}
	args := strings.Split(strings.TrimSpace(string(argsData)), "\n")
	if !containsString(args, "--cookies") || !containsString(args, cookiesPath) {
		t.Fatalf("yt-dlp args = %#v, want cookies path %q", args, cookiesPath)
	}
	if !containsString(args, "--no-playlist") || !containsString(args, "--dump-single-json") {
		t.Fatalf("yt-dlp args = %#v, want single video dump flags", args)
	}
}

func TestTrimJSONOutputStripsWarningPrelude(t *testing.T) {
	output := []byte("WARNING: redirected\n{\"title\":\"DVRST\"}\n")
	trimmed := trimJSONOutput(output)
	if string(trimmed) != "{\"title\":\"DVRST\"}" {
		t.Fatalf("trimJSONOutput() = %q", string(trimmed))
	}
}

func newYouTubeImportTestService(t *testing.T, gateway youtubeImportGateway) (*youtubeImportService, *trackStore, string, string) {
	t.Helper()

	root := t.TempDir()
	songsDir := filepath.Join(root, "songs")
	tempRoot := filepath.Join(root, "youtube_import_tmp")
	dbPath := filepath.Join(root, "tracks_db.json")

	for _, dir := range []string{songsDir, tempRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", dir, err)
		}
	}

	store, err := newTrackStore(dbPath)
	if err != nil {
		t.Fatalf("newTrackStore() error = %v", err)
	}

	ffmpegPath := filepath.Join(root, "fake-ffmpeg.sh")
	ffmpegScript := "#!/bin/sh\nIN=\nOUT=\nPREV=\nfor ARG in \"$@\"; do\n  if [ \"$PREV\" = \"-i\" ]; then IN=\"$ARG\"; fi\n  OUT=\"$ARG\"\n  PREV=\"$ARG\"\ndone\ncp \"$IN\" \"$OUT\"\n"
	if err := os.WriteFile(ffmpegPath, []byte(ffmpegScript), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", ffmpegPath, err)
	}

	service := newYouTubeImportService(youtubeImportConfig{
		ImportTempDir:   tempRoot,
		RequestTimeout:  time.Second,
		DownloadTimeout: time.Second,
		FFmpegBinary:    ffmpegPath,
	}, gateway, store, songsDir)

	return service, store, songsDir, tempRoot
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
