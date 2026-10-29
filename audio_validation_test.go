package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validMP3TestAudio(t *testing.T) []byte {
	t.Helper()
	if _, err := exec.LookPath(audioFFmpegBinary()); err != nil {
		t.Skip("FFmpeg required for audio validation")
	}
	data, err := os.ReadFile("testdata/mcp-silence.mp3")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestAudioUploadsValidateBeforeStorage(t *testing.T) {
	valid := validMP3TestAudio(t)
	// Drop the fixture's ID3v2 tag, matching extensionless Telegram MP3s.
	withoutTag := valid[10+int(valid[9]):]
	for _, staged := range []bool{false, true} {
		for _, test := range []struct {
			name   string
			data   []byte
			status int
		}{
			{"valid extensionless MP3", withoutTag, http.StatusCreated},
			{"text disguised as MP3", []byte("not audio"), http.StatusBadRequest},
			{"empty", nil, http.StatusBadRequest},
			{"ID3 without audio", valid[:44], http.StatusBadRequest},
			{"fake MPEG frames", append([]byte{0xff, 0xfb, 0x90, 0x64}, make([]byte, 2000)...), http.StatusBadRequest},
		} {
			t.Run(test.name+map[bool]string{false: "/public", true: "/submission"}[staged], func(t *testing.T) {
				store := newTestTrackStore(t)
				t.Cleanup(func() { _ = store.db.Close() })
				store.songsDir = t.TempDir()
				directory := store.songsDir
				var handler http.Handler = uploadSongHandler(directory)
				request := newMultipartUploadRequest(t, http.MethodPost, "/api/songs", "recording", test.data)
				if staged {
					directory = store.catalogSubmissionAudioDir()
					user := mustCreateCatalogTestUser(t, store, "audio-uploader@example.com")
					auth := newAuthManager([]byte("audio-test-secret"), time.Hour, time.Hour)
					token, _, err := auth.createAccessToken(user.ID)
					if err != nil {
						t.Fatal(err)
					}
					request.Header.Set("Authorization", "Bearer "+token)
					handler = requireAuth(auth, store, uploadCatalogSubmissionAudioHandler(store))
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != test.status {
					t.Fatalf("status=%d, want=%d, body=%s", response.Code, test.status, response.Body.String())
				}
				entries, err := os.ReadDir(directory)
				if err != nil {
					t.Fatal(err)
				}
				if test.status != http.StatusCreated {
					if len(entries) != 0 {
						t.Fatalf("invalid upload left files: %v", entries)
					}
					var count int
					if err := store.db.QueryRow("SELECT count(*) FROM catalog_submission_uploads").Scan(&count); err != nil {
						t.Fatal(err)
					}
					if count != 0 {
						t.Fatalf("invalid upload registered %d tokens", count)
					}
					return
				}
				if len(entries) != 1 || filepath.Ext(entries[0].Name()) != ".mp3" {
					t.Fatalf("stored MP3 filename missing: %v", entries)
				}
				path := filepath.Join(directory, entries[0].Name())
				stored, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(stored, test.data) {
					t.Fatalf("valid MP3 changed: %v", err)
				}
				playback := httptest.NewRecorder()
				playbackRequest := httptest.NewRequest(http.MethodGet, "/audio", nil)
				playbackRequest.Header.Set("Range", "bytes=0-1")
				serveCatalogSubmissionAudioFile(playback, playbackRequest, path)
				if playback.Code != http.StatusPartialContent || playback.Header().Get("Content-Type") != "audio/mpeg" || playback.Body.Len() != 2 {
					t.Fatalf("playback response: %d %v", playback.Code, playback.Header())
				}
				if staged {
					var upload catalogSubmissionUpload
					if err := json.Unmarshal(response.Body.Bytes(), &upload); err != nil {
						t.Fatal(err)
					}
					if upload.OriginalName != "recording" {
						t.Fatalf("original name lost: %q", upload.OriginalName)
					}
					published, _, _, err := store.publishStagedAudioToLibrary(catalogSubmissionUpload{StoredFileName: entries[0].Name(), OriginalName: upload.OriginalName})
					if err != nil || filepath.Ext(published) != ".mp3" {
						t.Fatalf("published name=%q error=%v", published, err)
					}
				}
			})
		}
	}
}

func TestAudioValidatorUnavailableDoesNotRejectAsBadAudio(t *testing.T) {
	audio := validMP3TestAudio(t)
	t.Setenv("FFMPEG_BINARY", filepath.Join(t.TempDir(), "missing-ffmpeg"))
	for _, staged := range []bool{false, true} {
		directory := t.TempDir()
		prefix := "/api/songs/"
		if staged {
			prefix = "/api/catalog-submissions/audio/"
		}
		request := newMultipartUploadRequest(t, http.MethodPost, "/upload", "track.mp3", audio)
		response := httptest.NewRecorder()
		_, err := uploadMediaFile(response, request, directory, "failed to create song file", prefix)
		if !errors.Is(err, errAudioValidatorUnavailable) {
			t.Fatalf("error=%v", err)
		}
		writeUploadError(response, request, err, "test.audio")
		if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "missing-ffmpeg") {
			t.Fatalf("unavailable validator response: %d %s", response.Code, response.Body.String())
		}
		entries, _ := os.ReadDir(directory)
		if len(entries) != 0 {
			t.Fatal("unvalidated audio was retained")
		}
	}
	s, _ := newMCPTestService(t)
	response := mcpUploadRequest(t, s, "audio", "missing-validator", "track.mp3", audio)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "media_validator_unavailable") {
		t.Fatalf("MCP response: %d %s", response.Code, response.Body.String())
	}
}

