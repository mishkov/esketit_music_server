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
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCatalogReviewLeaseRecoveryInvalidatesOldToken(t *testing.T) {
	store := newTestTrackStore(t)
	t.Cleanup(func() { _ = store.db.Close() })
	requester := mustCreateCatalogTestUser(t, store, "recovery-requester@example.com")
	reviewer := mustCreateCatalogTestUser(t, store, "recovery-reviewer@example.com")
	otherReviewer := mustCreateCatalogTestUser(t, store, "recovery-other-reviewer@example.com")
	setTestUserRole(t, store, reviewer.ID, roleAdmin)
	setTestUserRole(t, store, otherReviewer.ID, roleAdmin)
	submission, err := store.createAuthorSubmission(requester.ID, upsertAuthorRequest{CurrentName: "Recovery Artist"})
	if err != nil {
		t.Fatal(err)
	}
	auth := newAuthManager([]byte("review-recovery-test-secret"), time.Hour, time.Hour)
	handler := requirePermission(auth, store, permissionCatalogSubmissionsReview, catalogReviewRequesterRouteHandler(store))
	path := fmt.Sprintf("/api/catalog-reviews/requesters/%d/lease", requester.ID)
	request := func(userID int64) *httptest.ResponseRecorder {
		t.Helper()
		token, _, err := auth.createAccessToken(userID)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	initial := request(reviewer.ID)
	if initial.Code != http.StatusCreated {
		t.Fatalf("initial acquire status = %d, body = %s", initial.Code, initial.Body.String())
	}
	var original catalogReviewLease
	if err := json.Unmarshal(initial.Body.Bytes(), &original); err != nil {
		t.Fatal(err)
	}
	if original.LeaseToken == "" {
		t.Fatal("initial lease token is empty")
	}
	unauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodPost, path, nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated acquire status = %d, want 401", unauthenticated.Code)
	}
	denied := request(requester.ID)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("requester acquire status = %d, want 403", denied.Code)
	}
	conflict := request(otherReviewer.ID)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("other reviewer acquire status = %d, want 409", conflict.Code)
	}
	recoveredResponse := request(reviewer.ID)
	if recoveredResponse.Code != http.StatusCreated {
		t.Fatalf("recovery status = %d, body = %s", recoveredResponse.Code, recoveredResponse.Body.String())
	}
	var recovered catalogReviewLease
	if err := json.Unmarshal(recoveredResponse.Body.Bytes(), &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.LeaseToken == "" || recovered.LeaseToken == original.LeaseToken {
		t.Fatalf("recovered token = %q, original token = %q", recovered.LeaseToken, original.LeaseToken)
	}
	if recovered.RequesterUserID != requester.ID || recovered.ReviewerUserID != reviewer.ID ||
		!recovered.AcquiredAt.Equal(original.AcquiredAt) || recovered.ExpiresAt.Before(original.ExpiresAt) {
		t.Fatalf("recovered lease = %#v, original = %#v", recovered, original)
	}
	if _, err := store.renewCatalogReviewLease(reviewer.ID, requester.ID, original.LeaseToken); !errors.Is(err, errCatalogReviewLeaseInvalid) {
		t.Fatalf("stale renew error = %v, want invalid lease", err)
	}
	if err := store.releaseCatalogReviewLease(reviewer.ID, requester.ID, original.LeaseToken); !errors.Is(err, errCatalogReviewLeaseInvalid) {
		t.Fatalf("stale release error = %v, want invalid lease", err)
	}
	if _, err := store.approveCatalogSubmission(reviewer.ID, submission.ID, original.LeaseToken); !errors.Is(err, errCatalogReviewLeaseInvalid) {
		t.Fatalf("stale decision error = %v, want invalid lease", err)
	}
	if _, err := store.listCatalogReviewSubmissions(reviewer.ID, requester.ID, recovered.LeaseToken); err != nil {
		t.Fatalf("recovered token cannot list submissions: %v", err)
	}
	if _, err := store.renewCatalogReviewLease(reviewer.ID, requester.ID, recovered.LeaseToken); err != nil {
		t.Fatalf("recovered token cannot renew: %v", err)
	}
}

