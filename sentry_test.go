package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
)

type errorReadCloser struct {
	err error
}

func (r errorReadCloser) Read([]byte) (int, error) {
	return 0, r.err
}

func (errorReadCloser) Close() error {
	return nil
}

func newSentryTestHub(t *testing.T) (*sentry.Hub, *sentry.MockTransport) {
	t.Helper()
	transport := &sentry.MockTransport{}
	client, err := sentry.NewClient(sentry.ClientOptions{Transport: transport})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return sentry.NewHub(client, sentry.NewScope()), transport
}

func TestBuildHTTPHandlerCapturesPanicWithSentry(t *testing.T) {
	hub, transport := newSentryTestHub(t)

	var sawRequestHub bool
	panicHandler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		sawRequestHub = sentry.GetHubFromContext(r.Context()) != nil
		panic("boom")
	})
	handler := buildHTTPHandler(panicHandler, logModeErrorOnly, true)
	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	req = req.WithContext(sentry.SetHubOnContext(req.Context(), hub))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !sawRequestHub {
		t.Fatal("request handler did not receive a request-scoped Sentry hub")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if got, want := rec.Body.String(), "internal server error\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}

	events := transport.Events()
	if len(events) != 1 {
		t.Fatalf("captured events = %d, want 1", len(events))
	}
	if got, want := events[0].Message, "boom"; got != want {
		t.Errorf("event message = %q, want %q", got, want)
	}
	if got, want := events[0].Level, sentry.LevelFatal; got != want {
		t.Errorf("event level = %q, want %q", got, want)
	}
}

func TestBuildHTTPHandlerRecordsPanicTransactionAsServerError(t *testing.T) {
	transport := &sentry.MockTransport{}
	client, err := sentry.NewClient(sentry.ClientOptions{
		Transport:        transport,
		EnableTracing:    true,
		TracesSampleRate: 1,
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	hub := sentry.NewHub(client, sentry.NewScope())
	mux := http.NewServeMux()
	mux.HandleFunc("GET /panic/{id}", func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})
	handler := buildHTTPHandler(mux, logModeErrorOnly, true)
	req := httptest.NewRequest(http.MethodGet, "/panic/42", nil)
	req = req.WithContext(sentry.SetHubOnContext(req.Context(), hub))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	var transaction *sentry.Event
	for _, event := range transport.Events() {
		if event.Type == "transaction" {
			transaction = event
			break
		}
	}
	if transaction == nil {
		t.Fatal("panic transaction was not captured")
	}
	trace := transaction.Contexts["trace"]
	if got := trace["status"]; got != sentry.SpanStatusInternalError {
		t.Fatalf("transaction status = %#v, want %v", got, sentry.SpanStatusInternalError)
	}
	data, ok := trace["data"].(map[string]any)
	if !ok || data["http.response.status_code"] != http.StatusInternalServerError {
		t.Fatalf("transaction data = %#v, want HTTP 500", trace["data"])
	}
}

func TestBuildHTTPHandlerCapturesUnhandledServerResponse(t *testing.T) {
	hub, transport := newSentryTestHub(t)
	handler := buildHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "generic failure", http.StatusInternalServerError)
	}), logModeErrorOnly, true)
	req := httptest.NewRequest(http.MethodGet, "/generic-failure", nil)
	req = req.WithContext(sentry.SetHubOnContext(req.Context(), hub))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	events := transport.Events()
	if len(events) != 1 {
		t.Fatalf("captured events = %d, want 1", len(events))
	}
	if got := events[0].Tags["operation"]; got != "unhandled_server_response" {
		t.Errorf("operation tag = %q, want unhandled_server_response", got)
	}
}

func TestBuildHTTPHandlerUsesMatchedRouteForSentryTransaction(t *testing.T) {
	transport := &sentry.MockTransport{}
	client, err := sentry.NewClient(sentry.ClientOptions{
		Transport:        transport,
		EnableTracing:    true,
		TracesSampleRate: 1,
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	hub := sentry.NewHub(client, sentry.NewScope())
	mux := http.NewServeMux()
	mux.HandleFunc("GET /things/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := buildHTTPHandler(mux, logModeErrorOnly, true)
	req := httptest.NewRequest(http.MethodGet, "/things/123", nil)
	req = req.WithContext(sentry.SetHubOnContext(req.Context(), hub))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	events := transport.Events()
	if len(events) != 1 {
		t.Fatalf("captured events = %d, want one transaction", len(events))
	}
	if got, want := events[0].Transaction, "GET /things/{id}"; got != want {
		t.Fatalf("transaction = %q, want %q", got, want)
	}
}

func TestExplicitServerErrorIsCapturedOnlyOnce(t *testing.T) {
	hub, transport := newSentryTestHub(t)
	wantErr := errors.New("database unavailable")
	handler := buildHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSentryHTTPError(
			w,
			r,
			wantErr,
			"failed to load tracks",
			http.StatusInternalServerError,
			"database",
			"tracks.list",
		)
	}), logModeErrorOnly, true)
	req := httptest.NewRequest(http.MethodGet, "/api/tracks", nil)
	req = req.WithContext(sentry.SetHubOnContext(req.Context(), hub))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	events := transport.Events()
	if len(events) != 1 {
		t.Fatalf("captured events = %d, want 1", len(events))
	}
	if len(events[0].Exception) != 1 || events[0].Exception[0].Value != wantErr.Error() {
		t.Fatalf("captured exception = %#v, want %q", events[0].Exception, wantErr)
	}
	if got := events[0].Tags["operation"]; got != "tracks.list" {
		t.Errorf("operation tag = %q, want tracks.list", got)
	}
}

