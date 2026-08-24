package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestValidateRemoteAlbumCoverURLHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := validateRemoteAlbumCoverURL(ctx, "https://context-cancellation-test.invalid/cover.jpg")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("validateRemoteAlbumCoverURL() error = %v, want context canceled", err)
	}
}

func TestSanitizeAlbumCoverDNSLookupErrorRemovesHostname(t *testing.T) {
	err := sanitizeAlbumCoverDNSLookupError(context.Background(), &net.DNSError{
		Err:  "no such host",
		Name: "private-customer-host.example",
	})
	if !errors.Is(err, errAlbumCoverDNSLookupFailed) {
		t.Fatalf("sanitizeAlbumCoverDNSLookupError() error = %v", err)
	}
	if strings.Contains(err.Error(), "private-customer-host") {
		t.Fatalf("sanitized DNS error leaked hostname: %v", err)
	}
}

func TestAlbumCoverSuggestionsHandlerSuccess(t *testing.T) {
	service := &albumCoverService{
		searchProvider: albumCoverSearchProviderFunc(func(ctx context.Context, query string, limit int) ([]albumCoverSuggestion, error) {
			if query != "Nevermind cover image" {
				t.Fatalf("query = %q, want %q", query, "Nevermind cover image")
			}
			if limit != 20 {
				t.Fatalf("limit = %d, want 20", limit)
			}
			return []albumCoverSuggestion{
				{
					ThumbnailURL:  "https://images.example/thumb.jpg",
					ImageURL:      "https://images.example/full.jpg",
					Width:         1400,
					Height:        1400,
					SourcePageURL: "https://example.com/album",
				},
			}, nil
		}),
	}

	handler := newAdminOnlyTestHandler(t, albumCoverSuggestionsHandler(service))
	req := httptest.NewRequest(http.MethodGet, "/api/album-covers/suggestions?query=Nevermind+cover+image&limit=20", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got albumCoverSuggestionsResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("len(got.Items) = %d, want 1", len(got.Items))
	}
	if got.Items[0].ImageURL != "https://images.example/full.jpg" {
		t.Fatalf("got.Items[0].ImageURL = %q", got.Items[0].ImageURL)
	}
}

func TestAlbumCoverSuggestionsHandlerUnavailableWithoutConfig(t *testing.T) {
	service := &albumCoverService{
		searchUnavailableMessage: "album cover suggestions are unavailable: SPOTIFY_CLIENT_ID or SPOTIFY_CLIENT_SECRET is not configured",
	}

	handler := newAdminOnlyTestHandler(t, albumCoverSuggestionsHandler(service))
	req := httptest.NewRequest(http.MethodGet, "/api/album-covers/suggestions?query=Nevermind", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusNotImplemented, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "SPOTIFY_CLIENT_ID") {
		t.Fatalf("body = %q, want config hint", rec.Body.String())
	}
}

func TestSpotifyAlbumCoverSearchProviderSplitsRequestsAboveTen(t *testing.T) {
	var searchLimits []int
	var searchOffsets []int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"test-token","token_type":"Bearer","expires_in":3600}`))
		case "/v1/search":
			if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
				t.Fatalf("Authorization header = %q, want %q", got, "Bearer test-token")
			}
			limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
			if err != nil {
				t.Fatalf("limit parse error = %v", err)
			}
			offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
			if err != nil {
				t.Fatalf("offset parse error = %v", err)
			}
			searchLimits = append(searchLimits, limit)
			searchOffsets = append(searchOffsets, offset)

			items := make([]string, 0, limit)
			for i := 0; i < limit; i++ {
				id := offset + i
				items = append(items, fmt.Sprintf(`{
					"id":"album-%d",
					"name":"Album %d",
					"external_urls":{"spotify":"https://open.spotify.com/album/%d"},
					"images":[
						{"url":"https://i.scdn.co/image/%d-large","width":640,"height":640},
						{"url":"https://i.scdn.co/image/%d-small","width":64,"height":64}
					]
				}`, id, id, id, id, id))
			}

			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"albums":{"items":[%s]}}`, strings.Join(items, ","))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	provider := &spotifyAlbumCoverSearchProvider{
		clientID:     "client-id",
		clientSecret: "client-secret",
		apiBaseURL:   server.URL + "/v1",
		tokenURL:     server.URL + "/token",
		client:       server.Client(),
	}

	items, err := provider.Search(context.Background(), "query", 15)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	if len(items) != 15 {
		t.Fatalf("len(items) = %d, want 15", len(items))
	}
	if len(searchLimits) != 2 {
		t.Fatalf("request count = %d, want 2", len(searchLimits))
	}
	if searchLimits[0] != 10 || searchOffsets[0] != 0 {
		t.Fatalf("first request limit/offset = %d/%d, want 10/0", searchLimits[0], searchOffsets[0])
	}
	if searchLimits[1] != 5 || searchOffsets[1] != 10 {
		t.Fatalf("second request limit/offset = %d/%d, want 5/10", searchLimits[1], searchOffsets[1])
	}
}

