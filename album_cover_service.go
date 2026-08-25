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

	"github.com/getsentry/sentry-go"
)

const (
	defaultAlbumCoverSuggestionsLimit = 20
	maxAlbumCoverSuggestionsLimit     = 20
	maxAlbumCoverImportSizeBytes      = 15 << 20
	maxAlbumCoverRedirects            = 5
	albumCoverSearchTimeout           = 10 * time.Second
	albumCoverImportTimeout           = 15 * time.Second
	spotifySearchPageLimit            = 10
	maxSpotifySearchResponseBytes     = 2 << 20
	maxSpotifyTokenResponseBytes      = 64 << 10
)

var (
	errAlbumCoverSuggestionsUnavailable   = errors.New("album cover suggestions are not configured")
	errAlbumCoverBlockedRemoteTarget      = errors.New("remote image URL points to a blocked host")
	errAlbumCoverInvalidRemoteTarget      = errors.New("imageUrl must be an absolute http or https URL")
	errAlbumCoverRemoteNotImage           = errors.New("remote response is not a supported image")
	errAlbumCoverRemoteTooLarge           = errors.New("remote image exceeds the maximum allowed size")
	errAlbumCoverRemoteEmpty              = errors.New("remote image is empty")
	errAlbumCoverImporterUnavailable      = errors.New("remote album cover importer is not configured")
	errAlbumCoverStorage                  = errors.New("album cover storage failed")
	errAlbumCoverDNSLookupFailed          = errors.New("remote host DNS lookup failed")
	errAlbumCoverProviderResponseTooLarge = errors.New("album cover provider response is too large")
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
	Source        string `json:"source,omitempty"`
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
	searchProvider, searchUnavailableMessage := loadAlbumCoverSearchConfigurationFromEnv()
	return &albumCoverService{
		albumCoversDir:           albumCoversDir,
		searchProvider:           searchProvider,
		searchUnavailableMessage: searchUnavailableMessage,
		remoteFetcher:            newSSRFProtectedRemoteImageFetcher(maxAlbumCoverImportSizeBytes, albumCoverImportTimeout),
	}
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
		return nil, fmt.Errorf("fetch Spotify access token: %w", err)
	}

	items := make([]albumCoverSuggestion, 0, limit)
	droppedItems := 0
	for offset := 0; offset < limit && len(items) < limit; offset += spotifySearchPageLimit {
		pageSize := min(limit-offset, spotifySearchPageLimit)
		page, err := p.searchAlbumsPage(ctx, token, query, pageSize, offset)
		if err != nil {
			return nil, fmt.Errorf("search Spotify albums: %w", err)
		}
		for _, album := range page.Albums.Items {
			image, thumbnail, ok := pickSpotifyAlbumImages(album.Images)
			if !ok {
				droppedItems++
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
			} else {
				droppedItems++
			}
		}
		if len(page.Albums.Items) < pageSize {
			break
		}
	}
	if droppedItems > 0 {
		log.Printf("Spotify album search omitted %d unusable result(s)", droppedItems)
	}
	return items, nil
}