func TestCaptureSentryErrorPreservesNonCancellationCause(t *testing.T) {
	hub, transport := newSentryTestHub(t)
	req := httptest.NewRequest(http.MethodGet, "/cleanup", nil)
	req = req.WithContext(sentry.SetHubOnContext(req.Context(), hub))
	req = withSentryRequestCaptureState(req)

	captureSentryError(
		req.Context(),
		errors.Join(context.Canceled, errors.New("cleanup failed")),
		"test",
		"cleanup",
	)

	if events := transport.Events(); len(events) != 1 {
		t.Fatalf("captured events = %d, want 1", len(events))
	}
}

func TestCaptureSentryErrorSuppressesCancellationOnly(t *testing.T) {
	hub, transport := newSentryTestHub(t)
	req := httptest.NewRequest(http.MethodGet, "/canceled", nil)
	req = req.WithContext(sentry.SetHubOnContext(req.Context(), hub))
	req = withSentryRequestCaptureState(req)

	captureSentryError(req.Context(), fmt.Errorf("request stopped: %w", context.Canceled), "test", "canceled")

	if events := transport.Events(); len(events) != 0 {
		t.Fatalf("captured events = %d, want 0", len(events))
	}
}

func TestRemoveReportedSentryErrorPreservesOtherLifecycleFailures(t *testing.T) {
	closeErr := errors.New("database close failed")
	err := errors.Join(fmt.Errorf("serve HTTP: %w", errHTTPServerPanic), closeErr)

	got := removeReportedSentryError(err, errHTTPServerPanic)
	if got == nil || !errors.Is(got, closeErr) {
		t.Fatalf("filtered error = %v, want database close failure", got)
	}
	if errors.Is(got, errHTTPServerPanic) {
		t.Fatalf("filtered error still includes already reported panic: %v", got)
	}
}

func TestExpectedClientErrorIsNotCaptured(t *testing.T) {
	hub, transport := newSentryTestHub(t)
	handler := buildHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSentryHTTPError(
			w,
			r,
			errors.New("invalid request"),
			"invalid request",
			http.StatusBadRequest,
			"test",
			"validate",
		)
	}), logModeErrorOnly, true)
	req := httptest.NewRequest(http.MethodPost, "/invalid", nil)
	req = req.WithContext(sentry.SetHubOnContext(req.Context(), hub))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if got := len(transport.Events()); got != 0 {
		t.Fatalf("captured events = %d, want 0", got)
	}
}

func TestSentryUserDoesNotLeakBetweenRequests(t *testing.T) {
	transport := &sentry.MockTransport{}
	client, err := sentry.NewClient(sentry.ClientOptions{Transport: transport})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	currentHub := sentry.CurrentHub()
	previousClient := currentHub.Client()
	currentHub.PushScope()
	currentHub.BindClient(client)
	t.Cleanup(func() {
		currentHub.BindClient(previousClient)
		currentHub.PopScope()
	})

	handler := buildHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/with-user" {
			setSentryUser(r.Context(), 42)
		}
		writeSentryInternalError(w, r, errors.New("request failed"), "request failed", "test", "user_isolation")
	}), logModeErrorOnly, true)

	for _, path := range []string{"/with-user", "/without-user"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}

	events := transport.Events()
	if len(events) != 2 {
		t.Fatalf("captured events = %d, want 2", len(events))
	}
	if got := events[0].User.ID; got != "42" {
		t.Errorf("first event user ID = %q, want 42", got)
	}
	if got := events[1].User.ID; got != "" {
		t.Errorf("second event user ID = %q, want empty", got)
	}
}

