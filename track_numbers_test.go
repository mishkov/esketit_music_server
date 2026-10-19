package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"
)

func fixtureNumberAssignments(ids []int64) []trackNumberAssignment {
	if ids == nil {
		return nil
	}
	result := make([]trackNumberAssignment, 0, len(ids))
	for i, id := range ids {
		result = append(result, trackNumberAssignment{TrackID: id, TrackNumber: i + 1})
	}
	return result
}
func assignmentTrackIDs(assignments []trackNumberAssignment) []int64 {
	ids := []int64{}
	for _, a := range assignments {
		ids = append(ids, a.TrackID)
	}
	return ids
}

func TestPersistentTrackNumbersGapsAndConflicts(t *testing.T) {
	s := newTestTrackStore(t)
	defer s.db.Close()
	admin := mustCreateCatalogTestUser(t, s, "number-admin@example.com")
	artist, err := s.createAuthor(upsertAuthorRequest{CurrentName: "Artist"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.createAlbum(upsertAlbumRequest{Title: "Numbers", ReleaseDate: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	create := func(name string, number int) (track, error) {
		return s.create(upsertTrackRequest{Name: name, AlbumID: a.ID, AuthorIDs: []int64{artist.ID}, TrackNumber: number, AudioFilePath: "/songs/test.mp3"})
	}
	tenth, err := create("Tenth", 10)
	if err != nil {
		t.Fatal(err)
	}
	first, err := create("First", 1)
	if err != nil {
		t.Fatal(err)
	}
	third, err := create("Third", 3)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := s.getAlbum(a.ID)
	if err != nil || !slices.Equal(got.TrackIDs, []int64{first.ID, third.ID, tenth.ID}) {
		t.Fatalf("derived order=%+v %v", got, err)
	}
	stored, _, _ := s.get(tenth.ID)
	if stored.TrackNumber != 10 {
		t.Fatalf("lost number=%+v", stored)
	}
	if _, err := create("Collision", 10); !errors.Is(err, errTrackNumberConflict) {
		t.Fatalf("collision=%v", err)
	}
	if _, err := create("Missing", 0); !errors.Is(err, errInvalidTrack) {
		t.Fatalf("missing=%v", err)
	}
	assignments := []trackNumberAssignment{{TrackID: first.ID, TrackNumber: 10}, {TrackID: third.ID, TrackNumber: 30}, {TrackID: tenth.ID, TrackNumber: 1}}
	result, err := s.reorderAlbumTracks(context.Background(), admin.ID, a.ID, reorderAlbumTracksRequest{Tracks: assignments, ExpectedOrderRevision: got.OrderRevision})
	if err != nil {
		t.Fatal(err)
	}
	if result.OrderRevision != got.OrderRevision+1 {
		t.Fatalf("revision=%+v", result)
	}
	got, _, _ = s.getAlbum(a.ID)
	if !slices.Equal(got.TrackIDs, []int64{tenth.ID, first.ID, third.ID}) {
		t.Fatalf("reorder=%+v", got)
	}
	// Changing a label without changing relative order must still invalidate proposals.
	assignments[1].TrackNumber = 40
	result, err = s.reorderAlbumTracks(context.Background(), admin.ID, a.ID, reorderAlbumTracksRequest{Tracks: assignments, ExpectedOrderRevision: got.OrderRevision})
	if err != nil || result.OrderRevision != got.OrderRevision+1 {
		t.Fatalf("renumber=%+v %v", result, err)
	}
	if _, err := s.reorderAlbumTracks(context.Background(), admin.ID, a.ID, reorderAlbumTracksRequest{Tracks: assignments, ExpectedOrderRevision: got.OrderRevision}); !errors.Is(err, errTrackNumberStale) {
		t.Fatalf("stale=%v", err)
	}
}

func TestPersistentTrackNumbersMigration(t *testing.T) {
	db, err := openSQLiteDB(filepath.Join(t.TempDir(), "numbers.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	migrations := defaultSchemaMigrations()
	if err := runSQLiteMigrations(ctx, db, migrations[:11]); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users(id,email,password_hash,created_at) VALUES(1,'number@example.test','hash','2026-01-01T00:00:00Z')`,
		`INSERT INTO user_roles(user_id,role_id,assigned_at) SELECT 1,id,'2026-01-01T00:00:00Z' FROM roles WHERE name='admin'`,
		`INSERT INTO authors(id,current_name,photos_json) VALUES(1,'Artist','[]')`,
		`INSERT INTO albums(id,title,cover_image_path,author_ids_json,release_date,is_published,track_ids_json,additional_info_json) VALUES(1,'Album','','[1]','2026-01-01T00:00:00Z',1,'[2,1]','[]')`,
		`INSERT INTO tracks(id,name,author_ids_json,album_id,audio_file_path,additional_info_json,source_metadata_json,created_at) VALUES(1,'Second','[1]',1,'/songs/second.mp3','[]','[]','2026-01-01T00:00:00Z'),(2,'First','[1]',1,'/songs/first.mp3','[]','[]','2026-01-01T00:00:00Z')`,
		`INSERT INTO catalog_submission_uploads(token,requester_user_id,stored_file_name,original_file_name,created_at,claimed_track_id) VALUES('preserved',1,'staged.mp3','staged.mp3','2026-01-01T00:00:00Z',1)`,
		`INSERT INTO catalog_submissions(entity_type,entity_id,requester_user_id,status,snapshot_json,created_at,submitted_at,updated_at) VALUES('album_track_order',1,1,'pending_review','{"albumId":1,"baseTrackIds":[2,1],"trackIds":[1,2],"expectedOrderRevision":1,"reason":"Release"}','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := runSQLiteMigrations(ctx, db, migrations); err != nil {
		t.Fatal(err)
	}
	repos := newDomainRepositories(db)
	tracks, err := repos.catalog.ListAlbumTracks(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if tracks[0].ID != 2 || tracks[0].TrackNumber != 1 || tracks[1].ID != 1 || tracks[1].TrackNumber != 2 {
		t.Fatalf("migration numbers=%+v", tracks)
	}
	upload, found, err := repos.submissions.FindSubmissionUpload(ctx, "preserved")
	if err != nil || !found || upload.ClaimedTrackID == nil || *upload.ClaimedTrackID != 1 {
		t.Fatalf("upload lost=%+v %v", upload, err)
	}
	sub, found, err := repos.submissions.FindSubmissionByID(ctx, 1)
	if err != nil || !found {
		t.Fatal(err)
	}
	proposal, err := decodeAlbumTrackOrderProposal(sub)
	if err != nil || !reflect.DeepEqual(proposal.Tracks, []trackNumberAssignment{{TrackID: 1, TrackNumber: 1}, {TrackID: 2, TrackNumber: 2}}) {
		t.Fatalf("proposal=%+v %v", proposal, err)
	}
	var columns int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('albums') WHERE name='track_ids_json'`).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("legacy column=%d %v", columns, err)
	}
	if err := runSQLiteStartupRepairs(ctx, db); err != nil {
		t.Fatal(err)
	}
}

func fixtureNextTrackNumber(t *testing.T, s *trackStore, albumID int64) int {
	t.Helper()
	tracks, err := s.catalogRepository.ListAlbumTracks(context.Background(), albumID)
	if err != nil {
		t.Fatal(err)
	}
	number := 1
	for _, track := range tracks {
		if track.TrackNumber >= number {
			number = track.TrackNumber + 1
		}
	}
	return number
}

func TestReviewedTrackNumbersPreserveGaps(t *testing.T) {
	s, admin, owner, a, request := orderProposalFixture(t)
	request.Tracks[0].TrackNumber = 10
	request.Tracks[1].TrackNumber = 4
	request.Tracks[2].TrackNumber = 1
	sub, err := s.createAlbumTrackOrderSubmission(owner.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(sub.Entity)
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Tracks       []trackNumberAssignment   `json:"tracks"`
		TrackDetails []trackNumberTrackSummary `json:"trackDetails"`
	}
	if err := json.Unmarshal(raw, &view); err != nil || !reflect.DeepEqual(view.Tracks, request.Tracks) || len(view.TrackDetails) != 3 {
		t.Fatalf("proposal wire shape: %s %v", raw, err)
	}
	assertTrackNumber(t, s, a.ID, a.TrackIDs, a.OrderRevision)
	lease, err := s.acquireCatalogReviewLease(admin.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.approveCatalogSubmission(admin.ID, sub.ID, lease.LeaseToken); err != nil {
		t.Fatal(err)
	}
	tracks, err := s.catalogRepository.ListAlbumTracks(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(assignmentsForTracks(tracks), []trackNumberAssignment{request.Tracks[2], request.Tracks[1], request.Tracks[0]}) {
		t.Fatalf("approved numbers=%+v", tracks)
	}
}

func TestMCPRequiredGappedNumbers(t *testing.T) {
	s, _ := newMCPTestService(t)
	_, _, artist := trackNumberFixture(t, s.store)
	album, err := s.store.createAlbum(upsertAlbumRequest{Title: "MCP gaps", ReleaseDate: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	audio, err := os.ReadFile("testdata/mcp-silence.mp3")
	if err != nil {
		t.Fatal(err)
	}
	upload := uploadMCP(t, s, "audio", "number-audio", "track.mp3", audio)
	args := map[string]any{"requestId": "number-ten", "name": "Tenth", "authorIds": []int64{artist.ID}, "albumId": album.ID, "audioUploadToken": upload["uploadToken"]}
	raw, _ := json.Marshal(args)
	if _, err := s.call(context.Background(), "submit_track", raw, true); err == nil {
		t.Fatal("missing track number accepted")
	}
	args["trackNumber"] = 10
	args["requestId"] = "number-ten-corrected"
	tenth := callMCP(t, s, "submit_track", args)
	if tenth["entity"].(map[string]any)["trackNumber"] != float64(10) {
		t.Fatalf("number changed: %v", tenth)
	}
	next := uploadMCP(t, s, "audio", "upload-number-first", "first.mp3", audio)
	args["requestId"], args["name"], args["trackNumber"], args["audioUploadToken"] = "number-first", "First", 1, next["uploadToken"]
	callMCP(t, s, "submit_track", args)
	third := uploadMCP(t, s, "audio", "upload-number-collision", "duplicate.mp3", audio)
	args["requestId"], args["audioUploadToken"], args["trackNumber"] = "number-collision", third["uploadToken"], 10
	raw, _ = json.Marshal(args)
	if _, err := s.call(context.Background(), "submit_track", raw, true); !errors.Is(err, errTrackNumberConflict) || mcpError(err).Code != "track_number_conflict" {
		t.Fatalf("collision=%v", err)
	}
	stored, _, err := s.store.get(mcpID(tenth, "entityId"))
	if err != nil || stored.TrackNumber != 10 {
		t.Fatalf("shifted=%+v %v", stored, err)
	}
}

func TestPersistentTrackNumberMigrationRejectsAmbiguousMembership(t *testing.T) {
	for _, members := range []string{`[99]`, `[1,1]`, `[]`} {
		t.Run(members, func(t *testing.T) {
			db, err := openSQLiteDB(filepath.Join(t.TempDir(), "invalid.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx := context.Background()
			migrations := defaultSchemaMigrations()
			if err := runSQLiteMigrations(ctx, db, migrations[:11]); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO albums(id,title,cover_image_path,author_ids_json,release_date,is_published,track_ids_json,additional_info_json) VALUES(1,'Invalid','','[]','2026-01-01T00:00:00Z',1,?,'[]')`, members); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO tracks(id,name,author_ids_json,album_id,audio_file_path,additional_info_json,source_metadata_json,created_at) VALUES(1,'First','[1]',1,'/songs/test.mp3','[]','[]','2026-01-01T00:00:00Z')`); err != nil {
				t.Fatal(err)
			}
			if err := runSQLiteMigrations(ctx, db, migrations); err == nil {
				t.Fatal("ambiguous membership migrated")
			}
			var original string
			if err := db.QueryRow(`SELECT track_ids_json FROM albums WHERE id=1`).Scan(&original); err != nil || original != members {
				t.Fatalf("migration did not roll back: %q %v", original, err)
			}
			var applied int
			if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=12`).Scan(&applied); err != nil || applied != 0 {
				t.Fatalf("migration marked applied: %d %v", applied, err)
			}
		})
	}
}