func (p *spotifyAlbumCoverSearchProvider) searchAlbumsPage(ctx context.Context, token, query string, limit, offset int) (spotifyAlbumSearchResponse, error) {
	endpoint, err := url.Parse(strings.TrimRight(p.apiBaseURL, "/") + "/search")
	if err != nil {
		return spotifyAlbumSearchResponse{}, fmt.Errorf("invalid Spotify API base URL: %w", albumCoverURLCause(err))
	}
	params := endpoint.Query()
	params.Set("q", query)
	params.Set("type", "album")
	params.Set("limit", strconv.Itoa(limit))
	params.Set("offset", strconv.Itoa(offset))
	endpoint.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return spotifyAlbumSearchResponse{}, fmt.Errorf("build Spotify search request: %w", albumCoverURLCause(err))
	}
	req.Header.Set("Authorization", "Bearer "+token)

	req, span := withAlbumCoverProviderSpan(req, "spotify", "GET spotify.album_search")
	resp, err := p.client.Do(req)
	defer finishAlbumCoverProviderSpan(span, resp, err)
	if err != nil {
		return spotifyAlbumSearchResponse{}, fmt.Errorf("spotify search request failed: %w", albumCoverURLCause(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return spotifyAlbumSearchResponse{}, fmt.Errorf("spotify search returned status %d", resp.StatusCode)
	}

	var payload spotifyAlbumSearchResponse
	if err := decodeSpotifyJSON(resp.Body, maxSpotifySearchResponseBytes, &payload); err != nil {
		if span != nil {
			span.Status = sentry.SpanStatusDataLoss
		}
		return spotifyAlbumSearchResponse{}, fmt.Errorf("spotify search returned invalid JSON: %w", err)
	}
	return payload, nil
}

func (p *spotifyAlbumCoverSearchProvider) fetchAccessToken(ctx context.Context) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build Spotify token request: %w", albumCoverURLCause(err))
	}
	req.SetBasicAuth(p.clientID, p.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	req, span := withAlbumCoverProviderSpan(req, "spotify", "POST spotify.oauth_token")
	resp, err := p.client.Do(req)
	defer finishAlbumCoverProviderSpan(span, resp, err)
	if err != nil {
		return "", fmt.Errorf("spotify token request failed: %w", albumCoverURLCause(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("spotify token request returned status %d", resp.StatusCode)
	}

	var payload spotifyTokenResponse
	if err := decodeSpotifyJSON(resp.Body, maxSpotifyTokenResponseBytes, &payload); err != nil {
		if span != nil {
			span.Status = sentry.SpanStatusDataLoss
		}
		return "", fmt.Errorf("spotify token response was invalid JSON: %w", err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		if span != nil {
			span.Status = sentry.SpanStatusDataLoss
		}
		return "", errors.New("spotify token response did not include an access token")
	}

	return payload.AccessToken, nil
}

func decodeSpotifyJSON(body io.Reader, limit int64, dst any) error {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return errAlbumCoverProviderResponseTooLarge
	}
	return json.Unmarshal(data, dst)
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
		return fmt.Errorf("%w: %s", errAlbumCoverSuggestionsUnavailable, s.searchUnavailableMessage)
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
		return albumCoverImportResponse{}, errAlbumCoverImporterUnavailable
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
		return albumCoverImportResponse{}, fmt.Errorf("%w: %w", errAlbumCoverStorage, err)
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

	var storedPath string
	for attempt := 0; attempt < 10; attempt++ {
		storedName, err := randomizedStoredFileName(name)
		if err != nil {
			return albumCoverInfo{}, fmt.Errorf("generate album cover filename: %w", err)
		}

		fullPath := filepath.Join(dir, storedName)
		file, err := os.OpenFile(fullPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			if _, copyErr := io.Copy(file, bytes.NewReader(data)); copyErr != nil {
				cause := error(albumCoverPathCause(copyErr))
				if closeErr := file.Close(); closeErr != nil {
					cause = errors.Join(cause, fmt.Errorf("close incomplete album cover: %w", albumCoverPathCause(closeErr)))
				}
				if removeErr := removeFileForCleanup(fullPath); removeErr != nil {
					cause = errors.Join(cause, fmt.Errorf("remove incomplete album cover: %w", albumCoverPathCause(removeErr)))
				}
				return albumCoverInfo{}, fmt.Errorf("write album cover file: %w", cause)
			}
			if closeErr := file.Close(); closeErr != nil {
				cause := error(albumCoverPathCause(closeErr))
				if removeErr := removeFileForCleanup(fullPath); removeErr != nil {
					cause = errors.Join(cause, fmt.Errorf("remove album cover after close failure: %w", albumCoverPathCause(removeErr)))
				}
				return albumCoverInfo{}, fmt.Errorf("close album cover file: %w", cause)
			}
			name = storedName
			storedPath = fullPath
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return albumCoverInfo{}, fmt.Errorf("create album cover file: %w", albumCoverPathCause(err))
		}
	}
	if storedPath == "" {
		return albumCoverInfo{}, errors.New("could not allocate a unique album cover filename")
	}

	info, err := os.Stat(storedPath)
	if err != nil {
		cause := error(albumCoverPathCause(err))
		if removeErr := removeFileForCleanup(storedPath); removeErr != nil {
			cause = errors.Join(cause, fmt.Errorf("remove unreadable album cover: %w", albumCoverPathCause(removeErr)))
		}
		return albumCoverInfo{}, fmt.Errorf("read saved album cover file: %w", cause)
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
				return validateRemoteAlbumCoverURL(req.Context(), req.URL.String())
			},
		},
		maxBytes: maxBytes,
	}
}

