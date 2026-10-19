package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"
)

func orderProposalFixture(t *testing.T) (*trackStore, user, user, album, createAlbumTrackOrderSubmissionRequest) {
	t.Helper()
	s := newTestTrackStore(t)
	t.Cleanup(func() { _ = s.db.Close() })
	admin := mustCreateCatalogTestUser(t, s, "order-reviewer@example.com")
	owner := mustCreateCatalogTestUser(t, s, "order-requester@example.com")
	a, _, _ := albumOrderFixture(t, s)
	ids := slices.Clone(a.TrackIDs)
	slices.Reverse(ids)
	return s, admin, owner, a, createAlbumTrackOrderSubmissionRequest{AlbumID: a.ID, reorderAlbumTracksRequest: reorderAlbumTracksRequest{ids, a.OrderRevision}, Reason: "Original release order"}
}

func TestAlbumOrderSubmissionLifecycle(t *testing.T) {
	for _, action := range []string{"approve", "reject", "cancel"} {
		t.Run(action, func(t *testing.T) {
			s, admin, owner, a, req := orderProposalFixture(t)
			sub, err := s.createAlbumTrackOrderSubmission(owner.ID, req)
			if err != nil {
				t.Fatal(err)
			}
			assertAlbumOrder(t, s, a.ID, a.TrackIDs, a.OrderRevision)
			view := sub.Entity.(albumTrackOrderSubmissionView)
			if sub.Status != catalogSubmissionStatusPendingReview || !slices.Equal(view.BaseTrackIDs, a.TrackIDs) || !slices.Equal(view.TrackIDs, req.TrackIDs) || len(view.Tracks) != 3 {
				t.Fatalf("proposal = %+v", sub)
			}
			if _, err := s.createAlbumTrackOrderSubmission(owner.ID, req); !errors.Is(err, errAlbumOrderProposalExists) {
				t.Fatalf("duplicate = %v", err)
			}
			if _, err := s.approveCatalogSubmission(admin.ID, sub.ID, "invalid"); !errors.Is(err, errCatalogReviewLeaseInvalid) {
				t.Fatalf("lease = %v", err)
			}
			if _, err := s.acquireCatalogReviewLease(owner.ID, owner.ID); !errors.Is(err, errCatalogSelfReview) {
				t.Fatalf("self review = %v", err)
			}
			lease, err := s.acquireCatalogReviewLease(admin.ID, owner.ID)
			if err != nil {
				t.Fatal(err)
			}
			queue, err := s.listCatalogReviewSubmissions(admin.ID, owner.ID, lease.LeaseToken)
			if err != nil || len(queue) != 1 {
				t.Fatalf("queue = %+v %v", queue, err)
			}
			switch action {
			case "approve":
				sub, err = s.approveCatalogSubmission(admin.ID, sub.ID, lease.LeaseToken)
			case "reject":
				sub, err = s.rejectCatalogSubmission(admin.ID, sub.ID, lease.LeaseToken, catalogReviewDecisionRequest{Message: "Incorrect order"})
			case "cancel":
				sub, err = s.cancelCatalogSubmission(owner.ID, sub.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if action == "approve" {
				assertAlbumOrder(t, s, a.ID, req.TrackIDs, a.OrderRevision+1)
				if sub.Status != catalogSubmissionStatusApproved {
					t.Fatalf("status=%s", sub.Status)
				}
				p, err := decodeAlbumTrackOrderProposal(sub.catalogSubmission)
				if err != nil || p.AppliedOrderRevision != a.OrderRevision+1 {
					t.Fatalf("applied=%+v %v", p, err)
				}
				if _, err := s.approveCatalogSubmission(admin.ID, sub.ID, lease.LeaseToken); !errors.Is(err, errCatalogSubmissionState) {
					t.Fatalf("repeat approval=%v", err)
				}
			} else {
				assertAlbumOrder(t, s, a.ID, a.TrackIDs, a.OrderRevision)
			}
			current, _, _ := s.getAlbum(a.ID)
			if current.PublicationStatus != catalogPublicationPublished || current.RequestedByUserID != nil {
				t.Fatalf("publication changed: %+v", current)
			}
			for _, id := range a.TrackIDs {
				if _, found, err := s.get(id); err != nil || !found {
					t.Fatalf("deleted track %d: %v", id, err)
				}
			}
			// A terminal proposal must not block a later correction of this album.
			req.ExpectedOrderRevision = current.OrderRevision
			next, err := s.createAlbumTrackOrderSubmission(owner.ID, req)
			if err != nil || next.ID == sub.ID {
				t.Fatalf("new proposal=%+v %v", next, err)
			}
			if err := runSQLiteStartupRepairs(context.Background(), s.db); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAlbumOrderSubmissionStaleAndCorrection(t *testing.T) {
	s, admin, owner, a, req := orderProposalFixture(t)
	sub, err := s.createAlbumTrackOrderSubmission(owner.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	other := mustCreateCatalogTestUser(t, s, "competing@example.com")
	competing, err := s.createAlbumTrackOrderSubmission(other.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.acquireCatalogReviewLease(admin.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The deployed direct API remains functional and invalidates a pending proposal.
	newOrder := []int64{a.TrackIDs[1], a.TrackIDs[0], a.TrackIDs[2]}
	if _, err := s.reorderAlbumTracks(context.Background(), admin.ID, a.ID, reorderAlbumTracksRequest{newOrder, a.OrderRevision}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.approveCatalogSubmission(admin.ID, sub.ID, lease.LeaseToken); !errors.Is(err, errAlbumOrderStale) {
		t.Fatalf("stale approval=%v", err)
	}
	assertAlbumOrder(t, s, a.ID, newOrder, a.OrderRevision+1)
	sub, err = s.requestCatalogSubmissionChanges(admin.ID, sub.ID, lease.LeaseToken, catalogReviewDecisionRequest{Message: "Refresh order revision"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.resubmitCatalogSubmission(owner.ID, sub.ID); !errors.Is(err, errCatalogSubmissionDependency) {
		t.Fatalf("stale resubmit=%v", err)
	}
	if _, err := s.updateAlbumTrackOrderSubmission(other.ID, sub.ID, updateAlbumTrackOrderSubmissionRequest{ExpectedRevision: sub.Revision, TrackIDs: &req.TrackIDs}); !errors.Is(err, errCatalogSubmissionForbidden) {
		t.Fatalf("ownership=%v", err)
	}
	base := a.OrderRevision + 1
	patch := updateAlbumTrackOrderSubmissionRequest{ExpectedRevision: sub.Revision, ExpectedOrderRevision: &base}
	sub, err = s.updateAlbumTrackOrderSubmission(owner.ID, sub.ID, patch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.updateAlbumTrackOrderSubmission(owner.ID, sub.ID, patch); !errors.Is(err, errMCPStaleRevision) {
		t.Fatalf("stale patch=%v", err)
	}
	if !slices.Equal(sub.Entity.(albumTrackOrderSubmissionView).BaseTrackIDs, newOrder) {
		t.Fatalf("base was not refreshed: %+v", sub)
	}
	sub, err = s.resubmitCatalogSubmission(owner.ID, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.approveCatalogSubmission(admin.ID, sub.ID, lease.LeaseToken); err != nil {
		t.Fatal(err)
	}
	assertAlbumOrder(t, s, a.ID, req.TrackIDs, base+1)
	if err := s.releaseCatalogReviewLease(admin.ID, owner.ID, lease.LeaseToken); err != nil {
		t.Fatal(err)
	}
	lease, err = s.acquireCatalogReviewLease(admin.ID, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.approveCatalogSubmission(admin.ID, competing.ID, lease.LeaseToken); !errors.Is(err, errAlbumOrderStale) {
		t.Fatalf("competing proposal=%v", err)
	}
}

func TestAlbumOrderSubmissionValidation(t *testing.T) {
	s, _, owner, a, req := orderProposalFixture(t)
	for name, ids := range map[string][]int64{"missing": a.TrackIDs[:2], "duplicate": {a.TrackIDs[0], a.TrackIDs[0], a.TrackIDs[1]}, "foreign": {a.TrackIDs[0], a.TrackIDs[1], 99999}, "nonpositive": {0, a.TrackIDs[1], a.TrackIDs[2]}, "null": nil} {
		t.Run(name, func(t *testing.T) {
			copy := req
			copy.TrackIDs = ids
			if _, err := s.createAlbumTrackOrderSubmission(owner.ID, copy); !errors.Is(err, errInvalidAlbum) {
				t.Fatalf("invalid=%v", err)
			}
		})
	}
	pending, err := s.createAlbumSubmission(owner.ID, upsertAlbumRequest{Title: "Pending", ReleaseDate: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	copy := req
	copy.AlbumID = pending.EntityID
	copy.TrackIDs = []int64{}
	copy.ExpectedOrderRevision = 1
	if _, err := s.createAlbumTrackOrderSubmission(owner.ID, copy); !errors.Is(err, errAlbumOrderState) {
		t.Fatalf("pending album=%v", err)
	}
	// Another requester's staged track prevents proposing a filtered subset.
	other := mustCreateCatalogTestUser(t, s, "hidden-track@example.com")
	_, _, artist := albumOrderFixture(t, s)
	upload := catalogSubmissionUpload{Token: "hidden-audio", RequesterUserID: other.ID, StoredFileName: "hidden.mp3", OriginalName: "hidden.mp3", CreatedAt: time.Now().UTC()}
	if err := newDomainRepositories(s.db).submissions.InsertSubmissionUpload(context.Background(), upload); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createTrackSubmission(other.ID, createTrackSubmissionRequest{Name: "Hidden", AuthorIDs: []int64{artist.ID}, AlbumID: a.ID, AlbumOrder: len(a.TrackIDs), AudioUploadToken: upload.Token}); err != nil {
		t.Fatal(err)
	}
	req.ExpectedOrderRevision++
	if _, err := s.createAlbumTrackOrderSubmission(owner.ID, req); !errors.Is(err, errAlbumOrderHidden) {
		t.Fatalf("hidden membership=%v", err)
	}
}

func TestAlbumOrderSubmissionMCPRetriesAndActions(t *testing.T) {
	s, owner := newMCPTestService(t)
	a, _, _ := albumOrderFixture(t, s.store)
	ids := slices.Clone(a.TrackIDs)
	slices.Reverse(ids)
	args := map[string]any{"requestId": "propose-order", "albumId": a.ID, "trackIds": ids, "expectedOrderRevision": a.OrderRevision, "reason": "Release order"}
	originalUOW := s.store.unitOfWork
	s.store.unitOfWork = failingMCPUnitOfWork{originalUOW}
	raw, _ := json.Marshal(args)
	if _, err := s.call(context.Background(), "submit_album_track_order", raw, true); !errors.Is(err, errRepositoryPersistence) {
		t.Fatalf("receipt rollback=%v", err)
	}
	s.store.unitOfWork = originalUOW
	first := callMCP(t, s, "submit_album_track_order", args)
	protocolBody, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "submit_album_track_order", "arguments": args}})
	protocolRequest := httptest.NewRequest(http.MethodPost, "http://localhost/mcp", bytes.NewReader(protocolBody))
	protocolRequest.Header.Set("Authorization", "Bearer "+s.accessToken)
	protocolRequest.Header.Set("Content-Type", "application/json")
	protocolRequest.Header.Set("Accept", "application/json, text/event-stream")
	protocolRequest.Header.Set("MCP-Protocol-Version", "2025-11-25")
	protocolResponse := httptest.NewRecorder()
	s.handler().ServeHTTP(protocolResponse, protocolRequest)
	if protocolResponse.Code != http.StatusOK || !bytes.Contains(protocolResponse.Body.Bytes(), []byte(`"ok":true`)) {
		t.Fatalf("MCP protocol: %d %s", protocolResponse.Code, protocolResponse.Body.String())
	}
	if !reflect.DeepEqual(first, callMCP(t, s, "submit_album_track_order", args)) {
		t.Fatal("retry changed receipt")
	}
	if !reflect.DeepEqual(first["allowedActions"], []any{"cancel_submission"}) {
		t.Fatalf("actions=%v", first["allowedActions"])
	}
	assertAlbumOrder(t, s.store, a.ID, a.TrackIDs, a.OrderRevision)
	args["reason"] = "Changed payload"
	raw, _ = json.Marshal(args)
	if _, err := s.call(context.Background(), "submit_album_track_order", raw, true); !errors.Is(err, errMCPRequestConflict) {
		t.Fatalf("conflict=%v", err)
	}
	list := callMCP(t, s, "list_submissions", map[string]any{"entityType": "album_track_order"})
	if list["totalItems"].(float64) != 1 {
		t.Fatalf("list=%v", list)
	}
	admin := mustCreateCatalogTestUser(t, s.store, "mcp-order-reviewer@example.com")
	setTestUserRole(t, s.store, admin.ID, roleAdmin)
	lease, err := s.store.acquireCatalogReviewLease(admin.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := s.store.requestCatalogSubmissionChanges(admin.ID, mcpID(first, "submissionId"), lease.LeaseToken, catalogReviewDecisionRequest{Message: "Add source explanation"})
	if err != nil {
		t.Fatal(err)
	}
	current := callMCP(t, s, "get_submission", map[string]any{"submissionId": sub.ID})
	if !slices.Contains(current["allowedActions"].([]any), any("update_album_track_order_submission")) {
		t.Fatalf("actions=%v", current)
	}
	updated := callMCP(t, s, "update_album_track_order_submission", map[string]any{"requestId": "correct-order", "submissionId": sub.ID, "expectedRevision": sub.Revision, "reason": "Verified against release sleeve"})
	resub := callMCP(t, s, "resubmit_submission", map[string]any{"requestId": "resubmit-order", "submissionId": sub.ID, "expectedRevision": mcpID(updated, "revision")})
	if resub["status"] != catalogSubmissionStatusPendingReview {
		t.Fatalf("resubmit=%v", resub)
	}
	if _, err := s.store.approveCatalogSubmission(admin.ID, sub.ID, lease.LeaseToken); err != nil {
		t.Fatal(err)
	}
	assertAlbumOrder(t, s.store, a.ID, ids, a.OrderRevision+1)
	var sequence int
	var databaseName, databasePath string
	if err := s.store.db.QueryRow(`PRAGMA database_list`).Scan(&sequence, &databaseName, &databasePath); err != nil {
		t.Fatal(err)
	}
	if err := s.store.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := newTrackStore(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.db.Close() })
	s.store = reopened
	args["reason"] = "Release order"
	if !reflect.DeepEqual(first, callMCP(t, s, "submit_album_track_order", args)) {
		t.Fatal("receipt changed after restart and approval")
	}
}

func TestAlbumOrderSubmissionApprovalRollback(t *testing.T) {
	s, admin, owner, a, req := orderProposalFixture(t)
	sub, err := s.createAlbumTrackOrderSubmission(owner.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.acquireCatalogReviewLease(admin.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_order_approval BEFORE UPDATE ON catalog_submissions WHEN NEW.entity_type = 'album_track_order' AND NEW.status = 'approved' BEGIN SELECT RAISE(ABORT,'injected decision failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.approveCatalogSubmission(admin.ID, sub.ID, lease.LeaseToken); err == nil {
		t.Fatal("approval unexpectedly succeeded")
	}
	assertAlbumOrder(t, s, a.ID, a.TrackIDs, a.OrderRevision)
	stored, found, err := newDomainRepositories(s.db).submissions.FindSubmissionByID(context.Background(), sub.ID)
	if err != nil || !found || stored.Status != catalogSubmissionStatusPendingReview || stored.Revision != sub.Revision {
		t.Fatalf("rollback failed: %+v %v", stored, err)
	}
}

func TestAlbumOrderSubmissionMembershipChange(t *testing.T) {
	s, admin, owner, a, req := orderProposalFixture(t)
	sub, err := s.createAlbumTrackOrderSubmission(owner.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.acquireCatalogReviewLease(admin.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deleted, err := s.delete(a.TrackIDs[0]); err != nil || !deleted {
		t.Fatalf("delete=%v %v", deleted, err)
	}
	if _, err := s.approveCatalogSubmission(admin.ID, sub.ID, lease.LeaseToken); !errors.Is(err, errAlbumOrderStale) {
		t.Fatalf("membership change=%v", err)
	}
	assertAlbumOrder(t, s, a.ID, a.TrackIDs[1:], a.OrderRevision+1)
}

func TestAlbumOrderSubmissionHTTP(t *testing.T) {
	s, admin, owner, a, req := orderProposalFixture(t)
	auth := newAuthManager([]byte("proposal-http-test-secret"), time.Hour, time.Hour)
	mux := http.NewServeMux()
	mux.Handle("POST /api/catalog-submissions/album-track-orders", requirePermission(auth, s, permissionAlbumsSubmitTrackOrder, albumTrackOrderSubmissionHandler(s, false)))
	mux.Handle("PUT /api/catalog-submissions/album-track-orders/", requirePermission(auth, s, permissionCatalogSubmissionsUpdate, albumTrackOrderSubmissionHandler(s, true)))
	invoke := func(method, path string, userID int64, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		if userID != 0 {
			token, _, err := auth.createAccessToken(userID)
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	path := "/api/catalog-submissions/album-track-orders"
	if w := invoke("POST", path, 0, req); w.Code != 401 {
		t.Fatalf("unauthenticated=%d", w.Code)
	}
	w := invoke("POST", path, owner.ID, req)
	if w.Code != 201 {
		t.Fatalf("create=%d %s", w.Code, w.Body.String())
	}
	var sub catalogSubmissionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &sub); err != nil {
		t.Fatal(err)
	}
	if w := invoke("POST", path, owner.ID, req); w.Code != 409 {
		t.Fatalf("duplicate=%d %s", w.Code, w.Body.String())
	}
	updatePath := fmt.Sprintf("%s/%d", path, sub.ID)
	if w := invoke("PUT", updatePath, owner.ID, map[string]any{"expectedRevision": sub.Revision, "reason": "Update"}); w.Code != 409 {
		t.Fatalf("pending update=%d %s", w.Code, w.Body.String())
	}
	lease, err := s.acquireCatalogReviewLease(admin.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	sub, err = s.requestCatalogSubmissionChanges(admin.ID, sub.ID, lease.LeaseToken, catalogReviewDecisionRequest{Message: "Explain"})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"trackIds", "expectedOrderRevision", "reason"} {
		body := map[string]any{"expectedRevision": sub.Revision, "reason": "Explanation", field: nil}
		if w := invoke("PUT", updatePath, owner.ID, body); w.Code != 400 {
			t.Fatalf("null %s accepted: %d %s", field, w.Code, w.Body.String())
		}
	}
	if w := invoke("PUT", updatePath, owner.ID, map[string]any{"expectedRevision": sub.Revision, "reason": "Corrected"}); w.Code != 200 {
		t.Fatalf("update=%d %s", w.Code, w.Body.String())
	}
	assertAlbumOrder(t, s, a.ID, a.TrackIDs, a.OrderRevision)
	role, err := s.createAccessRole(admin.ID, upsertAccessRoleRequest{Name: "no-proposals"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.replaceUserRoles(admin.ID, owner.ID, []int64{role.ID}); err != nil {
		t.Fatal(err)
	}
	if w := invoke("POST", path, owner.ID, req); w.Code != 403 {
		t.Fatalf("forbidden=%d", w.Code)
	}
}

func TestAlbumOrderSubmissionConcurrentApproval(t *testing.T) {
	s, admin, owner, a, req := orderProposalFixture(t)
	sub, err := s.createAlbumTrackOrderSubmission(owner.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.acquireCatalogReviewLease(admin.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.approveCatalogSubmission(admin.ID, sub.ID, lease.LeaseToken)
		}(i)
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("concurrent approvals=%v", errs)
	}
	assertAlbumOrder(t, s, a.ID, req.TrackIDs, a.OrderRevision+1)
}

func TestAlbumOrderSubmissionMigrationPreservesHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.sqlite")
	db, err := openSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	migrations := defaultSchemaMigrations()
	if err := runSQLiteMigrations(ctx, db, migrations[:10]); err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`INSERT INTO users(id,email,password_hash,created_at) VALUES(1,'upgrade@example.com','hash','2026-01-01T00:00:00Z')`,
		`INSERT INTO user_roles(user_id,role_id,assigned_at) SELECT 1,id,'2026-01-01T00:00:00Z' FROM roles WHERE name='admin'`,
		`INSERT INTO authors(id,current_name,photos_json,publication_status,requested_by_user_id) VALUES(1,'Artist','[]','changes_requested',1)`,
		`INSERT INTO catalog_submissions(id,entity_type,entity_id,requester_user_id,status,snapshot_json,created_at,submitted_at,updated_at,revision) VALUES(30,'author',1,1,'changes_requested','{}','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z',3)`,
		`UPDATE sqlite_sequence SET seq=80 WHERE name='catalog_submissions'`,
		`INSERT INTO catalog_submission_feedback(id,submission_id,kind,message,rating_penalty,created_at) VALUES(20,30,'changes_requested','Fix spelling',2,'2026-01-01T00:00:00Z')`,
		`INSERT INTO import_rating_events(id,user_id,submission_id,event_key,kind,delta,reason,created_at) VALUES(10,1,30,'old-penalty','review_penalty',-2,'Fix spelling','2026-01-01T00:00:00Z')`,
		`INSERT INTO mcp_uploads(token,user_id,kind,original_name,media_type,size_bytes,sha256,stored_name,media_path,claimed_submission_id,created_at) VALUES('old-upload',1,'author_photo','photo.jpg','image/jpeg',10,'sha','photo.jpg','/photo.jpg',30,'2026-01-01T00:00:00Z')`,
		`INSERT INTO mcp_requests VALUES(1,'old-request','submit_author','hash','{"ok":true}','2026-01-01T00:00:00Z')`,
		`DELETE FROM permissions WHERE code='albums.submit_track_order'`,
	}
	for _, q := range statements {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := runSQLiteMigrations(ctx, db, migrations); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM catalog_submission_feedback WHERE id=20 AND submission_id=30 AND rating_penalty=2`,
		`SELECT COUNT(*) FROM import_rating_events WHERE id=10 AND submission_id=30 AND delta=-2`,
		`SELECT COUNT(*) FROM mcp_uploads WHERE token='old-upload' AND claimed_submission_id=30`,
		`SELECT COUNT(*) FROM mcp_requests WHERE request_id='old-request' AND result_json='{"ok":true}'`,
		`SELECT COUNT(*) FROM catalog_submissions WHERE id=30 AND revision=3`,
		`SELECT COUNT(*) FROM sqlite_sequence WHERE name='catalog_submissions' AND seq=80`,
	} {
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil || n != 1 {
			t.Fatalf("migration preservation: %s: %d %v", q, n, err)
		}
	}
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Fatal("foreign keys broken")
	}
	rows.Close()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := newTrackStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
}
