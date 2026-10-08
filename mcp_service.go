package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"strings"
)

var (
	errMCPDisabled           = errors.New("MCP acting user is not configured")
	errMCPCredentialDisabled = errors.New("MCP access credential is not configured or is too short")
	errMCPForbidden          = errors.New("acting user lacks permission for this operation")
	errMCPStaleRevision      = errors.New("revision has changed; fetch current details before retrying")
	errMCPRequestConflict    = errors.New("requestId was already used for a different operation or payload")
)

type mcpProblem struct {
	Code      string   `json:"code"`
	Message   string   `json:"message"`
	Fields    []string `json:"fields"`
	Retryable bool     `json:"retryable"`
	Recovery  string   `json:"recovery"`
}

func (e *mcpProblem) Error() string { return e.Message }
func mcpInvalid(field, message string) error {
	return &mcpProblem{Code: "invalid_input", Message: message, Fields: []string{field}, Recovery: "Correct the indicated field, then retry."}
}
func mcpError(err error) *mcpProblem {
	var problem *mcpProblem
	if errors.As(err, &problem) {
		if problem.Fields == nil {
			problem.Fields = []string{}
		}
		return problem
	}
	p := &mcpProblem{Code: "invalid_input", Message: err.Error(), Fields: []string{}, Recovery: "Correct the request using get_context and the tool schema."}
	switch {
	case errors.Is(err, errMCPCredentialDisabled):
		p.Code = "not_configured"
		p.Recovery = "Ask the administrator to configure a dedicated MCP_ACCESS_TOKEN with at least 32 characters."
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		p.Code = "temporarily_unavailable"
		p.Message = "The operation was interrupted."
		p.Retryable = true
		p.Recovery = "Retry with the same request ID and payload."
	case errors.Is(err, errMCPDisabled):
		p.Code = "not_configured"
		p.Recovery = "Ask an administrator to select an MCP acting user in console settings."
	case errors.Is(err, errMCPForbidden), errors.Is(err, errCatalogSubmissionForbidden):
		p.Code = "forbidden"
		p.Recovery = "Check get_context permissions; use only the acting user's submissions."
	case errors.Is(err, errMCPStaleRevision):
		p.Code = "stale_revision"
		p.Fields = []string{"expectedRevision"}
		p.Recovery = "Call get_submission, reassess feedback, and use its revision with a new requestId."
	case errors.Is(err, errMCPRequestConflict):
		p.Code = "request_conflict"
		p.Fields = []string{"requestId"}
		p.Recovery = "Retry the original operation with its original payload, or use a new requestId for new work."
	case errors.Is(err, errCatalogSubmissionNotFound), errors.Is(err, errCatalogUploadNotFound):
		p.Code = "not_found"
		p.Recovery = "Use list_submissions or get_upload to locate an item owned by the acting user."
	case errors.Is(err, errCatalogUploadClaimed):
		p.Code = "upload_claimed"
		p.Recovery = "Retry the original submission with its requestId, or upload a new file with a new Idempotency-Key."
	case errors.Is(err, errCatalogSubmissionDependency):
		p.Code = "unresolved_dependencies"
		p.Recovery = "Call get_submission and resolve the listed author/album dependencies first."
	case errors.Is(err, errCatalogSubmissionState):
		p.Code = "invalid_state"
		p.Recovery = "Call get_submission and follow allowedActions; edits require changes_requested."
	case errors.Is(err, errRepositoryPersistence), errors.Is(err, errRepositoryConflict):
		p.Code = "temporarily_unavailable"
		p.Message = "The database operation could not complete."
		p.Retryable = true
		p.Recovery = "Retry with exactly the same requestId and payload."
	}
	return p
}

