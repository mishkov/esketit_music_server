package main

import (
	"slices"
	"strings"
	"testing"
)

func TestLoadAlbumCoverSearchConfigurationFromEnv(t *testing.T) {
	tests := []struct {
		name               string
		configuredProvider string
		spotifyClientID    string
		spotifySecret      string
		wantSources        []string
		wantUnavailable    string
	}{
		{
			name:            "unset defaults to Spotify then Deezer when Spotify credentials exist",
			spotifyClientID: "client-id",
			spotifySecret:   "client-secret",
			wantSources:     []string{spotifySearchSource, deezerSearchSource},
		},
		{
			name:        "unset falls back to keyless Deezer without Spotify credentials",
			wantSources: []string{deezerSearchSource},
		},
		{
			name:               "explicit Spotify selects only Spotify",
			configuredProvider: "spotify",
			spotifyClientID:    "client-id",
			spotifySecret:      "client-secret",
			wantSources:        []string{spotifySearchSource},
		},
		{
			name:               "explicit Deezer needs no credentials",
			configuredProvider: "deezer",
			wantSources:        []string{deezerSearchSource},
		},
		{
			name:               "provider names are normalized and deduplicated",
			configuredProvider: " Deezer, SPOTIFY, deezer ",
			spotifyClientID:    "client-id",
			spotifySecret:      "client-secret",
			wantSources:        []string{deezerSearchSource, spotifySearchSource},
		},
		{
			name:               "unusable Spotify does not prevent configured Deezer",
			configuredProvider: "spotify,deezer",
			wantSources:        []string{deezerSearchSource},
		},
		{
			name:               "explicit Spotify without credentials is unavailable",
			configuredProvider: "spotify",
			wantUnavailable:    "SPOTIFY_CLIENT_ID or SPOTIFY_CLIENT_SECRET is not configured",
		},
		{
			name:               "unknown provider is rejected",
			configuredProvider: "spotify,unknown",
			spotifyClientID:    "client-id",
			spotifySecret:      "client-secret",
			wantUnavailable:    `unsupported provider "unknown"`,
		},
		{
			name:               "empty provider entry is rejected",
			configuredProvider: "spotify,",
			spotifyClientID:    "client-id",
			spotifySecret:      "client-secret",
			wantUnavailable:    "contains an empty provider name",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setAlbumCoverSearchEnvironmentForTest(t, test.configuredProvider, test.spotifyClientID, test.spotifySecret)

			provider, unavailableMessage := loadAlbumCoverSearchConfigurationFromEnv()
			if test.wantUnavailable != "" {
				if provider != nil {
					t.Fatalf("provider = %T, want nil", provider)
				}
				if !strings.Contains(unavailableMessage, test.wantUnavailable) {
					t.Fatalf("unavailable message = %q, want it to contain %q", unavailableMessage, test.wantUnavailable)
				}
				return
			}

			if provider == nil {
				t.Fatalf("provider = nil, unavailable message = %q", unavailableMessage)
			}
			if unavailableMessage != "" {
				t.Fatalf("unavailable message = %q, want empty", unavailableMessage)
			}
			if got := albumCoverSearchSourceNamesForTest(t, provider); !slices.Equal(got, test.wantSources) {
				t.Fatalf("source names = %#v, want %#v", got, test.wantSources)
			}
		})
	}
}

func TestLoadAlbumCoverSearchConfigurationUsesEndpointOverrides(t *testing.T) {
	setAlbumCoverSearchEnvironmentForTest(t, "spotify,deezer", "client-id", "client-secret")
	t.Setenv("SPOTIFY_API_BASE_URL", "https://spotify-api.example/v1")
	t.Setenv("SPOTIFY_TOKEN_URL", "https://spotify-accounts.example/token")
	t.Setenv("DEEZER_API_BASE_URL", "https://deezer-api.example/")

	provider, unavailableMessage := loadAlbumCoverSearchConfigurationFromEnv()
	if provider == nil {
		t.Fatalf("provider = nil, unavailable message = %q", unavailableMessage)
	}
	multiProvider := provider.(*multiAlbumCoverSearchProvider)
	spotifyProvider := multiProvider.sources[0].provider.(*spotifyAlbumCoverSearchProvider)
	if spotifyProvider.apiBaseURL != "https://spotify-api.example/v1" || spotifyProvider.tokenURL != "https://spotify-accounts.example/token" {
		t.Fatalf("Spotify endpoints = %q, %q", spotifyProvider.apiBaseURL, spotifyProvider.tokenURL)
	}
	deezerProvider := multiProvider.sources[1].provider.(*deezerAlbumCoverSearchProvider)
	if deezerProvider.apiBaseURL != "https://deezer-api.example" {
		t.Fatalf("Deezer API base URL = %q, want trimmed override", deezerProvider.apiBaseURL)
	}
}

func setAlbumCoverSearchEnvironmentForTest(t *testing.T, provider, clientID, clientSecret string) {
	t.Helper()
	t.Setenv(albumCoverSearchProviderEnv, provider)
	t.Setenv("SPOTIFY_CLIENT_ID", clientID)
	t.Setenv("SPOTIFY_CLIENT_SECRET", clientSecret)
	t.Setenv("SPOTIFY_API_BASE_URL", "")
	t.Setenv("SPOTIFY_TOKEN_URL", "")
	t.Setenv("DEEZER_API_BASE_URL", "")
}

func albumCoverSearchSourceNamesForTest(t *testing.T, provider albumCoverSearchProvider) []string {
	t.Helper()
	multiProvider, ok := provider.(*multiAlbumCoverSearchProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *multiAlbumCoverSearchProvider", provider)
	}
	names := make([]string, 0, len(multiProvider.sources))
	for _, source := range multiProvider.sources {
		names = append(names, source.name)
	}
	return names
}
