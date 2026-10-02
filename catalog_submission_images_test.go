package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCatalogSubmissionImageFiles(t *testing.T) {
	for _, entity := range []string{"authors", "albums"} {
		t.Run(entity, func(t *testing.T) {
			store := newTestTrackStore(t)
			t.Cleanup(func() { _ = store.db.Close() })
			dir := t.TempDir()
			requester := mustCreateCatalogTestUser(t, store, entity+"-image-requester@example.com")
			reviewer := mustCreateCatalogTestUser(t, store, entity+"-image-reviewer@example.com")
			setTestUserRole(t, store, reviewer.ID, roleAdmin)
			field, prefix := "photos", "/api/author-photos/"
			metadata := `{"currentName":"Image artist"}`
			create := createAuthorSubmissionHandler(store, dir)
			update := updateAuthorSubmissionHandler(store, dir)
			serve := getAuthorPhotoHandler(dir)
			if entity == "albums" {
				field, prefix = "cover", "/api/album-covers/"
				metadata = `{"title":"Image album","releaseDate":"2026-01-01T00:00:00Z","isPublished":true}`
				create = createAlbumSubmissionHandler(store, dir)
				update = updateAlbumSubmissionHandler(store, dir)
				serve = getAlbumCoverHandler(dir)
			}
			endpoint := "/api/catalog-submissions/" + entity
			imageData := catalogTestPNG(t, 2, 2)
			req := catalogImageMultipartRequest(t, http.MethodPost, endpoint, metadata, field, imageData)
			rec := catalogImageServe(create, req, requester.ID)
			if rec.Code != http.StatusCreated {
				t.Fatalf("create status = %d: %s", rec.Code, rec.Body.String())
			}
			var submission catalogSubmissionResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &submission); err != nil {
				t.Fatal(err)
			}
			imagePath := catalogTestSnapshotImage(t, submission.Snapshot, entity)
			if !strings.HasPrefix(imagePath, prefix+"image-") || !strings.HasSuffix(imagePath, ".png") {
				t.Fatalf("image path = %q", imagePath)
			}
			fileResponse := httptest.NewRecorder()
			serve.ServeHTTP(fileResponse, httptest.NewRequest(http.MethodGet, imagePath, nil))
			if fileResponse.Code != http.StatusOK {
				t.Fatalf("serve image status = %d", fileResponse.Code)
			}
			if _, _, err := image.Decode(bytes.NewReader(fileResponse.Body.Bytes())); err != nil {
				t.Fatalf("stored image: %v", err)
			}
			lease, err := store.acquireCatalogReviewLease(reviewer.ID, requester.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.requestCatalogSubmissionChanges(reviewer.ID, submission.ID, lease.LeaseToken, catalogReviewDecisionRequest{Message: "Update metadata"}); err != nil {
				t.Fatal(err)
			}
			updateEndpoint := fmt.Sprintf("%s/%d", endpoint, submission.EntityID)
			// Editing metadata without a new image preserves the uploaded image.
			rec = catalogImageServe(update, httptest.NewRequest(http.MethodPut, updateEndpoint, strings.NewReader(metadata)), requester.ID)
			if rec.Code != http.StatusOK {
				t.Fatalf("update status = %d: %s", rec.Code, rec.Body.String())
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &submission); err != nil {
				t.Fatal(err)
			}
			if got := catalogTestSnapshotImage(t, submission.Snapshot, entity); got != imagePath {
				t.Fatalf("preserved path = %q, want %q", got, imagePath)
			}
			// A replacement image is accepted as a file, including on edits.
			req = catalogImageMultipartRequest(t, http.MethodPut, updateEndpoint, metadata, field, imageData)
			rec = catalogImageServe(update, req, requester.ID)
			if rec.Code != http.StatusOK {
				t.Fatalf("replace status = %d: %s", rec.Code, rec.Body.String())
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &submission); err != nil {
				t.Fatal(err)
			}
			if got := catalogTestSnapshotImage(t, submission.Snapshot, entity); got == imagePath {
				t.Fatalf("replacement kept old path = %q", got)
			}

			for _, ref := range []string{"https://external.example/image.png", imagePath, "data:image/png;base64,AAAA", "//external.example/image.png"} {
				var value map[string]any
				if err := json.Unmarshal([]byte(metadata), &value); err != nil {
					t.Fatal(err)
				}
				if entity == "authors" {
					value["photos"] = []string{ref}
				} else {
					value["coverImagePath"] = ref
				}
				body, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				for _, action := range []string{"create", "update"} {
					handler, method, path := create, http.MethodPost, endpoint
					if action == "update" {
						handler, method, path = update, http.MethodPut, updateEndpoint
					}
					for _, multipartBody := range []bool{false, true} {
						req := httptest.NewRequest(method, path, bytes.NewReader(body))
						if multipartBody {
							req = catalogImageMultipartRequest(t, method, path, string(body), field, imageData)
						}
						rec := catalogImageServe(handler, req, requester.ID)
						if rec.Code != http.StatusBadRequest {
							t.Fatalf("%s URL/path ref %q multipart=%v status=%d: %s", action, ref, multipartBody, rec.Code, rec.Body.String())
						}
					}
				}
			}
			// Any failure after file storage removes the newly uploaded file.
			before, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			other := mustCreateCatalogTestUser(t, store, entity+"-image-other@example.com")
			rec = catalogImageServe(update, catalogImageMultipartRequest(t, http.MethodPut, updateEndpoint, metadata, field, imageData), other.ID)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("other requester status = %d: %s", rec.Code, rec.Body.String())
			}
			badMetadata := `{"currentName":""}`
			if entity == "albums" {
				badMetadata = `{"title":""}`
			}
			rec = catalogImageServe(create, catalogImageMultipartRequest(t, http.MethodPost, endpoint, badMetadata, field, imageData), requester.ID)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("invalid metadata status = %d: %s", rec.Code, rec.Body.String())
			}
			after, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before) {
				t.Fatalf("failed submissions leaked files: before=%d after=%d", len(before), len(after))
			}
			rec = catalogImageServe(create, catalogImageMultipartRequest(t, http.MethodPost, endpoint, metadata, field, imageData), 0)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated upload status = %d", rec.Code)
			}
		})
	}
}

