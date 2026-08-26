package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"
	"modernc.org/sqlite"
)

const (
	sentryFlushTimeout               = 2 * time.Second
	insufficientStoragePublicMessage = "server does not have enough storage space to complete the request"
	sqliteFullResultCode             = 13
)

var (
	sentryURLTextPattern         = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)
	sentryUnixPathTextPattern    = regexp.MustCompile(`(^|[\s("'=:])(/[^\s"'<>]+)`)
	sentryWindowsPathTextPattern = regexp.MustCompile(`(?i)(^|[\s("'=:])([a-z]:\\[^\s"'<>]+)`)
	sentrySecretTextPattern      = regexp.MustCompile(`(?i)\b(authorization|password|passwd|token|secret|cookie|code|phone(?:number)?)\s*[:=]\s*[^\r\n,;]+`)
	sentryEmailTextPattern       = regexp.MustCompile(`(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`)
)

type sentryRequestCaptureStateKey struct{}

type sentryRequestCaptureState struct {
	handled atomic.Bool
}

type sentryHTTPStatusError struct {
	method string
	route  string
	status int
}

func (e sentryHTTPStatusError) Error() string {
	return fmt.Sprintf("unreported HTTP server error: %s %s returned %d", e.method, e.route, e.status)
}

func initializeSentry() (bool, error) {
	dsn := strings.TrimSpace(os.Getenv("SENTRY_DSN"))
	if dsn == "" {
		log.Print("Sentry error reporting disabled: SENTRY_DSN is not set")
		return false, nil
	}

	environment := strings.TrimSpace(os.Getenv("SENTRY_ENVIRONMENT"))
	release := currentSentryRelease()
	tracesSampleRate, err := parseSentryTracesSampleRate(os.Getenv("SENTRY_TRACES_SAMPLE_RATE"))
	if err != nil {
		return false, err
	}
	if err := sentry.Init(sentry.ClientOptions{
		Dsn:              dsn,
		Environment:      environment,
		Release:          release,
		EnableTracing:    tracesSampleRate > 0,
		TracesSampleRate: tracesSampleRate,
		AttachStacktrace: true,
		SendDefaultPII:   false,
		ServerName:       "esketit-music-backend",
		DataCollection: &sentry.DataCollection{
			UserInfo: sentry.Set(false),
			Cookies: &sentry.KeyValueCollectionBehavior{
				Mode: sentry.CollectionOff,
			},
			HTTPBodies: []sentry.BodyType{},
			HTTPHeaders: &sentry.HeaderCollectionConfig{
				Request:  &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionOff},
				Response: &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionOff},
			},
			QueryParams: &sentry.KeyValueCollectionBehavior{
				Mode: sentry.CollectionOff,
			},
		},
		BeforeSend:            scrubSentryEvent,
		BeforeSendTransaction: scrubSentryEvent,
	}); err != nil {
		return false, fmt.Errorf("initialize Sentry SDK: %w", err)
	}

	log.Printf(
		"Sentry error reporting enabled environment=%q release=%q traces_sample_rate=%g",
		environment,
		release,
		tracesSampleRate,
	)
	return true, nil
}

func parseSentryTracesSampleRate(raw string) (float64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}

	rate, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 || rate > 1 {
		return 0, fmt.Errorf("SENTRY_TRACES_SAMPLE_RATE must be a number between 0 and 1")
	}
	return rate, nil
}

func scrubSentryEvent(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	event.Message = redactSentryText(event.Message)
	event.User.Email = ""
	event.User.Username = ""
	event.User.IPAddress = ""
	event.User.Name = ""
	event.User.Data = nil
	if event.Request != nil {
		event.Request.URL = redactSentryRequestURL(event.Request.URL)
		// Request bodies, query strings, and cookies can contain passwords,
		// tokens, Telegram credentials, YouTube cookies, or listening metadata.
		// They are intentionally excluded even if a future SDK configuration
		// enables broader automatic data collection.
		event.Request.Data = ""
		event.Request.QueryString = ""
		event.Request.Cookies = ""
		event.Request.Headers = nil
		event.Request.Env = nil
	}
	if event.TransactionInfo != nil && event.TransactionInfo.Source == sentry.SourceURL {
		method := "HTTP"
		if event.Request != nil && strings.TrimSpace(event.Request.Method) != "" {
			method = event.Request.Method
		} else if candidate, _, ok := strings.Cut(event.Transaction, " "); ok && strings.TrimSpace(candidate) != "" {
			method = candidate
		}
		event.Transaction = method + " unmatched route"
		if event.Request != nil {
			event.Request.URL = "/[unmatched-route]"
		}
	}
	for index := range event.Exception {
		event.Exception[index].Value = redactSentryText(event.Exception[index].Value)
		scrubSentryStacktrace(event.Exception[index].Stacktrace)
	}
	for index := range event.Threads {
		scrubSentryStacktrace(event.Threads[index].Stacktrace)
	}
	return event
}