type mcpEnvelope struct {
	OK    bool        `json:"ok"`
	Data  any         `json:"data,omitempty"`
	Error *mcpProblem `json:"error,omitempty"`
}
type mcpListInput struct {
	Status       string `json:"status"`
	EntityType   string `json:"entityType"`
	UpdatedSince string `json:"updatedSince"`
	Page         int    `json:"page"`
	PageSize     int    `json:"pageSize"`
}
type mcpService struct {
	store                                        *trackStore
	authorPhotosDir, albumCoversDir, accessToken string
	publicBaseURL                                string
}

// An optional canonical public URL supports trusted reverse-proxy Host headers
// and gives remote agents an absolute upload URL without trusting request input.
func (s *mcpService) configurePublicURL(raw string) error {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("MCP_PUBLIC_URL must be an absolute HTTP(S) server base URL without credentials, query or fragment")
	}
	s.publicBaseURL = raw
	return nil
}

func mcpIdentity(ctx context.Context, repos domainRepositories, permission string) (mcpSettings, int64, error) {
	settings, err := repos.mcp.GetMCPSettings(ctx)
	if err != nil {
		return settings, 0, err
	}
	if settings.ActingUserID == nil {
		return settings, 0, errMCPDisabled
	}
	userID := *settings.ActingUserID
	if _, found, err := repos.users.FindByID(ctx, userID); err != nil {
		return settings, 0, err
	} else if !found {
		return settings, 0, errMCPDisabled
	}
	if permission != "" {
		allowed, err := repos.access.UserHasPermission(ctx, userID, permission)
		if err != nil {
			return settings, 0, err
		}
		if !allowed {
			return settings, 0, errMCPForbidden
		}
	}
	return settings, userID, nil
}
func mcpPermission(name string) string {
	switch name {
	case "get_context":
		return ""
	case "submit_author":
		return permissionAuthorsSubmit
	case "submit_album":
		return permissionAlbumsSubmit
	case "submit_track":
		return permissionTracksSubmit
	case "update_author_submission", "update_album_submission", "update_track_submission", "resubmit_submission":
		return permissionCatalogSubmissionsUpdate
	case "cancel_submission":
		return permissionCatalogSubmissionsCancel
	default:
		return permissionCatalogSubmissionsRead
	}
}
func hashMCPPayload(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
func decodeMCPArguments(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	return d.Decode(v)
}
func argString(args map[string]json.RawMessage, key string) string {
	var v string
	_ = json.Unmarshal(args[key], &v)
	return v
}
func argID(args map[string]json.RawMessage, key string) int64 {
	var v int64
	_ = json.Unmarshal(args[key], &v)
	return v
}

func (s *mcpService) call(ctx context.Context, name string, raw json.RawMessage, write bool) (any, error) {
	var result any
	var cleanup []string
	operation := func(repos domainRepositories) error {
		_, userID, err := mcpIdentity(ctx, repos, mcpPermission(name))
		if err != nil {
			return err
		}
		var args map[string]json.RawMessage
		if len(raw) == 0 {
			raw = []byte("{}")
		}
		if err := json.Unmarshal(raw, &args); err != nil {
			return mcpInvalid("arguments", "expected a JSON object")
		}
		if !write {
			result, err = s.read(ctx, repos, userID, name, args)
			return err
		}
		requestID := argString(args, "requestId")
		if strings.TrimSpace(requestID) == "" || len(requestID) > 128 {
			return mcpInvalid("requestId", "requestId must contain 1–128 characters")
		}
		hash, err := hashMCPPayload(args)
		if err != nil {
			return err
		}
		receipt, found, err := repos.mcp.FindMCPRequest(ctx, userID, requestID)
		if err != nil {
			return err
		}
		if found {
			if receipt.Operation != name || receipt.PayloadHash != hash {
				return errMCPRequestConflict
			}
			return json.Unmarshal(receipt.Result, &result)
		}
		// Reuse domain validation with repositories joined to this transaction.
		joined := *s.store
		joined.unitOfWork = joinedUnitOfWork{repos}
		joined.deferSubmissionCleanup = func(path string) error { cleanup = append(cleanup, path); return nil }
		response, err := s.mutate(ctx, repos, &joined, userID, name, args)
		if err != nil {
			return err
		}
		result, err = s.submission(ctx, repos, userID, response.ID)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return err
		}
		return repos.mcp.InsertMCPRequest(ctx, mcpRequestReceipt{UserID: userID, RequestID: requestID, Operation: name, PayloadHash: hash, Result: encoded})
	}
	var err error
	if write {
		err = s.store.unitOfWork.WithinTransaction(ctx, operation)
	} else {
		err = s.store.unitOfWork.WithinReadTransaction(ctx, operation)
	}
	if err == nil {
		for _, path := range cleanup {
			if e := removeFileForCleanup(path); e != nil {
				log.Printf("MCP post-commit file cleanup failed: %v", e)
			}
		}
	}
	return result, err
}

