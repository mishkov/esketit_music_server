package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	permissionAccountReadSelf        = "account.read_self"
	permissionPlaylistsRead          = "playlists.read"
	permissionPlaylistsCreate        = "playlists.create"
	permissionPlaylistsUpdate        = "playlists.update"
	permissionPlaylistsDelete        = "playlists.delete"
	permissionPreferencesUpdate      = "preferences.update"
	permissionAutoplayUse            = "autoplay.use"
	permissionCatalogUnpublishedRead = "catalog.unpublished.read"
	permissionSongsUpload            = "songs.upload"
	permissionSongsCleanup           = "songs.cleanup"
	permissionAlbumsCreate           = "albums.create"
	permissionAlbumsUpdate           = "albums.update"
	permissionAlbumsDelete           = "albums.delete"
	permissionAlbumCoversManage      = "album_covers.manage"
	permissionTracksCreate           = "tracks.create"
	permissionTracksUpdate           = "tracks.update"
	permissionTracksDelete           = "tracks.delete"
	permissionLyricsManage           = "lyrics.manage"
	permissionAuthorsCreate          = "authors.create"
	permissionAuthorsUpdate          = "authors.update"
	permissionAuthorsDelete          = "authors.delete"
	permissionAuthorPhotosUpload     = "author_photos.upload"
	permissionTelegramManage         = "integrations.telegram.manage"
	permissionYouTubeManage          = "integrations.youtube.manage"
	permissionAccessControlManage    = "access_control.manage"
)

var (
	errInvalidAccessRole    = errors.New("invalid access role")
	errAccessRoleNotFound   = errors.New("access role not found")
	errAccessUserNotFound   = errors.New("access user not found")
	errInvalidRoleSelection = errors.New("one or more roles do not exist")
	errInvalidPermissionSet = errors.New("one or more permissions do not exist")
	errLastAccessManager    = errors.New("at least one user must retain access-control management permission")
	errSystemAccessRole     = errors.New("system access roles cannot be renamed or deleted")
)

