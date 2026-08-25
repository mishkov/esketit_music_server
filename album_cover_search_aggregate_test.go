package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestMultiAlbumCoverSearchProviderCombinesResultsInPriorityOrder(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	receivedLimits := map[string]int{}
	provider := newMultiAlbumCoverSearchProvider([]albumCoverSearchSource{
		{
			name: "spotify",
			provider: albumCoverSearchProviderFunc(func(_ context.Context, _ string, limit int) ([]albumCoverSuggestion, error) {
				mu.Lock()
				receivedLimits["spotify"] = limit
				mu.Unlock()
				return albumCoverSuggestionsForTest("shared", 6), nil
			}),
		},
		{
			name: "deezer",
			provider: albumCoverSearchProviderFunc(func(_ context.Context, _ string, limit int) ([]albumCoverSuggestion, error) {
				mu.Lock()
				receivedLimits["deezer"] = limit
				mu.Unlock()
				return albumCoverSuggestionsForTest("shared", 6), nil
			}),
		},
	})

	items, err := provider.Search(context.Background(), "query", 6)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(items) != 6 {
		t.Fatalf("len(items) = %d, want 6", len(items))
	}
	for index, item := range items {
		wantSource := "spotify"
		if index >= 3 {
			wantSource = "deezer"
		}
		if item.Source != wantSource {
			t.Fatalf("items[%d].Source = %q, want %q", index, item.Source, wantSource)
		}
	}
	if items[0].ImageURL != items[3].ImageURL {
		t.Fatalf("matching cross-provider URLs were unexpectedly removed: %q != %q", items[0].ImageURL, items[3].ImageURL)
	}

	mu.Lock()
	defer mu.Unlock()
	if receivedLimits["spotify"] != 6 || receivedLimits["deezer"] != 6 {
		t.Fatalf("provider limits = %#v, want full global limit for each provider", receivedLimits)
	}
}

func TestMultiAlbumCoverSearchProviderRedistributesUnusedCapacity(t *testing.T) {
	t.Parallel()

	provider := newMultiAlbumCoverSearchProvider([]albumCoverSearchSource{
		{
			name: "spotify",
			provider: albumCoverSearchProviderFunc(func(context.Context, string, int) ([]albumCoverSuggestion, error) {
				return albumCoverSuggestionsForTest("spotify", 2), nil
			}),
		},
		{
			name: "deezer",
			provider: albumCoverSearchProviderFunc(func(context.Context, string, int) ([]albumCoverSuggestion, error) {
				return albumCoverSuggestionsForTest("deezer", 10), nil
			}),
		},
	})

	items, err := provider.Search(context.Background(), "query", 6)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(items) != 6 {
		t.Fatalf("len(items) = %d, want 6", len(items))
	}
	for index, item := range items {
		wantSource := "spotify"
		if index >= 2 {
			wantSource = "deezer"
		}
		if item.Source != wantSource {
			t.Fatalf("items[%d].Source = %q, want %q", index, item.Source, wantSource)
		}
	}
}

func TestMultiAlbumCoverSearchProviderReturnsPartialSuccess(t *testing.T) {
	t.Parallel()

	spotifyErr := errors.New("spotify unavailable")
	provider := newMultiAlbumCoverSearchProvider([]albumCoverSearchSource{
		{
			name: "spotify",
			provider: albumCoverSearchProviderFunc(func(context.Context, string, int) ([]albumCoverSuggestion, error) {
				return nil, spotifyErr
			}),
		},
		{
			name: "deezer",
			provider: albumCoverSearchProviderFunc(func(context.Context, string, int) ([]albumCoverSuggestion, error) {
				return albumCoverSuggestionsForTest("deezer", 3), nil
			}),
		},
	})

	items, err := provider.Search(context.Background(), "query", 20)
	if err != nil {
		t.Fatalf("Search() error = %v, want successful fallback", err)
	}
	if len(items) != 3 {
		t.Fatalf("len(items) = %d, want 3", len(items))
	}
	for index, item := range items {
		if item.Source != "deezer" {
			t.Fatalf("items[%d].Source = %q, want deezer", index, item.Source)
		}
	}
}

