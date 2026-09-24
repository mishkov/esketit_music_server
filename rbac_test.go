package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestRBACMigrationNormalizesLegacyRolesAndRemovesRoleColumn(t *testing.T) {
	db, err := openSQLiteDB(filepath.Join(t.TempDir(), "legacy-rbac.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	migrations := defaultSchemaMigrations()
	if err := runSQLiteMigrations(context.Background(), db, migrations[:5]); err != nil {
		t.Fatal(err)
	}
	createdAt := formatSQLiteTime(time.Now().UTC())
	if _, err := db.Exec(`INSERT INTO users (id, email, role, password_hash, created_at) VALUES
		(1, 'admin@example.com', 'admin', 'hash', ?),
		(2, 'listener@example.com', 'listener', 'hash', ?)`, createdAt, createdAt); err != nil {
		t.Fatal(err)
	}
	if err := runSQLiteMigrations(context.Background(), db, migrations); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Query(`PRAGMA table_info(users)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "role" {
			t.Fatal("legacy users.role column still exists")
		}
	}

	for userID, wantRole := range map[int64]string{1: roleAdmin, 2: roleListener} {
		var got string
		if err := db.QueryRow(`SELECT roles.name FROM user_roles JOIN roles ON roles.id = user_roles.role_id WHERE user_roles.user_id = ?`, userID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != wantRole {
			t.Fatalf("user %d role = %q, want %q", userID, got, wantRole)
		}
	}
}

func TestNewUsersReceiveDefaultDatabaseRoles(t *testing.T) {
	store := newTestTrackStore(t)
	t.Cleanup(func() { _ = store.db.Close() })
	admin, err := store.createUser("admin@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := store.createUser("listener@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}

	adminProfile, err := store.getUserAccessProfile(admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !profileHasRole(adminProfile, roleAdmin) || !profileHasPermission(adminProfile, permissionAccessControlManage) {
		t.Fatalf("admin profile = %#v", adminProfile)
	}
	if len(adminProfile.Permissions) != len(definedPermissions()) {
		t.Fatalf("admin permissions = %d, want all %d defined permissions", len(adminProfile.Permissions), len(definedPermissions()))
	}
	listenerProfile, err := store.getUserAccessProfile(listener.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !profileHasRole(listenerProfile, roleListener) || profileHasPermission(listenerProfile, permissionTracksCreate) {
		t.Fatalf("listener profile = %#v", listenerProfile)
	}
	if !profileHasPermission(listenerProfile, permissionPlaylistsUpdate) {
		t.Fatalf("listener lacks default playlist permission: %#v", listenerProfile)
	}
}

func TestPermissionMiddlewareUsesCurrentDatabaseAssignments(t *testing.T) {
	store := newTestTrackStore(t)
	t.Cleanup(func() { _ = store.db.Close() })
	auth := newAuthManager([]byte("test-secret-test-secret-test-secret!!"), time.Hour, time.Hour)
	admin, err := store.createUser("admin@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	worker, err := store.createUser("worker@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	role, err := store.createAccessRole(admin.ID, upsertAccessRoleRequest{Name: "importer", Description: "Imports catalog tracks"})
	if err != nil {
		t.Fatal(err)
	}
	createTrackPermission := permissionByCode(t, store, permissionTracksCreate)
	if _, err := store.replaceRolePermissions(admin.ID, role.ID, []int64{createTrackPermission.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.replaceUserRoles(admin.ID, worker.ID, []int64{role.ID}); err != nil {
		t.Fatal(err)
	}

	token, _, err := auth.createAccessToken(worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/allowed", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	allowed := httptest.NewRecorder()
	requirePermission(auth, store, permissionTracksCreate, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(allowed, request)
	if allowed.Code != http.StatusNoContent {
		t.Fatalf("allowed status = %d, body=%s", allowed.Code, allowed.Body.String())
	}

	denied := httptest.NewRecorder()
	requirePermission(auth, store, permissionTracksDelete, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(denied, request)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("denied status = %d, want %d", denied.Code, http.StatusForbidden)
	}

	if _, err := store.replaceRolePermissions(admin.ID, role.ID, nil); err != nil {
		t.Fatal(err)
	}
	revoked := httptest.NewRecorder()
	requirePermission(auth, store, permissionTracksCreate, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(revoked, request)
	if revoked.Code != http.StatusForbidden {
		t.Fatalf("revoked status = %d, want %d", revoked.Code, http.StatusForbidden)
	}
}

func TestRBACMutationsCannotRemoveLastAccessManager(t *testing.T) {
	store := newTestTrackStore(t)
	t.Cleanup(func() { _ = store.db.Close() })
	admin, err := store.createUser("admin@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := store.getUserAccessProfile(admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(profile.Roles) != 1 {
		t.Fatalf("admin roles = %#v", profile.Roles)
	}
	if _, err := store.replaceUserRoles(admin.ID, admin.ID, nil); !errors.Is(err, errLastAccessManager) {
		t.Fatalf("replaceUserRoles() error = %v, want %v", err, errLastAccessManager)
	}
	allowed, err := store.userHasPermission(admin.ID, permissionAccessControlManage)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatal("last-manager rollback did not restore permission")
	}
	if _, err := store.replaceRolePermissions(admin.ID, profile.Roles[0].ID, nil); !errors.Is(err, errLastAccessManager) {
		t.Fatalf("replaceRolePermissions() error = %v, want %v", err, errLastAccessManager)
	}
	allowed, err = store.userHasPermission(admin.ID, permissionAccessControlManage)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatal("last-manager permission rollback did not restore permission")
	}
	if _, err := store.deleteAccessRole(admin.ID, profile.Roles[0].ID); !errors.Is(err, errSystemAccessRole) {
		t.Fatalf("delete system role error = %v, want %v", err, errSystemAccessRole)
	}
}

func TestRBACMutationsWriteAuditEvents(t *testing.T) {
	store := newTestTrackStore(t)
	t.Cleanup(func() { _ = store.db.Close() })
	admin, err := store.createUser("admin@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	role, err := store.createAccessRole(admin.ID, upsertAccessRoleRequest{Name: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	permissionItem := permissionByCode(t, store, permissionCatalogUnpublishedRead)
	if _, err := store.replaceRolePermissions(admin.ID, role.ID, []int64{permissionItem.ID}); err != nil {
		t.Fatal(err)
	}
	events, err := store.listAccessControlAuditEvents(20)
	if err != nil {
		t.Fatal(err)
	}
	seenCreate, seenPermissions := false, false
	for _, event := range events {
		seenCreate = seenCreate || event.Action == "role.create"
		seenPermissions = seenPermissions || event.Action == "role.permissions.replace"
	}
	if !seenCreate || !seenPermissions {
		t.Fatalf("audit events = %#v", events)
	}
}

func permissionByCode(t *testing.T, store *trackStore, code string) permission {
	t.Helper()
	items, err := store.listPermissions()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Code == code {
			return item
		}
	}
	t.Fatalf("permission %q not found", code)
	return permission{}
}

func profileHasRole(profile userAccessProfile, name string) bool {
	for _, item := range profile.Roles {
		if item.Name == name {
			return true
		}
	}
	return false
}

func profileHasPermission(profile userAccessProfile, code string) bool {
	for _, item := range profile.Permissions {
		if item.Code == code {
			return true
		}
	}
	return false
}
