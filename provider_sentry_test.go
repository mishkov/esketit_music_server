package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getsentry/sentry-go"
)

func TestAlbumCoverSuggestionsSentryClassification(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		unavailable   bool
		wantStatus    int
		wantEvents    int
		wantOperation string
	}{
		{
			name:          "provider failure is captured once",
			err:           errors.New("spotify transport failed"),
			wantStatus:    http.StatusBadGateway,
			wantEvents:    1,
			wantOperation: "search_album_covers",
		},
		{
			name:          "provider timeout is captured once",
			err:           fmt.Errorf("spotify timeout: %w", context.DeadlineExceeded),
			wantStatus:    http.StatusGatewayTimeout,
			wantEvents:    1,
			wantOperation: "search_album_covers",
		},
		{
			name:        "missing configuration is expected",
			unavailable: true,
			wantStatus:  http.StatusNotImplemented,
		},
		{
			name:       "client cancellation is expected",
			err:        context.Canceled,
			wantStatus: http.StatusRequestTimeout,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &albumCoverService{}
			if test.unavailable {
				service.searchUnavailableMessage = "album cover suggestions are not configured"
			} else {
				service.searchProvider = albumCoverSearchProviderFunc(func(context.Context, string, int) ([]albumCoverSuggestion, error) {
					return nil, test.err
				})
			}

			rec, transport := serveProviderSentryRequest(
				t,
				albumCoverSuggestionsHandler(service),
				http.MethodGet,
				"/api/album-covers/suggestions?query=cover",
				nil,
			)

			if rec.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, test.wantStatus, rec.Body.String())
			}
			events := transport.Events()
			if len(events) != test.wantEvents {
				t.Fatalf("captured events = %d, want %d", len(events), test.wantEvents)
			}
			if test.wantEvents == 1 {
				assertProviderSentryEvent(t, events[0], "spotify", test.wantOperation, test.wantStatus)
				if strings.Contains(rec.Body.String(), test.err.Error()) {
					t.Fatalf("response exposed provider error: %q", rec.Body.String())
				}
			}
		})
	}
}

func TestAlbumCoverImportSentryClassification(t *testing.T) {
	t.Run("storage failure is captured once", func(t *testing.T) {
		blockedDirectory := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(blockedDirectory, []byte("file"), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		service := &albumCoverService{
			albumCoversDir: blockedDirectory,
			remoteFetcher: remoteImageFetcherFunc(func(_ context.Context, imageURL string) (remoteImageFetchResult, error) {
				return remoteImageFetchResult{
					ContentType: "image/png",
					Data:        mustDecodeBase64(t, tinyPNGBase64),
					FinalURL:    imageURL,
				}, nil
			}),
		}

		rec, transport := serveProviderSentryRequest(
			t,
			importAlbumCoverHandler(service),
			http.MethodPost,
			"/api/album-covers/import",
			strings.NewReader(`{"imageUrl":"https://images.example/cover.png"}`),
		)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusInternalServerError, rec.Body.String())
		}
		if got, want := strings.TrimSpace(rec.Body.String()), "failed to store album cover"; got != want {
			t.Fatalf("body = %q, want %q", got, want)
		}
		events := transport.Events()
		if len(events) != 1 {
			t.Fatalf("captured events = %d, want 1", len(events))
		}
		assertProviderSentryEvent(t, events[0], "album_cover", "store_imported_cover", http.StatusInternalServerError)
		if !sentryEventExceptionContains(events[0], "create album cover file") {
			t.Fatalf("captured exception did not retain storage cause: %#v", events[0].Exception)
		}
		if sentryEventExceptionContains(events[0], blockedDirectory) {
			t.Fatalf("captured exception exposed storage path %q: %#v", blockedDirectory, events[0].Exception)
		}
	})

	t.Run("blocked target is expected", func(t *testing.T) {
		service := &albumCoverService{
			albumCoversDir: t.TempDir(),
			remoteFetcher: remoteImageFetcherFunc(func(context.Context, string) (remoteImageFetchResult, error) {
				return remoteImageFetchResult{}, errAlbumCoverBlockedRemoteTarget
			}),
		}
		rec, transport := serveProviderSentryRequest(
			t,
			importAlbumCoverHandler(service),
			http.MethodPost,
			"/api/album-covers/import",
			strings.NewReader(`{"imageUrl":"http://127.0.0.1/cover.png"}`),
		)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
		if got := len(transport.Events()); got != 0 {
			t.Fatalf("captured events = %d, want 0", got)
		}
	})
}

