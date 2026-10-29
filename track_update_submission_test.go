package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func updateTestPtr[T any](v T) *T { return &v }
func trackUpdateTestAudio(t *testing.T, s *mcpService, key string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/mcp-silence.mp3")
	if err != nil {
		t.Fatal(err)
	}
	return uploadMCP(t, s, "audio", key, key+".mp3", data)["uploadToken"].(string)
}
func updateTestCall(s *mcpService, name string, args map[string]any) error {
	raw, _ := json.Marshal(args)
	_, err := s.call(context.Background(), name, raw, true)
	return err
}

func TestPublishedTrackUpdateLifecycle(t *testing.T) {
	for _, action := range []string{"approve", "reject", "cancel"} {
		t.Run(action, func(t *testing.T) {
			svc, owner := newMCPTestService(t)
			s := svc.store
			a, b, _ := trackNumberFixture(t, s)
			id := a.TrackIDs[0]
			before, _, _ := s.get(id)
			artist, err := s.createAuthor(upsertAuthorRequest{CurrentName: "Corrected artist"})
			if err != nil {
				t.Fatal(err)
			}
			token := trackUpdateTestAudio(t, svc, "replacement")
			lyrics, _ := json.Marshal(syncedSubmissionLyrics())
			sub, err := s.createTrackUpdateSubmission(owner.ID, createTrackUpdateSubmissionRequest{TrackID: id, trackUpdatePatch: trackUpdatePatch{
				Name: updateTestPtr("Corrected title"), AuthorIDs: updateTestPtr([]int64{artist.ID}), AlbumID: &b.ID, TrackNumber: updateTestPtr(8),
				AudioUploadToken: &token, Lyrics: lyrics, AdditionalInfo: updateTestPtr([]additionalInfo{{"type": "text", "title": "Edition", "text": "Studio recording"}}),
				SourceMetadata: updateTestPtr([]sourceMetadata{{"provider": "test", "identity": map[string]any{"recording": "corrected"}}}), Reason: updateTestPtr("Replace compressed audio and correct metadata"),
			}})
			if err != nil {
				t.Fatal(err)
			}
			current, _, _ := s.get(id)
			if !reflect.DeepEqual(current, before) {
				t.Fatal("unapproved update changed the published track")
			}
			p, err := decodeTrackUpdateProposal(sub.catalogSubmission)
			if err != nil || len(p.ChangedFields) != 8 {
				t.Fatalf("diff: %+v %v", p, err)
			}
			// A claimed MCP token cannot be stolen through the older track-creation API.
			_, err = s.createTrackSubmission(owner.ID, createTrackSubmissionRequest{Name: "Duplicate", AuthorIDs: before.AuthorIDs, AlbumID: a.ID, TrackNumber: 9, AudioUploadToken: token})
			if !errors.Is(err, errCatalogUploadClaimed) {
				t.Fatalf("reserved audio reused: %v", err)
			}
			reviewer := mustCreateCatalogTestUser(t, s, "update-reviewer@example.com")
			lease, err := s.acquireCatalogReviewLease(reviewer.ID, owner.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.approveCatalogSubmission(reviewer.ID, sub.ID, "wrong"); !errors.Is(err, errCatalogReviewLeaseInvalid) {
				t.Fatalf("lease: %v", err)
			}
			switch action {
			case "approve":
				sub, err = s.approveCatalogSubmission(reviewer.ID, sub.ID, lease.LeaseToken)
			case "reject":
				sub, err = s.rejectCatalogSubmission(reviewer.ID, sub.ID, lease.LeaseToken, catalogReviewDecisionRequest{Message: "Use another recording"})
			case "cancel":
				sub, err = s.cancelCatalogSubmission(owner.ID, sub.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			current, _, _ = s.get(id)
			if action == "approve" {
				if current.Name != "Corrected title" || current.AlbumID != b.ID || current.TrackNumber != 8 || current.ID != before.ID || !current.CreatedAt.Equal(before.CreatedAt) || current.PublicationStatus != "published" {
					t.Fatalf("applied track: %+v", current)
				}
				name, ok := extractReferencedSongFileName(current.AudioFilePath)
				if !ok {
					t.Fatal(current.AudioFilePath)
				}
				if _, err := os.Stat(filepath.Join(s.songsDir, name)); err != nil {
					t.Fatal(err)
				}
				got, err := s.getLyrics(id)
				if err != nil || got.Type != "synced" {
					t.Fatalf("lyrics: %+v %v", got, err)
				}
				album, _, _ := s.getAlbum(b.ID)
				if !containsInt64(album.AuthorIDs, artist.ID) {
					t.Fatal("album-derived artists not updated")
				}
			} else if !reflect.DeepEqual(current, before) {
				t.Fatalf("%s changed published data", action)
			}
			// The proposal's original data survives approval, rejection and cancellation.
			retained, err := decodeTrackUpdateProposal(sub.catalogSubmission)
			if err != nil || retained.Before.Name != before.Name {
				t.Fatal("lost baseline")
			}
			var files []string
			_ = s.withinReadTransaction(func(ctx context.Context, repos domainRepositories) error {
				var err error
				files, err = repos.submissions.ListSubmissionUploadFileNames(ctx)
				return err
			})
			if len(files) != 0 {
				t.Fatalf("staged files retained: %v", files)
			}
			if err := runSQLiteStartupRepairs(context.Background(), s.db); err != nil {
				t.Fatalf("restart validation: %v", err)
			}
		})
	}
}

func TestPublishedTrackUpdateRevisionRebaseAndRetries(t *testing.T) {
	svc, owner := newMCPTestService(t)
	a, _, _ := trackNumberFixture(t, svc.store)
	id := a.TrackIDs[0]
	args := map[string]any{"requestId": "create-update", "trackId": id, "name": "Proposed title"}
	first := callMCP(t, svc, "submit_track_update", args)
	retry := callMCP(t, svc, "submit_track_update", args)
	if !reflect.DeepEqual(first, retry) {
		t.Fatal("retry changed receipt")
	}
	if err := updateTestCall(svc, "submit_track_update", map[string]any{"requestId": "duplicate", "trackId": id, "name": "Another title"}); !errors.Is(err, errCatalogSubmissionState) {
		t.Fatalf("duplicate: %v", err)
	}
	reviewer := mustCreateCatalogTestUser(t, svc.store, "rebase-reviewer@example.com")
	lease, _ := svc.store.acquireCatalogReviewLease(reviewer.ID, owner.ID)
	sub, err := svc.store.requestCatalogSubmissionChanges(reviewer.ID, mcpID(first, "id"), lease.LeaseToken, catalogReviewDecisionRequest{Message: "Check latest catalog data"})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := json.Marshal([]additionalInfo{{"type": "text", "title": "Catalog correction", "text": "Keep this concurrent correction"}})
	if _, err := svc.store.db.Exec("UPDATE tracks SET additional_info_json=? WHERE id=?", string(info), id); err != nil {
		t.Fatal(err)
	}
	patch := map[string]any{"requestId": "patch-update", "submissionId": sub.ID, "expectedRevision": sub.Revision, "name": "Final proposed title"}
	if err := updateTestCall(svc, "update_track_update_submission", patch); !errors.Is(err, errCatalogSubmissionDependency) {
		t.Fatalf("unacknowledged stale baseline: %v", err)
	}
	if _, err := svc.store.approveCatalogSubmission(reviewer.ID, sub.ID, lease.LeaseToken); !errors.Is(err, errCatalogSubmissionState) {
		t.Fatalf("approved changes_requested: %v", err)
	}
	patch["refreshBaseline"] = true
	updated := callMCP(t, svc, "update_track_update_submission", patch)
	entity := updated["entity"].(map[string]any)
	after := entity["after"].(map[string]any)
	if len(after["additionalInfo"].([]any)) != 1 {
		t.Fatal("rebasing overwrote an untouched concurrent field")
	}
	patch["requestId"] = "stale-revision"
	if err := updateTestCall(svc, "update_track_update_submission", patch); !errors.Is(err, errMCPStaleRevision) {
		t.Fatalf("stale revision: %v", err)
	}
	other := mustCreateCatalogTestUser(t, svc.store, "other-proposer@example.com")
	if _, err := svc.store.updateTrackUpdateSubmission(other.ID, sub.ID, updateTrackUpdateSubmissionRequest{ExpectedRevision: mcpID(updated, "revision")}); !errors.Is(err, errCatalogSubmissionForbidden) {
		t.Fatalf("ownership: %v", err)
	}
	callMCP(t, svc, "resubmit_submission", map[string]any{"requestId": "resubmit-update", "submissionId": sub.ID, "expectedRevision": updated["revision"]})
	if _, err := svc.store.approveCatalogSubmission(reviewer.ID, sub.ID, lease.LeaseToken); err != nil {
		t.Fatal(err)
	}
	got, _, _ := svc.store.get(id)
	if got.Name != "Final proposed title" || len(got.AdditionalInfo) != 1 {
		t.Fatalf("final: %+v", got)
	}
}

func TestPublishedTrackUpdateHTTPAndAudioAccess(t *testing.T) {
	svc, owner := newMCPTestService(t)
	a, _, _ := trackNumberFixture(t, svc.store)
	id := a.TrackIDs[0]
	auth := newAuthManager([]byte("track-update-http-test-secret"), time.Hour, time.Hour)
	access, _, _ := auth.createAccessToken(owner.ID)
	handler := requirePermission(auth, svc.store, permissionTracksSubmit, trackUpdateSubmissionHandler(svc.store, false))
	post := func(payload string, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/catalog-submissions/track-updates", strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := post(fmt.Sprintf("{\"trackId\":%d,\"name\":\"Draft\"}", id), ""); rec.Code != 401 {
		t.Fatal(rec.Code)
	}
	for _, field := range []string{"name", "authorIds", "albumId", "trackNumber", "additionalInfo", "sourceMetadata", "reason", "audioUploadToken"} {
		rec := post(fmt.Sprintf("{\"trackId\":%d,\"%s\":null}", id, field), access)
		if rec.Code != 400 {
			t.Fatalf("null %s: %d %s", field, rec.Code, rec.Body)
		}
	}
	if rec := post(fmt.Sprintf("{\"trackId\":%d,\"unexpected\":true}", id), access); rec.Code != 400 {
		t.Fatalf("unknown fields: %d", rec.Code)
	}
	if rec := post(fmt.Sprintf("{\"trackId\":%d}", id), access); rec.Code != 400 {
		t.Fatalf("no-op: %d", rec.Code)
	}
	audio := trackUpdateTestAudio(t, svc, "private-preview")
	rec := post(fmt.Sprintf("{\"trackId\":%d,\"audioUploadToken\":%q}", id, audio), access)
	if rec.Code != 201 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var sub catalogSubmissionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &sub); err != nil {
		t.Fatal(err)
	}
	preview := requireAuth(auth, svc.store, serveTrackUpdateAudioHandler(svc.store))
	fetch := func(bearer, lease string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/catalog-submissions/track-updates/%d/audio", sub.ID), nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set(catalogReviewLeaseHeader, lease)
		req.Header.Set("Range", "bytes=0-31")
		rec := httptest.NewRecorder()
		preview.ServeHTTP(rec, req)
		return rec
	}
	if rec := fetch(access, ""); rec.Code != 206 || rec.Body.Len() != 32 {
		t.Fatalf("owner range: %d %s", rec.Code, rec.Body)
	}
	reviewer := mustCreateCatalogTestUser(t, svc.store, "audio-reviewer@example.com")
	reviewerAccess, _, _ := auth.createAccessToken(reviewer.ID)
	if rec := fetch(reviewerAccess, ""); rec.Code != 409 {
		t.Fatalf("private audio without lease: %d", rec.Code)
	}
	lease, err := svc.store.acquireCatalogReviewLease(reviewer.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec := fetch(reviewerAccess, lease.LeaseToken); rec.Code != 206 {
		t.Fatalf("reviewer audio: %d", rec.Code)
	}
}

func TestPublishedTrackUpdateRollbackAndLyricsRemoval(t *testing.T) {
	svc, owner := newMCPTestService(t)
	a, _, _ := trackNumberFixture(t, svc.store)
	id := a.TrackIDs[0]
	if _, _, err := svc.store.upsertLyrics(id, *syncedSubmissionLyrics()); err != nil {
		t.Fatal(err)
	}
	token := trackUpdateTestAudio(t, svc, "rollback")
	sub, err := svc.store.createTrackUpdateSubmission(owner.ID, createTrackUpdateSubmissionRequest{TrackID: id, trackUpdatePatch: trackUpdatePatch{AudioUploadToken: &token, Lyrics: json.RawMessage("null")}})
	if err != nil {
		t.Fatal(err)
	}
	reviewer := mustCreateCatalogTestUser(t, svc.store, "rollback-reviewer@example.com")
	lease, _ := svc.store.acquireCatalogReviewLease(reviewer.ID, owner.ID)
	_, err = svc.store.db.Exec("CREATE TRIGGER fail_track_update BEFORE UPDATE ON tracks BEGIN SELECT RAISE(ABORT,'forced rollback'); END")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.store.approveCatalogSubmission(reviewer.ID, sub.ID, lease.LeaseToken); err == nil {
		t.Fatal("expected rollback")
	}
	entries, _ := os.ReadDir(svc.store.songsDir)
	for _, entry := range entries {
		if !entry.IsDir() {
			t.Fatalf("unapproved public audio leaked: %s", entry.Name())
		}
	}
	if _, err := svc.store.getLyrics(id); err != nil {
		t.Fatal("failed approval changed lyrics")
	}
	if _, err := svc.store.db.Exec("DROP TRIGGER fail_track_update"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.approveCatalogSubmission(reviewer.ID, sub.ID, lease.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.getLyrics(id); !errors.Is(err, errLyricsNotFound) {
		t.Fatalf("lyrics not removed: %v", err)
	}
}

func TestPublishedTrackUpdateMigrationPreservesHistory(t *testing.T) {
	db, err := openSQLiteDB(filepath.Join(t.TempDir(), "upgrade.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	migrations := defaultSchemaMigrations()
	if err := runSQLiteMigrations(ctx, db, migrations[:13]); err != nil {
		t.Fatal(err)
	}
	now := "2026-10-28T10:00:00Z"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO users(id,email,password_hash,created_at) VALUES(1,'upgrade@example.com','hash',?)", now)
	exec("INSERT INTO catalog_submissions(id,entity_type,entity_id,requester_user_id,status,snapshot_json,created_at,submitted_at,updated_at,revision) VALUES(42,'track_lyrics',9,1,'changes_requested',?,?,?,?,5)", "{\"trackId\":9}", now, now, now)
	exec("UPDATE sqlite_sequence SET seq=90 WHERE name='catalog_submissions'")
	exec("INSERT INTO catalog_submission_feedback(id,submission_id,kind,message,rating_penalty,created_at) VALUES(12,42,'changes_requested','Check recording',2,?)", now)
	exec("INSERT INTO import_rating_events(id,user_id,submission_id,event_key,kind,delta,reason,created_at) VALUES(7,1,42,'old-penalty','review_penalty',-2,'Check recording',?)", now)
	exec("INSERT INTO mcp_uploads(token,user_id,kind,original_name,media_type,size_bytes,sha256,stored_name,media_path,claimed_submission_id,created_at) VALUES('claimed',1,'audio','old.mp3','audio/mpeg',10,'sha','old.mp3','',42,?)", now)
	exec("INSERT INTO mcp_requests VALUES(1,'old-request','submit_track_lyrics','hash','{}',?)", now)
	if err := runSQLiteMigrations(ctx, db, migrations); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"SELECT COUNT(*) FROM catalog_submissions WHERE id=42 AND revision=5",
		"SELECT COUNT(*) FROM catalog_submission_feedback WHERE submission_id=42",
		"SELECT COUNT(*) FROM import_rating_events WHERE submission_id=42",
		"SELECT COUNT(*) FROM mcp_uploads WHERE claimed_submission_id=42",
		"SELECT COUNT(*) FROM mcp_requests WHERE request_id='old-request'",
	} {
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s: count %d error %v", q, n, err)
		}
	}
	exec("INSERT INTO catalog_submissions(entity_type,entity_id,requester_user_id,status,snapshot_json,created_at,submitted_at,updated_at) VALUES('track_update',9,1,'pending_review','{}',?,?,?)", now, now, now)
	var id int64
	if err := db.QueryRow("SELECT MAX(id) FROM catalog_submissions").Scan(&id); err != nil || id <= 90 {
		t.Fatalf("sequence lost: %d %v", id, err)
	}
	var violations int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_foreign_key_check").Scan(&violations); err != nil || violations != 0 {
		t.Fatalf("foreign keys: %d %v", violations, err)
	}
}