func TestMultiAlbumCoverSearchProviderTreatsEmptyResponseAsSuccess(t *testing.T) {
	t.Parallel()

	provider := newMultiAlbumCoverSearchProvider([]albumCoverSearchSource{
		{
			name: "spotify",
			provider: albumCoverSearchProviderFunc(func(context.Context, string, int) ([]albumCoverSuggestion, error) {
				return []albumCoverSuggestion{}, nil
			}),
		},
		{
			name: "deezer",
			provider: albumCoverSearchProviderFunc(func(context.Context, string, int) ([]albumCoverSuggestion, error) {
				return nil, errors.New("deezer unavailable")
			}),
		},
	})

	items, err := provider.Search(context.Background(), "query", 20)
	if err != nil {
		t.Fatalf("Search() error = %v, want successful empty response", err)
	}
	if items == nil || len(items) != 0 {
		t.Fatalf("items = %#v, want non-nil empty slice", items)
	}
}

func TestMultiAlbumCoverSearchProviderReturnsAllFailures(t *testing.T) {
	t.Parallel()

	spotifyErr := errors.New("spotify unavailable")
	deezerErr := errors.New("deezer unavailable")
	provider := newMultiAlbumCoverSearchProvider([]albumCoverSearchSource{
		{
			name: "spotify",
			provider: albumCoverSearchProviderFunc(func(context.Context, string, int) ([]albumCoverSuggestion, error) {
				return nil, spotifyErr
			}),
		},
		{
			name: "deezer",
			provider: albumCoverSearchProviderFunc(func(context.Context, string, int) ([]albumCoverSuggestion, error) {
				return nil, deezerErr
			}),
		},
	})

	items, err := provider.Search(context.Background(), "query", 20)
	if err == nil {
		t.Fatal("Search() error = nil, want aggregate failure")
	}
	if items != nil {
		t.Fatalf("items = %#v, want nil", items)
	}
	if !errors.Is(err, spotifyErr) || !errors.Is(err, deezerErr) {
		t.Fatalf("Search() error = %v, want both provider causes", err)
	}
	if got := err.Error(); !stringsAppearInOrder(got, "spotify", "deezer") {
		t.Fatalf("Search() error = %q, want failures in provider priority order", got)
	}
}

func TestMultiAlbumCoverSearchProviderRunsSourcesConcurrently(t *testing.T) {
	t.Parallel()

	started := make(chan string, 2)
	release := make(chan struct{})
	providerFor := func(name string) albumCoverSearchProvider {
		return albumCoverSearchProviderFunc(func(context.Context, string, int) ([]albumCoverSuggestion, error) {
			started <- name
			<-release
			return albumCoverSuggestionsForTest(name, 1), nil
		})
	}
	provider := newMultiAlbumCoverSearchProvider([]albumCoverSearchSource{
		{name: "spotify", provider: providerFor("spotify")},
		{name: "deezer", provider: providerFor("deezer")},
	})

	done := make(chan error, 1)
	go func() {
		_, err := provider.Search(context.Background(), "query", 2)
		done <- err
	}()

	seen := map[string]bool{}
	for range 2 {
		seen[<-started] = true
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if !seen["spotify"] || !seen["deezer"] {
		t.Fatalf("started providers = %#v, want both", seen)
	}
}

func TestAlbumCoverSuggestionSourceJSONIsOptional(t *testing.T) {
	t.Parallel()

	withSource, err := json.Marshal(albumCoverSuggestion{Source: "deezer"})
	if err != nil {
		t.Fatalf("Marshal(with source) error = %v", err)
	}
	if !strings.Contains(string(withSource), `"source":"deezer"`) {
		t.Fatalf("Marshal(with source) = %s, want source", withSource)
	}

	withoutSource, err := json.Marshal(albumCoverSuggestion{})
	if err != nil {
		t.Fatalf("Marshal(without source) error = %v", err)
	}
	if strings.Contains(string(withoutSource), `"source"`) {
		t.Fatalf("Marshal(without source) = %s, want source omitted", withoutSource)
	}
}

func albumCoverSuggestionsForTest(prefix string, count int) []albumCoverSuggestion {
	items := make([]albumCoverSuggestion, 0, count)
	for index := 0; index < count; index++ {
		items = append(items, albumCoverSuggestion{
			ThumbnailURL:  fmt.Sprintf("https://images.example/%s-%d-thumb.jpg", prefix, index),
			ImageURL:      fmt.Sprintf("https://images.example/%s-%d.jpg", prefix, index),
			Width:         1000,
			Height:        1000,
			SourcePageURL: fmt.Sprintf("https://catalog.example/%s-%d", prefix, index),
		})
	}
	return items
}

func stringsAppearInOrder(value, first, second string) bool {
	firstIndex := strings.Index(value, first)
	secondIndex := strings.Index(value, second)
	return firstIndex >= 0 && secondIndex > firstIndex
}