type accessRole struct {
	ID          int64        `json:"id"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	System      bool         `json:"system"`
	CreatedAt   time.Time    `json:"createdAt"`
	Permissions []permission `json:"permissions"`
}

type permission struct {
	ID          int64     `json:"id"`
	Code        string    `json:"code"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"createdAt"`
}

type permissionDefinition struct {
	Code        string
	Description string
}

type defaultRoleDefinition struct {
	Name        string
	Description string
}

type userRoleAssignment struct {
	UserID int64
	Role   accessRole
}

type userPermissionAssignment struct {
	UserID     int64
	Permission permission
}

type userAccessProfile struct {
	Roles       []accessRole `json:"roles"`
	Permissions []permission `json:"permissions"`
}

type accessControlUser struct {
	ID          int64        `json:"id"`
	Email       string       `json:"email"`
	CreatedAt   time.Time    `json:"createdAt"`
	Roles       []accessRole `json:"roles"`
	Permissions []permission `json:"permissions"`
}

type accessControlAuditEvent struct {
	ID                 int64          `json:"id"`
	ActorUserID        *int64         `json:"actorUserId,omitempty"`
	Action             string         `json:"action"`
	TargetUserID       *int64         `json:"targetUserId,omitempty"`
	TargetRoleID       *int64         `json:"targetRoleId,omitempty"`
	TargetPermissionID *int64         `json:"targetPermissionId,omitempty"`
	Details            map[string]any `json:"details"`
	CreatedAt          time.Time      `json:"createdAt"`
}

type upsertAccessRoleRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type replaceUserRolesRequest struct {
	RoleIDs []int64 `json:"roleIds"`
}

type replaceRolePermissionsRequest struct {
	PermissionIDs []int64 `json:"permissionIds"`
}

func defaultRBACRoles() []defaultRoleDefinition {
	return []defaultRoleDefinition{
		{Name: roleAdmin, Description: "Full server administration"},
		{Name: roleListener, Description: "Personal library and playback access"},
	}
}

func definedPermissions() []permissionDefinition {
	return []permissionDefinition{
		{permissionAccountReadSelf, "Read the authenticated account"},
		{permissionPlaylistsRead, "Read personal playlists"},
		{permissionPlaylistsCreate, "Create personal playlists"},
		{permissionPlaylistsUpdate, "Update personal playlists and their tracks"},
		{permissionPlaylistsDelete, "Delete personal playlists"},
		{permissionPreferencesUpdate, "Update favorite and disliked tracks"},
		{permissionAutoplayUse, "Request autoplay tracks"},
		{permissionCatalogUnpublishedRead, "Read empty or unpublished catalog content"},
		{permissionSongsUpload, "Upload song files"},
		{permissionSongsCleanup, "Inspect and delete unused song files"},
		{permissionAlbumsCreate, "Create albums"},
		{permissionAlbumsUpdate, "Update albums"},
		{permissionAlbumsDelete, "Delete albums"},
		{permissionAlbumCoversManage, "Upload, search, and import album covers"},
		{permissionTracksCreate, "Create tracks"},
		{permissionTracksUpdate, "Update tracks"},
		{permissionTracksDelete, "Delete tracks"},
		{permissionLyricsManage, "Search, update, and delete track lyrics"},
		{permissionAuthorsCreate, "Create authors"},
		{permissionAuthorsUpdate, "Update authors"},
		{permissionAuthorsDelete, "Delete authors"},
		{permissionAuthorPhotosUpload, "Upload author photos"},
		{permissionTelegramManage, "Manage Telegram authorization and imports"},
		{permissionYouTubeManage, "Manage YouTube imports and cookies"},
		{permissionAccessControlManage, "Manage users, roles, and permissions"},
	}
}

func defaultListenerPermissionCodes() []string {
	return []string{
		permissionAccountReadSelf,
		permissionPlaylistsRead,
		permissionPlaylistsCreate,
		permissionPlaylistsUpdate,
		permissionPlaylistsDelete,
		permissionPreferencesUpdate,
		permissionAutoplayUse,
	}
}

func normalizeAccessRoleRequest(request upsertAccessRoleRequest) (upsertAccessRoleRequest, error) {
	request.Name = strings.TrimSpace(request.Name)
	request.Description = strings.TrimSpace(request.Description)
	switch {
	case request.Name == "":
		return upsertAccessRoleRequest{}, fmt.Errorf("%w: name is required", errInvalidAccessRole)
	case len(request.Name) > 100:
		return upsertAccessRoleRequest{}, fmt.Errorf("%w: name must be at most 100 characters", errInvalidAccessRole)
	case len(request.Description) > 500:
		return upsertAccessRoleRequest{}, fmt.Errorf("%w: description must be at most 500 characters", errInvalidAccessRole)
	}
	return request, nil
}

func normalizeAccessIDs(ids []int64) ([]int64, error) {
	seen := make(map[int64]struct{}, len(ids))
	result := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, errors.New("ids must be positive")
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

func (s *trackStore) getUserAccessProfile(userID int64) (userAccessProfile, error) {
	var result userAccessProfile
	err := s.withinReadTransaction(func(ctx context.Context, repositories domainRepositories) error {
		var err error
		result.Roles, err = repositories.access.ListUserRoles(ctx, userID)
		if err != nil {
			return err
		}
		if err := populateRolePermissions(ctx, repositories, result.Roles); err != nil {
			return err
		}
		result.Permissions, err = repositories.access.ListEffectivePermissions(ctx, userID)
		if err == nil {
			normalizeAccessProfile(&result)
		}
		return err
	})
	return result, err
}

func (s *trackStore) userHasPermission(userID int64, code string) (bool, error) {
	return s.accessRepository.UserHasPermission(context.Background(), userID, code)
}

func (s *trackStore) listAccessControlUsers() ([]accessControlUser, error) {
	var result []accessControlUser
	err := s.withinReadTransaction(func(ctx context.Context, repositories domainRepositories) error {
		users, err := repositories.users.List(ctx)
		if err != nil {
			return err
		}
		roleAssignments, err := repositories.access.ListAllUserRoles(ctx)
		if err != nil {
			return err
		}
		permissionAssignments, err := repositories.access.ListAllEffectivePermissions(ctx)
		if err != nil {
			return err
		}
		roles, err := repositories.access.ListRoles(ctx)
		if err != nil {
			return err
		}
		if err := populateRolePermissions(ctx, repositories, roles); err != nil {
			return err
		}
		rolesByID := make(map[int64]accessRole, len(roles))
		for _, item := range roles {
			rolesByID[item.ID] = item
		}
		byUser := make(map[int64]*accessControlUser, len(users))
		result = make([]accessControlUser, 0, len(users))
		for _, item := range users {
			result = append(result, accessControlUser{ID: item.ID, Email: item.Email, CreatedAt: item.CreatedAt, Roles: []accessRole{}, Permissions: []permission{}})
			byUser[item.ID] = &result[len(result)-1]
		}
		for _, assignment := range roleAssignments {
			if item := byUser[assignment.UserID]; item != nil {
				item.Roles = append(item.Roles, rolesByID[assignment.Role.ID])
			}
		}
		for _, assignment := range permissionAssignments {
			if item := byUser[assignment.UserID]; item != nil {
				item.Permissions = append(item.Permissions, assignment.Permission)
			}
		}
		return nil
	})
	return result, err
}

func (s *trackStore) listAccessRoles() ([]accessRole, error) {
	var result []accessRole
	err := s.withinReadTransaction(func(ctx context.Context, repositories domainRepositories) error {
		roles, err := repositories.access.ListRoles(ctx)
		if err != nil {
			return err
		}
		if err := populateRolePermissions(ctx, repositories, roles); err != nil {
			return err
		}
		result = roles
		return nil
	})
	return result, err
}

func (s *trackStore) listPermissions() ([]permission, error) {
	return s.accessRepository.ListPermissions(context.Background())
}

func (s *trackStore) createAccessRole(actorUserID int64, request upsertAccessRoleRequest) (result accessRole, returnErr error) {
	request, err := normalizeAccessRoleRequest(request)
	if err != nil {
		return accessRole{}, err
	}
	returnErr = s.unitOfWork.WithinTransaction(context.Background(), func(repositories domainRepositories) error {
		result, err = repositories.access.InsertRole(context.Background(), request.Name, request.Description, time.Now().UTC())
		if err != nil {
			return err
		}
		result.Permissions = []permission{}
		return repositories.access.InsertAuditEvent(context.Background(), accessControlAuditEvent{
			ActorUserID: &actorUserID, Action: "role.create", TargetRoleID: &result.ID,
			Details: map[string]any{"name": result.Name}, CreatedAt: time.Now().UTC(),
		})
	})
	return
}

func (s *trackStore) updateAccessRole(actorUserID, roleID int64, request upsertAccessRoleRequest) (result accessRole, found bool, returnErr error) {
	request, err := normalizeAccessRoleRequest(request)
	if err != nil {
		return accessRole{}, false, err
	}
	returnErr = s.unitOfWork.WithinTransaction(context.Background(), func(repositories domainRepositories) error {
		current, ok, err := repositories.access.FindRoleByID(context.Background(), roleID)
		if err != nil || !ok {
			return err
		}
		found = true
		if current.System && current.Name != request.Name {
			return errSystemAccessRole
		}
		current.Name, current.Description = request.Name, request.Description
		if err := repositories.access.UpdateRole(context.Background(), current); err != nil {
			return err
		}
		current.Permissions, err = repositories.access.ListRolePermissions(context.Background(), roleID)
		if err != nil {
			return err
		}
		if current.Permissions == nil {
			current.Permissions = []permission{}
		}
		result = current
		return repositories.access.InsertAuditEvent(context.Background(), accessControlAuditEvent{
			ActorUserID: &actorUserID, Action: "role.update", TargetRoleID: &roleID,
			Details: map[string]any{"name": result.Name}, CreatedAt: time.Now().UTC(),
		})
	})
	return
}

func (s *trackStore) deleteAccessRole(actorUserID, roleID int64) (deleted bool, returnErr error) {
	returnErr = s.unitOfWork.WithinTransaction(context.Background(), func(repositories domainRepositories) error {
		current, ok, err := repositories.access.FindRoleByID(context.Background(), roleID)
		if err != nil || !ok {
			return err
		}
		if current.System {
			return errSystemAccessRole
		}
		if err := repositories.access.DeleteRole(context.Background(), roleID); err != nil {
			return err
		}
		if err := ensureAccessManagerExists(context.Background(), repositories); err != nil {
			return err
		}
		deleted = true
		return repositories.access.InsertAuditEvent(context.Background(), accessControlAuditEvent{
			ActorUserID: &actorUserID, Action: "role.delete",
			Details: map[string]any{"roleId": roleID, "name": current.Name}, CreatedAt: time.Now().UTC(),
		})
	})
	return
}

func (s *trackStore) replaceUserRoles(actorUserID, userID int64, roleIDs []int64) (userAccessProfile, error) {
	roleIDs, err := normalizeAccessIDs(roleIDs)
	if err != nil {
		return userAccessProfile{}, fmt.Errorf("%w: %v", errInvalidRoleSelection, err)
	}
	var result userAccessProfile
	err = s.unitOfWork.WithinTransaction(context.Background(), func(repositories domainRepositories) error {
		if _, ok, err := repositories.users.FindByID(context.Background(), userID); err != nil {
			return err
		} else if !ok {
			return errAccessUserNotFound
		}
		count, err := repositories.access.CountRolesByIDs(context.Background(), roleIDs)
		if err != nil {
			return err
		}
		if count != len(roleIDs) {
			return errInvalidRoleSelection
		}
		if err := repositories.access.ReplaceUserRoles(context.Background(), userID, roleIDs, actorUserID, time.Now().UTC()); err != nil {
			return err
		}
		if err := ensureAccessManagerExists(context.Background(), repositories); err != nil {
			return err
		}
		result.Roles, err = repositories.access.ListUserRoles(context.Background(), userID)
		if err != nil {
			return err
		}
		if err := populateRolePermissions(context.Background(), repositories, result.Roles); err != nil {
			return err
		}
		result.Permissions, err = repositories.access.ListEffectivePermissions(context.Background(), userID)
		if err != nil {
			return err
		}
		normalizeAccessProfile(&result)
		return repositories.access.InsertAuditEvent(context.Background(), accessControlAuditEvent{
			ActorUserID: &actorUserID, Action: "user.roles.replace", TargetUserID: &userID,
			Details: map[string]any{"roleIds": roleIDs}, CreatedAt: time.Now().UTC(),
		})
	})
	return result, err
}

func (s *trackStore) replaceRolePermissions(actorUserID, roleID int64, permissionIDs []int64) (accessRole, error) {
	permissionIDs, err := normalizeAccessIDs(permissionIDs)
	if err != nil {
		return accessRole{}, fmt.Errorf("%w: %v", errInvalidPermissionSet, err)
	}
	var result accessRole
	err = s.unitOfWork.WithinTransaction(context.Background(), func(repositories domainRepositories) error {
		current, ok, err := repositories.access.FindRoleByID(context.Background(), roleID)
		if err != nil {
			return err
		}
		if !ok {
			return errAccessRoleNotFound
		}
		count, err := repositories.access.CountPermissionsByIDs(context.Background(), permissionIDs)
		if err != nil {
			return err
		}
		if count != len(permissionIDs) {
			return errInvalidPermissionSet
		}
		if err := repositories.access.ReplaceRolePermissions(context.Background(), roleID, permissionIDs); err != nil {
			return err
		}
		if err := ensureAccessManagerExists(context.Background(), repositories); err != nil {
			return err
		}
		current.Permissions, err = repositories.access.ListRolePermissions(context.Background(), roleID)
		if err != nil {
			return err
		}
		if current.Permissions == nil {
			current.Permissions = []permission{}
		}
		result = current
		return repositories.access.InsertAuditEvent(context.Background(), accessControlAuditEvent{
			ActorUserID: &actorUserID, Action: "role.permissions.replace", TargetRoleID: &roleID,
			Details: map[string]any{"permissionIds": permissionIDs}, CreatedAt: time.Now().UTC(),
		})
	})
	return result, err
}

func ensureAccessManagerExists(ctx context.Context, repositories domainRepositories) error {
	count, err := repositories.access.CountUsersWithPermission(ctx, permissionAccessControlManage)
	if err != nil {
		return err
	}
	if count == 0 {
		return errLastAccessManager
	}
	return nil
}

func normalizeAccessProfile(profile *userAccessProfile) {
	if profile.Roles == nil {
		profile.Roles = []accessRole{}
	}
	if profile.Permissions == nil {
		profile.Permissions = []permission{}
	}
}

func populateRolePermissions(ctx context.Context, repositories domainRepositories, roles []accessRole) error {
	for index := range roles {
		permissions, err := repositories.access.ListRolePermissions(ctx, roles[index].ID)
		if err != nil {
			return err
		}
		if permissions == nil {
			permissions = []permission{}
		}
		roles[index].Permissions = permissions
	}
	return nil
}

func (s *trackStore) listAccessControlAuditEvents(limit int) ([]accessControlAuditEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	return s.accessRepository.ListAuditEvents(context.Background(), limit)
}