func (f *ssrfProtectedRemoteImageFetcher) Fetch(ctx context.Context, imageURL string) (remoteImageFetchResult, error) {
	if err := validateRemoteAlbumCoverURL(ctx, imageURL); err != nil {
		return remoteImageFetchResult{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return remoteImageFetchResult{}, errAlbumCoverInvalidRemoteTarget
	}
	req.Header.Set("Accept", "image/*")

	req, span := withAlbumCoverProviderSpan(req, "remote_image", "GET album_cover.remote_image")
	resp, err := f.client.Do(req)
	defer finishAlbumCoverProviderSpan(span, resp, err)
	if err != nil {
		return remoteImageFetchResult{}, fmt.Errorf("remote image request failed: %w", albumCoverURLCause(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return remoteImageFetchResult{}, fmt.Errorf("remote server returned status %d", resp.StatusCode)
	}
	if resp.ContentLength > f.maxBytes {
		if span != nil {
			span.Status = sentry.SpanStatusResourceExhausted
		}
		return remoteImageFetchResult{}, errAlbumCoverRemoteTooLarge
	}

	limited := io.LimitReader(resp.Body, f.maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		if span != nil {
			span.Status = sentry.SpanStatusDataLoss
		}
		return remoteImageFetchResult{}, fmt.Errorf("failed to read remote image: %w", err)
	}
	if int64(len(data)) > f.maxBytes {
		if span != nil {
			span.Status = sentry.SpanStatusResourceExhausted
		}
		return remoteImageFetchResult{}, errAlbumCoverRemoteTooLarge
	}

	return remoteImageFetchResult{
		ContentType: resp.Header.Get("Content-Type"),
		Data:        data,
		FinalURL:    resp.Request.URL.String(),
	}, nil
}

func validateRemoteAlbumCoverURL(ctx context.Context, raw string) error {
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

	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return sanitizeAlbumCoverDNSLookupError(ctx, err)
	}
	return rejectBlockedIPAddrs(ips)
}

func sanitizeAlbumCoverDNSLookupError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("remote host DNS lookup timed out: %w", context.DeadlineExceeded)
	}
	return errAlbumCoverDNSLookupFailed
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
			if errors.Is(err, errAlbumCoverSuggestionsUnavailable) {
				markSentryErrorHandled(r.Context())
				http.Error(w, err.Error(), http.StatusNotImplemented)
				return
			}
			if errors.Is(r.Context().Err(), context.Canceled) || albumCoverSearchFailuresOnlyMatch(err, context.Canceled) {
				markSentryErrorHandled(r.Context())
				http.Error(w, "request canceled", http.StatusRequestTimeout)
				return
			}
			log.Printf("album cover suggestions failed: %s", safeOperationalError(err))
			status := http.StatusBadGateway
			message := "failed to fetch album cover suggestions"
			if albumCoverSearchFailuresOnlyMatch(err, context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
				message = "album cover provider timed out"
			}
			writeSentryHTTPError(w, r, err, message, status, "album_cover_search", "search_album_covers")
			return
		}

		writeJSON(w, http.StatusOK, albumCoverSuggestionsResponse{Items: items})
	}
}

func importAlbumCoverHandler(service *albumCoverService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req albumCoverImportRequest
		if err := decodeJSON(r, &req); err != nil {
			writeRequestDecodeError(w, err)
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
			switch {
			case errors.Is(err, errAlbumCoverBlockedRemoteTarget),
				errors.Is(err, errAlbumCoverInvalidRemoteTarget),
				errors.Is(err, errAlbumCoverRemoteNotImage),
				errors.Is(err, errAlbumCoverRemoteTooLarge),
				errors.Is(err, errAlbumCoverRemoteEmpty):
				http.Error(w, err.Error(), http.StatusBadRequest)
			case errors.Is(err, errAlbumCoverImporterUnavailable):
				markSentryErrorHandled(r.Context())
				http.Error(w, "album cover import is not configured", http.StatusNotImplemented)
			case errors.Is(err, context.Canceled):
				markSentryErrorHandled(r.Context())
				http.Error(w, "request canceled", http.StatusRequestTimeout)
			case errors.Is(err, context.DeadlineExceeded):
				log.Printf("album cover import timed out: %s", safeOperationalError(err))
				writeSentryHTTPError(w, r, err, "album cover provider timed out", http.StatusGatewayTimeout, "remote_image", "import_album_cover")
			case errors.Is(err, errAlbumCoverStorage):
				log.Printf("album cover storage failed: %s", safeOperationalError(err))
				writeSentryHTTPError(w, r, err, "failed to store album cover", http.StatusInternalServerError, "album_cover", "store_imported_cover")
			default:
				log.Printf("album cover import failed: %s", safeOperationalError(err))
				writeSentryHTTPError(w, r, err, "failed to import album cover", http.StatusBadGateway, "remote_image", "import_album_cover")
			}
			return
		}

		writeJSON(w, http.StatusCreated, resp)
	}
}

func withAlbumCoverProviderSpan(req *http.Request, component, description string) (*http.Request, *sentry.Span) {
	parent := sentry.SpanFromContext(req.Context())
	if parent == nil {
		return req, nil
	}
	span := parent.StartChild("http.client", sentry.WithDescription(description))
	span.SetTag("component", component)
	return req.WithContext(span.Context()), span
}

func finishAlbumCoverProviderSpan(span *sentry.Span, resp *http.Response, err error) {
	if span == nil {
		return
	}
	if span.Status == sentry.SpanStatusUndefined {
		switch {
		case errors.Is(err, context.Canceled):
			span.Status = sentry.SpanStatusCanceled
		case errors.Is(err, context.DeadlineExceeded):
			span.Status = sentry.SpanStatusDeadlineExceeded
		case err != nil:
			span.Status = sentry.SpanStatusInternalError
		case resp != nil:
			span.Status = sentry.HTTPtoSpanStatus(resp.StatusCode)
		default:
			span.Status = sentry.SpanStatusUnknown
		}
	}
	span.Finish()
}

func albumCoverURLCause(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return albumCoverURLCause(urlErr.Err)
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return sanitizeAlbumCoverDNSLookupError(context.Background(), dnsErr)
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		return albumCoverURLCause(opErr.Err)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return context.DeadlineExceeded
	}
	return err
}

func albumCoverPathCause(err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) && pathErr.Err != nil {
		return pathErr.Err
	}
	return err
}