func TestCatalogReviewLeaseConcurrentAcquisition(t *testing.T) {
	store := newTestTrackStore(t)
	t.Cleanup(func() { _ = store.db.Close() })
	requester := mustCreateCatalogTestUser(t, store, "concurrent-requester@example.com")
	first := mustCreateCatalogTestUser(t, store, "concurrent-first@example.com")
	second := mustCreateCatalogTestUser(t, store, "concurrent-second@example.com")
	if _, err := store.createAuthorSubmission(requester.ID, upsertAuthorRequest{CurrentName: "Concurrent Artist"}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, reviewerID := range []int64{first.ID, second.ID} {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			<-start
			_, err := store.acquireCatalogReviewLease(id, requester.ID)
			results <- err
		}(reviewerID)
	}
	close(start)
	wg.Wait()
	close(results)
	var acquired, conflicted int
	for err := range results {
		switch {
		case err == nil:
			acquired++
		case errors.Is(err, errCatalogReviewLeaseConflict):
			conflicted++
		default:
			t.Fatalf("concurrent acquire error = %v", err)
		}
	}
	if acquired != 1 || conflicted != 1 {
		t.Fatalf("concurrent acquire: %d acquired, %d conflicted; want 1 each", acquired, conflicted)
	}
}

func TestCatalogReviewLeaseRecoveryAfterExpiryStartsNewLease(t *testing.T) {
	store := newTestTrackStore(t)
	t.Cleanup(func() { _ = store.db.Close() })
	requester := mustCreateCatalogTestUser(t, store, "expired-requester@example.com")
	reviewer := mustCreateCatalogTestUser(t, store, "expired-reviewer@example.com")
	if _, err := store.createAuthorSubmission(requester.ID, upsertAuthorRequest{CurrentName: "Expired Artist"}); err != nil {
		t.Fatal(err)
	}
	initial, err := store.acquireCatalogReviewLease(reviewer.ID, requester.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE catalog_review_leases SET expires_at = ? WHERE requester_user_id = ?`,
		formatSQLiteTime(time.Now().UTC().Add(-time.Minute)), requester.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.releaseCatalogReviewLease(reviewer.ID, requester.ID, initial.LeaseToken); !errors.Is(err, errCatalogReviewLeaseInvalid) {
		t.Fatalf("expired release error = %v, want invalid lease", err)
	}
	recovered, err := store.acquireCatalogReviewLease(reviewer.ID, requester.ID)
	if err != nil {
		t.Fatalf("acquire after expiry: %v", err)
	}
	if recovered.LeaseToken == initial.LeaseToken || !recovered.AcquiredAt.After(initial.AcquiredAt) {
		t.Fatalf("lease after expiry = %#v, initial = %#v", recovered, initial)
	}
	if _, err := store.renewCatalogReviewLease(reviewer.ID, requester.ID, initial.LeaseToken); !errors.Is(err, errCatalogReviewLeaseInvalid) {
		t.Fatalf("expired token renew error = %v, want invalid lease", err)
	}
}

func TestCatalogSubmissionApprovalWorkflow(t *testing.T) {
	store := newTestTrackStore(t)
	store.songsDir = t.TempDir()
	requester := mustCreateCatalogTestUser(t, store, "requester@example.com")
	otherUser := mustCreateCatalogTestUser(t, store, "other@example.com")
	reviewer := mustCreateCatalogTestUser(t, store, "reviewer@example.com")
	secondReviewer := mustCreateCatalogTestUser(t, store, "second-reviewer@example.com")

	authorSubmission, err := store.createAuthorSubmission(requester.ID, upsertAuthorRequest{CurrentName: "Pending Artist"})
	if err != nil {
		t.Fatalf("createAuthorSubmission() error = %v", err)
	}
	authorID := authorSubmission.Entity.(author).ID
	albumSubmission, err := store.createAlbumSubmission(requester.ID, upsertAlbumRequest{
		Title: "Pending Album", AuthorIDs: []int64{authorID}, ReleaseDate: time.Now().UTC(), IsPublished: true,
	})
	if err != nil {
		t.Fatalf("createAlbumSubmission() error = %v", err)
	}
	albumID := albumSubmission.Entity.(album).ID
	emptyOwnedAlbums, err := store.listAlbums(albumListFilter{ViewerUserID: requester.ID, AuthorID: authorID})
	if err != nil || !containsCatalogAlbum(emptyOwnedAlbums.Items, albumID) {
		t.Fatalf("owned empty pending albums = %#v, error = %v; pending album missing", emptyOwnedAlbums, err)
	}
	otherEmptyAlbums, err := store.listAlbums(albumListFilter{ViewerUserID: otherUser.ID, AuthorID: authorID})
	if err != nil || containsCatalogAlbum(otherEmptyAlbums.Items, albumID) {
		t.Fatalf("other user's empty albums = %#v, error = %v; pending album leaked", otherEmptyAlbums, err)
	}
	upload := mustCreateCatalogTestUpload(t, store, requester.ID, "requester-song.mp3")
	trackSubmission, err := store.createTrackSubmission(requester.ID, createTrackSubmissionRequest{
		Name: "Pending Song", AuthorIDs: []int64{authorID}, AlbumID: albumID, AudioUploadToken: upload.Token,
	})
	if err != nil {
		t.Fatalf("createTrackSubmission() error = %v", err)
	}
	trackID := trackSubmission.Entity.(track).ID

	assertCatalogTrackVisibility(t, store, requester.ID, trackID, true)
	assertCatalogTrackVisibility(t, store, otherUser.ID, trackID, false)
	assertCatalogTrackVisibility(t, store, 0, trackID, false)
	requesterAuthors, err := store.listAuthors(authorListFilter{ViewerUserID: requester.ID})
	if err != nil || !containsCatalogAuthor(requesterAuthors, authorID) {
		t.Fatalf("requester authors = %#v, error = %v; pending author missing", requesterAuthors, err)
	}
	otherAuthors, err := store.listAuthors(authorListFilter{ViewerUserID: otherUser.ID})
	if err != nil || containsCatalogAuthor(otherAuthors, authorID) {
		t.Fatalf("other user authors = %#v, error = %v; pending author leaked", otherAuthors, err)
	}
	requesterAlbums, err := store.listAlbums(albumListFilter{ViewerUserID: requester.ID})
	if err != nil || !containsCatalogAlbum(requesterAlbums.Items, albumID) {
		t.Fatalf("requester albums = %#v, error = %v; pending album missing", requesterAlbums, err)
	}
	publicAlbums, err := store.listAlbums(albumListFilter{IncludeEmpty: true})
	if err != nil || containsCatalogAlbum(publicAlbums.Items, albumID) {
		t.Fatalf("public albums = %#v, error = %v; pending album leaked", publicAlbums, err)
	}
	search, err := store.search(requester.ID, searchListFilter{Query: "Pending", IncludeEmpty: true})
	if err != nil || search.TotalItems != 3 {
		t.Fatalf("requester search total = %d, error = %v, want author+album+track", search.TotalItems, err)
	}
	publicSearch, err := store.search(0, searchListFilter{Query: "Pending", IncludeEmpty: true})
	if err != nil || publicSearch.TotalItems != 0 {
		t.Fatalf("public search total = %d, error = %v, want 0", publicSearch.TotalItems, err)
	}

	if _, _, err := store.update(trackID, upsertTrackRequest{
		Name: "Reviewer edit", AuthorIDs: []int64{authorID}, AlbumID: albumID, AudioFilePath: "/api/songs/edited.mp3",
	}); !errors.Is(err, errCatalogSubmissionState) {
		t.Fatalf("direct update pending track error = %v, want catalog submission state error", err)
	}

	lease, err := store.acquireCatalogReviewLease(reviewer.ID, requester.ID)
	if err != nil {
		t.Fatalf("acquireCatalogReviewLease() error = %v", err)
	}
	if _, err := store.acquireCatalogReviewLease(secondReviewer.ID, requester.ID); !errors.Is(err, errCatalogReviewLeaseConflict) {
		t.Fatalf("second reviewer lease error = %v, want conflict", err)
	}
	if _, err := store.acquireCatalogReviewLease(reviewer.ID, otherUser.ID); !errors.Is(err, errCatalogSubmissionNotFound) {
		t.Fatalf("lease requester without submissions error = %v, want not found", err)
	}

	changed, err := store.requestCatalogSubmissionChanges(reviewer.ID, trackSubmission.ID, lease.LeaseToken, catalogReviewDecisionRequest{
		Message: "Please correct the title", RatingPenalty: 2,
	})
	if err != nil {
		t.Fatalf("requestCatalogSubmissionChanges() error = %v", err)
	}
	if changed.Status != catalogSubmissionStatusChangesRequested || len(changed.Feedback) != 1 {
		t.Fatalf("changed submission = %#v", changed)
	}
	reviewContext, err := store.listCatalogReviewSubmissions(reviewer.ID, requester.ID, lease.LeaseToken)
	if err != nil || len(reviewContext) != 3 {
		t.Fatalf("review context after feedback has %d submissions, error = %v, want all 3 dependency records", len(reviewContext), err)
	}
	if reviewContext[0].ID != authorSubmission.ID || reviewContext[1].ID != albumSubmission.ID {
		t.Fatalf("review order = %#v, want author then album before track", reviewContext)
	}
	if reviewContext[len(reviewContext)-1].ID != trackSubmission.ID || reviewContext[len(reviewContext)-1].Status != catalogSubmissionStatusChangesRequested {
		t.Fatalf("changes-requested track missing from review context: %#v", reviewContext)
	}
	assertCatalogTrackVisibility(t, store, requester.ID, trackID, true)
	updated, err := store.updateTrackSubmission(requester.ID, trackID, updateTrackSubmissionRequest{
		Name: "Corrected Song", AuthorIDs: []int64{authorID}, AlbumID: albumID,
	})
	if err != nil {
		t.Fatalf("updateTrackSubmission() error = %v", err)
	}
	if updated.Entity.(track).Name != "Corrected Song" {
		t.Fatalf("updated track name = %q", updated.Entity.(track).Name)
	}
	resubmitted, err := store.resubmitCatalogSubmission(requester.ID, trackSubmission.ID)
	if err != nil {
		t.Fatalf("resubmitCatalogSubmission() error = %v", err)
	}
	if resubmitted.Status != catalogSubmissionStatusPendingReview {
		t.Fatalf("resubmitted status = %q", resubmitted.Status)
	}
	if _, err := store.requestCatalogSubmissionChanges(reviewer.ID, authorSubmission.ID, lease.LeaseToken, catalogReviewDecisionRequest{Message: "Confirm artist details"}); err != nil {
		t.Fatalf("request author changes error = %v", err)
	}
	reviewContext, err = store.listCatalogReviewSubmissions(reviewer.ID, requester.ID, lease.LeaseToken)
	if err != nil || len(reviewContext) != 3 || reviewContext[0].ID != authorSubmission.ID || reviewContext[1].ID != albumSubmission.ID || reviewContext[2].ID != trackSubmission.ID {
		t.Fatalf("review order with changed author = %#v, error = %v", reviewContext, err)
	}
	if _, err := store.resubmitCatalogSubmission(requester.ID, authorSubmission.ID); err != nil {
		t.Fatalf("resubmit author error = %v", err)
	}
	plainText := "These are the lyrics"
	if _, created, err := store.upsertCatalogSubmissionLyrics(requester.ID, trackID, upsertLyricsRequest{Type: lyricsTypePlain, PlainText: &plainText}); err != nil || !created {
		t.Fatalf("upsertCatalogSubmissionLyrics() created=%v error=%v", created, err)
	}

	if _, err := store.approveCatalogSubmission(reviewer.ID, trackSubmission.ID, lease.LeaseToken); !errors.Is(err, errCatalogTrackReviewPrerequisite) || !strings.Contains(err.Error(), "review album and authors first") {
		t.Fatalf("premature track approval error = %v, want review prerequisite", err)
	}
	setTestUserRole(t, store, reviewer.ID, roleAdmin)
	auth := newAuthManager([]byte("review-prerequisite-test-secret"), time.Hour, time.Hour)
	accessToken, _, err := auth.createAccessToken(reviewer.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/catalog-reviews/submissions/%d/approve", trackSubmission.ID)
	request := httptest.NewRequest(http.MethodPost, path, nil)
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set(catalogReviewLeaseHeader, lease.LeaseToken)
	response := httptest.NewRecorder()
	requirePermission(auth, store, permissionCatalogSubmissionsReview, catalogReviewDecisionRouteHandler(store)).ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "review album and authors first") {
		t.Fatalf("premature track approval HTTP status=%d body=%q", response.Code, response.Body.String())
	}
	if _, err := store.approveCatalogSubmission(reviewer.ID, albumSubmission.ID, lease.LeaseToken); !errors.Is(err, errCatalogTrackReviewPrerequisite) {
		t.Fatalf("premature album approval error = %v, want author prerequisite", err)
	}
	if _, err := store.approveCatalogSubmission(reviewer.ID, authorSubmission.ID, lease.LeaseToken); err != nil {
		t.Fatalf("approve author error = %v", err)
	}
	if _, err := store.approveCatalogSubmission(reviewer.ID, albumSubmission.ID, lease.LeaseToken); err != nil {
		t.Fatalf("approve album error = %v", err)
	}
	approved, err := store.approveCatalogSubmission(reviewer.ID, trackSubmission.ID, lease.LeaseToken)
	if err != nil {
		t.Fatalf("approveCatalogSubmission() error = %v", err)
	}
	approvedTrack := approved.Entity.(track)
	if approved.Status != catalogSubmissionStatusApproved || approvedTrack.PublicationStatus != catalogPublicationPublished || approvedTrack.RequestedByUserID != nil {
		t.Fatalf("approved submission = %#v, entity = %#v", approved.catalogSubmission, approvedTrack)
	}
	assertCatalogTrackVisibility(t, store, otherUser.ID, trackID, true)
	if _, err := os.Stat(filepath.Join(store.songsDir, filepath.Base(approvedTrack.AudioFilePath))); err != nil {
		t.Fatalf("published audio stat error = %v", err)
	}
	if _, err := os.Stat(store.catalogSubmissionAudioPath(upload.StoredFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged audio remains after approval: %v", err)
	}

	items, err := store.listCatalogSubmissions(requester.ID)
	if err != nil {
		t.Fatalf("listCatalogSubmissions() error = %v", err)
	}
	if items.ImportRating != 13 {
		t.Fatalf("import rating = %d, want 13", items.ImportRating)
	}
	for _, submission := range items.Items {
		if submission.Status != catalogSubmissionStatusApproved {
			t.Fatalf("dependency submission %d status = %q, want approved", submission.ID, submission.Status)
		}
	}
}

func TestCatalogReviewLeasesExpireAndCannotBeReused(t *testing.T) {
	store := newTestTrackStore(t)
	firstRequester := mustCreateCatalogTestUser(t, store, "first-requester@example.com")
	secondRequester := mustCreateCatalogTestUser(t, store, "second-requester@example.com")
	reviewer := mustCreateCatalogTestUser(t, store, "lease-reviewer@example.com")
	otherReviewer := mustCreateCatalogTestUser(t, store, "other-lease-reviewer@example.com")
	firstSubmission, err := store.createAuthorSubmission(firstRequester.ID, upsertAuthorRequest{CurrentName: "First Pending Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.createAuthorSubmission(secondRequester.ID, upsertAuthorRequest{CurrentName: "Second Pending Artist"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.acquireCatalogReviewLease(firstRequester.ID, firstRequester.ID); !errors.Is(err, errCatalogSelfReview) {
		t.Fatalf("self-review lease error = %v, want self-review rejection", err)
	}
	staleLease, err := store.acquireCatalogReviewLease(reviewer.ID, firstRequester.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE catalog_review_leases SET expires_at = ? WHERE requester_user_id = ?`,
		formatSQLiteTime(time.Now().UTC().Add(-time.Minute)), firstRequester.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.acquireCatalogReviewLease(reviewer.ID, secondRequester.ID); err != nil {
		t.Fatalf("reviewer could not acquire a new lease after expiry: %v", err)
	}
	if _, err := store.acquireCatalogReviewLease(otherReviewer.ID, firstRequester.ID); err != nil {
		t.Fatalf("requester could not be leased after expiry: %v", err)
	}
	if _, err := store.requestCatalogSubmissionChanges(reviewer.ID, firstSubmission.ID, staleLease.LeaseToken,
		catalogReviewDecisionRequest{Message: "stale reviewer"}); !errors.Is(err, errCatalogReviewLeaseInvalid) {
		t.Fatalf("stale review decision error = %v, want invalid lease", err)
	}
}

func TestCatalogSubmissionAudioStartupCleanupKeepsReferencedFiles(t *testing.T) {
	store := newTestTrackStore(t)
	store.songsDir = t.TempDir()
	requester := mustCreateCatalogTestUser(t, store, "cleanup-requester@example.com")
	referenced := mustCreateCatalogTestUpload(t, store, requester.ID, "referenced.mp3")
	orphanPath := store.catalogSubmissionAudioPath("orphan.mp3")
	if err := os.WriteFile(orphanPath, []byte("orphan"), 0o644); err != nil {
		t.Fatal(err)
	}
	publicOrphanPath := filepath.Join(store.songsDir, "interrupted-publication.mp3")
	if err := os.Link(store.catalogSubmissionAudioPath(referenced.StoredFileName), publicOrphanPath); err != nil {
		t.Fatal(err)
	}
	if err := store.cleanupCatalogSubmissionAudio(); err != nil {
		t.Fatalf("cleanupCatalogSubmissionAudio() error = %v", err)
	}
	if _, err := os.Stat(store.catalogSubmissionAudioPath(referenced.StoredFileName)); err != nil {
		t.Fatalf("referenced staged file was removed: %v", err)
	}
	if _, err := os.Stat(orphanPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan staged file remains: %v", err)
	}
	if _, err := os.Stat(publicOrphanPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted public hard link remains: %v", err)
	}
}

func TestCatalogSubmissionRejectRemovesEntityAndKeepsTombstone(t *testing.T) {
	store := newTestTrackStore(t)
	store.songsDir = t.TempDir()
	requester := mustCreateCatalogTestUser(t, store, "reject-requester@example.com")
	reviewer := mustCreateCatalogTestUser(t, store, "reject-reviewer@example.com")
	authorItem, err := store.createAuthor(upsertAuthorRequest{CurrentName: "Published Artist"})
	if err != nil {
		t.Fatal(err)
	}
	albumItem, err := store.createAlbum(upsertAlbumRequest{Title: "Published Album", ReleaseDate: time.Now().UTC(), IsPublished: true})
	if err != nil {
		t.Fatal(err)
	}
	upload := mustCreateCatalogTestUpload(t, store, requester.ID, "rejected.mp3")
	submission, err := store.createTrackSubmission(requester.ID, createTrackSubmissionRequest{
		Name: "Rejected Song", AuthorIDs: []int64{authorItem.ID}, AlbumID: albumItem.ID, AudioUploadToken: upload.Token,
	})
	if err != nil {
		t.Fatal(err)
	}
	trackID := submission.Entity.(track).ID
	lease, err := store.acquireCatalogReviewLease(reviewer.ID, requester.ID)
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := store.rejectCatalogSubmission(reviewer.ID, submission.ID, lease.LeaseToken, catalogReviewDecisionRequest{Message: "Wrong recording"})
	if err != nil {
		t.Fatalf("rejectCatalogSubmission() error = %v", err)
	}
	if rejected.Status != catalogSubmissionStatusRejected || len(rejected.Feedback) != 1 {
		t.Fatalf("rejected submission = %#v", rejected)
	}
	if _, found, err := store.get(trackID); err != nil || found {
		t.Fatalf("get rejected track found=%v error=%v", found, err)
	}
	if _, err := os.Stat(store.catalogSubmissionAudioPath(upload.StoredFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected staged audio remains: %v", err)
	}
	items, err := store.listCatalogSubmissions(requester.ID)
	if err != nil || len(items.Items) != 1 || items.Items[0].Entity == nil {
		t.Fatalf("submission tombstone = %#v, error = %v", items, err)
	}
}

func TestRejectedCatalogDependencyBlocksTrackReview(t *testing.T) {
	for _, rejectedType := range []string{catalogSubmissionEntityAuthor, catalogSubmissionEntityAlbum} {
		t.Run(rejectedType, func(t *testing.T) {
			store := newTestTrackStore(t)
			store.songsDir = t.TempDir()
			requester := mustCreateCatalogTestUser(t, store, rejectedType+"-requester@example.com")
			reviewer := mustCreateCatalogTestUser(t, store, rejectedType+"-reviewer@example.com")
			authorSubmission, err := store.createAuthorSubmission(requester.ID, upsertAuthorRequest{CurrentName: "Artist"})
			if err != nil {
				t.Fatal(err)
			}
			authorID := authorSubmission.Entity.(author).ID
			albumSubmission, err := store.createAlbumSubmission(requester.ID, upsertAlbumRequest{
				Title: "Album", AuthorIDs: []int64{authorID}, ReleaseDate: time.Now().UTC(), IsPublished: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			albumID := albumSubmission.Entity.(album).ID
			upload := mustCreateCatalogTestUpload(t, store, requester.ID, rejectedType+"-song.mp3")
			trackSubmission, err := store.createTrackSubmission(requester.ID, createTrackSubmissionRequest{
				Name: "Song", AuthorIDs: []int64{authorID}, AlbumID: albumID, AudioUploadToken: upload.Token,
			})
			if err != nil {
				t.Fatal(err)
			}
			lease, err := store.acquireCatalogReviewLease(reviewer.ID, requester.ID)
			if err != nil {
				t.Fatal(err)
			}
			rejectedID := authorSubmission.ID
			if rejectedType == catalogSubmissionEntityAlbum {
				rejectedID = albumSubmission.ID
			}
			rejected, err := store.rejectCatalogSubmission(reviewer.ID, rejectedID, lease.LeaseToken, catalogReviewDecisionRequest{Message: "Not acceptable"})
			if err != nil || rejected.Status != catalogSubmissionStatusRejected {
				t.Fatalf("reject dependency = %#v, error = %v", rejected, err)
			}
			if err := runSQLiteStartupRepairs(context.Background(), store.db); err != nil {
				t.Fatalf("startup validation with rejected dependency: %v", err)
			}
			if _, err := store.approveCatalogSubmission(reviewer.ID, trackSubmission.ID, lease.LeaseToken); !errors.Is(err, errCatalogRejectedDependency) {
				t.Fatalf("approve dependent track error = %v", err)
			}
			if _, err := store.requestCatalogSubmissionChanges(reviewer.ID, trackSubmission.ID, lease.LeaseToken, catalogReviewDecisionRequest{Message: "Fix it"}); !errors.Is(err, errCatalogRejectedDependency) {
				t.Fatalf("request changes for dependent track error = %v", err)
			}
			if _, err := store.rejectCatalogSubmission(reviewer.ID, trackSubmission.ID, lease.LeaseToken, catalogReviewDecisionRequest{Message: "Reject it"}); !errors.Is(err, errCatalogRejectedDependency) {
				t.Fatalf("reject dependent track error = %v", err)
			}
			items, err := store.listCatalogReviewSubmissions(reviewer.ID, requester.ID, lease.LeaseToken)
			if err != nil || len(items) == 0 {
				t.Fatalf("list review submissions error = %v", err)
			}
			if _, err := store.cancelCatalogSubmission(requester.ID, trackSubmission.ID); err != nil {
				t.Fatalf("cancel blocked track error = %v", err)
			}
		})
	}
}

func mustCreateCatalogTestUser(t *testing.T, store *trackStore, email string) user {
	t.Helper()
	item, err := store.createUser(email, "test-password-hash")
	if err != nil {
		t.Fatalf("createUser(%q) error = %v", email, err)
	}
	return item
}

func mustCreateCatalogTestUpload(t *testing.T, store *trackStore, requesterID int64, originalName string) catalogSubmissionUpload {
	t.Helper()
	if err := os.MkdirAll(store.catalogSubmissionAudioDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	storedName := "staged-" + originalName
	content := []byte("test audio")
	if err := os.WriteFile(store.catalogSubmissionAudioPath(storedName), content, 0o644); err != nil {
		t.Fatal(err)
	}
	item, err := store.registerCatalogSubmissionUpload(requesterID, storedName, originalName, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func assertCatalogTrackVisibility(t *testing.T, store *trackStore, userID, trackID int64, want bool) {
	t.Helper()
	_, found, err := store.getTrackResponse(trackID, userID)
	if err != nil {
		t.Fatalf("getTrackResponse(%d, %d) error = %v", trackID, userID, err)
	}
	if found != want {
		t.Fatalf("getTrackResponse(%d, %d) found = %v, want %v", trackID, userID, found, want)
	}
}

func containsCatalogAuthor(items []author, id int64) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}

func containsCatalogAlbum(items []album, id int64) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}
