package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
)

const (
	albumCoverSearchProviderEnv = "ALBUM_COVER_SEARCH_PROVIDER"
	spotifySearchSource         = "spotify"
	deezerSearchSource          = "deezer"
)

func loadAlbumCoverSearchConfigurationFromEnv() (albumCoverSearchProvider, string) {
	providerNames, explicitlyConfigured, err := configuredAlbumCoverSearchProviderNames()
	if err != nil {
		return nil, "album cover suggestions are unavailable: " + err.Error()
	}

	sources := make([]albumCoverSearchSource, 0, len(providerNames))
	skippedReasons := make([]string, 0, len(providerNames))
	for _, providerName := range providerNames {
		provider, reason := newConfiguredAlbumCoverSearchProvider(providerName)
		if provider == nil {
			skippedReasons = append(skippedReasons, reason)
			if explicitlyConfigured {
				log.Printf("album cover search provider skipped source=%s reason=%s", providerName, reason)
			}
			continue
		}
		sources = append(sources, albumCoverSearchSource{name: providerName, provider: provider})
	}

	provider := newMultiAlbumCoverSearchProvider(sources)
	if provider != nil {
		return provider, ""
	}
	if len(skippedReasons) > 0 {
		return nil, "album cover suggestions are unavailable: " + strings.Join(skippedReasons, "; ")
	}
	return nil, errAlbumCoverSuggestionsUnavailable.Error()
}

func configuredAlbumCoverSearchProviderNames() ([]string, bool, error) {
	rawValue := strings.TrimSpace(os.Getenv(albumCoverSearchProviderEnv))
	if rawValue == "" {
		return []string{spotifySearchSource, deezerSearchSource}, false, nil
	}

	parts := strings.Split(rawValue, ",")
	names := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" {
			return nil, true, fmt.Errorf("%s contains an empty provider name", albumCoverSearchProviderEnv)
		}
		switch name {
		case spotifySearchSource, deezerSearchSource:
		default:
			return nil, true, fmt.Errorf("unsupported provider %q", name)
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names, true, nil
}

func newConfiguredAlbumCoverSearchProvider(providerName string) (albumCoverSearchProvider, string) {
	switch providerName {
	case spotifySearchSource:
		clientID := strings.TrimSpace(os.Getenv("SPOTIFY_CLIENT_ID"))
		clientSecret := strings.TrimSpace(os.Getenv("SPOTIFY_CLIENT_SECRET"))
		if clientID == "" || clientSecret == "" {
			return nil, "SPOTIFY_CLIENT_ID or SPOTIFY_CLIENT_SECRET is not configured"
		}

		apiBaseURL := strings.TrimSpace(os.Getenv("SPOTIFY_API_BASE_URL"))
		if apiBaseURL == "" {
			apiBaseURL = "https://api.spotify.com/v1"
		}
		tokenURL := strings.TrimSpace(os.Getenv("SPOTIFY_TOKEN_URL"))
		if tokenURL == "" {
			tokenURL = "https://accounts.spotify.com/api/token"
		}
		return &spotifyAlbumCoverSearchProvider{
			clientID:     clientID,
			clientSecret: clientSecret,
			apiBaseURL:   apiBaseURL,
			tokenURL:     tokenURL,
			client: &http.Client{
				Timeout: albumCoverSearchTimeout,
			},
		}, ""

	case deezerSearchSource:
		return newDeezerAlbumCoverSearchProvider(os.Getenv("DEEZER_API_BASE_URL"), nil), ""

	default:
		return nil, fmt.Sprintf("unsupported provider %q", providerName)
	}
}
