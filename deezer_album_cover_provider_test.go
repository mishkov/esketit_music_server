package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDeezerAlbumCoverSearchProviderMapsBalancesAndDeduplicatesAlbums(t *testing.T) {
	var (
		requestMu    sync.Mutex
		requestPaths []string
		requestQuery []string
		requestLimit []string
		userAgents   []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestMu.Lock()
		requestPaths = append(requestPaths, r.URL.Path)
		requestQuery = append(requestQuery, r.URL.Query().Get("q"))
		requestLimit = append(requestLimit, r.URL.Query().Get("limit"))
		userAgents = append(userAgents, r.Header.Get("User-Agent"))
		requestMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/search/album":
			_, _ = io.WriteString(w, `{"data":[
				{"id":1,"link":"https://www.deezer.com/album/1","cover_medium":"https://cdn.example/1-medium.jpg","cover_xl":"https://cdn.example/1-xl.jpg"},
				{"id":2,"cover_medium":"https://cdn.example/2-medium.jpg","cover_xl":"https://cdn.example/2-xl.jpg"}
			]}`)
		case "/search/track":
			_, _ = io.WriteString(w, `{"data":[
				{"album":{"id":1,"cover_medium":"https://cdn.example/duplicate-medium.jpg","cover_xl":"https://cdn.example/duplicate-xl.jpg"}},
				{"album":{"id":3,"cover_medium":"https://cdn.example/shared-medium.jpg","cover_xl":"https://cdn.example/shared-xl.jpg"}},
				{"album":{"id":4,"cover_medium":"https://cdn.example/shared-medium.jpg","cover_xl":"https://cdn.example/shared-xl.jpg"}}
			]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := newDeezerAlbumCoverSearchProvider(server.URL, server.Client())
	items, err := provider.Search(context.Background(), "Loyalty Above Money", 4)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("len(items) = %d, want 4", len(items))
	}

	wantImageURLs := []string{
		"https://cdn.example/1-xl.jpg",
		"https://cdn.example/shared-xl.jpg",
		"https://cdn.example/2-xl.jpg",
		"https://cdn.example/shared-xl.jpg",
	}
	gotImageURLs := make([]string, 0, len(items))
	for _, item := range items {
		gotImageURLs = append(gotImageURLs, item.ImageURL)
		if item.Width != deezerCoverExtraLargeDimension || item.Height != deezerCoverExtraLargeDimension {
			t.Errorf("item dimensions = %dx%d, want %dx%d", item.Width, item.Height, deezerCoverExtraLargeDimension, deezerCoverExtraLargeDimension)
		}
		if !strings.Contains(item.ThumbnailURL, "medium") {
			t.Errorf("item.ThumbnailURL = %q, want cover_medium URL", item.ThumbnailURL)
		}
	}
	if !slices.Equal(gotImageURLs, wantImageURLs) {
		t.Fatalf("image URLs = %#v, want %#v", gotImageURLs, wantImageURLs)
	}
	if items[2].SourcePageURL != "https://www.deezer.com/album/2" {
		t.Errorf("album fallback source URL = %q", items[2].SourcePageURL)
	}
	if items[1].SourcePageURL != "https://www.deezer.com/album/3" || items[3].SourcePageURL != "https://www.deezer.com/album/4" {
		t.Errorf("track-derived source URLs = %q, %q", items[1].SourcePageURL, items[3].SourcePageURL)
	}

	requestMu.Lock()
	defer requestMu.Unlock()
	slices.Sort(requestPaths)
	if !slices.Equal(requestPaths, []string{"/search/album", "/search/track"}) {
		t.Errorf("request paths = %#v", requestPaths)
	}
	for i := range requestQuery {
		if requestQuery[i] != "Loyalty Above Money" {
			t.Errorf("request query = %q", requestQuery[i])
		}
		if requestLimit[i] != "4" {
			t.Errorf("request limit = %q", requestLimit[i])
		}
		if userAgents[i] != defaultDeezerUserAgent {
			t.Errorf("User-Agent = %q, want %q", userAgents[i], defaultDeezerUserAgent)
		}
	}
}

func TestBalanceDeezerSuggestionsRedistributesUnusedCapacity(t *testing.T) {
	items := func(prefix string, count int) []albumCoverSuggestion {
		result := make([]albumCoverSuggestion, 0, count)
		for i := range count {
			result = append(result, albumCoverSuggestion{ImageURL: fmt.Sprintf("%s-%d", prefix, i)})
		}
		return result
	}
	tests := []struct {
		name   string
		albums int
		tracks int
		limit  int
		want   []string
	}{
		{name: "even split with album first", albums: 5, tracks: 5, limit: 5, want: []string{"album-0", "track-0", "album-1", "track-1", "album-2"}},
		{name: "unused album capacity goes to tracks", albums: 1, tracks: 5, limit: 5, want: []string{"album-0", "track-0", "track-1", "track-2", "track-3"}},
		{name: "unused track capacity goes to albums", albums: 5, tracks: 1, limit: 5, want: []string{"album-0", "track-0", "album-1", "album-2", "album-3"}},
		{name: "empty albums allow tracks to fill limit", albums: 0, tracks: 5, limit: 5, want: []string{"track-0", "track-1", "track-2", "track-3", "track-4"}},
		{name: "empty tracks allow albums to fill limit", albums: 5, tracks: 0, limit: 5, want: []string{"album-0", "album-1", "album-2", "album-3", "album-4"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := balanceDeezerSuggestions(items("album", test.albums), items("track", test.tracks), test.limit)
			if len(got) != len(test.want) {
				t.Fatalf("len(got) = %d, want %d", len(got), len(test.want))
			}
			for index, want := range test.want {
				if got[index].ImageURL != want {
					t.Errorf("got[%d].ImageURL = %q, want %q", index, got[index].ImageURL, want)
				}
			}
		})
	}
}

func TestDeezerAlbumCoverSearchProviderAllowsPartialSearchSuccess(t *testing.T) {
	tests := []struct {
		name          string
		albumResponse string
		albumStatus   int
		trackResponse string
		trackStatus   int
		wantItems     int
	}{
		{
			name:          "album fails and track succeeds",
			albumResponse: `upstream body containing private query text`,
			albumStatus:   http.StatusServiceUnavailable,
			trackResponse: `{"data":[{"album":{"id":10,"cover_medium":"https://cdn.example/10-medium.jpg","cover_xl":"https://cdn.example/10-xl.jpg"}}]}`,
			trackStatus:   http.StatusOK,
			wantItems:     1,
		},
		{
			name:          "album succeeds and track fails",
			albumResponse: `{"data":[{"id":11,"cover_medium":"https://cdn.example/11-medium.jpg","cover_xl":"https://cdn.example/11-xl.jpg"}]}`,
			albumStatus:   http.StatusOK,
			trackResponse: `upstream failure`,
			trackStatus:   http.StatusBadGateway,
			wantItems:     1,
		},
		{
			name:          "successful empty album search is not a failure",
			albumResponse: `{"data":[]}`,
			albumStatus:   http.StatusOK,
			trackResponse: `upstream failure`,
			trackStatus:   http.StatusBadGateway,
			wantItems:     0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/search/album" {
					w.WriteHeader(test.albumStatus)
					_, _ = io.WriteString(w, test.albumResponse)
					return
				}
				w.WriteHeader(test.trackStatus)
				_, _ = io.WriteString(w, test.trackResponse)
			}))
			defer server.Close()

			provider := newDeezerAlbumCoverSearchProvider(server.URL, server.Client())
			items, err := provider.Search(context.Background(), "private query text", 3)
			if err != nil {
				t.Fatalf("Search() error = %v", err)
			}
			if items == nil {
				t.Fatal("Search() returned a nil slice on success")
			}
			if len(items) != test.wantItems {
				t.Fatalf("len(items) = %d, want %d", len(items), test.wantItems)
			}
		})
	}
}

func TestDeezerAlbumCoverSearchProviderReturnsTypedSanitizedErrorWhenBothSearchesFail(t *testing.T) {
	const sensitiveText = "private-search-query"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search/album" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, "provider echoed "+sensitiveText)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"error":{"code":4,"message":"provider echoed `+sensitiveText+`"}}`)
	}))
	defer server.Close()

	provider := newDeezerAlbumCoverSearchProvider(server.URL, server.Client())
	items, err := provider.Search(context.Background(), sensitiveText, 3)
	if err == nil {
		t.Fatal("Search() error = nil, want an error")
	}
	if items != nil {
		t.Fatalf("items = %#v, want nil", items)
	}
	var providerErr *deezerAlbumCoverSearchError
	if !errors.As(err, &providerErr) {
		t.Fatalf("Search() error type = %T, want *deezerAlbumCoverSearchError", err)
	}
	if strings.Contains(err.Error(), sensitiveText) || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("Search() error leaked sensitive input or request URL: %v", err)
	}
	if !strings.Contains(err.Error(), "status 429") || !strings.Contains(err.Error(), "API error code 4") {
		t.Fatalf("Search() error = %v, want sanitized status and API error code", err)
	}
}

