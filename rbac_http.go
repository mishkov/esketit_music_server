package main

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
)

func listAccessControlUsersHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := store.listAccessControlUsers()
		if err != nil {
			writeSentryInternalError(w, r, err, "failed to list access-control users", "database", "access_control.users.list")
			return
		}
		writeJSON(w, http.StatusOK, items)
	}
}

func listAccessRolesHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := store.listAccessRoles()
		if err != nil {
			writeSentryInternalError(w, r, err, "failed to list access roles", "database", "access_control.roles.list")
			return
		}
		writeJSON(w, http.StatusOK, items)
	}
}

func listPermissionsHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := store.listPermissions()
		if err != nil {
			writeSentryInternalError(w, r, err, "failed to list permissions", "database", "access_control.permissions.list")
			return
		}
		writeJSON(w, http.StatusOK, items)
	}
}

func createAccessRoleHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actorUserID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		var request upsertAccessRoleRequest
		if err := decodeJSON(r, &request); err != nil {
			writeRequestDecodeError(w, err)
			return
		}
		item, err := store.createAccessRole(actorUserID, request)
		if err != nil {
			writeAccessControlMutationError(w, r, err, "access_control.roles.create")
			return
		}
		writeJSON(w, http.StatusCreated, item)
	}
}

func updateAccessRoleByRouteHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/permissions") {
			replaceRolePermissionsHandler(store).ServeHTTP(w, r)
			return
		}
		actorUserID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		roleID, err := parseResourceID(r.URL.Path, "/api/access-control/roles/")
		if err != nil {
			http.Error(w, "invalid role id", http.StatusBadRequest)
			return
		}
		var request upsertAccessRoleRequest
		if err := decodeJSON(r, &request); err != nil {
			writeRequestDecodeError(w, err)
			return
		}
		item, found, err := store.updateAccessRole(actorUserID, roleID, request)
		if err != nil {
			writeAccessControlMutationError(w, r, err, "access_control.roles.update")
			return
		}
		if !found {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, item)
	}
}

func deleteAccessRoleHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actorUserID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		roleID, err := parseResourceID(r.URL.Path, "/api/access-control/roles/")
		if err != nil {
			http.Error(w, "invalid role id", http.StatusBadRequest)
			return
		}
		deleted, err := store.deleteAccessRole(actorUserID, roleID)
		if err != nil {
			writeAccessControlMutationError(w, r, err, "access_control.roles.delete")
			return
		}
		if !deleted {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func replaceUserRolesHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actorUserID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		userID, err := parseSuffixedAccessID(r.URL.Path, "/api/access-control/users/", "/roles")
		if err != nil {
			http.Error(w, "invalid user id", http.StatusBadRequest)
			return
		}
		var request replaceUserRolesRequest
		if err := decodeJSON(r, &request); err != nil {
			writeRequestDecodeError(w, err)
			return
		}
		profile, err := store.replaceUserRoles(actorUserID, userID, request.RoleIDs)
		if err != nil {
			writeAccessControlMutationError(w, r, err, "access_control.users.replace_roles")
			return
		}
		writeJSON(w, http.StatusOK, profile)
	}
}

func replaceRolePermissionsHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actorUserID, ok := userIDFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		roleID, err := parseSuffixedAccessID(r.URL.Path, "/api/access-control/roles/", "/permissions")
		if err != nil {
			http.Error(w, "invalid role id", http.StatusBadRequest)
			return
		}
		var request replaceRolePermissionsRequest
		if err := decodeJSON(r, &request); err != nil {
			writeRequestDecodeError(w, err)
			return
		}
		item, err := store.replaceRolePermissions(actorUserID, roleID, request.PermissionIDs)
		if err != nil {
			writeAccessControlMutationError(w, r, err, "access_control.roles.replace_permissions")
			return
		}
		writeJSON(w, http.StatusOK, item)
	}
}

func listAccessAuditEventsHandler(store *trackStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 100
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed <= 0 || parsed > 500 {
				http.Error(w, "limit must be between 1 and 500", http.StatusBadRequest)
				return
			}
			limit = parsed
		}
		items, err := store.listAccessControlAuditEvents(limit)
		if err != nil {
			writeSentryInternalError(w, r, err, "failed to list access-control audit events", "database", "access_control.audit.list")
			return
		}
		writeJSON(w, http.StatusOK, items)
	}
}

func parseSuffixedAccessID(path, prefix, suffix string) (int64, error) {
	if !strings.HasSuffix(path, suffix) {
		return 0, errors.New("invalid access-control path")
	}
	return parseResourceID(strings.TrimSuffix(path, suffix), prefix)
}

func writeAccessControlMutationError(w http.ResponseWriter, r *http.Request, err error, operation string) {
	switch {
	case errors.Is(err, errInvalidAccessRole), errors.Is(err, errInvalidRoleSelection), errors.Is(err, errInvalidPermissionSet):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, errAccessRoleNotFound), errors.Is(err, errAccessUserNotFound), errors.Is(err, errRepositoryNotFound):
		http.NotFound(w, r)
	case errors.Is(err, errRepositoryConflict), errors.Is(err, errLastAccessManager), errors.Is(err, errSystemAccessRole):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		writeSentryInternalError(w, r, err, "failed to update access control", "database", operation)
	}
}
