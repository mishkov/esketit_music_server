package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newMCPTestService(t *testing.T) (*mcpService, user) {
	t.Helper()
	store := newTestTrackStore(t)
	t.Cleanup(func() { _ = store.db.Close() })
	store.songsDir = t.TempDir()
	_ = mustCreateCatalogTestUser(t, store, "bootstrap-admin@example.com")
	requester := mustCreateCatalogTestUser(t, store, "mcp@example.com")
	svc := &mcpService{store: store, authorPhotosDir: t.TempDir(), albumCoversDir: t.TempDir(), accessToken: strings.Repeat("x", 32)}
	err := store.unitOfWork.WithinTransaction(context.Background(), func(repos domainRepositories) error {
		settings, err := repos.mcp.GetMCPSettings(context.Background())
		if err != nil {
			return err
		}
		settings.ActingUserID = &requester.ID
		return repos.mcp.PatchMCPSettings(context.Background(), settings, settings.Version)
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, requester
}
func callMCP(t *testing.T, s *mcpService, name string, args map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(args)
	write := strings.HasPrefix(name, "submit_") || strings.HasPrefix(name, "update_") || name == "resubmit_submission" || name == "cancel_submission"
	result, err := s.call(context.Background(), name, raw, write)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	encoded, _ := json.Marshal(result)
	var m map[string]any
	_ = json.Unmarshal(encoded, &m)
	return m
}
func mcpID(v map[string]any, key string) int64 { return int64(v[key].(float64)) }
func mcpUploadRequest(t *testing.T, s *mcpService, kind, key, filename string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("kind", kind)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(data)
	_ = writer.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/uploads", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+s.accessToken)
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	s.authenticate(s.uploadHandler()).ServeHTTP(rec, req)
	return rec
}
func uploadMCP(t *testing.T, s *mcpService, kind, key, name string, data []byte) map[string]any {
	t.Helper()
	rec := mcpUploadRequest(t, s, kind, key, name, data)
	if rec.Code != 200 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	var result struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &result)
	return result.Data
}

func TestMCPProtocolAndAuthorization(t *testing.T) {
	s, _ := newMCPTestService(t)
	handler := s.handler()
	invoke := func(method string, params any, credential string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		req := httptest.NewRequest(http.MethodPost, "http://localhost/mcp", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+credential)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2025-11-25")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := invoke("tools/list", map[string]any{}, "wrong"); rec.Code != 401 {
		t.Fatalf("unauthorized=%d", rec.Code)
	}
	rec := invoke("initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1"}}, s.accessToken)
	if rec.Code != 200 {
		t.Fatalf("initialize: %d %s", rec.Code, rec.Body.String())
	}
	rec = invoke("tools/list", map[string]any{}, s.accessToken)
	var listing struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
		t.Fatalf("list: %s", rec.Body.String())
	}
	if len(listing.Result.Tools) != 14 {
		t.Fatalf("tools=%d %s", len(listing.Result.Tools), rec.Body.String())
	}
	for _, tool := range listing.Result.Tools {
		if _, ok := tool.InputSchema["required"].([]any); !ok {
			t.Fatalf("invalid required schema for %s", tool.Name)
		}
	}
	rec = invoke("tools/call", map[string]any{"name": "get_context", "arguments": map[string]any{}}, s.accessToken)
	if !strings.Contains(rec.Body.String(), "configurationVersion") || !strings.Contains(rec.Body.String(), "workflowGuidance") {
		t.Fatalf("context: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), s.accessToken) {
		t.Fatal("credential leaked")
	}
	rec = invoke("tools/call", map[string]any{"name": "submit_author", "arguments": map[string]any{"requestId": "bad", "currentName": "Name", "actingUserId": 999}}, s.accessToken)
	if !strings.Contains(rec.Body.String(), `"isError":true`) || !strings.Contains(rec.Body.String(), "invalid_input") {
		t.Fatalf("unknown field accepted: %s", rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodPost, "http://localhost/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+s.accessToken)
	req.Header.Set("Origin", "https://untrusted.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("cross-origin=%d %s", rec.Code, rec.Body.String())
	}
}

func TestMCPFeedbackPatchAndRetries(t *testing.T) {
	s, requester := newMCPTestService(t)
	photo := uploadMCP(t, s, "author_photo", "photo-1", "photo.png", catalogTestPNG(t, 2, 2))
	args := map[string]any{"requestId": "author-1", "currentName": "Original", "photoUploadTokens": []string{photo["uploadToken"].(string)}}
	created := callMCP(t, s, "submit_author", args)
	replayed := callMCP(t, s, "submit_author", args)
	if !reflect.DeepEqual(created, replayed) {
		t.Fatal("retry changed original result")
	}
	raw, _ := json.Marshal(map[string]any{"requestId": "author-1", "currentName": "Different"})
	if _, err := s.call(context.Background(), "submit_author", raw, true); !errors.Is(err, errMCPRequestConflict) {
		t.Fatalf("conflict=%v", err)
	}
	reviewer := mustCreateCatalogTestUser(t, s.store, "reviewer@example.com")
	lease, err := s.store.acquireCatalogReviewLease(reviewer.ID, requester.ID)
	if err != nil {
		t.Fatal(err)
	}
	id := mcpID(created, "submissionId")
	if _, err := s.store.requestCatalogSubmissionChanges(reviewer.ID, id, lease.LeaseToken, catalogReviewDecisionRequest{Message: "Correct the spelling"}); err != nil {
		t.Fatal(err)
	}
	details := callMCP(t, s, "get_submission", map[string]any{"submissionId": id})
	if len(details["feedback"].([]any)) != 1 || details["status"] != "changes_requested" {
		t.Fatalf("feedback=%v", details)
	}
	raw, _ = json.Marshal(map[string]any{"requestId": "stale", "submissionId": id, "expectedRevision": 1, "currentName": "Corrected"})
	if _, err := s.call(context.Background(), "update_author_submission", raw, true); !errors.Is(err, errMCPStaleRevision) {
		t.Fatalf("stale=%v", err)
	}
	updated := callMCP(t, s, "update_author_submission", map[string]any{"requestId": "correct-1", "submissionId": id, "expectedRevision": mcpID(details, "revision"), "currentName": "Corrected"})
	entity := updated["entity"].(map[string]any)
	if entity["currentName"] != "Corrected" || len(entity["photos"].([]any)) != 1 {
		t.Fatalf("patch lost photos: %v", updated)
	}
	cleared := callMCP(t, s, "update_author_submission", map[string]any{"requestId": "clear-photos", "submissionId": id, "expectedRevision": mcpID(updated, "revision"), "photoUploadTokens": []string{}})
	if len(cleared["entity"].(map[string]any)["photos"].([]any)) != 0 {
		t.Fatal("photos not cleared")
	}
	resubmitted := callMCP(t, s, "resubmit_submission", map[string]any{"requestId": "resubmit-1", "submissionId": id, "expectedRevision": mcpID(cleared, "revision")})
	if resubmitted["status"] != "pending_review" {
		t.Fatalf("resubmit=%v", resubmitted)
	}
	list := callMCP(t, s, "list_submissions", map[string]any{"status": "pending_review", "entityType": "author", "page": 1, "pageSize": 1, "updatedSince": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)})
	if list["totalItems"].(float64) != 1 {
		t.Fatalf("list=%v", list)
	}
	search := callMCP(t, s, "search_catalog", map[string]any{"query": "correct", "entityType": "author"})
	if search["totalItems"].(float64) != 1 {
		t.Fatalf("search=%v", search)
	}
	if _, err := s.store.db.Exec(`UPDATE mcp_settings SET acting_user_id=NULL,version=version+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.call(context.Background(), "get_context", json.RawMessage(`{}`), false); !errors.Is(err, errMCPDisabled) {
		t.Fatalf("disabled=%v", err)
	}
}

func TestMCPAudioAlbumAndApproval(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg required for real MP3 validation")
	}
	s, requester := newMCPTestService(t)
	mp3, err := os.ReadFile("testdata/mcp-silence.mp3")
	if err != nil {
		t.Fatal(err)
	}
	audio := uploadMCP(t, s, "audio", "audio-1", "recording.mp3", mp3)
	retry := uploadMCP(t, s, "audio", "audio-1", "recording.mp3", mp3)
	if audio["uploadToken"] != retry["uploadToken"] {
		t.Fatal("upload retry created new token")
	}
	files, err := os.ReadDir(s.store.catalogSubmissionAudioDir())
	if err != nil || len(files) != 1 {
		t.Fatalf("staging files=%d err=%v", len(files), err)
	}
	changed := append([]byte{}, mp3...)
	if pos := bytes.Index(changed, []byte("Lavf")); pos >= 0 {
		changed[pos] = 'X'
	} else {
		t.Fatal("fixture has no encoder tag")
	}
	if rec := mcpUploadRequest(t, s, "audio", "audio-1", "recording.mp3", changed); rec.Code != 409 {
		t.Fatalf("different upload: %d %s", rec.Code, rec.Body.String())
	}
	author := callMCP(t, s, "submit_author", map[string]any{"requestId": "author", "currentName": "Artist"})
	cover := uploadMCP(t, s, "album_cover", "cover-1", "cover.png", catalogTestPNG(t, 2, 2))
	album := callMCP(t, s, "submit_album", map[string]any{"requestId": "album", "title": "Album", "releaseDate": "2024-01-01T00:00:00Z", "authorIds": []int64{mcpID(author, "entityId")}, "coverUploadToken": cover["uploadToken"], "additionalInfo": []any{map[string]any{"type": "text", "title": "Note", "text": "Keep me"}}})
	track := callMCP(t, s, "submit_track", map[string]any{"requestId": "track", "name": "Recording", "authorIds": []int64{mcpID(author, "entityId")}, "albumId": mcpID(album, "entityId"), "audioUploadToken": audio["uploadToken"], "sourceMetadata": []any{map[string]any{"provider": "website", "identity": map[string]any{"url": "https://example.com/recording"}, "url": "https://example.com/recording"}}})
	upload := callMCP(t, s, "get_upload", map[string]any{"uploadToken": audio["uploadToken"]})
	if int64(upload["claimedSubmissionId"].(float64)) != mcpID(track, "submissionId") {
		t.Fatalf("claim=%v", upload)
	}
	reviewer := mustCreateCatalogTestUser(t, s.store, "reviewer@example.com")
	lease, err := s.store.acquireCatalogReviewLease(reviewer.ID, requester.ID)
	if err != nil {
		t.Fatal(err)
	}
	feedback, err := s.store.requestCatalogSubmissionChanges(reviewer.ID, mcpID(album, "submissionId"), lease.LeaseToken, catalogReviewDecisionRequest{Message: "Remove cover"})
	if err != nil {
		t.Fatal(err)
	}
	patched := callMCP(t, s, "update_album_submission", map[string]any{"requestId": "clear-cover", "submissionId": feedback.ID, "expectedRevision": feedback.Revision, "coverUploadToken": nil})
	if patched["entity"].(map[string]any)["coverImagePath"] != "" || len(patched["entity"].(map[string]any)["additionalInfo"].([]any)) != 1 {
		t.Fatalf("album patch=%v", patched)
	}
	callMCP(t, s, "resubmit_submission", map[string]any{"requestId": "album-resubmit", "submissionId": feedback.ID, "expectedRevision": mcpID(patched, "revision")})
	for _, id := range []int64{mcpID(author, "submissionId"), feedback.ID, mcpID(track, "submissionId")} {
		if _, err := s.store.approveCatalogSubmission(reviewer.ID, id, lease.LeaseToken); err != nil {
			t.Fatal(err)
		}
	}
	final := callMCP(t, s, "get_submission", map[string]any{"submissionId": mcpID(track, "submissionId")})
	if final["status"] != "approved" {
		t.Fatalf("final=%v", final)
	}
	path := final["entity"].(map[string]any)["audioFilePath"].(string)
	if _, err := os.Stat(filepath.Join(s.store.songsDir, filepath.Base(path))); err != nil {
		t.Fatal("approved audio missing", err)
	}
}

func TestMCPUploadValidationAndPermissions(t *testing.T) {
	s, user := newMCPTestService(t)
	for _, kind := range []string{"audio", "author_photo", "album_cover"} {
		rec := mcpUploadRequest(t, s, kind, "invalid-"+kind, "fake.mp3", []byte("not a media file"))
		if rec.Code != 400 {
			t.Fatalf("invalid %s=%d %s", kind, rec.Code, rec.Body.String())
		}
	}
	other := mustCreateCatalogTestUser(t, s.store, "other@example.com")
	foreign, err := s.store.createAuthorSubmission(other.ID, upsertAuthorRequest{CurrentName: "Private"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"submissionId": foreign.ID})
	if _, err := s.call(context.Background(), "get_submission", raw, false); !errors.Is(err, errCatalogSubmissionForbidden) {
		t.Fatalf("foreign=%v", err)
	}
	if _, err := s.store.replaceUserRoles(user.ID, user.ID, []int64{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.call(context.Background(), "submit_author", json.RawMessage(`{"requestId":"denied","currentName":"Name"}`), true); !errors.Is(err, errMCPForbidden) {
		t.Fatalf("permission=%v", err)
	}
	contextData := callMCP(t, s, "get_context", map[string]any{})
	if len(contextData["permissions"].([]any)) != 0 {
		t.Fatalf("revoked permissions not reflected: %v", contextData)
	}
}

func TestMCPConcurrentRetryAndDurability(t *testing.T) {
	s, _ := newMCPTestService(t)
	raw := json.RawMessage(`{"requestId":"parallel","currentName":"One artist"}`)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.call(context.Background(), "submit_author", raw, true)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM catalog_submissions`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d %v", count, err)
	}
	var databasePath string
	var sequence int
	var name string
	if err := s.store.db.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &databasePath); err != nil {
		t.Fatal(err)
	}
	if err := s.store.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := newTrackStore(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	s.store = reopened
	retry := callMCP(t, s, "submit_author", map[string]any{"requestId": "parallel", "currentName": "One artist"})
	if mcpID(retry, "submissionId") != 1 {
		t.Fatalf("durable receipt=%v", retry)
	}
}

type failedMCPReceiptRepository struct{ MCPRepository }

func (r failedMCPReceiptRepository) InsertMCPRequest(context.Context, mcpRequestReceipt) error {
	return errRepositoryPersistence
}

type failingMCPUnitOfWork struct{ unitOfWork }

func (u failingMCPUnitOfWork) WithinTransaction(ctx context.Context, f func(domainRepositories) error) error {
	return u.unitOfWork.WithinTransaction(ctx, func(repos domainRepositories) error {
		repos.mcp = failedMCPReceiptRepository{repos.mcp}
		return f(repos)
	})
}
func TestMCPReceiptFailureRollsBackSubmission(t *testing.T) {
	s, _ := newMCPTestService(t)
	original := s.store.unitOfWork
	s.store.unitOfWork = failingMCPUnitOfWork{original}
	if _, err := s.call(context.Background(), "submit_author", json.RawMessage(`{"requestId":"fail","currentName":"Rollback"}`), true); !errors.Is(err, errRepositoryPersistence) {
		t.Fatalf("failure=%v", err)
	}
	s.store.unitOfWork = original
	var count int
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM authors`).Scan(&count)
	if count != 0 {
		t.Fatalf("partial commit: %d", count)
	}
	callMCP(t, s, "submit_author", map[string]any{"requestId": "fail", "currentName": "Rollback"})
}

func TestMCPSettingsVersionAndAudit(t *testing.T) {
	s, requester := newMCPTestService(t)
	admin := mustCreateCatalogTestUser(t, s.store, "admin@example.com")
	setTestUserRole(t, s.store, admin.ID, roleAdmin)
	patch := func(actor int64, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/api/mcp/settings", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, actor))
		rec := httptest.NewRecorder()
		s.settingsHandler().ServeHTTP(rec, req)
		return rec
	}
	if rec := patch(requester.ID, `{"expectedVersion":2,"workflowGuidance":"bad"}`); rec.Code != 403 {
		t.Fatalf("admin-only=%d", rec.Code)
	}
	body := fmt.Sprintf(`{"expectedVersion":2,"actingUserId":%d,"workflowGuidance":"Check source carefully","uploadGuidance":"Use MP3"}`, requester.ID)
	rec := patch(admin.ID, body)
	if rec.Code != 200 {
		t.Fatalf("patch=%d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), s.accessToken) {
		t.Fatal("secret leaked")
	}
	if rec := patch(admin.ID, body); rec.Code != 409 {
		t.Fatalf("stale settings=%d", rec.Code)
	}
	context := callMCP(t, s, "get_context", map[string]any{})
	if context["workflowGuidance"] != "Check source carefully" || context["configurationVersion"].(float64) != 3 {
		t.Fatalf("context settings=%v", context)
	}
	var events int
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM access_control_audit_events WHERE action='mcp.settings.update'`).Scan(&events)
	if events != 1 {
		t.Fatalf("audit=%d", events)
	}
}

