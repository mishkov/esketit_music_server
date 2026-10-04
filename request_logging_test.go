package main

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func captureRequestLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previousWriter, previousFlags, previousPrefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(&logs)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
		log.SetPrefix(previousPrefix)
	})
	return &logs
}

func TestResolveLogBodies(t *testing.T) {
	logs := captureRequestLogs(t)
	for _, test := range []struct {
		value string
		want  bool
	}{
		{value: ""},
		{value: "  "},
		{value: "false"},
		{value: "FALSE"},
		{value: "0"},
		{value: "true", want: true},
		{value: " TRUE ", want: true},
		{value: "1", want: true},
		{value: "invalid"},
	} {
		t.Run(test.value, func(t *testing.T) {
			if got := resolveLogBodies(test.value); got != test.want {
				t.Fatalf("resolveLogBodies(%q) = %t, want %t", test.value, got, test.want)
			}
		})
	}
	if !strings.Contains(logs.String(), "invalid LOG_BODIES value, defaulting to false") {
		t.Fatalf("invalid flag did not log fallback: %s", logs)
	}
}

func TestRequestBodyLoggingPreservesPayloadsAndLogMode(t *testing.T) {
	for _, test := range []struct {
		name    string
		mode    string
		status  int
		wantLog bool
	}{
		{name: "verbose success", mode: logModeVerbose, status: http.StatusOK, wantLog: true},
		{name: "error-only success", mode: logModeErrorOnly, status: http.StatusOK},
		{name: "error-only failure", mode: logModeErrorOnly, status: http.StatusInternalServerError, wantLog: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			logs := captureRequestLogs(t)
			const requestBody = `{"title":"debug request"}`
			const responseBody = `{"title":"debug response"}`
			handler := buildHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != requestBody {
					t.Fatalf("handler received body=%q, err=%v", body, err)
				}
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, responseBody)
			}), test.mode, false, true)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/tracks", strings.NewReader(requestBody)))
			if recorder.Code != test.status || recorder.Body.String() != responseBody {
				t.Fatalf("response status=%d body=%q", recorder.Code, recorder.Body.String())
			}
			if !test.wantLog {
				if logs.Len() != 0 {
					t.Fatalf("successful request was logged in error-only mode: %s", logs)
				}
				return
			}
			for _, field := range []string{
				"request_body=" + strconv.Quote(requestBody),
				"response_body=" + strconv.Quote(responseBody),
			} {
				if !strings.Contains(logs.String(), field) {
					t.Fatalf("logs missing %s: %s", field, logs)
				}
			}
		})
	}
}

func TestRequestBodyLoggingBoundsCaptureWithoutTruncatingTraffic(t *testing.T) {
	logs := captureRequestLogs(t)
	payload := strings.Repeat("a", maxLoggedBodySize) + "beyond-capture-limit"
	handler := buildHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(w, r.Body); err != nil {
			t.Fatal(err)
		}
	}), logModeVerbose, false, true)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/tracks", strings.NewReader(payload)))
	if recorder.Body.String() != payload {
		t.Fatal("body logging truncated the response payload")
	}
	if strings.Contains(logs.String(), "beyond-capture-limit") {
		t.Fatal("logs contain bytes beyond the capture limit")
	}
	if got := strings.Count(logs.String(), " [truncated]"); got != 2 {
		t.Fatalf("truncation markers = %d, want 2", got)
	}
}

func TestEnabledBodyLoggingDoesNotReadIgnoredBody(t *testing.T) {
	logs := captureRequestLogs(t)
	body := &trackedLoggingBody{}
	request := httptest.NewRequest(http.MethodPost, "/api/tracks", nil)
	request.Body = body
	handler := buildHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.Body.Close(); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}), logModeVerbose, false, true)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if body.read || !body.closed {
		t.Fatalf("body read=%t closed=%t, want false/true", body.read, body.closed)
	}
	if recorder.Code != http.StatusNoContent || !strings.Contains(logs.String(), `request_body=""`) {
		t.Fatalf("status=%d logs=%s", recorder.Code, logs)
	}
}

func TestLoggingRequestBodyPreservesReadError(t *testing.T) {
	wantErr := io.ErrUnexpectedEOF
	body := &loggingRequestBody{ReadCloser: errorReadCloser{err: wantErr}, body: &loggedBody{}}
	if _, err := body.Read(make([]byte, 8)); !errors.Is(err, wantErr) {
		t.Fatalf("read error = %v, want %v", err, wantErr)
	}
}

type trackedLoggingBody struct {
	read   bool
	closed bool
}

func (b *trackedLoggingBody) Read([]byte) (int, error) {
	b.read = true
	return 0, io.ErrUnexpectedEOF
}

func (b *trackedLoggingBody) Close() error {
	b.closed = true
	return nil
}