func TestSpotifyAlbumCoverSearchProviderRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_, _ = io.WriteString(w, `{"access_token":"test-token","token_type":"Bearer","expires_in":3600}`)
		case "/v1/search":
			_, _ = io.WriteString(w, strings.Repeat("x", maxSpotifySearchResponseBytes+1))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	provider := &spotifyAlbumCoverSearchProvider{
		clientID:     "client-id",
		clientSecret: "client-secret",
		apiBaseURL:   server.URL + "/v1",
		tokenURL:     server.URL + "/token",
		client:       server.Client(),
	}

	_, err := provider.Search(context.Background(), "query", 1)
	if !errors.Is(err, errAlbumCoverProviderResponseTooLarge) {
		t.Fatalf("Search() error = %v, want %v", err, errAlbumCoverProviderResponseTooLarge)
	}
}

func TestImportAlbumCoverHandlerSuccess(t *testing.T) {
	albumCoversDir := t.TempDir()
	service := &albumCoverService{
		albumCoversDir: albumCoversDir,
		remoteFetcher: remoteImageFetcherFunc(func(ctx context.Context, imageURL string) (remoteImageFetchResult, error) {
			if imageURL != "https://cdn.example.com/nevermind" {
				t.Fatalf("imageURL = %q", imageURL)
			}
			return remoteImageFetchResult{
				ContentType: "image/png",
				Data:        mustDecodeBase64(t, tinyPNGBase64),
				FinalURL:    imageURL,
			}, nil
		}),
	}

	handler := newAdminOnlyTestHandler(t, importAlbumCoverHandler(service))
	body := strings.NewReader(`{"imageUrl":"https://cdn.example.com/nevermind","suggestedFileName":"nevermind.jpg"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/album-covers/import", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var got albumCoverImportResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if !strings.HasPrefix(got.Name, "nevermind-") {
		t.Fatalf("got.Name = %q, want normalized prefix", got.Name)
	}
	if !strings.HasSuffix(got.Name, ".png") {
		t.Fatalf("got.Name = %q, want .png suffix", got.Name)
	}
	if got.URL != "/api/album-covers/"+got.Name {
		t.Fatalf("got.URL = %q, want %q", got.URL, "/api/album-covers/"+got.Name)
	}
	if _, err := os.Stat(filepath.Join(albumCoversDir, got.Name)); err != nil {
		t.Fatalf("stored file Stat() error = %v", err)
	}
}

func TestImportAlbumCoverHandlerRejectsNonImage(t *testing.T) {
	service := &albumCoverService{
		albumCoversDir: t.TempDir(),
		remoteFetcher: remoteImageFetcherFunc(func(ctx context.Context, imageURL string) (remoteImageFetchResult, error) {
			return remoteImageFetchResult{
				ContentType: "text/plain; charset=utf-8",
				Data:        []byte("not an image"),
				FinalURL:    imageURL,
			}, nil
		}),
	}

	handler := newAdminOnlyTestHandler(t, importAlbumCoverHandler(service))
	req := httptest.NewRequest(http.MethodPost, "/api/album-covers/import", strings.NewReader(`{"imageUrl":"https://example.com/file.txt"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), errAlbumCoverRemoteNotImage.Error()) {
		t.Fatalf("body = %q, want %q", rec.Body.String(), errAlbumCoverRemoteNotImage.Error())
	}
}

func TestImportAlbumCoverHandlerRejectsBlockedPrivateTarget(t *testing.T) {
	service := &albumCoverService{
		albumCoversDir: t.TempDir(),
		remoteFetcher:  newSSRFProtectedRemoteImageFetcher(maxAlbumCoverImportSizeBytes, albumCoverImportTimeout),
	}

	handler := newAdminOnlyTestHandler(t, importAlbumCoverHandler(service))
	req := httptest.NewRequest(http.MethodPost, "/api/album-covers/import", strings.NewReader(`{"imageUrl":"http://127.0.0.1/cover.jpg"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), errAlbumCoverBlockedRemoteTarget.Error()) {
		t.Fatalf("body = %q, want %q", rec.Body.String(), errAlbumCoverBlockedRemoteTarget.Error())
	}
}

func newAdminOnlyTestHandler(t *testing.T, next http.Handler) http.Handler {
	t.Helper()

	store := newTestTrackStore(t)
	auth := newAuthManager([]byte("test-secret-test-secret-test-secret!!"), defaultAccessTokenTTL, defaultRefreshTokenTTL)
	admin, err := store.createUser("admin@example.com", "hash")
	if err != nil {
		t.Fatalf("createUser() error = %v", err)
	}

	store.mu.Lock()
	admin.Role = roleAdmin
	store.users[admin.ID] = admin
	store.mu.Unlock()

	token, _, err := auth.createAccessToken(admin.ID)
	if err != nil {
		t.Fatalf("createAccessToken() error = %v", err)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := r.Clone(r.Context())
		req.Header = r.Header.Clone()
		req.Header.Set("Authorization", "Bearer "+token)
		requireRole(auth, store, roleAdmin, next).ServeHTTP(w, req)
	})
}

func mustDecodeBase64(t *testing.T, value string) []byte {
	t.Helper()

	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatalf("DecodeString() error = %v", err)
	}
	return data
}

const tinyPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO9WJm0AAAAASUVORK5CYII="