func redactSentryText(value string) string {
	if value == "" {
		return ""
	}
	value = sentryURLTextPattern.ReplaceAllString(value, "[Filtered URL]")
	value = sentryWindowsPathTextPattern.ReplaceAllString(value, "${1}[Filtered path]")
	value = sentryUnixPathTextPattern.ReplaceAllString(value, "${1}[Filtered path]")
	value = sentrySecretTextPattern.ReplaceAllString(value, "${1}=[Filtered]")
	return sentryEmailTextPattern.ReplaceAllString(value, "[Filtered email]")
}

func safeOperationalError(err error) string {
	if err == nil {
		return "none"
	}
	return redactSentryText(sanitizeSentryCapturedError(err).Error())
}

func scrubSentryStacktrace(stacktrace *sentry.Stacktrace) {
	if stacktrace == nil {
		return
	}
	for index := range stacktrace.Frames {
		frame := &stacktrace.Frames[index]
		if frame.AbsPath != "" && frame.Filename == "" {
			frame.Filename = filepath.Base(frame.AbsPath)
		}
		frame.AbsPath = ""
		frame.Vars = nil
	}
}

func redactSentryRequestURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		redacted := redactSensitiveRequestPath(rawURL)
		if queryIndex := strings.IndexAny(redacted, "?#"); queryIndex >= 0 {
			redacted = redacted[:queryIndex]
		}
		return redacted
	}

	parsed.Path = redactSensitiveRequestPath(parsed.Path)
	parsed.Scheme = ""
	parsed.Host = ""
	parsed.User = nil
	parsed.Opaque = ""
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String()
}

func redactSensitiveRequestPath(value string) string {
	for _, prefix := range []string{
		"/api/shared/playlists/",
		"/api/songs/",
		"/api/album-covers/",
		"/api/author-photos/",
	} {
		value = redactPathSegment(value, prefix)
	}
	return value
}

func redactPathSegment(value, prefix string) string {
	index := strings.Index(value, prefix)
	if index < 0 {
		return value
	}
	segmentStart := index + len(prefix)
	segmentEnd := len(value)
	for _, delimiter := range []byte{'/', '?', '#'} {
		if delimiterIndex := strings.IndexByte(value[segmentStart:], delimiter); delimiterIndex >= 0 && segmentStart+delimiterIndex < segmentEnd {
			segmentEnd = segmentStart + delimiterIndex
		}
	}
	if segmentEnd == segmentStart {
		return value
	}
	return value[:segmentStart] + "[Filtered]" + value[segmentEnd:]
}

func redactSharedPlaylistToken(value string) string {
	return redactPathSegment(value, "/api/shared/playlists/")
}

func reportAndFlushSentry(runErr error) {
	if unreportedErr := removeReportedSentryError(runErr, errHTTPServerPanic); unreportedErr != nil {
		captureSentryError(context.Background(), unreportedErr, "server", "lifecycle")
	}
	flushSentry()
}

func removeReportedSentryError(err, reported error) error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		remaining := make([]error, 0, len(causes))
		for _, cause := range causes {
			if filtered := removeReportedSentryError(cause, reported); filtered != nil {
				remaining = append(remaining, filtered)
			}
		}
		return errors.Join(remaining...)
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		cause := wrapped.Unwrap()
		filtered := removeReportedSentryError(cause, reported)
		if filtered == nil {
			return nil
		}
		if errors.Is(cause, reported) {
			return filtered
		}
		return err
	}
	if errors.Is(err, reported) {
		return nil
	}
	return err
}

func reportPanicAndFlushSentry(recovered any) {
	sentry.CurrentHub().Recover(recovered)
	flushSentry()
}

func captureSentryPanic(ctx context.Context, recovered any) {
	hub := sentry.GetHubFromContext(ctx)
	if hub == nil && sentryCaptureStateFromContext(ctx) == nil {
		hub = sentry.CurrentHub()
	}
	if hub != nil && hub.Client() != nil {
		hub.Recover(recovered)
		markSentryErrorHandled(ctx)
	}
}

func flushSentry() {
	if !sentry.Flush(sentryFlushTimeout) {
		log.Printf("Sentry event delivery did not complete within %s", sentryFlushTimeout)
	}
}

func withSentry(next http.Handler) http.Handler {
	return sentryhttp.New(sentryhttp.Options{
		Repanic: true,
	}).Handle(next)
}

func setSentryUser(ctx context.Context, userID int64) {
	if hub := sentry.GetHubFromContext(ctx); hub != nil {
		hub.Scope().SetUser(sentry.User{ID: strconv.FormatInt(userID, 10)})
	}
}

func withSentryRequestCaptureState(r *http.Request) *http.Request {
	if sentryCaptureStateFromContext(r.Context()) != nil {
		return r
	}
	state := &sentryRequestCaptureState{}
	return r.WithContext(context.WithValue(r.Context(), sentryRequestCaptureStateKey{}, state))
}

func withSentryRequestState(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, withSentryRequestCaptureState(r))
	})
}

func sentryCaptureStateFromContext(ctx context.Context) *sentryRequestCaptureState {
	state, _ := ctx.Value(sentryRequestCaptureStateKey{}).(*sentryRequestCaptureState)
	return state
}

