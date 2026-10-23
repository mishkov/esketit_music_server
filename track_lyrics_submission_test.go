package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func syncedSubmissionLyrics() *upsertLyricsRequest {
	end := 2000
	return &upsertLyricsRequest{Type: lyricsTypeSynced, Lines: []upsertSyncedLyricsLine{{StartMs: 100, EndMs: &end, Text: "First line"}, {StartMs: 2500, Text: "Last line"}}}
}

func TestTrackLyricsProposalLifecycle(t *testing.T) {
	for _, action := range []string{"approve", "reject", "cancel"} {
		t.Run(action, func(t *testing.T) {
			svc, owner := newMCPTestService(t)
			s := svc.store
			reviewer := mustCreateCatalogTestUser(t, s, "lyrics-reviewer@example.com")
			a, _, _ := trackNumberFixture(t, s)
			id := a.TrackIDs[0]
			req := createTrackLyricsSubmissionRequest{TrackID: id, Lyrics: syncedSubmissionLyrics()}
			sub, err := s.createTrackLyricsSubmission(owner.ID, req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.getLyrics(id); !errors.Is(err, errLyricsNotFound) {
				t.Fatalf("proposed lyrics leaked: %v", err)
			}
			if _, err := s.createTrackLyricsSubmission(owner.ID, req); !errors.Is(err, errCatalogSubmissionState) {
				t.Fatalf("duplicate: %v", err)
			}
			if _, err := s.approveCatalogSubmission(reviewer.ID, sub.ID, "bad"); !errors.Is(err, errCatalogReviewLeaseInvalid) {
				t.Fatalf("lease: %v", err)
			}
			lease, err := s.acquireCatalogReviewLease(reviewer.ID, owner.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch action {
			case "approve":
				sub, err = s.approveCatalogSubmission(reviewer.ID, sub.ID, lease.LeaseToken)
			case "reject":
				sub, err = s.rejectCatalogSubmission(reviewer.ID, sub.ID, lease.LeaseToken, catalogReviewDecisionRequest{Message: "Wrong timing"})
			case "cancel":
				sub, err = s.cancelCatalogSubmission(owner.ID, sub.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			item, found, err := s.get(id)
			if err != nil || !found || normalizePublicationStatus(item.PublicationStatus) != catalogPublicationPublished {
				t.Fatalf("published track changed: %+v %v", item, err)
			}
			lyrics, err := s.getLyrics(id)
			if action == "approve" {
				if err != nil || lyrics.Type != lyricsTypeSynced || len(lyrics.Lines) != 2 {
					t.Fatalf("lyrics: %+v %v", lyrics, err)
				}
			} else if !errors.Is(err, errLyricsNotFound) {
				t.Fatalf("unapproved lyrics: %v", err)
			}
			if _, err := s.createTrackLyricsSubmission(owner.ID, req); err != nil {
				t.Fatalf("terminal proposal blocks new proposal: %v", err)
			}

		})
	}
}

func TestMCPTrackLyricsProposalRevisionAndRetry(t *testing.T) {
	svc, owner := newMCPTestService(t)
	a, _, _ := trackNumberFixture(t, svc.store)
	args := map[string]any{"requestId": "lyrics-create", "trackId": a.TrackIDs[0], "lyrics": syncedSubmissionLyrics()}
	first := callMCP(t, svc, "submit_track_lyrics", args)
	retry := callMCP(t, svc, "submit_track_lyrics", args)
	if mcpID(first, "id") != mcpID(retry, "id") {
		t.Fatal("retry duplicated proposal")
	}
	reviewer := mustCreateCatalogTestUser(t, svc.store, "lyrics-revision-reviewer@example.com")
	lease, err := svc.store.acquireCatalogReviewLease(reviewer.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := svc.store.requestCatalogSubmissionChanges(reviewer.ID, mcpID(first, "id"), lease.LeaseToken, catalogReviewDecisionRequest{Message: "Fix timing"})
	if err != nil {
		t.Fatal(err)
	}
	// A direct edit after submission must never be overwritten by stale approval.
	replacement := upsertLyricsRequest{Type: lyricsTypePlain, PlainText: lyricsTestString("Existing published correction")}
	if _, _, err := svc.store.upsertLyrics(a.TrackIDs[0], replacement); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.resubmitCatalogSubmission(owner.ID, sub.ID); !errors.Is(err, errCatalogSubmissionDependency) {
		t.Fatalf("stale resubmit: %v", err)
	}
	patch := map[string]any{"requestId": "lyrics-patch", "submissionId": sub.ID, "expectedRevision": sub.Revision, "lyrics": syncedSubmissionLyrics()}
	updated := callMCP(t, svc, "update_track_lyrics_submission", patch)
	callMCP(t, svc, "resubmit_submission", map[string]any{"requestId": "lyrics-resubmit", "submissionId": sub.ID, "expectedRevision": updated["revision"]})
	if _, _, err := svc.store.upsertLyrics(a.TrackIDs[0], upsertLyricsRequest{Type: lyricsTypePlain, PlainText: lyricsTestString("Newer correction")}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.approveCatalogSubmission(reviewer.ID, sub.ID, lease.LeaseToken); !errors.Is(err, errCatalogSubmissionDependency) {
		t.Fatalf("stale approval: %v", err)
	}
	current, _ := svc.store.getLyrics(a.TrackIDs[0])
	if *current.PlainText != "Newer correction" {
		t.Fatal("lost correction")
	}
}

func TestTrackSubmissionWithLyricsIsAtomicAndVisibleToReviewer(t *testing.T) {
	svc, owner := newMCPTestService(t)
	s := svc.store
	a, _, artist := trackNumberFixture(t, s)
	upload := mustCreateCatalogTestUpload(t, s, owner.ID, "synced.mp3")
	req := createTrackSubmissionRequest{Name: "With synced lyrics", AuthorIDs: []int64{artist.ID}, AlbumID: a.ID, TrackNumber: 8, AudioUploadToken: upload.Token, Lyrics: syncedSubmissionLyrics()}
	bad := *req.Lyrics
	bad.Lines = []upsertSyncedLyricsLine{{StartMs: -1, Text: "Invalid"}}
	req.Lyrics = &bad
	if _, err := s.createTrackSubmission(owner.ID, req); !errors.Is(err, errInvalidLyricsPayload) {
		t.Fatalf("invalid payload accepted: %v", err)
	}
	req.Lyrics = syncedSubmissionLyrics()
	sub, err := s.createTrackSubmission(owner.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Lyrics == nil || len(sub.Lyrics.Lines) != 2 {
		t.Fatalf("missing lyrics: %+v", sub)
	}
	reviewer := mustCreateCatalogTestUser(t, s, "synced-queue@example.com")
	lease, err := s.acquireCatalogReviewLease(reviewer.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := s.listCatalogReviewSubmissions(reviewer.ID, owner.ID, lease.LeaseToken)
	if err != nil || len(queue) != 1 || queue[0].Lyrics == nil {
		t.Fatalf("queue lyrics: %+v %v", queue, err)
	}
}

func TestTrackLyricsSubmissionHTTP(t *testing.T) {
	svc, owner := newMCPTestService(t)
	a, _, _ := trackNumberFixture(t, svc.store)
	auth := newAuthManager([]byte("lyrics-http-test-secret"), 3600000000000, 3600000000000)
	token, _, _ := auth.createAccessToken(owner.ID)
	handler := requirePermission(auth, svc.store, permissionTracksSubmit, trackLyricsSubmissionHandler(svc.store, false))
	payload, _ := json.Marshal(createTrackLyricsSubmissionRequest{TrackID: a.TrackIDs[0], Lyrics: syncedSubmissionLyrics()})
	request := httptest.NewRequest(http.MethodPost, "/api/catalog-submissions/track-lyrics", strings.NewReader(string(payload)))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d %s", response.Code, response.Body)
	}
	var sub catalogSubmissionResponse
	if err := json.Unmarshal(response.Body.Bytes(), &sub); err != nil {
		t.Fatal(err)
	}
	unauth := httptest.NewRecorder()
	handler.ServeHTTP(unauth, httptest.NewRequest(http.MethodPost, "/api/catalog-submissions/track-lyrics", strings.NewReader(string(payload))))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatal(fmt.Sprint(unauth.Code))
	}
}

func lyricsTestString(value string) *string { return &value }

func TestMCPSubmitAndPatchSyncedTrackLyrics(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg required for real MP3 validation")
	}
	svc, owner := newMCPTestService(t)
	a, _, artist := trackNumberFixture(t, svc.store)
	audio, err := os.ReadFile("testdata/mcp-silence.mp3")
	if err != nil {
		t.Fatal(err)
	}
	upload := uploadMCP(t, svc, "audio", "synced-audio", "synced.mp3", audio)
	args := map[string]any{"requestId": "track-with-lyrics", "name": "Synced", "authorIds": []int64{artist.ID}, "albumId": a.ID, "trackNumber": 8, "audioUploadToken": upload["uploadToken"], "lyrics": syncedSubmissionLyrics()}
	result := callMCP(t, svc, "submit_track", args)
	if len(result["lyrics"].(map[string]any)["lines"].([]any)) != 2 {
		t.Fatal("MCP omitted lyrics")
	}
	reviewer := mustCreateCatalogTestUser(t, svc.store, "mcp-synced-reviewer@example.com")
	lease, err := svc.store.acquireCatalogReviewLease(reviewer.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := svc.store.requestCatalogSubmissionChanges(reviewer.ID, mcpID(result, "id"), lease.LeaseToken, catalogReviewDecisionRequest{Message: "Correct timing"})
	if err != nil {
		t.Fatal(err)
	}
	patched := callMCP(t, svc, "update_track_submission", map[string]any{"requestId": "rename-with-lyrics", "submissionId": sub.ID, "expectedRevision": sub.Revision, "name": "Renamed"})
	if len(patched["lyrics"].(map[string]any)["lines"].([]any)) != 2 {
		t.Fatal("metadata patch erased lyrics")
	}
	lyrics := syncedSubmissionLyrics()
	lyrics.Lines[0].Text = "Corrected first line"
	updated := callMCP(t, svc, "update_track_submission", map[string]any{"requestId": "correct-lyrics", "submissionId": sub.ID, "expectedRevision": patched["revision"], "lyrics": lyrics})
	if updated["lyrics"].(map[string]any)["lines"].([]any)[0].(map[string]any)["text"] != "Corrected first line" {
		t.Fatal("MCP lyrics patch did not apply")
	}
	// The same proposal ID must not be usable by a different requester.
	other := mustCreateCatalogTestUser(t, svc.store, "lyrics-other@example.com")
	proposal, err := svc.store.createTrackLyricsSubmission(owner.ID, createTrackLyricsSubmissionRequest{TrackID: a.TrackIDs[0], Lyrics: syncedSubmissionLyrics()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.updateTrackLyricsSubmission(other.ID, proposal.ID, updateTrackLyricsSubmissionRequest{ExpectedRevision: proposal.Revision, Lyrics: lyrics}); !errors.Is(err, errCatalogSubmissionForbidden) {
		t.Fatalf("ownership: %v", err)
	}
}