func TestMCPReplacementRollbackAndCancellation(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg required for real MP3 validation")
	}
	s, requester := newMCPTestService(t)
	audio, _ := os.ReadFile("testdata/mcp-silence.mp3")
	first := uploadMCP(t, s, "audio", "first-audio", "first.mp3", audio)
	artist := callMCP(t, s, "submit_author", map[string]any{"requestId": "artist", "currentName": "Artist"})
	album := callMCP(t, s, "submit_album", map[string]any{"requestId": "album", "title": "Album", "authorIds": []int64{mcpID(artist, "entityId")}, "releaseDate": "2024-01-01T00:00:00Z"})
	args := map[string]any{"requestId": "track", "name": "Original", "authorIds": []int64{mcpID(artist, "entityId")}, "albumId": mcpID(album, "entityId"), "audioUploadToken": first["uploadToken"]}
	created := callMCP(t, s, "submit_track", args)
	reviewer := mustCreateCatalogTestUser(t, s.store, "reviewer@example.com")
	lease, err := s.store.acquireCatalogReviewLease(reviewer.ID, requester.ID)
	if err != nil {
		t.Fatal(err)
	}
	feedback, err := s.store.requestCatalogSubmissionChanges(reviewer.ID, mcpID(created, "submissionId"), lease.LeaseToken, catalogReviewDecisionRequest{Message: "Replace the recording"})
	if err != nil {
		t.Fatal(err)
	}
	second := uploadMCP(t, s, "audio", "second-audio", "second.mp3", audio)
	patch := map[string]any{"requestId": "replace", "submissionId": feedback.ID, "expectedRevision": feedback.Revision, "audioUploadToken": second["uploadToken"]}
	originalUnit := s.store.unitOfWork
	s.store.unitOfWork = failingMCPUnitOfWork{originalUnit}
	raw, _ := json.Marshal(patch)
	if _, err := s.call(context.Background(), "update_track_submission", raw, true); !errors.Is(err, errRepositoryPersistence) {
		t.Fatalf("receipt failure=%v", err)
	}
	s.store.unitOfWork = originalUnit
	files, _ := os.ReadDir(s.store.catalogSubmissionAudioDir())
	if len(files) != 2 {
		t.Fatalf("rollback removed original media: files=%d", len(files))
	}
	replacement := callMCP(t, s, "update_track_submission", patch)
	entity := replacement["entity"].(map[string]any)
	if entity["name"] != "Original" || mcpID(entity, "albumId") != mcpID(album, "entityId") {
		t.Fatalf("patch discarded metadata: %v", entity)
	}
	files, _ = os.ReadDir(s.store.catalogSubmissionAudioDir())
	if len(files) != 1 {
		t.Fatalf("old audio was not removed after commit: %d", len(files))
	}
	callMCP(t, s, "update_track_submission", patch) // Original receipt survives removal of old upload.
	cancelled := callMCP(t, s, "cancel_submission", map[string]any{"requestId": "cancel", "submissionId": feedback.ID, "expectedRevision": mcpID(replacement, "revision")})
	if cancelled["status"] != "cancelled" {
		t.Fatalf("cancel=%v", cancelled)
	}
	files, _ = os.ReadDir(s.store.catalogSubmissionAudioDir())
	if len(files) != 0 {
		t.Fatalf("cancelled audio retained: %d", len(files))
	}
	tombstone := callMCP(t, s, "get_submission", map[string]any{"submissionId": feedback.ID})
	if tombstone["entity"].(map[string]any)["name"] != "Original" {
		t.Fatalf("missing cancellation snapshot: %v", tombstone)
	}
}