func TestNormalizeImportedMP3PreservesValidAudioAndRejectsGarbage(t *testing.T) {
	audio := validMP3TestAudio(t)
	for _, data := range [][]byte{audio, audio[44:]} {
		path := filepath.Join(t.TempDir(), "1751922162040")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := normalizeImportedMP3(context.Background(), path); err != nil {
			t.Fatal(err)
		}
		result, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(result, data) {
			t.Fatalf("valid MP3 was re-encoded: %v", err)
		}
	}
	path := filepath.Join(t.TempDir(), "fake.mp3")
	if err := os.WriteFile(path, []byte("bad audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	var invalid invalidAudioError
	if err := normalizeImportedMP3(context.Background(), path); !errors.As(err, &invalid) {
		t.Fatalf("error=%v", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("failed conversion left temporary output: %v", entries)
	}
}

func TestNormalizeImportedAudioConvertsWaveToMP3(t *testing.T) {
	audio := validMP3TestAudio(t)
	directory := t.TempDir()
	source := filepath.Join(directory, "source.mp3")
	wave := filepath.Join(directory, "telegram-download")
	if err := os.WriteFile(source, audio, 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(audioFFmpegBinary(), "-nostdin", "-v", "error", "-i", source, "-f", "wav", wave).CombinedOutput(); err != nil {
		t.Fatalf("generate WAVE: %v %s", err, output)
	}
	if err := normalizeImportedMP3(context.Background(), wave); err != nil {
		t.Fatal(err)
	}
	if err := validateMP3Audio(context.Background(), wave); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(directory)
	if len(entries) != 2 {
		t.Fatalf("conversion left temporary output: %v", entries)
	}
}

func TestTelegramImportRepairsAudioBeforePreviewAndPublication(t *testing.T) {
	audio := validMP3TestAudio(t)[44:]
	for _, format := range []string{"mp3", "wav"} {
		t.Run(format, func(t *testing.T) {
			input := audio
			if format == "wav" {
				directory := t.TempDir()
				path := filepath.Join(directory, "source.mp3")
				wave := filepath.Join(directory, "source.wav")
				if err := os.WriteFile(path, audio, 0o600); err != nil {
					t.Fatal(err)
				}
				var err error
				if output, err := exec.Command(audioFFmpegBinary(), "-nostdin", "-v", "error", "-i", path, wave).CombinedOutput(); err != nil {
					t.Fatalf("generate WAVE: %v %s", err, output)
				}
				input, err = os.ReadFile(wave)
				if err != nil {
					t.Fatal(err)
				}
			}
			service, store, songsDirectory, _ := newTelegramImportTestService(t, &fakeTelegramGateway{
				status:       telegramAuthStatus{Configured: true, Authorized: true},
				scannedItems: []telegramScannedTrack{{MessageID: 739, FileName: "1751922162040", MimeType: "application/octet-stream"}},
				downloadData: input,
			})
			t.Cleanup(func() { _ = service.Close(); _ = store.db.Close() })
			artist, albumItem := seedTrackDependencies(t, store)
			ctx := context.Background()
			if _, err := service.StartSession(ctx, 1, "test_channel", 0, false); err != nil {
				t.Fatal(err)
			}
			lease, err := service.OpenCurrentAudio(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			if lease.FileName != "1751922162040.mp3" {
				t.Fatalf("preview name=%q", lease.FileName)
			}
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			_, track, err := service.SaveCurrent(ctx, 1, telegramSaveTrackRequest{Name: "Recording", AuthorIDs: []int64{artist.ID}, AlbumID: albumItem.ID, TrackNumber: 1})
			if err != nil {
				t.Fatal(err)
			}
			if filepath.Ext(track.AudioFilePath) != ".mp3" {
				t.Fatalf("published path=%q", track.AudioFilePath)
			}
			storedPath := filepath.Join(songsDirectory, filepath.Base(track.AudioFilePath))
			if err := validateMP3Audio(ctx, storedPath); err != nil {
				t.Fatal(err)
			}
			if format == "mp3" {
				stored, err := os.ReadFile(storedPath)
				if err != nil || !bytes.Equal(stored, input) {
					t.Fatal("Telegram re-encoded a valid MP3")
				}
			}
		})
	}
}

func TestTelegramRejectsCorruptAudioWithoutRetainingDownloads(t *testing.T) {
	validMP3TestAudio(t)
	service, store, _, temporaryDirectory := newTelegramImportTestService(t, &fakeTelegramGateway{
		scannedItems: []telegramScannedTrack{{MessageID: 1, FileName: "fake.mp3", MimeType: "audio/mpeg"}},
		downloadData: []byte("not audio"),
	})
	t.Cleanup(func() { _ = service.Close(); _ = store.db.Close() })
	_, err := service.StartSession(context.Background(), 1, "test_channel", 0, false)
	var invalid invalidAudioError
	if !errors.As(err, &invalid) {
		t.Fatalf("error=%v", err)
	}
	if err := filepath.WalkDir(temporaryDirectory, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			t.Errorf("rejected import retained file %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	writeTelegramError(response, httptest.NewRequest(http.MethodPost, "/import", nil), err, "import.test")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("response=%d", response.Code)
	}
}

func TestYouTubeRejectsInvalidTranscoderOutput(t *testing.T) {
	service, store, songsDirectory, temporaryDirectory := newYouTubeImportTestService(t, &fakeYouTubeGateway{downloadData: []byte("source")})
	t.Cleanup(func() { _ = store.db.Close() })
	binary := filepath.Join(t.TempDir(), "bad-transcoder")
	script := "#!/bin/sh\nfor argument in \"$@\"; do target=$argument; done\nprintf 'not MP3' > \"$target\"\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	service.cfg.FFmpegBinary = binary
	_, err := service.downloadCurrentToSongs(context.Background(), 0, youtubeImportItem{VideoID: "test", ParsedTitle: "Test"}, "")
	var invalid invalidAudioError
	if !errors.As(err, &invalid) {
		t.Fatalf("error=%v", err)
	}
	for _, directory := range []string{songsDirectory, temporaryDirectory} {
		if err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				t.Errorf("invalid transcoder output retained file %s", path)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