func TestOptionalAuthenticationAddsSentryUser(t *testing.T) {
	hub, transport := newSentryTestHub(t)
	auth := newAuthManager([]byte(strings.Repeat("a", 32)), time.Minute, time.Hour)
	token, _, err := auth.createAccessToken(42)
	if err != nil {
		t.Fatalf("createAccessToken() error = %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/tracks", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req = req.WithContext(sentry.SetHubOnContext(req.Context(), hub))

	if got := optionalUserIDFromRequest(req, auth); got != 42 {
		t.Fatalf("optionalUserIDFromRequest() = %d, want 42", got)
	}
	captureSentryError(req.Context(), errors.New("test failure"), "test", "optional_auth")

	events := transport.Events()
	if len(events) != 1 {
		t.Fatalf("captured events = %d, want 1", len(events))
	}
	if got := events[0].User.ID; got != "42" {
		t.Fatalf("event user ID = %q, want 42", got)
	}
}

func TestRequestLoggingDoesNotReadRequestBody(t *testing.T) {
	hub, transport := newSentryTestHub(t)
	handler := buildHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), logModeErrorOnly, true)
	req := httptest.NewRequest(http.MethodPost, "/api/tracks", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Body = errorReadCloser{err: io.ErrUnexpectedEOF}
	req = req.WithContext(sentry.SetHubOnContext(req.Context(), hub))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	events := transport.Events()
	if len(events) != 0 {
		t.Fatalf("captured events = %d, want 0", len(events))
	}
}

func TestResolveSentryRelease(t *testing.T) {
	buildInfo := &debug.BuildInfo{
		Settings: []debug.BuildSetting{
			{Key: "vcs", Value: "git"},
			{Key: "vcs.revision", Value: "0123456789abcdef"},
		},
	}

	tests := []struct {
		name       string
		configured string
		buildInfo  *debug.BuildInfo
		want       string
	}{
		{
			name:       "configured release takes precedence",
			configured: " custom-release ",
			buildInfo:  buildInfo,
			want:       "custom-release",
		},
		{
			name:      "VCS revision is used by default",
			buildInfo: buildInfo,
			want:      "esketit_music_server@0123456789abcdef",
		},
		{
			name: "missing build information leaves release empty",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := resolveSentryRelease(test.configured, test.buildInfo); got != test.want {
				t.Fatalf("resolveSentryRelease() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRedactSentryRequestURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "shared playlist",
			url:  "/api/shared/playlists/sensitive-token",
			want: "/api/shared/playlists/%5BFiltered%5D",
		},
		{
			name: "shared playlist tracks and query",
			url:  "https://esketitmusic.online/api/shared/playlists/sensitive-token/tracks?page=2",
			want: "/api/shared/playlists/%5BFiltered%5D/tracks",
		},
		{
			name: "media filename and query",
			url:  "/api/songs/private-name.mp3?downloadToken=secret",
			want: "/api/songs/%5BFiltered%5D",
		},
		{
			name: "unrelated URL",
			url:  "/api/playlists/42",
			want: "/api/playlists/42",
		},
		{
			name: "malformed URL still redacts token",
			url:  "/api/shared/playlists/sensitive-token/%zz",
			want: "/api/shared/playlists/[Filtered]/%zz",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := redactSentryRequestURL(test.url); got != test.want {
				t.Fatalf("redactSentryRequestURL() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestScrubSentryEventRemovesRequestSecrets(t *testing.T) {
	event := &sentry.Event{
		Message:         "request failed password=body secret phrase; Authorization: Bearer auth-secret; URL=https://example.com/private?token=query-secret for person@example.com in /srv/private/data.db",
		Transaction:     "POST /api/shared/playlists/secret-token",
		TransactionInfo: &sentry.TransactionInfo{Source: sentry.SourceURL},
		User: sentry.User{
			ID:        "42",
			Email:     "person@example.com",
			IPAddress: "192.0.2.1",
			Data:      map[string]string{"secret": "value"},
		},
		Request: &sentry.Request{
			Method:      http.MethodPost,
			URL:         "https://example.com/api/shared/playlists/secret-token?token=query-secret",
			Data:        `{"password":"body-secret"}`,
			QueryString: "token=query-secret",
			Cookies:     "session=cookie-secret",
			Headers: map[string]string{
				"Authorization":   "Bearer auth-secret",
				"Cookie":          "session=cookie-secret",
				"User-Agent":      "test-agent",
				"Referer":         "https://example.com/?token=referer-secret",
				"X-Forwarded-For": "192.0.2.1",
			},
			Env: map[string]string{"REMOTE_ADDR": "192.0.2.1"},
		},
		Exception: []sentry.Exception{{
			Value: "upstream https://example.com/?credential=secret used cookie=session-secret from /srv/private/cookies.txt",
			Stacktrace: &sentry.Stacktrace{Frames: []sentry.Frame{{
				AbsPath: "/srv/esketit/main.go",
				Vars:    map[string]any{"secret": "value"},
			}}},
		}},
	}

	got := scrubSentryEvent(event, nil)
	if strings.Contains(got.Request.URL, "secret-token") || strings.Contains(got.Request.URL, "query-secret") {
		t.Fatalf("URL was not redacted: %q", got.Request.URL)
	}
	if got.Request.Data != "" || got.Request.QueryString != "" || got.Request.Cookies != "" {
		t.Fatalf("sensitive request fields remain: %#v", got.Request)
	}
	if len(got.Request.Headers) != 0 || len(got.Request.Env) != 0 {
		t.Fatalf("request metadata was not removed: headers=%#v env=%#v", got.Request.Headers, got.Request.Env)
	}
	if got.Transaction != "POST unmatched route" {
		t.Fatalf("transaction = %q, want safe unmatched route", got.Transaction)
	}
	for _, secret := range []string{"body secret phrase", "auth-secret", "query-secret", "person@example.com", "/srv/private", "session-secret", "credential=secret"} {
		if strings.Contains(got.Message, secret) || strings.Contains(got.Exception[0].Value, secret) {
			t.Fatalf("event text still contains %q: message=%q exception=%q", secret, got.Message, got.Exception[0].Value)
		}
	}
	if got.User.ID != "42" || got.User.Email != "" || got.User.IPAddress != "" || got.User.Data != nil {
		t.Fatalf("user context was not reduced to pseudonymous ID: %#v", got.User)
	}
	frame := got.Exception[0].Stacktrace.Frames[0]
	if frame.AbsPath != "" || frame.Filename != "main.go" || frame.Vars != nil {
		t.Fatalf("stack frame was not scrubbed: %#v", frame)
	}
}

func TestSanitizeSentryCapturedErrorRemovesFilesystemPath(t *testing.T) {
	err := fmt.Errorf("store media in private directory: %w", &os.PathError{
		Op:   "open",
		Path: "/srv/private/media/secret.mp3",
		Err:  os.ErrPermission,
	})

	got := sanitizeSentryCapturedError(err).Error()
	if strings.Contains(got, "/srv/private") || strings.Contains(got, "secret.mp3") {
		t.Fatalf("sanitized error contains private path: %q", got)
	}
	if !strings.Contains(got, "permission denied") {
		t.Fatalf("sanitized error lost root cause: %q", got)
	}
}

func TestSafeOperationalErrorRedactsUnknownProviderText(t *testing.T) {
	raw := errors.New("provider failed at https://example.com/private?token=url-secret password=my secret; contact person@example.com in /srv/private/data.db")
	got := safeOperationalError(raw)
	for _, secret := range []string{"url-secret", "my secret", "person@example.com", "/srv/private"} {
		if strings.Contains(got, secret) {
			t.Fatalf("safeOperationalError() leaked %q: %q", secret, got)
		}
	}
}

func TestSentryTracesSampleRate(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    float64
		wantErr bool
	}{
		{name: "empty disables tracing"},
		{name: "zero", raw: "0"},
		{name: "low production sample", raw: " 0.05 ", want: 0.05},
		{name: "all", raw: "1", want: 1},
		{name: "negative", raw: "-0.1", wantErr: true},
		{name: "above one", raw: "1.1", wantErr: true},
		{name: "not a number", raw: "nope", wantErr: true},
		{name: "NaN", raw: "NaN", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseSentryTracesSampleRate(test.raw)
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("rate = %g, want %g", got, test.want)
			}
		})
	}
}

func TestSensitiveRequestAndResponseBodiesAreNotLogged(t *testing.T) {
	var logs bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	previousPrefix := log.Prefix()
	log.SetOutput(&logs)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
		log.SetPrefix(previousPrefix)
	})

	handler := buildHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"accessToken":"response-secret"}`)
	}), logModeVerbose, false)
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/auth/login",
		strings.NewReader(`{"email":"person@example.com","password":"request-secret"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	logText := logs.String()
	for _, secret := range []string{"person@example.com", "request-secret", "response-secret"} {
		if strings.Contains(logText, secret) {
			t.Fatalf("logs contain secret %q: %s", secret, logText)
		}
	}
	if !strings.Contains(logText, "[body omitted for privacy]") {
		t.Fatalf("logs do not explain omission: %s", logText)
	}
}

func TestRedactRequestTargetRemovesSharedTokenAndQuery(t *testing.T) {
	got := redactRequestTarget("/api/shared/playlists/secret-token/tracks?query=private-search")
	if strings.Contains(got, "secret-token") || strings.Contains(got, "private-search") {
		t.Fatalf("redacted request target still contains private data: %q", got)
	}
	if !strings.Contains(got, "%5BFiltered%5D") {
		t.Fatalf("redacted request target = %q, want filtered marker", got)
	}
}