func (s *mcpService) read(ctx context.Context, repos domainRepositories, userID int64, name string, args map[string]json.RawMessage) (any, error) {
	switch name {
	case "get_context":
		settings, err := repos.mcp.GetMCPSettings(ctx)
		if err != nil {
			return nil, err
		}
		permissions, err := repos.access.ListEffectivePermissions(ctx, userID)
		if err != nil {
			return nil, err
		}
		codes := []string{}
		for _, p := range permissions {
			codes = append(codes, p.Code)
		}
		return map[string]any{"actingUserId": userID, "permissions": codes, "configurationVersion": settings.Version, "workflowGuidance": settings.WorkflowGuidance, "uploadGuidance": settings.UploadGuidance,
			"uploads": map[string]any{"endpoint": s.publicBaseURL + "/api/mcp/uploads", "method": "POST", "authentication": "Authorization: Bearer <MCP credential>; reuse your configured MCP connection credential", "contentType": "multipart/form-data", "fileField": "file", "kindField": "kind", "idempotencyHeader": "Idempotency-Key", "kinds": []string{"audio", "author_photo", "album_cover"}, "maxAudioBytes": maxSongUploadSize, "maxImageBytes": maxSubmissionImageSize, "maxImageDimension": maxSubmissionImageDimension, "maxAuthorPhotos": maxSubmissionAuthorPhotos, "audioFormats": []string{"mp3"}, "imageFormats": []string{"jpeg", "png", "gif"}, "example": "curl --fail-with-body -H \"Authorization: Bearer $MCP_ACCESS_TOKEN\" -H \"Idempotency-Key: <stable-upload-request-id>\" -F kind=audio -F file=@./track.mp3 <server-base-url>/api/mcp/uploads"},
			"rules":   []string{"Only owned submissions may be read or changed.", "Only changes_requested submissions can be edited or resubmitted.", "Use submissionId for workflow tools and entityId for authorIds/albumId.", "Authors and albums must be approved before track approval; correct dependency feedback before resubmitting tracks.", "Updates are patches: omitted fields are preserved. Use [] to clear photoUploadTokens/additionalInfo/sourceMetadata; use null to clear coverUploadToken. Required names and references cannot be cleared.", "Use the same requestId and exact payload after a timeout. Use a new requestId for changed work. Receipts persist without automatic expiry.", "expectedRevision must match current submission details. Refresh context at session start and after permission/configuration errors.", "Review approval, rejection, and settings changes are not MCP tools."}}, nil
	case "get_submission":
		return s.submission(ctx, repos, userID, argID(args, "submissionId"))
	case "list_submissions":
		encoded, _ := json.Marshal(args)
		var f mcpListInput
		if err := decodeMCPArguments(encoded, &f); err != nil {
			return nil, err
		}
		if f.Page == 0 {
			f.Page = 1
		}
		if f.PageSize == 0 {
			f.PageSize = 25
		}
		items, total, err := repos.mcp.ListMCPSubmissions(ctx, userID, f)
		if err != nil {
			return nil, err
		}
		responses := []any{}
		for _, item := range items {
			v, err := s.submission(ctx, repos, userID, item.ID)
			if err != nil {
				return nil, err
			}
			responses = append(responses, v)
		}
		return map[string]any{"items": responses, "page": f.Page, "pageSize": f.PageSize, "totalItems": total}, nil
	case "get_upload":
		item, found, err := repos.mcp.FindMCPUpload(ctx, userID, argString(args, "uploadToken"))
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errCatalogUploadNotFound
		}
		return item, nil
	case "get_catalog_item":
		return mcpCatalogItem(ctx, repos, userID, argString(args, "entityType"), argID(args, "entityId"))
	case "search_catalog":
		var input struct {
			Query      string `json:"query"`
			EntityType string `json:"entityType"`
			Page       int    `json:"page"`
			PageSize   int    `json:"pageSize"`
		}
		encoded, _ := json.Marshal(args)
		if err := decodeMCPArguments(encoded, &input); err != nil {
			return nil, err
		}
		if input.Page == 0 {
			input.Page = 1
		}
		if input.PageSize == 0 {
			input.PageSize = 25
		}
		refs, total, err := repos.mcp.SearchMCPCatalog(ctx, userID, input.Query, input.EntityType, input.Page, input.PageSize)
		if err != nil {
			return nil, err
		}
		items := []any{}
		for _, ref := range refs {
			v, err := mcpCatalogItem(ctx, repos, userID, ref.Type, ref.ID)
			if err != nil {
				return nil, err
			}
			items = append(items, map[string]any{"entityType": ref.Type, "entityId": ref.ID, "entity": v})
		}
		return map[string]any{"items": items, "page": input.Page, "pageSize": input.PageSize, "totalItems": total}, nil
	}
	return nil, mcpInvalid("name", "unknown tool")
}