func TestMCPFailedUploadReceiptRemovesFiles(t *testing.T) {
	s, _ := newMCPTestService(t)
	original := s.store.unitOfWork
	s.store.unitOfWork = failingMCPUnitOfWork{original}
	rec := mcpUploadRequest(t, s, "album_cover", "fail-cover", "cover.png", catalogTestPNG(t, 2, 2))
	if rec.Code != 503 {
		t.Fatalf("upload failure=%d %s", rec.Code, rec.Body.String())
	}
	files, _ := os.ReadDir(s.albumCoversDir)
	if len(files) != 0 {
		t.Fatalf("rolled-back image leaked: %d", len(files))
	}
	var count int
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM mcp_uploads`).Scan(&count)
	if count != 0 {
		t.Fatalf("rolled-back upload retained: %d", count)
	}
}

func TestMCPDisabledCredentialAndMissingAlbumDate(t *testing.T) {
	s, _ := newMCPTestService(t)
	s.accessToken = "short"
	handler := s.handler()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "access credential") {
		t.Fatalf("short credential=%d %s", rec.Code, rec.Body.String())
	}
	s.accessToken = strings.Repeat("x", 32)
	handler = s.handler()
	raw := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"submit_album","arguments":{"requestId":"album-no-date","title":"Album","authorIds":[1]}}}`
	req = httptest.NewRequest(http.MethodPost, "http://localhost/mcp", strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+s.accessToken)
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"isError":true`) || !strings.Contains(rec.Body.String(), "releaseDate") {
		t.Fatalf("missing releaseDate accepted: %s", rec.Body.String())
	}
}

type mcpBearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t mcpBearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	cloned := r.Clone(r.Context())
	cloned.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(cloned)
}
func TestMCPOfficialClientInteroperability(t *testing.T) {
	s, _ := newMCPTestService(t)
	server := httptest.NewServer(s.handler())
	defer server.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "agent-integration-test", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: &http.Client{Transport: mcpBearerTransport{s.accessToken, http.DefaultTransport}}, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 14 {
		t.Fatalf("tools=%v err=%v", tools, err)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "submit_author", Arguments: map[string]any{"requestId": "sdk-author", "currentName": "SDK Artist"}})
	if err != nil || result.IsError {
		t.Fatalf("call=%v err=%v", result, err)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		OK   bool           `json:"ok"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK || envelope.Data["status"] != "pending_review" {
		t.Fatalf("result=%s", raw)
	}
	retry, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "submit_author", Arguments: map[string]any{"requestId": "sdk-author", "currentName": "SDK Artist"}})
	if err != nil || retry.IsError {
		t.Fatalf("retry=%v err=%v", retry, err)
	}
	retried, _ := json.Marshal(retry.StructuredContent)
	if !bytes.Equal(raw, retried) {
		t.Fatalf("retry result changed: %s", retried)
	}
}

