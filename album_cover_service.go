package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAlbumCoverSuggestionsLimit = 20
	maxAlbumCoverSuggestionsLimit     = 20
	maxAlbumCoverImportSizeBytes      = 15 << 20
	maxAlbumCoverRedirects            = 5
	albumCoverSearchTimeout           = 10 * time.Second
	albumCoverImportTimeout           = 15 * time.Second
	spotifySearchPageLimit            = 10
)

var (
	errAlbumCoverSuggestionsUnavailable = errors.New("album cover suggestions are not configured")
	errAlbumCoverBlockedRemoteTarget    = errors.New("remote image URL points to a blocked host")
	errAlbumCoverInvalidRemoteTarget    = errors.New("imageUrl must be an absolute http or https URL")
	errAlbumCoverRemoteNotImage         = errors.New("remote response is not a supported image")
	errAlbumCoverRemoteTooLarge         = errors.New("remote image exceeds the maximum allowed size")
	errAlbumCoverRemoteEmpty            = errors.New("remote image is empty")
)

var acceptedAlbumCoverMIMETypes = map[string]string{
	"image/gif":  ".gif",
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
}

var blockedAlbumCoverIPPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

type albumCoverSuggestion struct {
	ThumbnailURL  string `json:"thumbnailUrl"`
	ImageURL      string `json:"imageUrl"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	SourcePageURL string `json:"sourcePageUrl"`
}

type albumCoverSuggestionsResponse struct {
	Items []albumCoverSuggestion `json:"items"`
}

type albumCoverImportRequest struct {
	ImageURL          string `json:"imageUrl"`
	SuggestedFileName string `json:"suggestedFileName"`
}

type albumCoverImportResponse struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type albumCoverSearchProvider interface {
	Search(ctx context.Context, query string, limit int) ([]albumCoverSuggestion, error)
}

type albumCoverSearchProviderFunc func(ctx context.Context, query string, limit int) ([]albumCoverSuggestion, error)

func (f albumCoverSearchProviderFunc) Search(ctx context.Context, query string, limit int) ([]albumCoverSuggestion, error) {
	return f(ctx, query, limit)
}

type remoteImageFetcher interface {
	Fetch(ctx context.Context, imageURL string) (remoteImageFetchResult, error)
}

type remoteImageFetcherFunc func(ctx context.Context, imageURL string) (remoteImageFetchResult, error)

func (f remoteImageFetcherFunc) Fetch(ctx context.Context, imageURL string) (remoteImageFetchResult, error) {
	return f(ctx, imageURL)
}

type remoteImageFetchResult struct {
	ContentType string
	Data        []byte
	FinalURL    string
}

type albumCoverService struct {
	albumCoversDir           string
	searchProvider           albumCoverSearchProvider
	searchUnavailableMessage string
	remoteFetcher            remoteImageFetcher
}

func newAlbumCoverServiceFromEnv(albumCoversDir string) *albumCoverService {
	return &albumCoverService{
		albumCoversDir:           albumCoversDir,
		searchProvider:           loadAlbumCoverSearchProviderFromEnv(),
		searchUnavailableMessage: loadAlbumCoverSearchUnavailableMessage(),
		remoteFetcher:            newSSRFProtectedRemoteImageFetcher(maxAlbumCoverImportSizeBytes, albumCoverImportTimeout),
	}
}

func loadAlbumCoverSearchProviderFromEnv() albumCoverSearchProvider {
	provider := strings.TrimSpace(os.Getenv("ALBUM_COVER_SEARCH_PROVIDER"))
	spotifyClientID := strings.TrimSpace(os.Getenv("SPOTIFY_CLIENT_ID"))
	spotifyClientSecret := strings.TrimSpace(os.Getenv("SPOTIFY_CLIENT_SECRET"))
	if provider == "" && spotifyClientID != "" && spotifyClientSecret != "" {
		provider = "spotify"
	}

	switch strings.ToLower(provider) {
	case "spotify":
		if spotifyClientID == "" || spotifyClientSecret == "" {
			return nil
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
			clientID:     spotifyClientID,
			clientSecret: spotifyClientSecret,
			apiBaseURL:   apiBaseURL,
			tokenURL:     tokenURL,
			client: &http.Client{
				Timeout: albumCoverSearchTimeout,
			},
		}
	default:
		return nil
	}
}

func loadAlbumCoverSearchUnavailableMessage() string {
	provider := strings.TrimSpace(os.Getenv("ALBUM_COVER_SEARCH_PROVIDER"))
	if provider == "" && strings.TrimSpace(os.Getenv("SPOTIFY_CLIENT_ID")) != "" && strings.TrimSpace(os.Getenv("SPOTIFY_CLIENT_SECRET")) != "" {
		provider = "spotify"
	}
	if provider == "" {
		return "album cover suggestions are unavailable: set SPOTIFY_CLIENT_ID and SPOTIFY_CLIENT_SECRET"
	}
	if !strings.EqualFold(provider, "spotify") {
		return fmt.Sprintf("album cover suggestions are unavailable: unsupported provider %q", provider)
	}
	if strings.TrimSpace(os.Getenv("SPOTIFY_CLIENT_ID")) == "" || strings.TrimSpace(os.Getenv("SPOTIFY_CLIENT_SECRET")) == "" {
		return "album cover suggestions are unavailable: SPOTIFY_CLIENT_ID or SPOTIFY_CLIENT_SECRET is not configured"
	}
	return errAlbumCoverSuggestionsUnavailable.Error()
}

type spotifyAlbumCoverSearchProvider struct {
	clientID     string
	clientSecret string
	apiBaseURL   string
	tokenURL     string
	client       *http.Client
}

type spotifyTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

type spotifyAlbumSearchResponse struct {
	Albums struct {
		Items []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			ExternalURLs struct {
				Spotify string `json:"spotify"`
			} `json:"external_urls"`
			Images []struct {
				URL    string `json:"url"`
				Width  int    `json:"width"`
				Height int    `json:"height"`
			} `json:"images"`
		} `json:"items"`
	} `json:"albums"`
}

func (p *spotifyAlbumCoverSearchProvider) Search(ctx context.Context, query string, limit int) ([]albumCoverSuggestion, error) {
	token, err := p.fetchAccessToken(ctx)
	if err != nil {
		return nil, err
	}

	items := make([]albumCoverSuggestion, 0, limit)
	for offset := 0; offset < limit && len(items) < limit; offset += spotifySearchPageLimit {
		pageSize := min(limit-offset, spotifySearchPageLimit)
		page, err := p.searchAlbumsPage(ctx, token, query, pageSize, offset)
		if err != nil {
			return nil, err
		}
		for _, album := range page.Albums.Items {
			image, thumbnail, ok := pickSpotifyAlbumImages(album.Images)
			if !ok {
				continue
			}
			suggestion := albumCoverSuggestion{
				ThumbnailURL:  thumbnail.URL,
				ImageURL:      image.URL,
				Width:         image.Width,
				Height:        image.Height,
				SourcePageURL: album.ExternalURLs.Spotify,
			}
			if isValidAlbumCoverSuggestion(suggestion) {
				items = append(items, suggestion)
			}
		}
		if len(page.Albums.Items) < pageSize {
			break
		}
	}
	return items, nil
}

func (p *spotifyAlbumCoverSearchProvider) searchAlbumsPage(ctx context.Context, token, query string, limit, offset int) (spotifyAlbumSearchResponse, error) {
	endpoint, err := url.Parse(strings.TrimRight(p.apiBaseURL, "/") + "/search")
	if err != nil {
		return spotifyAlbumSearchResponse{}, fmt.Errorf("invalid spotify API base URL: %w", err)
	}
	params := endpoint.Query()
	params.Set("q", query)
	params.Set("type", "album")
	params.Set("limit", strconv.Itoa(limit))
	params.Set("offset", strconv.Itoa(offset))
	endpoint.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return spotifyAlbumSearchResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := p.client.Do(req)
	if err != nil {
		return spotifyAlbumSearchResponse{}, fmt.Errorf("spotify search request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return spotifyAlbumSearchResponse{}, fmt.Errorf("spotify search returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload spotifyAlbumSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return spotifyAlbumSearchResponse{}, fmt.Errorf("spotify search returned invalid JSON: %w", err)
	}
	return payload, nil
}

func (p *spotifyAlbumCoverSearchProvider) fetchAccessToken(ctx context.Context) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(p.clientID, p.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("spotify token request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("spotify token request returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload spotifyTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("spotify token response was invalid JSON: %w", err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return "", errors.New("spotify token response did not include an access token")
	}

	return payload.AccessToken, nil
}

type spotifyAlbumImage struct {
	URL    string
	Width  int
	Height int
}

func pickSpotifyAlbumImages(images []struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}) (spotifyAlbumImage, spotifyAlbumImage, bool) {
	if len(images) == 0 {
		return spotifyAlbumImage{}, spotifyAlbumImage{}, false
	}

	var (
		largest   spotifyAlbumImage
		thumbnail spotifyAlbumImage
	)
	for i, image := range images {
		if !isValidExternalHTTPURL(image.URL) || image.Width <= 0 || image.Height <= 0 {
			continue
		}
		candidate := spotifyAlbumImage{URL: image.URL, Width: image.Width, Height: image.Height}
		if largest.URL == "" || candidate.Width*candidate.Height > largest.Width*largest.Height {
			largest = candidate
		}
		if i == 0 || thumbnail.URL == "" || candidate.Width*candidate.Height < thumbnail.Width*thumbnail.Height {
			thumbnail = candidate
		}
	}
	if largest.URL == "" || thumbnail.URL == "" {
		return spotifyAlbumImage{}, spotifyAlbumImage{}, false
	}
	return largest, thumbnail, true
}

func isValidAlbumCoverSuggestion(item albumCoverSuggestion) bool {
	return item.Width > 0 &&
		item.Height > 0 &&
		isValidExternalHTTPURL(item.ThumbnailURL) &&
		isValidExternalHTTPURL(item.ImageURL) &&
		isValidExternalHTTPURL(item.SourcePageURL)
}

func isValidExternalHTTPURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func (s *albumCoverService) suggestionsUnavailableError() error {
	if strings.TrimSpace(s.searchUnavailableMessage) != "" {
		return errors.New(s.searchUnavailableMessage)
	}
	return errAlbumCoverSuggestionsUnavailable
}

func (s *albumCoverService) searchSuggestions(ctx context.Context, query string, limit int) ([]albumCoverSuggestion, error) {
	if s.searchProvider == nil {
		return nil, s.suggestionsUnavailableError()
	}

	ctx, cancel := context.WithTimeout(ctx, albumCoverSearchTimeout)
	defer cancel()

	return s.searchProvider.Search(ctx, query, limit)
}

func (s *albumCoverService) importRemoteCover(ctx context.Context, imageURL, suggestedFileName string) (albumCoverImportResponse, error) {
	if s.remoteFetcher == nil {
		return albumCoverImportResponse{}, errors.New("remote album cover importer is not configured")
	}

	ctx, cancel := context.WithTimeout(ctx, albumCoverImportTimeout)
	defer cancel()

	result, err := s.remoteFetcher.Fetch(ctx, imageURL)
	if err != nil {
		return albumCoverImportResponse{}, err
	}

	mimeType, err := normalizeImportedAlbumCoverMIMEType(result.ContentType, result.Data)
	if err != nil {
		return albumCoverImportResponse{}, err
	}

	fileName, err := normalizeImportedAlbumCoverFileName(suggestedFileName, result.FinalURL, mimeType)
	if err != nil {
		return albumCoverImportResponse{}, err
	}

	info, err := storeMediaBytes(s.albumCoversDir, fileName, result.Data, "/api/album-covers/")
	if err != nil {
		return albumCoverImportResponse{}, err
	}

	return albumCoverImportResponse{
		Name: info.Name,
		URL:  info.URL,
	}, nil
}

func normalizeImportedAlbumCoverMIMEType(contentType string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", errAlbumCoverRemoteEmpty
	}

	headerType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	sniffedType := strings.ToLower(http.DetectContentType(data))

	if headerType != "" && !strings.HasPrefix(headerType, "image/") && headerType != "application/octet-stream" {
		return "", errAlbumCoverRemoteNotImage
	}

	if ext, ok := acceptedAlbumCoverMIMETypes[sniffedType]; ok && ext != "" {
		return sniffedType, nil
	}
	if ext, ok := acceptedAlbumCoverMIMETypes[headerType]; ok && ext != "" {
		return headerType, nil
	}

	return "", errAlbumCoverRemoteNotImage
}

func normalizeImportedAlbumCoverFileName(suggestedFileName, finalURL, mimeType string) (string, error) {
	name := strings.TrimSpace(suggestedFileName)
	if name == "" && finalURL != "" {
		if parsed, err := url.Parse(finalURL); err == nil {
			name = path.Base(parsed.Path)
		}
	}
	if name == "" {
		name = "cover"
	}

	cleanName, err := sanitizeSongFileName(name)
	if err != nil {
		cleanName = "cover"
	}

	base := strings.TrimSuffix(cleanName, filepath.Ext(cleanName))
	if base == "" || base == "." {
		base = "cover"
	}

	ext := acceptedAlbumCoverMIMETypes[mimeType]
	return base + ext, nil
}

func storeMediaBytes(dir, fileName string, data []byte, urlPrefix string) (albumCoverInfo, error) {
	name, err := sanitizeSongFileName(fileName)
	if err != nil {
		return albumCoverInfo{}, err
	}
	if len(data) == 0 {
		return albumCoverInfo{}, errors.New("uploaded file is empty")
	}

	for attempt := 0; attempt < 10; attempt++ {
		storedName, err := randomizedStoredFileName(name)
		if err != nil {
			return albumCoverInfo{}, errors.New("failed to create album cover file")
		}

		fullPath := filepath.Join(dir, storedName)
		file, err := os.OpenFile(fullPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			if _, copyErr := io.Copy(file, bytes.NewReader(data)); copyErr != nil {
				_ = file.Close()
				_ = os.Remove(fullPath)
				return albumCoverInfo{}, errors.New("failed to save uploaded file")
			}
			if closeErr := file.Close(); closeErr != nil {
				_ = os.Remove(fullPath)
				return albumCoverInfo{}, errors.New("failed to save uploaded file")
			}
			name = storedName
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return albumCoverInfo{}, errors.New("failed to save uploaded file")
		}
	}

	info, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		return albumCoverInfo{}, errors.New("failed to read saved file")
	}

	return albumCoverInfo{
		Name:         name,
		SizeBytes:    info.Size(),
		LastModified: info.ModTime(),
		URL:          urlPrefix + url.PathEscape(name),
	}, nil
}

type ssrfProtectedRemoteImageFetcher struct {
	client   *http.Client
	maxBytes int64
}

func newSSRFProtectedRemoteImageFetcher(maxBytes int64, timeout time.Duration) remoteImageFetcher {
	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}

			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			if err := rejectBlockedIPAddrs(ips); err != nil {
				return nil, err
			}

			var dialErr error
			for _, ipAddr := range ips {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ipAddr.IP.String(), port))
				if err == nil {
					return conn, nil
				}
				dialErr = err
			}
			if dialErr == nil {
				dialErr = errors.New("no usable remote address")
			}
			return nil, dialErr
		},
		ForceAttemptHTTP2:     true,
		ResponseHeaderTimeout: timeout,
	}

	return &ssrfProtectedRemoteImageFetcher{
		client: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= maxAlbumCoverRedirects {
					return errors.New("too many redirects")
				}
				return validateRemoteAlbumCoverURL(req.URL.String())
			},
		},
		maxBytes: maxBytes,
	}
}

func (f *ssrfProtectedRemoteImageFetcher) Fetch(ctx context.Context, imageURL string) (remoteImageFetchResult, error) {
	if err := validateRemoteAlbumCoverURL(imageURL); err != nil {
		return remoteImageFetchResult{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return remoteImageFetchResult{}, errAlbumCoverInvalidRemoteTarget
	}
	req.Header.Set("Accept", "image/*")

	resp, err := f.client.Do(req)
	if err != nil {
		return remoteImageFetchResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return remoteImageFetchResult{}, fmt.Errorf("remote server returned status %d", resp.StatusCode)
	}
	if resp.ContentLength > f.maxBytes {
		return remoteImageFetchResult{}, errAlbumCoverRemoteTooLarge
	}

	limited := io.LimitReader(resp.Body, f.maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return remoteImageFetchResult{}, fmt.Errorf("failed to read remote image: %w", err)
	}
	if int64(len(data)) > f.maxBytes {
		return remoteImageFetchResult{}, errAlbumCoverRemoteTooLarge
	}

	return remoteImageFetchResult{
		ContentType: resp.Header.Get("Content-Type"),
		Data:        data,
		FinalURL:    resp.Request.URL.String(),
	}, nil
}

func validateRemoteAlbumCoverURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errAlbumCoverInvalidRemoteTarget
	}

	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return errAlbumCoverBlockedRemoteTarget
	}

	if ip, err := netip.ParseAddr(host); err == nil {
		if isBlockedAlbumCoverIP(ip) {
			return errAlbumCoverBlockedRemoteTarget
		}
		return nil
	}

	ips, err := net.DefaultResolver.LookupIPAddr(context.Background(), host)
	if err != nil {
		return fmt.Errorf("failed to resolve remote host: %w", err)
	}
	return rejectBlockedIPAddrs(ips)
}

func rejectBlockedIPAddrs(ips []net.IPAddr) error {
	if len(ips) == 0 {
		return errors.New("remote host did not resolve to any address")
	}
	for _, ipAddr := range ips {
		addr, ok := netip.AddrFromSlice(ipAddr.IP)
		if !ok {
			return errors.New("failed to parse resolved IP address")
		}
		if isBlockedAlbumCoverIP(addr.Unmap()) {
			return errAlbumCoverBlockedRemoteTarget
		}
	}
	return nil
}

func isBlockedAlbumCoverIP(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	for _, prefix := range blockedAlbumCoverIPPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func albumCoverSuggestionsHandler(service *albumCoverService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := strings.TrimSpace(r.URL.Query().Get("query"))
		if query == "" {
			http.Error(w, "query is required", http.StatusBadRequest)
			return
		}

		limit := defaultAlbumCoverSuggestionsLimit
		if rawLimit := strings.TrimSpace(r.URL.Query().Get("limit")); rawLimit != "" {
			value, err := strconv.Atoi(rawLimit)
			if err != nil || value <= 0 {
				http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
				return
			}
			if value > maxAlbumCoverSuggestionsLimit {
				value = maxAlbumCoverSuggestionsLimit
			}
			limit = value
		}

		items, err := service.searchSuggestions(r.Context(), query, limit)
		if err != nil {
			if errors.Is(err, errAlbumCoverSuggestionsUnavailable) || strings.Contains(err.Error(), "unavailable") || strings.Contains(err.Error(), "not configured") {
				http.Error(w, err.Error(), http.StatusNotImplemented)
				return
			}
			log.Printf("album cover suggestions failed query=%q limit=%d err=%v", query, limit, err)
			http.Error(w, "failed to fetch album cover suggestions", http.StatusBadGateway)
			return
		}

		writeJSON(w, http.StatusOK, albumCoverSuggestionsResponse{Items: items})
	}
}

func importAlbumCoverHandler(service *albumCoverService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req albumCoverImportRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}

		req.ImageURL = strings.TrimSpace(req.ImageURL)
		req.SuggestedFileName = strings.TrimSpace(req.SuggestedFileName)
		if req.ImageURL == "" {
			http.Error(w, "imageUrl is required", http.StatusBadRequest)
			return
		}

		resp, err := service.importRemoteCover(r.Context(), req.ImageURL, req.SuggestedFileName)
		if err != nil {
			log.Printf("album cover import failed imageUrl=%q suggestedFileName=%q err=%v", req.ImageURL, req.SuggestedFileName, err)
			switch {
			case errors.Is(err, errAlbumCoverBlockedRemoteTarget),
				errors.Is(err, errAlbumCoverInvalidRemoteTarget),
				errors.Is(err, errAlbumCoverRemoteNotImage),
				errors.Is(err, errAlbumCoverRemoteTooLarge),
				errors.Is(err, errAlbumCoverRemoteEmpty):
				http.Error(w, err.Error(), http.StatusBadRequest)
			default:
				http.Error(w, "failed to import album cover", http.StatusBadGateway)
			}
			return
		}

		writeJSON(w, http.StatusCreated, resp)
	}
}