func mcpCatalogItem(ctx context.Context, repos domainRepositories, userID int64, kind string, id int64) (any, error) {
	item, err := catalogEntityByType(ctx, repos, kind, id)
	if err != nil {
		return nil, err
	}
	var status string
	var owner *int64
	switch v := item.(type) {
	case author:
		status, owner = v.PublicationStatus, v.RequestedByUserID
	case album:
		status, owner = v.PublicationStatus, v.RequestedByUserID
	case track:
		status, owner = v.PublicationStatus, v.RequestedByUserID
	default:
		return nil, errCatalogSubmissionNotFound
	}
	if !catalogEntityVisible(status, owner, userID) {
		return nil, errCatalogSubmissionNotFound
	}
	if trackItem, ok := item.(track); ok {
		return mcpTrackWithOrder(ctx, repos, trackItem)
	}
	return item, nil
}

func mcpTrackWithOrder(ctx context.Context, repos domainRepositories, item track) (map[string]any, error) {
	raw, err := json.Marshal(item)
	if err != nil {
		return nil, err
	}
	var entity map[string]any
	if err := json.Unmarshal(raw, &entity); err != nil {
		return nil, err
	}
	albumItem, found, err := repos.catalog.FindAlbumByID(ctx, item.AlbumID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errCatalogSubmissionDependency
	}
	for index, id := range albumItem.TrackIDs {
		if id == item.ID {
			entity["albumOrder"] = index
			return entity, nil
		}
	}
	return nil, errCatalogSubmissionDependency
}