func TestCatalogSubmissionRejectsInvalidImageUploads(t *testing.T) {
	store := newTestTrackStore(t)
	t.Cleanup(func() { _ = store.db.Close() })
	requester := mustCreateCatalogTestUser(t, store, "invalid-image@example.com")
	dir := t.TempDir()
	pngData := catalogTestPNG(t, 2, 2)
	tests := []struct {
		name, field, metadata string
		files                 [][]byte
		want                  int
	}{
		{"text disguised as image", "photos", `{"currentName":"Artist"}`, [][]byte{[]byte("https://external.example/image.png")}, 400},
		{"SVG", "photos", `{"currentName":"Artist"}`, [][]byte{[]byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)}, 400},
		{"empty", "photos", `{"currentName":"Artist"}`, [][]byte{nil}, 400},
		{"oversized", "photos", `{"currentName":"Artist"}`, [][]byte{make([]byte, maxSubmissionImageSize+1)}, 413},
		{"oversized dimensions", "photos", `{"currentName":"Artist"}`, [][]byte{catalogTestPNG(t, maxSubmissionImageDimension+1, 1)}, 400},
		{"truncated PNG", "photos", `{"currentName":"Artist"}`, [][]byte{pngData[:len(pngData)/2]}, 400},
		{"unexpected field", "imageUrl", `{"currentName":"Artist"}`, [][]byte{pngData}, 400},
		{"unknown metadata", "photos", `{"currentName":"Artist","imageUrl":"https://external.example/x"}`, [][]byte{pngData}, 400},
		{"too many photos", "photos", `{"currentName":"Artist"}`, make([][]byte, maxSubmissionAuthorPhotos+1), 400},
		{"later invalid file", "photos", `{"currentName":"Artist"}`, [][]byte{pngData, []byte("invalid")}, 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := catalogImageMultipartRequest(t, http.MethodPost, "/api/catalog-submissions/authors", tc.metadata, tc.field, tc.files...)
			rec := catalogImageServe(createAuthorSubmissionHandler(store, dir), req, requester.ID)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			files, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != 0 {
				t.Fatalf("rejected images leaked %d files", len(files))
			}
		})
	}
	rec := catalogImageServe(createAlbumSubmissionHandler(store, dir), catalogImageMultipartRequest(t, http.MethodPost, "/api/catalog-submissions/albums", `{"title":"Album"}`, "cover", pngData, pngData), requester.ID)
	if rec.Code != 400 {
		t.Fatalf("multiple covers status = %d: %s", rec.Code, rec.Body.String())
	}
	// Text form fields cannot substitute for image files.
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("metadata", `{"currentName":"Artist"}`); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("photos", "https://external.example/x.png"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/catalog-submissions/authors", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec = catalogImageServe(createAuthorSubmissionHandler(store, dir), req, requester.ID)
	if rec.Code != 400 {
		t.Fatalf("text image field status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCatalogSubmissionMultiplePhotosAndReencoding(t *testing.T) {
	store := newTestTrackStore(t)
	t.Cleanup(func() { _ = store.db.Close() })
	requester := mustCreateCatalogTestUser(t, store, "multiple-images@example.com")
	dir := t.TempDir()
	data := append(catalogTestPNG(t, 2, 2), []byte("<script>untrusted trailing content</script>")...)
	rec := catalogImageServe(createAuthorSubmissionHandler(store, dir), catalogImageMultipartRequest(t, http.MethodPost, "/api/catalog-submissions/authors", `{"currentName":"Artist"}`, "photos", data, data), requester.ID)
	if rec.Code != 201 {
		t.Fatalf("multiple photos status = %d: %s", rec.Code, rec.Body.String())
	}
	var response catalogSubmissionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	photos := response.Snapshot["photos"].([]any)
	if len(photos) != 2 || photos[0] == photos[1] {
		t.Fatalf("photos = %#v", photos)
	}
	for _, ref := range photos {
		path, err := url.Parse(ref.(string))
		if err != nil {
			t.Fatal(err)
		}
		stored, err := os.ReadFile(filepath.Join(dir, filepath.Base(path.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(stored, []byte("<script>")) {
			t.Fatal("re-encoding retained trailing content")
		}
	}
}

func TestCatalogSubmissionImageFormats(t *testing.T) {
	img := image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black, color.White})
	for _, format := range []string{"jpeg", "gif"} {
		t.Run(format, func(t *testing.T) {
			var data bytes.Buffer
			var err error
			if format == "jpeg" {
				err = jpeg.Encode(&data, img, nil)
			} else {
				err = gif.Encode(&data, img, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			name, err := saveCatalogSubmissionImage(data.Bytes(), dir)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			_, storedFormat, err := image.Decode(bytes.NewReader(stored))
			if err != nil {
				t.Fatal(err)
			}
			wantFormat, wantExtension := "jpeg", ".jpg"
			if format == "gif" {
				wantFormat, wantExtension = "png", ".png"
			}
			if storedFormat != wantFormat || filepath.Ext(name) != wantExtension {
				t.Fatalf("stored format=%q name=%q, want %s %s", storedFormat, name, wantFormat, wantExtension)
			}
		})
	}
}

func catalogTestPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var data bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	img.Set(0, 0, color.NRGBA{R: 255, A: 255})
	if err := png.Encode(&data, img); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func catalogImageMultipartRequest(t *testing.T, method, path, metadata, field string, files ...[]byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("metadata", metadata); err != nil {
		t.Fatal(err)
	}
	for _, data := range files {
		file, err := writer.CreateFormFile(field, "untrusted.svg")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(file, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

func catalogImageServe(handler http.HandlerFunc, req *http.Request, userID int64) *httptest.ResponseRecorder {
	if userID > 0 {
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, userID))
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func catalogTestSnapshotImage(t *testing.T, snapshot map[string]any, entity string) string {
	t.Helper()
	if entity == "authors" {
		photos, ok := snapshot["photos"].([]any)
		if !ok || len(photos) != 1 {
			t.Fatalf("snapshot photos = %#v", snapshot["photos"])
		}
		return photos[0].(string)
	}
	return snapshot["coverImagePath"].(string)
}