func markSentryErrorHandled(ctx context.Context) {
	if state := sentryCaptureStateFromContext(ctx); state != nil {
		state.handled.Store(true)
	}
}

func claimSentryError(ctx context.Context) bool {
	state := sentryCaptureStateFromContext(ctx)
	return state == nil || state.handled.CompareAndSwap(false, true)
}

func captureSentryError(ctx context.Context, err error, component, operation string) {
	captureSentryErrorWithTags(ctx, err, map[string]string{
		"component": component,
		"operation": operation,
	})
}

func captureSentryErrorWithTags(ctx context.Context, err error, tags map[string]string) {
	if err == nil || !claimSentryError(ctx) {
		return
	}
	if isCancellationOnly(err) {
		return
	}

	hub := sentry.GetHubFromContext(ctx)
	if hub == nil && sentryCaptureStateFromContext(ctx) == nil {
		hub = sentry.CurrentHub()
	}
	if hub == nil || hub.Client() == nil {
		return
	}

	hub.WithScope(func(scope *sentry.Scope) {
		for key, value := range tags {
			if value != "" {
				scope.SetTag(key, value)
			}
		}
		hub.CaptureException(sanitizeSentryCapturedError(err))
	})
}

func sanitizeSentryCapturedError(err error) error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		sanitized := make([]error, 0, len(causes))
		for _, cause := range causes {
			if cause != nil {
				sanitized = append(sanitized, sanitizeSentryCapturedError(cause))
			}
		}
		return errors.Join(sanitized...)
	}

	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return fmt.Errorf("filesystem %s failed: %w", pathErr.Op, sanitizeSentryCapturedError(pathErr.Err))
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return fmt.Errorf("filesystem %s failed: %w", linkErr.Op, sanitizeSentryCapturedError(linkErr.Err))
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("network %s failed: %w", urlErr.Op, sanitizeSentryCapturedError(urlErr.Err))
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		cause := errors.New("DNS lookup failed")
		if dnsErr.IsTimeout {
			cause = context.DeadlineExceeded
		}
		return cause
	}
	var networkErr *net.OpError
	if errors.As(err, &networkErr) {
		return fmt.Errorf("network %s failed: %w", networkErr.Op, sanitizeSentryCapturedError(networkErr.Err))
	}
	return err
}

func isCancellationOnly(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !isCancellationOnly(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return isCancellationOnly(wrapped.Unwrap())
	}
	return errors.Is(err, context.Canceled)
}

func writeSentryHTTPError(
	w http.ResponseWriter,
	r *http.Request,
	err error,
	publicMessage string,
	status int,
	component string,
	operation string,
) {
	tags := map[string]string{
		"component":        component,
		"operation":        operation,
		"http.status_code": strconv.Itoa(status),
	}
	if isInsufficientStorageError(err) {
		status = http.StatusInsufficientStorage
		publicMessage = insufficientStoragePublicMessage
		tags["http.status_code"] = strconv.Itoa(status)
		tags["error.kind"] = "insufficient_storage"
	}
	if status >= http.StatusInternalServerError {
		captureSentryErrorWithTags(r.Context(), err, tags)
	}
	if publicMessage == "" {
		publicMessage = http.StatusText(status)
	}
	http.Error(w, publicMessage, status)
}

func isInsufficientStorageError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return true
	}

	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqliteFullResultCode
}

func writeSentryInternalError(
	w http.ResponseWriter,
	r *http.Request,
	err error,
	publicMessage string,
	component string,
	operation string,
) {
	writeSentryHTTPError(
		w,
		r,
		err,
		publicMessage,
		http.StatusInternalServerError,
		component,
		operation,
	)
}

func captureUnhandledHTTPStatus(r *http.Request, status int) {
	if status < http.StatusInternalServerError {
		return
	}
	route := strings.TrimSpace(r.Pattern)
	if route == "" {
		route = "unmatched route"
	}
	captureSentryErrorWithTags(r.Context(), sentryHTTPStatusError{
		method: r.Method,
		route:  route,
		status: status,
	}, map[string]string{
		"component":        "http",
		"operation":        "unhandled_server_response",
		"http.status_code": strconv.Itoa(status),
		"http.route":       route,
	})
}

func currentSentryRelease() string {
	configured := strings.TrimSpace(os.Getenv("SENTRY_RELEASE"))
	buildInfo, ok := debug.ReadBuildInfo()
	if !ok {
		buildInfo = nil
	}
	return resolveSentryRelease(configured, buildInfo)
}

func resolveSentryRelease(configured string, buildInfo *debug.BuildInfo) string {
	if configured = strings.TrimSpace(configured); configured != "" {
		return configured
	}
	if buildInfo == nil {
		return ""
	}

	for _, setting := range buildInfo.Settings {
		if setting.Key != "vcs.revision" {
			continue
		}
		revision := strings.TrimSpace(setting.Value)
		if revision != "" {
			return "esketit_music_server@" + revision
		}
	}
	return ""
}