func TestLyricsSearchSentryClassification(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		wantStatus    int
		wantEvents    int
		wantOperation string
	}{
		{
			name:          "provider failure is captured once",
			err:           fmt.Errorf("%w: upstream unavailable", errLyricsProvider),
			wantStatus:    http.StatusBadGateway,
			wantEvents:    1,
			wantOperation: "search_lyrics",
		},
		{
			name:          "provider timeout is captured once",
			err:           fmt.Errorf("%w: %w", errLyricsProviderTimeout, context.DeadlineExceeded),
			wantStatus:    http.StatusGatewayTimeout,
			wantEvents:    1,
			wantOperation: "search_lyrics",
		},
		{
			name:       "client cancellation is expected",
			err:        context.Canceled,
			wantStatus: http.StatusRequestTimeout,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newTestTrackStore(t)
			_ = seedLyricsTrack(t, store)
			service := newLyricsSearchService(&fakeLyricsSearchProvider{err: test.err})
			rec, transport := serveProviderSentryRequest(
				t,
				lyricsSearchHandler(store, service),
				http.MethodPost,
				"/api/tracks/1/lyrics/search",
				strings.NewReader(`{"trackName":"Track","artistNames":["Artist"]}`),
			)

			if rec.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, test.wantStatus, rec.Body.String())
			}
			events := transport.Events()
			if len(events) != test.wantEvents {
				t.Fatalf("captured events = %d, want %d", len(events), test.wantEvents)
			}
			if test.wantEvents == 1 {
				assertProviderSentryEvent(t, events[0], "lrclib", test.wantOperation, test.wantStatus)
				if strings.Contains(rec.Body.String(), test.err.Error()) {
					t.Fatalf("response exposed provider error: %q", rec.Body.String())
				}
			}
		})
	}
}

func TestLyricsPersistenceSentryClassification(t *testing.T) {
	t.Run("database failure is captured once", func(t *testing.T) {
		store := newTestTrackStore(t)
		_ = seedLyricsTrack(t, store)
		if err := store.db.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}

		rec, transport := serveProviderSentryRequest(
			t,
			putTrackLyricsHandler(store),
			http.MethodPut,
			"/api/tracks/1/lyrics",
			strings.NewReader(`{"type":"plain","plainText":"Lyrics"}`),
		)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusInternalServerError, rec.Body.String())
		}
		if got, want := strings.TrimSpace(rec.Body.String()), "failed to save lyrics"; got != want {
			t.Fatalf("body = %q, want %q", got, want)
		}
		events := transport.Events()
		if len(events) != 1 {
			t.Fatalf("captured events = %d, want 1", len(events))
		}
		assertProviderSentryEvent(t, events[0], "lyrics", "save", http.StatusInternalServerError)
		if !sentryEventExceptionContains(events[0], "persist lyrics") {
			t.Fatalf("captured exception did not retain persistence cause: %#v", events[0].Exception)
		}
	})

	t.Run("invalid payload is expected", func(t *testing.T) {
		store := newTestTrackStore(t)
		_ = seedLyricsTrack(t, store)
		rec, transport := serveProviderSentryRequest(
			t,
			putTrackLyricsHandler(store),
			http.MethodPut,
			"/api/tracks/1/lyrics",
			strings.NewReader(`{"type":"synced","plainText":"invalid","lines":[]}`),
		)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
		if got := len(transport.Events()); got != 0 {
			t.Fatalf("captured events = %d, want 0", got)
		}
	})
}

func TestProviderCausesPreserveErrorsWithoutSensitiveLocations(t *testing.T) {
	wantErr := io.ErrUnexpectedEOF
	secretURL := "https://provider.example/search?token=secret&query=private"
	tests := []struct {
		name     string
		sanitize func(error) error
	}{
		{name: "album cover", sanitize: albumCoverURLCause},
		{name: "lyrics", sanitize: lyricsProviderURLCause},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := test.sanitize(&url.Error{Op: http.MethodGet, URL: secretURL, Err: wantErr})
			if !errors.Is(got, wantErr) {
				t.Fatalf("sanitized error = %v, want wrapped %v", got, wantErr)
			}
			if strings.Contains(got.Error(), secretURL) || strings.Contains(got.Error(), "secret") {
				t.Fatalf("sanitized error exposed request URL: %v", got)
			}
		})
	}
}

func serveProviderSentryRequest(
	t *testing.T,
	handler http.Handler,
	method string,
	target string,
	body io.Reader,
) (*httptest.ResponseRecorder, *sentry.MockTransport) {
	t.Helper()
	hub, transport := newSentryTestHub(t)
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(sentry.SetHubOnContext(req.Context(), hub))
	rec := httptest.NewRecorder()
	buildHTTPHandler(handler, logModeErrorOnly, true).ServeHTTP(rec, req)
	return rec, transport
}

func assertProviderSentryEvent(t *testing.T, event *sentry.Event, component, operation string, status int) {
	t.Helper()
	if got := event.Tags["component"]; got != component {
		t.Errorf("component tag = %q, want %q", got, component)
	}
	if got := event.Tags["operation"]; got != operation {
		t.Errorf("operation tag = %q, want %q", got, operation)
	}
	if got, want := event.Tags["http.status_code"], fmt.Sprint(status); got != want {
		t.Errorf("http.status_code tag = %q, want %q", got, want)
	}
}

func sentryEventExceptionContains(event *sentry.Event, substring string) bool {
	for _, exception := range event.Exception {
		if strings.Contains(exception.Value, substring) {
			return true
		}
	}
	return false
}
