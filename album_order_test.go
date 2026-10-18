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

func albumOrderFixture(t *testing.T, store *trackStore) (album, album, author) {
	t.Helper()
	artist, err := store.createAuthor(upsertAuthorRequest{CurrentName: "Ordering Artist"})
	if err != nil {
		t.Fatal(err)
	}
	create := func(title string) album {
		item, err := store.createAlbum(upsertAlbumRequest{Title: title, ReleaseDate: time.Now().UTC(), IsPublished: false})
		if err != nil {
			t.Fatal(err)
		}
		if item.OrderRevision != 1 {
			t.Fatalf("new album revision = %d", item.OrderRevision)
		}
		return item
	}
	a, b := create("Album A"), create("Album B")
	for i, id := range []int64{a.ID, a.ID, a.ID, b.ID} {
		if _, err := store.create(upsertTrackRequest{Name: fmt.Sprintf("Track %d", i), AlbumID: id, AuthorIDs: []int64{artist.ID}, AudioFilePath: fmt.Sprintf("/songs/order-%d.mp3", i)}); err != nil {
			t.Fatal(err)
		}
	}
	a, _, _ = store.getAlbum(a.ID)
	b, _, _ = store.getAlbum(b.ID)
	return a, b, artist
}

func assertAlbumOrder(t *testing.T, store *trackStore, id int64, ids []int64, revision int64) album {
	t.Helper()
	a, found, err := store.getAlbum(id)
	if err != nil || !found || !slices.Equal(a.TrackIDs, ids) || a.OrderRevision != revision {
		t.Fatalf("album = %+v, found=%v, err=%v; want ids=%v revision=%d", a, found, err, ids, revision)
	}
	return a
}