func TestDeezerAlbumCoverSearchProviderRejectsOversizedResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", maxDeezerSearchResponseBytes+1))
	}))
	defer server.Close()

	provider := newDeezerAlbumCoverSearchProvider(server.URL, server.Client())
	_, err := provider.Search(context.Background(), "query", 1)
	if !errors.Is(err, errAlbumCoverProviderResponseTooLarge) {
		t.Fatalf("Search() error = %v, want %v", err, errAlbumCoverProviderResponseTooLarge)
	}
	if strings.Contains(err.Error(), strings.Repeat("x", 32)) {
		t.Fatalf("Search() error leaked response body: %v", err)
	}
}

func TestDeezerAlbumCoverSearchProviderHonorsContextCancellation(t *testing.T) {
	started := make(chan struct{}, 2)
	ctx, cancel := context.WithCancel(context.Background())
	provider := newDeezerAlbumCoverSearchProvider("https://api.deezer.test", &http.Client{
		Transport: deezerRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			started <- struct{}{}
			<-req.Context().Done()
			return nil, req.Context().Err()
		}),
	})
	type searchResult struct {
		items []albumCoverSuggestion
		err   error
	}
	done := make(chan searchResult, 1)
	go func() {
		items, err := provider.Search(ctx, "query", 1)
		done <- searchResult{items: items, err: err}
	}()

	waitForDeezerTestSignal(t, started)
	waitForDeezerTestSignal(t, started)
	cancel()
	result := <-done
	if result.items != nil {
		t.Fatalf("items = %#v, want nil", result.items)
	}
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("Search() error = %v, want context.Canceled", result.err)
	}
}