func (s *mcpService) submission(ctx context.Context, repos domainRepositories, userID, id int64) (any, error) {
	item, found, err := repos.submissions.FindSubmissionByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errCatalogSubmissionNotFound
	}
	if err := validateSubmissionOwner(item, userID); err != nil {
		return nil, err
	}
	responses, err := buildCatalogSubmissionResponses(ctx, repos, []catalogSubmission{item})
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(responses[0])
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, err
	}
	result["submissionId"] = id
	if trackItem, ok := responses[0].Entity.(track); ok {
		entity, err := mcpTrackWithOrder(ctx, repos, trackItem)
		if err != nil {
			return nil, err
		}
		result["entity"] = entity
	}
	actions := []string{}
	editable := item.Status == catalogSubmissionStatusChangesRequested
	cancelable := editable || item.Status == catalogSubmissionStatusPendingReview
	canUpdate, err := repos.access.UserHasPermission(ctx, userID, permissionCatalogSubmissionsUpdate)
	if err != nil {
		return nil, err
	}
	canCancel, err := repos.access.UserHasPermission(ctx, userID, permissionCatalogSubmissionsCancel)
	if err != nil {
		return nil, err
	}
	dependencies := []any{}
	if item.EntityType != "author" {
		var albumID int64
		var authorIDs []int64
		switch v := responses[0].Entity.(type) {
		case album:
			authorIDs = v.AuthorIDs
		case track:
			authorIDs = v.AuthorIDs
			albumID = v.AlbumID
		}
		appendDependency := func(kind string, id int64) error {
			sub, found, err := repos.submissions.FindByEntity(ctx, kind, id)
			if err != nil {
				return err
			}
			if found {
				dependencies = append(dependencies, map[string]any{"entityType": kind, "entityId": id, "submissionId": sub.ID, "status": sub.Status, "revision": sub.Revision})
			}
			return nil
		}
		if albumID > 0 {
			if err := appendDependency("album", albumID); err != nil {
				return nil, err
			}
		}
		for _, id := range authorIDs {
			if err := appendDependency("author", id); err != nil {
				return nil, err
			}
		}
	}
	if editable && canUpdate {
		actions = append(actions, "update_"+item.EntityType+"_submission")
		if err := ensureSubmissionDependenciesReady(ctx, repos, item); err == nil {
			actions = append(actions, "resubmit_submission")
		} else if !errors.Is(err, errCatalogSubmissionDependency) {
			return nil, err
		}
	}
	if cancelable && canCancel {
		eligible := true
		if item.EntityType == "album" {
			if v, ok := responses[0].Entity.(album); ok && len(v.TrackIDs) > 0 {
				eligible = false
			}
		}
		if item.EntityType == "author" {
			refs, err := repos.mcp.MCPAuthorReferenced(ctx, item.EntityID)
			if err != nil {
				return nil, err
			}
			eligible = !refs
		}
		if eligible {
			actions = append(actions, "cancel_submission")
		}
	}
	result["allowedActions"] = actions
	result["dependencies"] = dependencies
	return result, nil
}

