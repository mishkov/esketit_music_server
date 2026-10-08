package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *mcpService) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.accessToken) < 32 {
			writeMCPHTTPError(w, errMCPCredentialDisabled, http.StatusServiceUnavailable)
			return
		}
		value := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(value, "Bearer ")
		expected, received := sha256.Sum256([]byte(s.accessToken)), sha256.Sum256([]byte(token))
		if !ok || subtle.ConstantTimeCompare(expected[:], received[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="esketit-mcp"`)
			writeMCPHTTPError(w, &mcpProblem{Code: "unauthorized", Message: "A valid MCP Bearer credential is required.", Fields: []string{}, Recovery: "Configure the MCP access credential in your client."}, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func writeMCPHTTPError(w http.ResponseWriter, err error, status int) {
	writeJSON(w, status, mcpEnvelope{OK: false, Error: mcpError(err)})
}
func mcpHTTPStatus(err error) int {
	switch {
	case errors.Is(err, errMCPForbidden):
		return http.StatusForbidden
	case errors.Is(err, errMCPDisabled):
		return http.StatusServiceUnavailable
	case errors.Is(err, errMCPStaleRevision), errors.Is(err, errMCPRequestConflict), errors.Is(err, errCatalogUploadClaimed):
		return http.StatusConflict
	case errors.Is(err, errRepositoryPersistence), errors.Is(err, errRepositoryConflict):
		return http.StatusServiceUnavailable
	case errors.Is(err, errCatalogUploadNotFound):
		return http.StatusNotFound
	default:
		return http.StatusBadRequest
	}
}
func (s *mcpService) settingsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actorID, ok := userIDFromContext(r.Context())
		if !ok {
			writeMCPHTTPError(w, errMCPForbidden, http.StatusForbidden)
			return
		}
		var patch map[string]json.RawMessage
		if r.Method == http.MethodPatch {
			r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
			if err := decodeJSON(r, &patch); err != nil {
				writeMCPHTTPError(w, mcpInvalid("body", "invalid settings JSON"), http.StatusBadRequest)
				return
			}
			for key := range patch {
				if key != "actingUserId" && key != "workflowGuidance" && key != "uploadGuidance" && key != "expectedVersion" {
					writeMCPHTTPError(w, mcpInvalid(key, "unknown settings field"), http.StatusBadRequest)
					return
				}
			}
			if len(patch) < 2 || argID(patch, "expectedVersion") < 1 {
				writeMCPHTTPError(w, mcpInvalid("expectedVersion", "expectedVersion and at least one changed field are required"), http.StatusBadRequest)
				return
			}
		}
		var settings mcpSettings
		operation := func(repos domainRepositories) error {
			allowed, err := repos.access.UserHasPermission(r.Context(), actorID, permissionAccessControlManage)
			if err != nil {
				return err
			}
			if !allowed {
				return errMCPForbidden
			}
			settings, err = repos.mcp.GetMCPSettings(r.Context())
			if err != nil {
				return err
			}
			if r.Method != http.MethodPatch {
				return nil
			}
			if raw, ok := patch["actingUserId"]; ok {
				if string(raw) == "null" {
					settings.ActingUserID = nil
				} else {
					var id int64
					if err := json.Unmarshal(raw, &id); err != nil || id < 1 {
						return mcpInvalid("actingUserId", "must be a positive user ID or null")
					}
					if _, found, err := repos.users.FindByID(r.Context(), id); err != nil {
						return err
					} else if !found {
						return mcpInvalid("actingUserId", "selected user does not exist")
					}
					settings.ActingUserID = &id
				}
			}
			for _, field := range []string{"workflowGuidance", "uploadGuidance"} {
				if raw, ok := patch[field]; ok {
					var value string
					if string(raw) == "null" || json.Unmarshal(raw, &value) != nil || len(value) > 32000 {
						return mcpInvalid(field, "must be text with at most 32000 bytes")
					}
					if field == "workflowGuidance" {
						settings.WorkflowGuidance = value
					} else {
						settings.UploadGuidance = value
					}
				}
			}
			if err := repos.mcp.PatchMCPSettings(r.Context(), settings, argID(patch, "expectedVersion")); err != nil {
				return err
			}
			settings.Version++
			return repos.access.InsertAuditEvent(r.Context(), accessControlAuditEvent{ActorUserID: &actorID, Action: "mcp.settings.update", TargetUserID: settings.ActingUserID, Details: map[string]any{"version": settings.Version}, CreatedAt: time.Now().UTC()})
		}
		var err error
		if r.Method == http.MethodPatch {
			err = s.store.unitOfWork.WithinTransaction(r.Context(), operation)
		} else {
			err = s.store.unitOfWork.WithinReadTransaction(r.Context(), operation)
		}
		if err != nil {
			writeMCPHTTPError(w, err, mcpHTTPStatus(err))
			return
		}
		// The configured credential itself is never exposed through this API.
		writeJSON(w, http.StatusOK, map[string]any{"actingUserId": settings.ActingUserID, "workflowGuidance": settings.WorkflowGuidance, "uploadGuidance": settings.UploadGuidance, "version": settings.Version, "credentialConfigured": len(s.accessToken) >= 32})
	}
}

func (s *mcpService) handler() http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "esketit-catalog-review", Version: "1.0.0"}, &mcp.ServerOptions{Instructions: "Call get_context before working. Search before submitting. Upload file bytes over the dedicated HTTP endpoint. Follow feedback and allowedActions. Reuse requestId and payload after timeouts; refresh submission revision before a changed operation."})
	for _, spec := range mcpToolSpecs() {
		encoded, _ := json.Marshal(spec.schema)
		var schema jsonschema.Schema
		if err := json.Unmarshal(encoded, &schema); err != nil {
			panic(err)
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			panic(err)
		}
		tool := &mcp.Tool{Name: spec.name, Description: spec.description, InputSchema: spec.schema, OutputSchema: map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}, "data": map[string]any{"type": "object"}, "error": map[string]any{"type": "object"}}, "required": []string{"ok"}}, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: !spec.write, IdempotentHint: true, DestructiveHint: boolPointer(spec.name == "cancel_submission" || strings.HasPrefix(spec.name, "update_")), OpenWorldHint: boolPointer(false)}}
		server.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			raw := req.Params.Arguments
			if len(raw) == 0 {
				raw = []byte("{}")
			}
			var input any
			var data any
			var callErr error
			if err := json.Unmarshal(raw, &input); err != nil {
				callErr = mcpInvalid("arguments", "expected a JSON object")
			} else if err := resolved.Validate(input); err != nil {
				callErr = mcpInvalid("arguments", err.Error())
			} else {
				data, callErr = s.call(ctx, spec.name, raw, spec.write)
			}
			envelope := mcpEnvelope{OK: callErr == nil, Data: data}
			if callErr != nil {
				envelope.Data = nil
				envelope.Error = mcpError(callErr)
			}
			encoded, _ := json.Marshal(envelope)
			return &mcp.CallToolResult{IsError: callErr != nil, StructuredContent: envelope, Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}}}, nil
		})
	}
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, DisableLocalhostProtection: s.publicBaseURL != ""})
	protection := http.NewCrossOriginProtection()
	if origin := strings.TrimSpace(os.Getenv("CORS_ALLOW_ORIGIN")); origin != "" {
		if err := protection.AddTrustedOrigin(origin); err != nil {
			log.Print("MCP ignored an invalid configured CORS origin; cross-origin protection remains enabled")
		}
	}
	return s.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.publicBaseURL != "" {
			public, _ := url.Parse(s.publicBaseURL)
			hostname := (&url.URL{Host: r.Host}).Hostname()
			local := hostname == "localhost" || net.ParseIP(hostname) != nil && net.ParseIP(hostname).IsLoopback()
			if !local && !strings.EqualFold(r.Host, public.Host) {
				http.Error(w, "Invalid MCP Host header", http.StatusForbidden)
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
		protection.Handler(transport).ServeHTTP(w, r)
	}))
}
func boolPointer(v bool) *bool { return &v }

type mcpToolSpec struct {
	name, description string
	schema            map[string]any
	write             bool
}

func mcpToolSpecs() []mcpToolSpec {
	text := func(description string) map[string]any {
		return map[string]any{"type": "string", "minLength": 1, "maxLength": 1000, "description": description}
	}
	id := map[string]any{"type": "integer", "minimum": 1}
	kind := map[string]any{"type": "string", "enum": []string{"author", "album", "track"}}
	ids := map[string]any{"type": "array", "items": id, "minItems": 1, "maxItems": 100, "uniqueItems": true}
	token := text("An owned, unused token returned by POST /api/mcp/uploads.")
	tokenList := map[string]any{"type": "array", "items": token, "maxItems": maxSubmissionAuthorPhotos, "uniqueItems": true}
	metadata := map[string]any{"type": "array", "maxItems": 100, "items": map[string]any{"type": "object", "required": []string{"type"}, "properties": map[string]any{"type": text("Metadata type: text or external_link."), "title": text("Required title for text records."), "text": text("Required content for text records."), "provider": text("Required provider for external_link records."), "url": text("Required URL for external_link records.")}}}
	page := map[string]any{"type": "integer", "minimum": 1, "default": 1}
	pageSize := map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 25}
	schema := func(properties map[string]any, required ...string) map[string]any {
		if required == nil {
			required = []string{}
		}
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	specs := []mcpToolSpec{
		{"get_context", "Read acting identity, permissions, editable workflow/upload guidance, upload limits and server-enforced rules. Call at session start.", schema(map[string]any{}), false},
		{"search_catalog", "Find candidate matches among published entities and the acting user's pending entities. Search before creating; inspect IDs and relationships to confirm matches.", schema(map[string]any{"query": text("Case-insensitive substring search."), "entityType": kind, "page": page, "pageSize": pageSize}, "query"), false},
		{"get_catalog_item", "Read a visible author, album or track and its relationship IDs. Use to confirm search matches.", schema(map[string]any{"entityType": kind, "entityId": id}, "entityType", "entityId"), false},
		{"list_submissions", "List owned submissions, feedback, revisions, dependencies and allowedActions, including terminal records. Poll this tool to discover reviewer feedback.", schema(map[string]any{"status": map[string]any{"type": "string", "enum": []string{"pending_review", "changes_requested", "approved", "rejected", "cancelled"}}, "entityType": kind, "updatedSince": map[string]any{"type": "string", "format": "date-time"}, "page": page, "pageSize": pageSize}), false},
		{"get_submission", "Read full current entity data, feedback history, revision, dependencies and permitted next actions. Call before correcting or resubmitting.", schema(map[string]any{"submissionId": id}, "submissionId"), false},
		{"get_upload", "Read an owned upload's kind, byte size, checksum and claimed submission ID. Claimed tokens cannot be reused for new submissions.", schema(map[string]any{"uploadToken": token}, "uploadToken"), false},
	}
	fields := map[string]map[string]any{
		"author": {"currentName": text("Author's canonical display name."), "photoUploadTokens": tokenList},
		"album":  {"title": text("Album title."), "authorIds": ids, "releaseDate": map[string]any{"type": "string", "format": "date-time", "description": "Required sourced release date in RFC3339 format; do not invent a date."}, "isPublished": map[string]any{"type": "boolean", "description": "Whether the album is released content after approval. Omit on creation to use true; omit on updates to preserve the current value. Approval is still required for public visibility."}, "coverUploadToken": token, "additionalInfo": metadata},
		"track":  {"name": text("Track title."), "authorIds": ids, "albumId": id, "audioUploadToken": token, "albumOrder": map[string]any{"type": "integer", "minimum": 0, "description": "Zero-based position. Omit to append a new/moved track or preserve an existing track position."}, "additionalInfo": metadata, "sourceMetadata": map[string]any{"type": "array", "maxItems": 100, "items": map[string]any{"type": "object", "required": []string{"provider", "identity"}, "properties": map[string]any{"provider": text("Source provider, e.g. website or youtube."), "identity": map[string]any{"type": "object", "minProperties": 1, "description": "Stable source identifiers, e.g. {url: https://example.com/recording}."}, "url": text("Canonical source URL.")}}, "description": "Provenance records; retain original identity and URL when correcting metadata."}},
	}
	required := map[string][]string{"author": {"currentName"}, "album": {"title", "authorIds", "releaseDate"}, "track": {"name", "authorIds", "albumId", "audioUploadToken"}}
	requestID := map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "pattern": `\S`, "description": "Stable unique operation ID. Repeat the same ID and exact payload after timeout; use a new ID for changed work."}
	for _, entityType := range []string{"author", "album", "track"} {
		for _, create := range []bool{true, false} {
			props := map[string]any{"requestId": requestID}
			for key, value := range fields[entityType] {
				props[key] = value
			}
			req := []string{"requestId"}
			name := "submit_" + entityType
			description := "Submit a new " + entityType + " for human review. Search first. Use entity IDs for relationships and HTTP upload tokens for files."
			if create {
				req = append(req, required[entityType]...)
			} else {
				name = "update_" + entityType + "_submission"
				description = "Patch an owned " + entityType + " submission in changes_requested state. Omitted fields stay unchanged. Fetch get_submission first; use its revision. Empty photo/metadata arrays clear those fields; null coverUploadToken clears the cover."
				props["submissionId"] = id
				props["expectedRevision"] = id
				req = append(req, "submissionId", "expectedRevision")
				if entityType == "album" {
					props["coverUploadToken"] = map[string]any{"anyOf": []any{token, map[string]any{"type": "null"}}}
				}
			}
			input := schema(props, req...)
			if !create {
				input["minProperties"] = 4
			}
			specs = append(specs, mcpToolSpec{name, description, input, true})
		}
	}
	for _, name := range []string{"resubmit_submission", "cancel_submission"} {
		description := "Return an owned corrected submission to pending_review. Resolve dependency feedback first; use the current revision."
		if name == "cancel_submission" {
			description = "Cancel an owned pending_review or changes_requested submission if no catalog items depend on it. Use current revision; inspect allowedActions first."
		}
		specs = append(specs, mcpToolSpec{name, description, schema(map[string]any{"requestId": requestID, "submissionId": id, "expectedRevision": id}, "requestId", "submissionId", "expectedRevision"), true})
	}
	return specs
}

func (s *mcpService) uploadHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("Idempotency-Key")
		if strings.TrimSpace(requestID) == "" || len(requestID) > 128 {
			writeMCPHTTPError(w, mcpInvalid("Idempotency-Key", "must contain 1–128 characters"), http.StatusBadRequest)
			return
		}
		// Verify identity before accepting a potentially large request body. Check
		// it again inside the commit transaction to handle configuration changes.
		var userID, settingsVersion int64
		err := s.store.unitOfWork.WithinReadTransaction(r.Context(), func(repos domainRepositories) error {
			settings, id, err := mcpIdentity(r.Context(), repos, "")
			userID = id
			settingsVersion = settings.Version
			return err
		})
		if err != nil {
			writeMCPHTTPError(w, err, mcpHTTPStatus(err))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxSongUploadSize+(1<<20))
		if err := r.ParseMultipartForm(multipartUploadMemoryThreshold); err != nil {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeMCPHTTPError(w, mcpInvalid("file", "upload exceeds the body size limit"), http.StatusRequestEntityTooLarge)
			} else {
				writeMCPHTTPError(w, mcpInvalid("body", "expected multipart kind and file fields"), http.StatusBadRequest)
			}
			return
		}
		defer r.MultipartForm.RemoveAll()
		if len(r.MultipartForm.Value) != 1 || len(r.MultipartForm.Value["kind"]) != 1 || len(r.MultipartForm.File) != 1 || len(r.MultipartForm.File["file"]) != 1 {
			writeMCPHTTPError(w, mcpInvalid("body", "exactly one kind and one file are required"), http.StatusBadRequest)
			return
		}
		kind := r.MultipartForm.Value["kind"][0]
		permission := permissionTracksSubmit
		dir := s.store.catalogSubmissionAudioDir()
		prefix := ""
		switch kind {
		case "audio":
		case "author_photo":
			permission = permissionAuthorsSubmit
			dir = s.authorPhotosDir
			prefix = "/api/author-photos/"
		case "album_cover":
			permission = permissionAlbumsSubmit
			dir = s.albumCoversDir
			prefix = "/api/album-covers/"
		default:
			writeMCPHTTPError(w, mcpInvalid("kind", "must be audio, author_photo or album_cover"), http.StatusBadRequest)
			return
		}
		allowed, err := s.store.userHasPermission(userID, permission)
		if err != nil || !allowed {
			if err == nil {
				err = errMCPForbidden
			}
			writeMCPHTTPError(w, err, mcpHTTPStatus(err))
			return
		}
		header := r.MultipartForm.File["file"][0]
		limit := int64(maxSongUploadSize)
		if kind != "audio" {
			limit = maxSubmissionImageSize
		}
		if header.Size <= 0 || header.Size > limit {
			writeMCPHTTPError(w, mcpInvalid("file", "file is empty or exceeds its kind's size limit"), http.StatusBadRequest)
			return
		}
		source, err := header.Open()
		if err != nil {
			writeMCPHTTPError(w, errRepositoryPersistence, http.StatusInternalServerError)
			return
		}
		defer source.Close()
		if dir == "" || (kind == "audio" && s.store.songsDir == "") {
			writeMCPHTTPError(w, errRepositoryPersistence, http.StatusServiceUnavailable)
			return
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			writeMCPHTTPError(w, errRepositoryPersistence, http.StatusInternalServerError)
			return
		}
		tempDir := dir
		if kind != "audio" {
			tempDir = os.TempDir()
		}
		temp, err := os.CreateTemp(tempDir, "mcp-upload-*.mp3")
		if err != nil {
			writeMCPHTTPError(w, errRepositoryPersistence, http.StatusInternalServerError)
			return
		}
		tempPath := temp.Name()
		defer func() {
			if tempPath != "" {
				_ = os.Remove(tempPath)
			}
		}()
		hash := sha256.New()
		size, copyErr := io.Copy(io.MultiWriter(temp, hash), io.LimitReader(source, limit+1))
		syncErr := temp.Sync()
		closeErr := temp.Close()
		if copyErr != nil || syncErr != nil || closeErr != nil {
			writeMCPHTTPError(w, errRepositoryPersistence, http.StatusInternalServerError)
			return
		}
		if size != header.Size || size > limit {
			writeMCPHTTPError(w, mcpInvalid("file", "invalid file size"), http.StatusBadRequest)
			return
		}
		digest := hex.EncodeToString(hash.Sum(nil))
		originalName := filepath.Base(header.Filename)
		payloadHash, _ := hashMCPPayload(map[string]any{"kind": kind, "originalName": originalName, "sizeBytes": size, "sha256": digest})
		// Resolve known retries before media validation or image re-encoding. The
		// input bytes have already been hashed, so changed payloads always conflict.
		var previous json.RawMessage
		err = s.store.unitOfWork.WithinReadTransaction(r.Context(), func(repos domainRepositories) error {
			settings, currentUser, err := mcpIdentity(r.Context(), repos, permission)
			if err != nil {
				return err
			}
			if currentUser != userID || settings.Version != settingsVersion {
				return &mcpProblem{Code: "configuration_changed", Message: "MCP settings changed during upload.", Recovery: "Refresh get_context, then retry the upload."}
			}
			receipt, found, err := repos.mcp.FindMCPRequest(r.Context(), userID, requestID)
			if err != nil {
				return err
			}
			if found {
				if receipt.Operation != "upload" || receipt.PayloadHash != payloadHash {
					return errMCPRequestConflict
				}
				previous = receipt.Result
			}
			return nil
		})
		if err != nil {
			writeMCPHTTPError(w, err, mcpHTTPStatus(err))
			return
		}
		if previous != nil {
			writeJSON(w, http.StatusOK, mcpEnvelope{OK: true, Data: previous})
			return
		}
		token, err := randomFileNameToken(24)
		if err != nil {
			writeMCPHTTPError(w, errRepositoryPersistence, http.StatusInternalServerError)
			return
		}
		upload := mcpUpload{Token: token, UserID: userID, Kind: kind, OriginalName: originalName, SizeBytes: size, SHA256: digest, CreatedAt: time.Now().UTC()}
		// Files are prepared before the database write lock; failed commits remove
		// them. Audio registration joins existing approval/cleanup machinery.
		if kind == "audio" {
			if err := validateMCPMP3(r.Context(), tempPath); err != nil {
				writeMCPHTTPError(w, err, mcpHTTPStatus(err))
				return
			}
			upload.MediaType = "audio/mpeg"
			upload.StoredName = filepath.Base(tempPath)
		} else {
			data, err := os.ReadFile(tempPath)
			if err != nil {
				writeMCPHTTPError(w, errRepositoryPersistence, http.StatusInternalServerError)
				return
			}
			name, err := saveCatalogSubmissionImage(data, dir)
			if err != nil {
				writeMCPHTTPError(w, mcpInvalid("file", err.Error()), http.StatusBadRequest)
				return
			}
			upload.StoredName = name
			upload.MediaPath = prefix + name
			upload.MediaType = "image/png"
			if filepath.Ext(name) == ".jpg" {
				upload.MediaType = "image/jpeg"
			}
		}
		persisted := false
		defer func() {
			if !persisted && kind != "audio" {
				_ = os.Remove(filepath.Join(dir, upload.StoredName))
			}
		}()
		var result any
		err = s.store.unitOfWork.WithinTransaction(r.Context(), func(repos domainRepositories) error {
			settings, currentUser, err := mcpIdentity(r.Context(), repos, permission)
			if err != nil {
				return err
			}
			if currentUser != userID || settings.Version != settingsVersion {
				return &mcpProblem{Code: "configuration_changed", Message: "MCP settings changed during upload.", Fields: []string{}, Recovery: "Refresh get_context and retry using the same upload request ID."}
			}
			receipt, found, err := repos.mcp.FindMCPRequest(r.Context(), userID, requestID)
			if err != nil {
				return err
			}
			if found {
				if receipt.Operation != "upload" || receipt.PayloadHash != payloadHash {
					return errMCPRequestConflict
				}
				return json.Unmarshal(receipt.Result, &result)
			}
			if kind == "audio" {
				if err := repos.submissions.InsertSubmissionUpload(r.Context(), catalogSubmissionUpload{Token: token, RequesterUserID: userID, StoredFileName: upload.StoredName, OriginalName: originalName, CreatedAt: upload.CreatedAt}); err != nil {
					return err
				}
			}
			if err := repos.mcp.InsertMCPUpload(r.Context(), upload); err != nil {
				return err
			}
			encoded, _ := json.Marshal(upload)
			result = upload
			return repos.mcp.InsertMCPRequest(r.Context(), mcpRequestReceipt{UserID: userID, RequestID: requestID, Operation: "upload", PayloadHash: payloadHash, Result: encoded})
		})
		if err != nil {
			writeMCPHTTPError(w, err, mcpHTTPStatus(err))
			return
		}
		if v, ok := result.(mcpUpload); ok && v.Token == token {
			persisted = true
			if kind == "audio" {
				tempPath = ""
			}
		}
		writeJSON(w, http.StatusOK, mcpEnvelope{OK: true, Data: result})
	}
}

// Check MP3 format and decode the complete stream with the existing ffmpeg
// dependency. Do not trust a filename, MIME header, or an ID3 tag alone.
func validateMCPMP3(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return errRepositoryPersistence
	}
	defer f.Close()
	probe := make([]byte, 64<<10)
	n, err := f.Read(probe)
	if err != nil && !errors.Is(err, io.EOF) {
		return errRepositoryPersistence
	}
	probe = probe[:n]
	offset := 0
	if len(probe) >= 10 && string(probe[:3]) == "ID3" {
		for _, v := range probe[6:10] {
			if v&0x80 != 0 {
				return mcpInvalid("file", "invalid MP3 ID3 header")
			}
		}
		offset = 10 + (int(probe[6]) << 21) + (int(probe[7]) << 14) + (int(probe[8]) << 7) + int(probe[9])
		if probe[5]&0x10 != 0 {
			offset += 10
		}
		if _, err := f.Seek(int64(offset), io.SeekStart); err != nil {
			return mcpInvalid("file", "invalid MP3 header")
		}
		probe = make([]byte, 64<<10)
		n, _ = f.Read(probe)
		probe = probe[:n]
	}
	isMP3 := false
	for i := 0; i+4 <= len(probe); i++ {
		a, b, c := probe[i], probe[i+1], probe[i+2]
		if a == 0xff && b&0xe0 == 0xe0 && b&0x18 != 0x08 && b&0x06 == 0x02 && c&0xf0 != 0 && c&0xf0 != 0xf0 && c&0x0c != 0x0c {
			isMP3 = true
			break
		}
	}
	if !isMP3 {
		return mcpInvalid("file", "file must contain MP3 audio")
	}
	binary := strings.TrimSpace(os.Getenv("FFMPEG_BINARY"))
	if binary == "" {
		binary = "ffmpeg"
	}
	decodeCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(decodeCtx, binary, "-nostdin", "-v", "error", "-xerror", "-f", "mp3", "-i", path, "-map", "0:a:0", "-f", "null", "-")
	if err := command.Run(); err != nil {
		if decodeCtx.Err() != nil {
			return decodeCtx.Err()
		}
		var missing *exec.Error
		if errors.As(err, &missing) {
			return &mcpProblem{Code: "media_validator_unavailable", Message: "The MP3 validator is unavailable.", Fields: []string{}, Retryable: true, Recovery: "Ask the administrator to check FFMPEG_BINARY, then retry the same upload."}
		}
		return mcpInvalid("file", "MP3 audio could not be decoded completely; download a valid recording and retry with a new Idempotency-Key")
	}
	return nil
}
