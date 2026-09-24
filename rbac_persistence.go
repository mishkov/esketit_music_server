package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func scanAccessRole(row rowScanner) (accessRole, error) {
	var item accessRole
	var system int
	var createdAt string
	if err := row.Scan(&item.ID, &item.Name, &item.Description, &system, &createdAt); err != nil {
		return accessRole{}, translateSQLiteError(err)
	}
	parsed, err := parseSQLiteTime(createdAt)
	if err != nil {
		return accessRole{}, fmt.Errorf("parse access role creation time: %w", err)
	}
	item.CreatedAt = parsed
	item.System = system != 0
	return item, nil
}

func scanPermission(row rowScanner) (permission, error) {
	var item permission
	var createdAt string
	if err := row.Scan(&item.ID, &item.Code, &item.Description, &createdAt); err != nil {
		return permission{}, translateSQLiteError(err)
	}
	parsed, err := parseSQLiteTime(createdAt)
	if err != nil {
		return permission{}, fmt.Errorf("parse permission creation time: %w", err)
	}
	item.CreatedAt = parsed
	return item, nil
}

func (r *sqliteRepositories) ListRoles(ctx context.Context) (items []accessRole, returnErr error) {
	rows, err := r.q.QueryContext(ctx, `SELECT id, name, description, system, created_at FROM roles ORDER BY name COLLATE NOCASE, id`)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer joinRowsCloseError(&returnErr, rows, "list access roles")
	for rows.Next() {
		item, err := scanAccessRole(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func (r *sqliteRepositories) FindRoleByID(ctx context.Context, id int64) (accessRole, bool, error) {
	item, err := scanAccessRole(r.q.QueryRowContext(ctx, `SELECT id, name, description, system, created_at FROM roles WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return accessRole{}, false, nil
	}
	return item, err == nil, err
}

func (r *sqliteRepositories) InsertRole(ctx context.Context, name, description string, createdAt time.Time) (accessRole, error) {
	result, err := r.q.ExecContext(ctx, `INSERT INTO roles (name, description, system, created_at) VALUES (?, ?, 0, ?)`, name, description, formatSQLiteTime(createdAt))
	if err != nil {
		return accessRole{}, translateSQLiteError(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return accessRole{}, translateSQLiteError(err)
	}
	item, ok, err := r.FindRoleByID(ctx, id)
	if err != nil {
		return accessRole{}, err
	}
	if !ok {
		return accessRole{}, errRepositoryNotFound
	}
	return item, nil
}

func (r *sqliteRepositories) UpdateRole(ctx context.Context, item accessRole) error {
	result, err := r.q.ExecContext(ctx, `UPDATE roles SET name = ?, description = ? WHERE id = ?`, item.Name, item.Description, item.ID)
	if err != nil {
		return translateSQLiteError(err)
	}
	return requireAffected(result)
}

func (r *sqliteRepositories) DeleteRole(ctx context.Context, id int64) error {
	result, err := r.q.ExecContext(ctx, `DELETE FROM roles WHERE id = ?`, id)
	if err != nil {
		return translateSQLiteError(err)
	}
	return requireAffected(result)
}

func (r *sqliteRepositories) ListPermissions(ctx context.Context) (items []permission, returnErr error) {
	rows, err := r.q.QueryContext(ctx, `SELECT id, code, description, created_at FROM permissions ORDER BY code, id`)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer joinRowsCloseError(&returnErr, rows, "list permissions")
	for rows.Next() {
		item, err := scanPermission(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func (r *sqliteRepositories) ListRolePermissions(ctx context.Context, roleID int64) (items []permission, returnErr error) {
	rows, err := r.q.QueryContext(ctx, `SELECT permissions.id, permissions.code, permissions.description, permissions.created_at
		FROM role_permissions
		JOIN permissions ON permissions.id = role_permissions.permission_id
		WHERE role_permissions.role_id = ?
		ORDER BY permissions.code, permissions.id`, roleID)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer joinRowsCloseError(&returnErr, rows, "list role permissions")
	for rows.Next() {
		item, err := scanPermission(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func (r *sqliteRepositories) ListUserRoles(ctx context.Context, userID int64) (items []accessRole, returnErr error) {
	rows, err := r.q.QueryContext(ctx, `SELECT roles.id, roles.name, roles.description, roles.system, roles.created_at
		FROM user_roles JOIN roles ON roles.id = user_roles.role_id
		WHERE user_roles.user_id = ?
		ORDER BY roles.name COLLATE NOCASE, roles.id`, userID)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer joinRowsCloseError(&returnErr, rows, "list user roles")
	for rows.Next() {
		item, err := scanAccessRole(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func (r *sqliteRepositories) ListEffectivePermissions(ctx context.Context, userID int64) (items []permission, returnErr error) {
	rows, err := r.q.QueryContext(ctx, `SELECT DISTINCT permissions.id, permissions.code, permissions.description, permissions.created_at
		FROM user_roles
		JOIN role_permissions ON role_permissions.role_id = user_roles.role_id
		JOIN permissions ON permissions.id = role_permissions.permission_id
		WHERE user_roles.user_id = ?
		ORDER BY permissions.code, permissions.id`, userID)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer joinRowsCloseError(&returnErr, rows, "list effective permissions")
	for rows.Next() {
		item, err := scanPermission(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func (r *sqliteRepositories) ListAllUserRoles(ctx context.Context) (items []userRoleAssignment, returnErr error) {
	rows, err := r.q.QueryContext(ctx, `SELECT user_roles.user_id, roles.id, roles.name, roles.description, roles.system, roles.created_at
		FROM user_roles JOIN roles ON roles.id = user_roles.role_id
		ORDER BY user_roles.user_id, roles.name COLLATE NOCASE, roles.id`)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer joinRowsCloseError(&returnErr, rows, "list all user roles")
	for rows.Next() {
		var item userRoleAssignment
		var system int
		var createdAt string
		if err := rows.Scan(&item.UserID, &item.Role.ID, &item.Role.Name, &item.Role.Description, &system, &createdAt); err != nil {
			return nil, translateSQLiteError(err)
		}
		parsed, err := parseSQLiteTime(createdAt)
		if err != nil {
			return nil, err
		}
		item.Role.CreatedAt = parsed
		item.Role.System = system != 0
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func (r *sqliteRepositories) ListAllEffectivePermissions(ctx context.Context) (items []userPermissionAssignment, returnErr error) {
	rows, err := r.q.QueryContext(ctx, `SELECT DISTINCT user_roles.user_id, permissions.id, permissions.code, permissions.description, permissions.created_at
		FROM user_roles
		JOIN role_permissions ON role_permissions.role_id = user_roles.role_id
		JOIN permissions ON permissions.id = role_permissions.permission_id
		ORDER BY user_roles.user_id, permissions.code, permissions.id`)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer joinRowsCloseError(&returnErr, rows, "list all effective permissions")
	for rows.Next() {
		var item userPermissionAssignment
		var createdAt string
		if err := rows.Scan(&item.UserID, &item.Permission.ID, &item.Permission.Code, &item.Permission.Description, &createdAt); err != nil {
			return nil, translateSQLiteError(err)
		}
		parsed, err := parseSQLiteTime(createdAt)
		if err != nil {
			return nil, err
		}
		item.Permission.CreatedAt = parsed
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func (r *sqliteRepositories) UserHasPermission(ctx context.Context, userID int64, code string) (bool, error) {
	var exists int
	err := r.q.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM user_roles
		JOIN role_permissions ON role_permissions.role_id = user_roles.role_id
		JOIN permissions ON permissions.id = role_permissions.permission_id
		WHERE user_roles.user_id = ? AND permissions.code = ?
	)`, userID, code).Scan(&exists)
	if err != nil {
		return false, translateSQLiteError(err)
	}
	return exists != 0, nil
}

func (r *sqliteRepositories) CountRolesByIDs(ctx context.Context, ids []int64) (int, error) {
	return countAccessIDs(ctx, r.q, "roles", ids)
}

func (r *sqliteRepositories) CountPermissionsByIDs(ctx context.Context, ids []int64) (int, error) {
	return countAccessIDs(ctx, r.q, "permissions", ids)
}

func countAccessIDs(ctx context.Context, q sqlExecutor, table string, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	placeholders, args := accessIDArguments(ids)
	var count int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE id IN (`+placeholders+`)`, args...).Scan(&count); err != nil {
		return 0, translateSQLiteError(err)
	}
	return count, nil
}

func (r *sqliteRepositories) CountUsersWithPermission(ctx context.Context, code string) (int, error) {
	var count int
	err := r.q.QueryRowContext(ctx, `SELECT COUNT(DISTINCT user_roles.user_id)
		FROM user_roles
		JOIN role_permissions ON role_permissions.role_id = user_roles.role_id
		JOIN permissions ON permissions.id = role_permissions.permission_id
		WHERE permissions.code = ?`, code).Scan(&count)
	if err != nil {
		return 0, translateSQLiteError(err)
	}
	return count, nil
}

func (r *sqliteRepositories) ReplaceUserRoles(ctx context.Context, userID int64, roleIDs []int64, actorUserID int64, assignedAt time.Time) error {
	if _, err := r.q.ExecContext(ctx, `DELETE FROM user_roles WHERE user_id = ?`, userID); err != nil {
		return translateSQLiteError(err)
	}
	for _, roleID := range roleIDs {
		if _, err := r.q.ExecContext(ctx, `INSERT INTO user_roles (user_id, role_id, assigned_at, assigned_by_user_id) VALUES (?, ?, ?, ?)`,
			userID, roleID, formatSQLiteTime(assignedAt), actorUserID); err != nil {
			return translateSQLiteError(err)
		}
	}
	return nil
}

func (r *sqliteRepositories) ReplaceRolePermissions(ctx context.Context, roleID int64, permissionIDs []int64) error {
	if _, err := r.q.ExecContext(ctx, `DELETE FROM role_permissions WHERE role_id = ?`, roleID); err != nil {
		return translateSQLiteError(err)
	}
	for _, permissionID := range permissionIDs {
		if _, err := r.q.ExecContext(ctx, `INSERT INTO role_permissions (role_id, permission_id) VALUES (?, ?)`, roleID, permissionID); err != nil {
			return translateSQLiteError(err)
		}
	}
	return nil
}

func (r *sqliteRepositories) AssignUserRoleByName(ctx context.Context, userID int64, roleName string, actorUserID *int64, assignedAt time.Time) error {
	result, err := r.q.ExecContext(ctx, `INSERT INTO user_roles (user_id, role_id, assigned_at, assigned_by_user_id)
		SELECT ?, roles.id, ?, ? FROM roles WHERE roles.name = ?`, userID, formatSQLiteTime(assignedAt), actorUserID, roleName)
	if err != nil {
		return translateSQLiteError(err)
	}
	return requireAffected(result)
}

func (r *sqliteRepositories) InsertAuditEvent(ctx context.Context, event accessControlAuditEvent) error {
	detailsJSON, err := marshalJSONColumn(event.Details)
	if err != nil {
		return err
	}
	_, err = r.q.ExecContext(ctx, `INSERT INTO access_control_audit_events (
		actor_user_id, action, target_user_id, target_role_id, target_permission_id, details_json, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?)`, event.ActorUserID, event.Action, event.TargetUserID, event.TargetRoleID, event.TargetPermissionID, detailsJSON, formatSQLiteTime(event.CreatedAt))
	return translateSQLiteError(err)
}

func (r *sqliteRepositories) ListAuditEvents(ctx context.Context, limit int) (items []accessControlAuditEvent, returnErr error) {
	rows, err := r.q.QueryContext(ctx, `SELECT id, actor_user_id, action, target_user_id, target_role_id, target_permission_id, details_json, created_at
		FROM access_control_audit_events ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, translateSQLiteError(err)
	}
	defer joinRowsCloseError(&returnErr, rows, "list access-control audit events")
	for rows.Next() {
		var item accessControlAuditEvent
		var actorUserID, targetUserID, targetRoleID, targetPermissionID sql.NullInt64
		var detailsJSON, createdAt string
		if err := rows.Scan(&item.ID, &actorUserID, &item.Action, &targetUserID, &targetRoleID, &targetPermissionID, &detailsJSON, &createdAt); err != nil {
			return nil, translateSQLiteError(err)
		}
		item.ActorUserID = nullInt64Pointer(actorUserID)
		item.TargetUserID = nullInt64Pointer(targetUserID)
		item.TargetRoleID = nullInt64Pointer(targetRoleID)
		item.TargetPermissionID = nullInt64Pointer(targetPermissionID)
		if err := unmarshalJSONColumn(detailsJSON, &item.Details); err != nil {
			return nil, err
		}
		parsed, err := parseSQLiteTime(createdAt)
		if err != nil {
			return nil, err
		}
		item.CreatedAt = parsed
		items = append(items, item)
	}
	return items, translateSQLiteError(rows.Err())
}

func accessIDArguments(ids []int64) (string, []any) {
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for index, id := range ids {
		placeholders[index] = "?"
		args[index] = id
	}
	return strings.Join(placeholders, ","), args
}

func nullInt64Pointer(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
}