func (s *mcpService) imageToken(ctx context.Context, repos domainRepositories, userID int64, token, kind string) (string, error) {
	v, found, err := repos.mcp.FindMCPUpload(ctx, userID, token)
	if err != nil {
		return "", err
	}
	if !found {
		return "", errCatalogUploadNotFound
	}
	if v.Kind != kind {
		return "", mcpInvalid("uploadToken", "upload kind does not match this field")
	}
	if v.ClaimedSubmissionID != nil {
		return "", errCatalogUploadClaimed
	}
	return v.MediaPath, nil
}
func (s *mcpService) mutate(ctx context.Context, repos domainRepositories, store *trackStore, userID int64, name string, args map[string]json.RawMessage) (catalogSubmissionResponse, error) {
	argsCopy := make(map[string]json.RawMessage, len(args))
	for key, value := range args {
		argsCopy[key] = value
	}
	delete(argsCopy, "requestId")
	delete(argsCopy, "expectedRevision")
	delete(argsCopy, "submissionId")
	var existing catalogSubmission
	isCreate := strings.HasPrefix(name, "submit_")
	kind := strings.TrimPrefix(name, "submit_")
	if !isCreate {
		var found bool
		var err error
		existing, found, err = repos.submissions.FindSubmissionByID(ctx, argID(args, "submissionId"))
		if err != nil {
			return catalogSubmissionResponse{}, err
		}
		if !found {
			return catalogSubmissionResponse{}, errCatalogSubmissionNotFound
		}
		if err := validateSubmissionOwner(existing, userID); err != nil {
			return catalogSubmissionResponse{}, err
		}
		if existing.Revision != argID(args, "expectedRevision") {
			return catalogSubmissionResponse{}, errMCPStaleRevision
		}
		kind = existing.EntityType
		if name == "resubmit_submission" {
			return store.resubmitCatalogSubmission(userID, existing.ID)
		}
		if name == "cancel_submission" {
			return store.cancelCatalogSubmission(userID, existing.ID)
		}
		if name != "update_"+kind+"_submission" {
			return catalogSubmissionResponse{}, mcpInvalid("submissionId", "submission type does not match the update tool")
		}
	}
	tokens := []string{}
	if raw, ok := argsCopy["photoUploadTokens"]; ok {
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil {
			return catalogSubmissionResponse{}, mcpInvalid("photoUploadTokens", "must be an array")
		}
		paths := []string{}
		for _, token := range list {
			path, err := s.imageToken(ctx, repos, userID, token, "author_photo")
			if err != nil {
				return catalogSubmissionResponse{}, err
			}
			paths = append(paths, path)
			tokens = append(tokens, token)
		}
		argsCopy["photos"], _ = json.Marshal(paths)
		delete(argsCopy, "photoUploadTokens")
	}
	clearCover := false
	if raw, ok := argsCopy["coverUploadToken"]; ok {
		var token string
		if string(raw) == "null" {
			clearCover = true
			argsCopy["coverImagePath"] = json.RawMessage(`""`)
		} else {
			if err := json.Unmarshal(raw, &token); err != nil {
				return catalogSubmissionResponse{}, err
			}
			path, err := s.imageToken(ctx, repos, userID, token, "album_cover")
			if err != nil {
				return catalogSubmissionResponse{}, err
			}
			argsCopy["coverImagePath"], _ = json.Marshal(path)
			tokens = append(tokens, token)
		}
		delete(argsCopy, "coverUploadToken")
	}
	if raw, ok := argsCopy["audioUploadToken"]; ok {
		var token string
		_ = json.Unmarshal(raw, &token)
		v, found, err := repos.mcp.FindMCPUpload(ctx, userID, token)
		if err != nil {
			return catalogSubmissionResponse{}, err
		}
		if !found {
			return catalogSubmissionResponse{}, errCatalogUploadNotFound
		}
		if v.Kind != "audio" {
			return catalogSubmissionResponse{}, mcpInvalid("audioUploadToken", "must reference an audio upload")
		}
		if v.ClaimedSubmissionID != nil {
			return catalogSubmissionResponse{}, errCatalogUploadClaimed
		}
		tokens = append(tokens, token)
	}
	payload := map[string]json.RawMessage{}
	if !isCreate {
		entity, err := catalogSubmissionEntity(ctx, repos, existing)
		if err != nil {
			return catalogSubmissionResponse{}, err
		}
		b, _ := json.Marshal(entity)
		_ = json.Unmarshal(b, &payload)
	}
	for key, value := range argsCopy {
		payload[key] = value
	}
	if kind == "track" {
		if _, supplied := argsCopy["albumOrder"]; !supplied {
			targetID := argID(payload, "albumId")
			target, found, err := repos.catalog.FindAlbumByID(ctx, targetID)
			if err != nil {
				return catalogSubmissionResponse{}, err
			}
			if !found {
				return catalogSubmissionResponse{}, mcpInvalid("albumId", "album is not available")
			}
			order := len(target.TrackIDs) // New or moved tracks append by default.
			if !isCreate {
				for index, id := range target.TrackIDs {
					if id == existing.EntityID {
						order = index
						break
					}
				}
			}
			payload["albumOrder"], _ = json.Marshal(order)
		}
	}
	if kind == "album" && !isCreate {
		if raw, supplied := argsCopy["authorIds"]; supplied {
			current, found, err := repos.catalog.FindAlbumByID(ctx, existing.EntityID)
			if err != nil {
				return catalogSubmissionResponse{}, err
			}
			if !found {
				return catalogSubmissionResponse{}, errCatalogSubmissionNotFound
			}
			if len(current.TrackIDs) > 0 {
				var requested []int64
				if err := json.Unmarshal(raw, &requested); err != nil {
					return catalogSubmissionResponse{}, err
				}
				same := len(requested) == len(current.AuthorIDs)
				for _, id := range requested {
					if !containsInt64(current.AuthorIDs, id) {
						same = false
					}
				}
				if !same {
					return catalogSubmissionResponse{}, mcpInvalid("authorIds", "Album authors are derived from its tracks; correct the track authors instead.")
				}
			}
		}
	}
	// Restrict decoding to the domain request's fields; entity lifecycle fields
	// from the current snapshot cannot be supplied by clients.
	encodeFields := func(keys ...string) json.RawMessage {
		p := map[string]json.RawMessage{}
		for _, key := range keys {
			if v, ok := payload[key]; ok {
				p[key] = v
			}
		}
		b, _ := json.Marshal(p)
		return b
	}
	var result catalogSubmissionResponse
	var err error
	switch kind {
	case "author":
		var request upsertAuthorRequest
		err = decodeMCPArguments(encodeFields("currentName", "photos"), &request)
		if err == nil {
			if isCreate {
				result, err = store.createAuthorSubmission(userID, request)
			} else {
				result, err = store.updateAuthorSubmission(userID, existing.EntityID, request)
			}
		}
	case "album":
		// New MCP albums are intended for release unless explicitly submitted
		// as drafts. Updates decode the current entity, preserving omitted fields.
		request := upsertAlbumRequest{IsPublished: isCreate}
		err = decodeMCPArguments(encodeFields("title", "authorIds", "releaseDate", "coverImagePath", "additionalInfo", "isPublished"), &request)
		if err == nil {
			if isCreate {
				result, err = store.createAlbumSubmission(userID, request)
			} else {
				result, err = store.updateAlbumSubmission(userID, existing.EntityID, request)
			}
		}
	case "track":
		if isCreate {
			var request createTrackSubmissionRequest
			err = decodeMCPArguments(encodeFields("name", "authorIds", "albumId", "albumOrder", "audioUploadToken", "additionalInfo", "sourceMetadata"), &request)
			if err == nil {
				result, err = store.createTrackSubmission(userID, request)
			}
		} else {
			var request updateTrackSubmissionRequest
			err = decodeMCPArguments(encodeFields("name", "authorIds", "albumId", "albumOrder", "audioUploadToken", "additionalInfo", "sourceMetadata"), &request)
			if err == nil {
				result, err = store.updateTrackSubmission(userID, existing.EntityID, request)
			}
		}
	default:
		err = mcpInvalid("entityType", "unsupported entity type")
	}
	if err != nil {
		return result, err
	}
	// The legacy album service treats an empty cover as preservation. Explicit
	// MCP null means clear, so persist that change within this same transaction.
	if clearCover && !isCreate {
		item, found, e := repos.catalog.FindAlbumByID(ctx, result.EntityID)
		if e != nil {
			return result, e
		}
		if !found {
			return result, errCatalogSubmissionNotFound
		}
		item.CoverImagePath = ""
		if e := repos.catalog.UpdateAlbum(ctx, item); e != nil {
			return result, e
		}
		sub, _, e := repos.submissions.FindSubmissionByID(ctx, result.ID)
		if e != nil {
			return result, e
		}
		if e := updateSubmissionSnapshot(ctx, repos, &sub, item, &result); e != nil {
			return result, e
		}
	}
	for _, token := range tokens {
		if err := repos.mcp.ClaimMCPUpload(ctx, userID, token, result.ID); err != nil {
			return result, err
		}
	}
	return result, nil
}
