package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxSubmissionImageSize      = 10 << 20
	maxSubmissionImageBodySize  = 32 << 20
	maxSubmissionImageDimension = 4096
	maxSubmissionAuthorPhotos   = 10
)

// Metadata can still be submitted as JSON. Images must be multipart file parts,
// never references to existing files or remote URLs.
func decodeCatalogImageSubmission(w http.ResponseWriter, r *http.Request, request any, dir, field, prefix string, maxFiles int) ([]string, func(bool), error) {
	var storedNames []string
	cleanup := func(committed bool) {
		if r.MultipartForm != nil {
			if err := r.MultipartForm.RemoveAll(); err != nil {
				captureSentryError(r.Context(), err, "storage", "catalog_submissions.images.cleanup_multipart")
			}
		}
		if !committed {
			for _, name := range storedNames {
				if err := removeUploadedMediaFile(dir, name); err != nil {
					captureSentryError(r.Context(), err, "storage", "catalog_submissions.images.cleanup")
				}
			}
		}
	}
	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if contentType != "multipart/form-data" {
		if err := decodeJSON(r, request); err != nil {
			return nil, cleanup, err
		}
		return nil, cleanup, validateCatalogSubmissionImageReferences(request)
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSubmissionImageBodySize)
	if err := r.ParseMultipartForm(multipartUploadMemoryThreshold); err != nil {
		var sizeErr *http.MaxBytesError
		if errors.As(err, &sizeErr) {
			return nil, cleanup, errRequestBodyTooLarge
		}
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			return nil, cleanup, uploadError{message: "failed to process uploaded file", cause: err}
		}
		return nil, cleanup, errors.New("invalid multipart form")
	}
	for name := range r.MultipartForm.Value {
		if name != "metadata" {
			return nil, cleanup, fmt.Errorf("%s must be uploaded as a file; only metadata may be a text field", field)
		}
	}
	for name := range r.MultipartForm.File {
		if name != field {
			return nil, cleanup, fmt.Errorf("unexpected file field %q; use %s", name, field)
		}
	}
	metadata := r.MultipartForm.Value["metadata"]
	if len(metadata) != 1 {
		return nil, cleanup, errors.New("exactly one metadata JSON field is required")
	}
	metadataRequest := &http.Request{Body: io.NopCloser(strings.NewReader(metadata[0]))}
	if err := decodeJSON(metadataRequest, request); err != nil {
		return nil, cleanup, err
	}
	if err := validateCatalogSubmissionImageReferences(request); err != nil {
		return nil, cleanup, err
	}
	files := r.MultipartForm.File[field]
	if len(files) > maxFiles {
		return nil, cleanup, fmt.Errorf("%s must contain at most %d image files", field, maxFiles)
	}
	var paths []string
	for _, header := range files {
		if header.Size > maxSubmissionImageSize {
			return nil, cleanup, errRequestBodyTooLarge
		}
		file, err := header.Open()
		if err != nil {
			return nil, cleanup, uploadError{message: "failed to process uploaded file", cause: err}
		}
		data, readErr := io.ReadAll(io.LimitReader(file, maxSubmissionImageSize+1))
		closeErr := file.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return nil, cleanup, uploadError{message: "failed to process uploaded file", cause: err}
		}
		if len(data) > maxSubmissionImageSize {
			return nil, cleanup, errRequestBodyTooLarge
		}
		name, err := saveCatalogSubmissionImage(data, dir)
		if err != nil {
			return nil, cleanup, err
		}
		storedNames = append(storedNames, name)
		paths = append(paths, prefix+name)
	}
	return paths, cleanup, nil
}

func validateCatalogSubmissionImageReferences(request any) error {
	switch value := request.(type) {
	case *upsertAuthorRequest:
		if len(value.Photos) != 0 {
			return errors.New("photos must be uploaded as files; image URLs and paths are not allowed")
		}
	case *upsertAlbumRequest:
		if strings.TrimSpace(value.CoverImagePath) != "" {
			return errors.New("cover must be uploaded as a file; coverImagePath URLs and paths are not allowed")
		}
	}
	return nil
}

func saveCatalogSubmissionImage(data []byte, dir string) (string, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "jpeg" && format != "png" && format != "gif") {
		return "", errors.New("file must be a valid JPEG, PNG, or GIF image")
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > maxSubmissionImageDimension || config.Height > maxSubmissionImageDimension {
		return "", fmt.Errorf("image dimensions must be between 1 and %d pixels", maxSubmissionImageDimension)
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", errors.New("file must be a valid JPEG, PNG, or GIF image")
	}
	if strings.TrimSpace(dir) == "" {
		return "", uploadError{message: "failed to save uploaded file", cause: errors.New("image directory is not configured")}
	}
	// Generate the filename from the decoded format, never the client filename.
	ext := ".png"
	if format == "jpeg" {
		ext = ".jpg"
	}
	token, err := randomFileNameToken(18)
	if err != nil {
		return "", uploadError{message: "failed to save uploaded file", cause: err}
	}
	name := "image-" + token + ext
	path := filepath.Join(dir, name)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", uploadError{message: "failed to save uploaded file", cause: err}
	}
	// Re-encoding strips metadata and any trailing non-image content. GIFs are
	// stored as a static PNG.
	if format == "jpeg" {
		err = jpeg.Encode(file, decoded, &jpeg.Options{Quality: 90})
	} else {
		err = png.Encode(file, decoded)
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		err = errors.Join(err, removeFileForCleanup(path))
		return "", uploadError{message: "failed to save uploaded file", cause: err}
	}
	return name, nil
}

func writeCatalogSubmissionImageError(w http.ResponseWriter, r *http.Request, err error) {
	var storageErr uploadError
	if errors.As(err, &storageErr) {
		writeUploadError(w, r, err, "catalog_submissions.images.upload")
		return
	}
	writeRequestDecodeError(w, err)
}