func TestMCPTrackPatchPreservesAlbumOrder(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg required for real MP3 validation")
	}
	s, requester := newMCPTestService(t)
	audio, _ := os.ReadFile("testdata/mcp-silence.mp3")
	artist := callMCP(t, s, "submit_author", map[string]any{"requestId": "artist", "currentName": "Artist"})
	album := callMCP(t, s, "submit_album", map[string]any{"requestId": "album", "title": "Album", "authorIds": []int64{mcpID(artist, "entityId")}, "releaseDate": "2024-01-01T00:00:00Z"})
	var second map[string]any
	for i := 0; i < 2; i++ {
		upload := uploadMCP(t, s, "audio", fmt.Sprintf("audio-%d", i), "track.mp3", audio)
		result := callMCP(t, s, "submit_track", map[string]any{"requestId": fmt.Sprintf("track-%d", i), "name": fmt.Sprintf("Track %d", i), "authorIds": []int64{mcpID(artist, "entityId")}, "albumId": mcpID(album, "entityId"), "audioUploadToken": upload["uploadToken"]})
		if result["entity"].(map[string]any)["albumOrder"].(float64) != float64(i) {
			t.Fatalf("new track not appended: %v", result)
		}
		second = result
	}
	reviewer := mustCreateCatalogTestUser(t, s.store, "reviewer@example.com")
	lease, err := s.store.acquireCatalogReviewLease(reviewer.ID, requester.ID)
	if err != nil {
		t.Fatal(err)
	}
	feedback, err := s.store.requestCatalogSubmissionChanges(reviewer.ID, mcpID(second, "submissionId"), lease.LeaseToken, catalogReviewDecisionRequest{Message: "Rename only"})
	if err != nil {
		t.Fatal(err)
	}
	result := callMCP(t, s, "update_track_submission", map[string]any{"requestId": "rename", "submissionId": feedback.ID, "expectedRevision": feedback.Revision, "name": "Renamed"})
	if result["entity"].(map[string]any)["albumOrder"].(float64) != 1 {
		t.Fatalf("patch moved track: %v", result)
	}
	visible := callMCP(t, s, "get_catalog_item", map[string]any{"entityType": "track", "entityId": feedback.EntityID})
	if visible["albumOrder"].(float64) != 1 {
		t.Fatalf("catalog order=%v", visible)
	}
}

func TestMCPPublicProxyHostAndGeneratedUploadURL(t *testing.T) {
	s, _ := newMCPTestService(t)
	for _, bad := range []string{"relative", "ftp://example.com", "https://user:secret@example.com", "https://example.com?token=secret"} {
		if err := s.configurePublicURL(bad); err == nil {
			t.Fatalf("accepted public URL %q", bad)
		}
	}
	if err := s.configurePublicURL("https://music.example.com"); err != nil {
		t.Fatal(err)
	}
	contextData := callMCP(t, s, "get_context", map[string]any{})
	if contextData["uploads"].(map[string]any)["endpoint"] != "https://music.example.com/api/mcp/uploads" {
		t.Fatalf("upload contract=%v", contextData)
	}
	handler := s.handler()
	for _, host := range []string{"music.example.com", "unknown.example.com"} {
		req := httptest.NewRequest(http.MethodPost, "http://"+host+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
		req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080}))
		req.Header.Set("Authorization", "Bearer "+s.accessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2025-11-25")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		expected := 200
		if host == "unknown.example.com" {
			expected = 403
		}
		if rec.Code != expected {
			t.Fatalf("host %s=%d %s", host, rec.Code, rec.Body.String())
		}
	}
}