func TestDeezerAlbumCoverSearchProviderKeepsSuccessWhenPeerIsCanceled(t *testing.T) {
	albumRead := make(chan struct{})
	trackStarted := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	provider := newDeezerAlbumCoverSearchProvider("https://api.deezer.test", &http.Client{
		Transport: deezerRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/search/album":
				body := `{"data":[{"id":20,"cover_medium":"https://cdn.example/20-medium.jpg","cover_xl":"https://cdn.example/20-xl.jpg"}]}`
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body: &deezerEOFNotifyingReadCloser{
						reader: strings.NewReader(body),
						done:   albumRead,
					},
					Request: req,
				}, nil
			case "/search/track":
				close(trackStarted)
				<-req.Context().Done()
				return nil, req.Context().Err()
			default:
				return nil, errors.New("unexpected Deezer test path")
			}
		}),
	})
	type searchResult struct {
		items []albumCoverSuggestion
		err   error
	}
	done := make(chan searchResult, 1)
	go func() {
		items, err := provider.Search(ctx, "query", 2)
		done <- searchResult{items: items, err: err}
	}()

	waitForDeezerTestSignal(t, trackStarted)
	waitForDeezerTestSignal(t, albumRead)
	cancel()
	result := <-done
	if result.err != nil {
		t.Fatalf("Search() error = %v, want partial success", result.err)
	}
	if len(result.items) != 1 || result.items[0].ImageURL != "https://cdn.example/20-xl.jpg" {
		t.Fatalf("items = %#v, want successful album result", result.items)
	}
}

func TestDeezerAlbumCoverSearchProviderRunsAlbumAndTrackSearchesConcurrently(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- r.URL.Path
		<-release
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer server.Close()

	provider := newDeezerAlbumCoverSearchProvider(server.URL, server.Client())
	done := make(chan error, 1)
	go func() {
		_, err := provider.Search(context.Background(), "query", 2)
		done <- err
	}()

	seen := make(map[string]bool, 2)
	for range 2 {
		select {
		case path := <-started:
			seen[path] = true
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatal("album and track searches did not start concurrently")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if !seen["/search/album"] || !seen["/search/track"] {
		t.Fatalf("started paths = %#v, want album and track searches", seen)
	}
}

type deezerRoundTripFunc func(*http.Request) (*http.Response, error)

func (f deezerRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type deezerEOFNotifyingReadCloser struct {
	reader io.Reader
	done   chan struct{}
	once   sync.Once
}

func (r *deezerEOFNotifyingReadCloser) Read(buffer []byte) (int, error) {
	count, err := r.reader.Read(buffer)
	if errors.Is(err, io.EOF) {
		r.once.Do(func() { close(r.done) })
	}
	return count, err
}

func (r *deezerEOFNotifyingReadCloser) Close() error {
	return nil
}

func waitForDeezerTestSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Deezer test request")
	}
}