func TestAlbumReorderHTTP(t *testing.T) {
	store := newTestTrackStore(t)
	t.Cleanup(func() { _ = store.db.Close() })
	admin := mustCreateCatalogTestUser(t, store, "ordering-admin@example.com")
	listener := mustCreateCatalogTestUser(t, store, "ordering-listener@example.com")
	a, other, _ := albumOrderFixture(t, store)
	auth := newAuthManager([]byte("album-reordering-test-secret"), time.Hour, time.Hour)
	mux := http.NewServeMux()
	mux.Handle("PUT /api/albums/", requirePermission(auth, store, permissionAlbumsUpdate, updateAlbumByRouteHandler(store)))
	mux.Handle("PUT /api/albums/{albumId}/track-order", requirePermission(auth, store, permissionAlbumsReorderTracks, reorderAlbumTracksHandler(store)))
	invoke := func(userID int64, ids []int64, revision int64) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(reorderAlbumTracksRequest{ids, revision})
		req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/albums/%d/track-order", a.ID), bytes.NewReader(raw))
		if userID != 0 {
			token, _, err := auth.createAccessToken(userID)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	if r := invoke(0, a.TrackIDs, a.OrderRevision); r.Code != 401 {
		t.Fatalf("unauthenticated: %d %s", r.Code, r.Body.String())
	}
	if r := invoke(listener.ID, a.TrackIDs, a.OrderRevision); r.Code != 403 {
		t.Fatalf("listener: %d %s", r.Code, r.Body.String())
	}
	if _, err := store.reorderAlbumTracks(context.Background(), listener.ID, a.ID, reorderAlbumTracksRequest{a.TrackIDs, a.OrderRevision}); !errors.Is(err, errAlbumOrderForbidden) {
		t.Fatalf("transaction permission check: %v", err)
	}
	// The dedicated permission suffices without albums.update or any other role.
	role, err := store.createAccessRole(admin.ID, upsertAccessRoleRequest{Name: "album-orderer"})
	if err != nil {
		t.Fatal(err)
	}
	p := permissionByCode(t, store, permissionAlbumsReorderTracks)
	if _, err := store.replaceRolePermissions(admin.ID, role.ID, []int64{p.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.replaceUserRoles(admin.ID, listener.ID, []int64{role.ID}); err != nil {
		t.Fatal(err)
	}
	for name, ids := range map[string][]int64{
		"duplicate": {a.TrackIDs[0], a.TrackIDs[0], a.TrackIDs[2]},
		"missing":   a.TrackIDs[:2], "additional": append(slices.Clone(a.TrackIDs), other.TrackIDs[0]),
		"foreign":     {a.TrackIDs[0], a.TrackIDs[1], other.TrackIDs[0]},
		"unknown":     {a.TrackIDs[0], a.TrackIDs[1], 999999},
		"nonpositive": {a.TrackIDs[0], a.TrackIDs[1], 0}, "null": nil,
	} {
		t.Run(name, func(t *testing.T) {
			if r := invoke(listener.ID, ids, a.OrderRevision); r.Code != 400 {
				t.Fatalf("invalid: %d %s", r.Code, r.Body.String())
			}
			assertAlbumOrder(t, store, a.ID, a.TrackIDs, a.OrderRevision)
		})
	}
	if r := invoke(listener.ID, a.TrackIDs, 0); r.Code != 400 {
		t.Fatalf("missing revision: %d", r.Code)
	}
	if r := invoke(listener.ID, a.TrackIDs, a.OrderRevision-1); r.Code != 409 {
		t.Fatalf("stale: %d", r.Code)
	}
	reversed := slices.Clone(a.TrackIDs)
	slices.Reverse(reversed)
	r := invoke(listener.ID, reversed, a.OrderRevision)
	if r.Code != 200 {
		t.Fatalf("reorder: %d %s", r.Code, r.Body.String())
	}
	var result albumTrackOrder
	if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.AlbumID != a.ID || !slices.Equal(result.TrackIDs, reversed) || result.OrderRevision != a.OrderRevision+1 {
		t.Fatalf("receipt = %+v", result)
	}
	after := assertAlbumOrder(t, store, a.ID, reversed, a.OrderRevision+1)
	before := a
	before.TrackIDs = reversed
	before.OrderRevision++
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("reorder changed metadata: before=%+v after=%+v", before, after)
	}
	if r := invoke(listener.ID, reversed, after.OrderRevision); r.Code != 200 {
		t.Fatalf("no-op: %d %s", r.Code, r.Body.String())
	}
	assertAlbumOrder(t, store, a.ID, reversed, after.OrderRevision)
	if r := invoke(listener.ID, reversed, a.OrderRevision); r.Code != 409 {
		t.Fatalf("stale no-op: %d", r.Code)
	}
	for _, status := range []string{catalogPublicationPendingReview, catalogPublicationChangesRequested, catalogPublicationRejected} {
		if _, err := store.db.Exec(`UPDATE albums SET publication_status=?, is_published=1 WHERE id=?`, status, a.ID); err != nil {
			t.Fatal(err)
		}
		if r := invoke(admin.ID, reversed, after.OrderRevision); r.Code != 409 {
			t.Fatalf("%s: %d %s", status, r.Code, r.Body.String())
		}
		assertAlbumOrder(t, store, a.ID, reversed, after.OrderRevision)
	}
}

func TestAlbumOrderMembershipAndMetadataRevisions(t *testing.T) {
	store := newTestTrackStore(t)
	t.Cleanup(func() { _ = store.db.Close() })
	a, b, artist := albumOrderFixture(t, store)
	if a.OrderRevision != 4 || b.OrderRevision != 2 {
		t.Fatalf("creation revisions: %d %d", a.OrderRevision, b.OrderRevision)
	}
	request := func(ids []int64) upsertAlbumRequest {
		return upsertAlbumRequest{Title: "Changed metadata", ReleaseDate: a.ReleaseDate, TrackIDs: ids}
	}
	updated, _, err := store.updateAlbum(a.ID, request(a.TrackIDs))
	if err != nil || updated.OrderRevision != a.OrderRevision {
		t.Fatalf("metadata: %+v %v", updated, err)
	}
	ids := slices.Clone(a.TrackIDs)
	slices.Reverse(ids)
	updated, _, err = store.updateAlbum(a.ID, request(ids))
	if err != nil || updated.OrderRevision != a.OrderRevision+1 {
		t.Fatalf("legacy order: %+v %v", updated, err)
	}
	a = updated
	target, _, _ := store.get(a.TrackIDs[1])
	index := 1
	trackReq := upsertTrackRequest{Name: "Renamed track", AuthorIDs: target.AuthorIDs, AlbumID: a.ID, AlbumOrder: index, AudioFilePath: target.AudioFilePath}
	if _, _, err := store.update(target.ID, trackReq); err != nil {
		t.Fatal(err)
	}
	assertAlbumOrder(t, store, a.ID, a.TrackIDs, a.OrderRevision)
	trackReq.AlbumOrder = 0
	if _, _, err := store.update(target.ID, trackReq); err != nil {
		t.Fatal(err)
	}
	newOrder := []int64{target.ID, a.TrackIDs[0], a.TrackIDs[2]}
	a = assertAlbumOrder(t, store, a.ID, newOrder, a.OrderRevision+1)
	trackReq.AlbumID = b.ID
	if _, _, err := store.update(target.ID, trackReq); err != nil {
		t.Fatal(err)
	}
	a = assertAlbumOrder(t, store, a.ID, newOrder[1:], a.OrderRevision+1)
	b = assertAlbumOrder(t, store, b.ID, append([]int64{target.ID}, b.TrackIDs...), b.OrderRevision+1)
	// Legacy album updates can move membership; both albums must advance.
	if _, _, err := store.updateAlbum(a.ID, request(append(slices.Clone(a.TrackIDs), target.ID))); err != nil {
		t.Fatal(err)
	}
	a = assertAlbumOrder(t, store, a.ID, append(slices.Clone(a.TrackIDs), target.ID), a.OrderRevision+1)
	b = assertAlbumOrder(t, store, b.ID, b.TrackIDs[1:], b.OrderRevision+1)
	// Creating an album with existing tracks starts at 1 and advances the source.
	newAlbum, err := store.createAlbum(upsertAlbumRequest{Title: "Moved on creation", ReleaseDate: time.Now().UTC(), TrackIDs: []int64{target.ID}})
	if err != nil || newAlbum.OrderRevision != 1 {
		t.Fatalf("create with membership: %+v %v", newAlbum, err)
	}
	a = assertAlbumOrder(t, store, a.ID, a.TrackIDs[:2], a.OrderRevision+1)
	if deleted, err := store.delete(a.TrackIDs[0]); err != nil || !deleted {
		t.Fatalf("delete: %v %v", deleted, err)
	}
	assertAlbumOrder(t, store, a.ID, a.TrackIDs[1:], a.OrderRevision+1)
	// Empty [] and nil are the same membership and never advance a revision.
	empty, err := store.createAlbum(upsertAlbumRequest{Title: "Empty", ReleaseDate: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.updateAlbum(empty.ID, upsertAlbumRequest{Title: "Empty renamed", ReleaseDate: empty.ReleaseDate, TrackIDs: []int64{}, AuthorIDs: []int64{artist.ID}})
	if err != nil {
		t.Fatal(err)
	}
	assertAlbumOrder(t, store, empty.ID, nil, 1)
}

func TestAlbumOrderPendingTrackVisibilityAndRemoval(t *testing.T) {
	for _, action := range []string{"cancel", "reject", "approve"} {
		t.Run(action, func(t *testing.T) {
			store := newTestTrackStore(t)
			t.Cleanup(func() { _ = store.db.Close() })
			store.songsDir = t.TempDir()
			admin := mustCreateCatalogTestUser(t, store, "admin@example.com")
			owner := mustCreateCatalogTestUser(t, store, "owner@example.com")
			setTestUserRole(t, store, owner.ID, roleAdmin)
			a, _, artist := albumOrderFixture(t, store)
			upload := mustCreateCatalogTestUpload(t, store, owner.ID, "pending.mp3")
			sub, err := store.createTrackSubmission(owner.ID, createTrackSubmissionRequest{Name: "Pending", AlbumID: a.ID, AlbumOrder: len(a.TrackIDs), AuthorIDs: []int64{artist.ID}, AudioUploadToken: upload.Token})
			if err != nil {
				t.Fatal(err)
			}
			pendingID := sub.Entity.(track).ID
			full := append(slices.Clone(a.TrackIDs), pendingID)
			a = assertAlbumOrder(t, store, a.ID, full, a.OrderRevision+1)
			for _, viewer := range []int64{0, admin.ID, owner.ID} {
				visible, found, err := store.getVisibleAlbum(a.ID, viewer)
				if err != nil || !found || visible.OrderRevision != a.OrderRevision {
					t.Fatalf("visible album: %+v %v", visible, err)
				}
				if slices.Contains(visible.TrackIDs, pendingID) != (viewer == owner.ID) {
					t.Fatalf("pending ID visibility: %+v", visible)
				}
				if viewer != 0 {
					item, err := mcpCatalogItem(context.Background(), newDomainRepositories(store.db), viewer, "album", a.ID)
					if err != nil || !slices.Equal(item.(album).TrackIDs, visible.TrackIDs) {
						t.Fatalf("MCP visibility: %+v %v", item, err)
					}
				}
			}
			for _, ids := range [][]int64{a.TrackIDs[:3], full} {
				_, err := store.reorderAlbumTracks(context.Background(), admin.ID, a.ID, reorderAlbumTracksRequest{ids, a.OrderRevision})
				if !errors.Is(err, errAlbumOrderHidden) {
					t.Fatalf("hidden membership: %v", err)
				}
			}
			slices.Reverse(full)
			if _, err := store.reorderAlbumTracks(context.Background(), owner.ID, a.ID, reorderAlbumTracksRequest{full, a.OrderRevision}); err != nil {
				t.Fatal(err)
			}
			a = assertAlbumOrder(t, store, a.ID, full, a.OrderRevision+1)
			currentSub, _, err := newDomainRepositories(store.db).submissions.FindSubmissionByID(context.Background(), sub.ID)
			if err != nil || currentSub.Revision != sub.Revision {
				t.Fatalf("order changed submission revision: %+v %v", currentSub, err)
			}
			if action == "cancel" {
				_, err = store.cancelCatalogSubmission(owner.ID, sub.ID)
			} else {
				lease, e := store.acquireCatalogReviewLease(admin.ID, owner.ID)
				if e != nil {
					t.Fatal(e)
				}
				if action == "reject" {
					_, err = store.rejectCatalogSubmission(admin.ID, sub.ID, lease.LeaseToken, catalogReviewDecisionRequest{Message: "Rejected"})
				} else {
					_, err = store.approveCatalogSubmission(admin.ID, sub.ID, lease.LeaseToken)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if action == "approve" {
				assertAlbumOrder(t, store, a.ID, full, a.OrderRevision)
			} else {
				assertAlbumOrder(t, store, a.ID, full[1:], a.OrderRevision+1)
			}
		})
	}
}

func TestAlbumOrderMCPPersistedRetriesAndRollback(t *testing.T) {
	s, acting := newMCPTestService(t)
	a, _, _ := albumOrderFixture(t, s.store)
	ids := slices.Clone(a.TrackIDs)
	slices.Reverse(ids)
	args := map[string]any{"requestId": "album-reorder", "albumId": a.ID, "trackIds": ids, "expectedOrderRevision": a.OrderRevision}
	raw, _ := json.Marshal(args)
	if _, err := s.call(context.Background(), "reorder_album_tracks", raw, true); !errors.Is(err, errMCPForbidden) {
		t.Fatalf("permission: %v", err)
	}
	setTestUserRole(t, s.store, acting.ID, roleAdmin)
	originalUOW := s.store.unitOfWork
	s.store.unitOfWork = failingMCPUnitOfWork{originalUOW}
	if _, err := s.call(context.Background(), "reorder_album_tracks", raw, true); !errors.Is(err, errRepositoryPersistence) {
		t.Fatalf("receipt failure: %v", err)
	}
	s.store.unitOfWork = originalUOW
	assertAlbumOrder(t, s.store, a.ID, a.TrackIDs, a.OrderRevision)
	first := callMCP(t, s, "reorder_album_tracks", args)
	assertAlbumOrder(t, s.store, a.ID, ids, a.OrderRevision+1)
	protocolBody, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "reorder_album_tracks", "arguments": args}})
	protocolReq := httptest.NewRequest(http.MethodPost, "http://localhost/mcp", bytes.NewReader(protocolBody))
	protocolReq.Header.Set("Authorization", "Bearer "+s.accessToken)
	protocolReq.Header.Set("Content-Type", "application/json")
	protocolReq.Header.Set("Accept", "application/json, text/event-stream")
	protocolReq.Header.Set("MCP-Protocol-Version", "2025-11-25")
	protocolRec := httptest.NewRecorder()
	s.handler().ServeHTTP(protocolRec, protocolReq)
	var protocolResult struct {
		Result struct {
			IsError           bool `json:"isError"`
			StructuredContent struct {
				OK   bool            `json:"ok"`
				Data albumTrackOrder `json:"data"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(protocolRec.Body.Bytes(), &protocolResult); err != nil || protocolRec.Code != 200 || protocolResult.Result.IsError || !protocolResult.Result.StructuredContent.OK || protocolResult.Result.StructuredContent.Data.OrderRevision != a.OrderRevision+1 {
		t.Fatalf("MCP protocol: %d %s %v", protocolRec.Code, protocolRec.Body.String(), err)
	}
	read := callMCP(t, s, "get_catalog_item", map[string]any{"entityType": "album", "entityId": a.ID})
	if mcpID(read, "orderRevision") != a.OrderRevision+1 || !reflect.DeepEqual(read["trackIds"], first["trackIds"]) {
		t.Fatalf("read=%v receipt=%v", read, first)
	}
	args["expectedOrderRevision"] = a.OrderRevision + 1
	changed, _ := json.Marshal(args)
	if _, err := s.call(context.Background(), "reorder_album_tracks", changed, true); !errors.Is(err, errMCPRequestConflict) {
		t.Fatalf("changed payload: %v", err)
	}
	args["requestId"] = "noop"
	noop := callMCP(t, s, "reorder_album_tracks", args)
	if mcpID(noop, "orderRevision") != a.OrderRevision+1 {
		t.Fatalf("no-op receipt: %v", noop)
	}
	args["requestId"] = "second-order"
	args["trackIds"] = a.TrackIDs
	callMCP(t, s, "reorder_album_tracks", args)
	// Restart against the same SQLite file, then replay the original receipt even
	// though both its expected revision and resulting order are now historical.
	var seq int
	var name, path string
	if err := s.store.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	reopened, err := newTrackStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.db.Close() })
	fresh := &mcpService{store: reopened}
	result, err := fresh.call(context.Background(), "reorder_album_tracks", raw, true)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	var retry map[string]any
	_ = json.Unmarshal(encoded, &retry)
	if !reflect.DeepEqual(retry, first) {
		t.Fatalf("retry=%v first=%v", retry, first)
	}
	assertAlbumOrder(t, reopened, a.ID, a.TrackIDs, a.OrderRevision+2)
	args["requestId"] = "stale"
	args["expectedOrderRevision"] = a.OrderRevision
	stale, _ := json.Marshal(args)
	if _, err := fresh.call(context.Background(), "reorder_album_tracks", stale, true); !errors.Is(err, errAlbumOrderStale) || mcpError(err).Fields[0] != "expectedOrderRevision" {
		t.Fatalf("MCP stale: %v", err)
	}
	args["requestId"] = "invalid-list"
	args["expectedOrderRevision"] = a.OrderRevision + 2
	args["trackIds"] = []int64{a.TrackIDs[0], a.TrackIDs[0], a.TrackIDs[2]}
	invalid, _ := json.Marshal(args)
	if _, err := fresh.call(context.Background(), "reorder_album_tracks", invalid, true); !errors.Is(err, errInvalidAlbum) {
		t.Fatalf("MCP invalid list: %v", err)
	}
	pending, err := reopened.createAlbumSubmission(acting.ID, upsertAlbumRequest{Title: "Pending order", ReleaseDate: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	args["requestId"] = "pending-order"
	args["albumId"] = pending.EntityID
	args["trackIds"] = []int64{}
	args["expectedOrderRevision"] = 1
	unapproved, _ := json.Marshal(args)
	if _, err := fresh.call(context.Background(), "reorder_album_tracks", unapproved, true); !errors.Is(err, errAlbumOrderState) || mcpError(err).Code != "invalid_state" {
		t.Fatalf("MCP unapproved: %v", err)
	}
}

func TestAlbumOrderEmptyAndInconsistentMembership(t *testing.T) {
	store := newTestTrackStore(t)
	t.Cleanup(func() { _ = store.db.Close() })
	admin := mustCreateCatalogTestUser(t, store, "empty-admin@example.com")
	empty, err := store.createAlbum(upsertAlbumRequest{Title: "Empty", ReleaseDate: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.reorderAlbumTracks(context.Background(), admin.ID, empty.ID, reorderAlbumTracksRequest{[]int64{}, 1})
	if err != nil || result.OrderRevision != 1 || result.TrackIDs == nil {
		t.Fatalf("empty no-op: %+v %v", result, err)
	}
	if _, err := store.reorderAlbumTracks(context.Background(), admin.ID, 999999, reorderAlbumTracksRequest{[]int64{}, 1}); !errors.Is(err, errRepositoryNotFound) {
		t.Fatalf("missing album: %v", err)
	}
	a, b, _ := albumOrderFixture(t, store)
	for _, stored := range [][]int64{a.TrackIDs[:2], {a.TrackIDs[0], a.TrackIDs[0], a.TrackIDs[2]}, {a.TrackIDs[0], a.TrackIDs[1], b.TrackIDs[0]}} {
		encoded, _ := json.Marshal(stored)
		if _, err := store.db.Exec(`UPDATE albums SET track_ids_json=? WHERE id=?`, string(encoded), a.ID); err != nil {
			t.Fatal(err)
		}
		for _, requested := range [][]int64{stored, a.TrackIDs} {
			if _, err := store.reorderAlbumTracks(context.Background(), admin.ID, a.ID, reorderAlbumTracksRequest{requested, a.OrderRevision}); !errors.Is(err, errInvalidAlbum) {
				t.Fatalf("inconsistent membership accepted: stored=%v requested=%v error=%v", stored, requested, err)
			}
		}
		assertAlbumOrder(t, store, a.ID, stored, a.OrderRevision)
	}
}

func TestAlbumOrderMigrationPreservesExistingOrder(t *testing.T) {
	db, err := openSQLiteDB(filepath.Join(t.TempDir(), "pre-order.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	migrations := defaultSchemaMigrations()
	if err := runSQLiteMigrations(ctx, db, migrations[:9]); err != nil {
		t.Fatal(err)
	}
	// Simulate an older installation where this permission did not exist yet.
	if _, err := db.Exec(`DELETE FROM permissions WHERE code=?`, permissionAlbumsReorderTracks); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO albums (id,title,cover_image_path,author_ids_json,release_date,is_published,track_ids_json,additional_info_json) VALUES (42,'Existing','','[]',?,0,'[103, 101, 102]','[]')`, formatSQLiteTime(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	if err := runSQLiteMigrations(ctx, db, migrations); err != nil {
		t.Fatal(err)
	}
	var raw string
	var revision int64
	if err := db.QueryRow(`SELECT track_ids_json,order_revision FROM albums WHERE id=42`).Scan(&raw, &revision); err != nil {
		t.Fatal(err)
	}
	if raw != "[103, 101, 102]" || revision != 1 {
		t.Fatalf("migration repaired order or wrong revision: %s %d", raw, revision)
	}
	var grants int
	if err := db.QueryRow(`SELECT COUNT(*) FROM role_permissions JOIN roles ON roles.id=role_id JOIN permissions ON permissions.id=permission_id WHERE roles.name=? AND permissions.code=?`, roleAdmin, permissionAlbumsReorderTracks).Scan(&grants); err != nil || grants != 1 {
		t.Fatalf("admin grant=%d %v", grants, err)
	}
}

func TestAlbumOrderConcurrentWriters(t *testing.T) {
	store := newTestTrackStore(t)
	t.Cleanup(func() { _ = store.db.Close() })
	admin := mustCreateCatalogTestUser(t, store, "concurrent-admin@example.com")
	a, _, _ := albumOrderFixture(t, store)
	orders := [][]int64{{a.TrackIDs[1], a.TrackIDs[0], a.TrackIDs[2]}, {a.TrackIDs[2], a.TrackIDs[1], a.TrackIDs[0]}}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range orders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = store.reorderAlbumTracks(context.Background(), admin.ID, a.ID, reorderAlbumTracksRequest{orders[i], a.OrderRevision})
		}(i)
	}
	wg.Wait()
	winner := -1
	for i, err := range errs {
		if err == nil {
			if winner != -1 {
				t.Fatal("both writes succeeded")
			}
			winner = i
		} else if !errors.Is(err, errAlbumOrderStale) && !errors.Is(err, errRepositoryConflict) {
			t.Fatalf("unexpected concurrency error: %v", err)
		}
	}
	if winner < 0 {
		t.Fatalf("no writer succeeded: %v", errs)
	}
	assertAlbumOrder(t, store, a.ID, orders[winner], a.OrderRevision+1)
}
