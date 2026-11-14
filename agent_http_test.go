package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentGatewayDoesNotTrustForwardedIdentity(t *testing.T) {
	store := newTestTrackStore(t)
	defer store.db.Close()
	owner, err := store.createUser("agent@example.test", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE mcp_settings SET acting_user_id=?`, owner.ID); err != nil {
		t.Fatal(err)
	}
	var receivedIdentity, receivedAuthorization, receivedPath string
	service := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		receivedIdentity, receivedAuthorization, receivedPath = request.Header.Get("X-Esketit-User-Id"), request.Header.Get("Authorization"), request.URL.Path
		writeJSON(writer, http.StatusOK, []string{})
	}))
	defer service.Close()
	token := strings.Repeat("s", 32)
	gateway, err := newAgentGateway(store, service.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/agent/chats", nil)
	request.Header.Set("X-Esketit-User-Id", "999")
	request.Header.Set("Authorization", "Bearer app-session")
	request = request.WithContext(context.WithValue(request.Context(), userContextKey, owner.ID))
	response := httptest.NewRecorder()
	gateway.serve(response, request)
	if response.Code != http.StatusOK || receivedIdentity != "1" || receivedAuthorization != "Bearer "+token || receivedPath != "/internal/chats" {
		t.Fatalf("unexpected gateway result status=%d identity=%q path=%q", response.Code, receivedIdentity, receivedPath)
	}
}

func TestAgentAccessRequiresPermissionAndEnabledMCPUser(t *testing.T) {
	store := newTestTrackStore(t)
	defer store.db.Close()
	owner, err := store.createUser("owner@example.test", "hash")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := store.createUser("listener@example.test", "hash")
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := newAgentGateway(store, "http://127.0.0.1:8312", strings.Repeat("x", 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE mcp_settings SET acting_user_id=?`, listener.ID); err != nil {
		t.Fatal(err)
	}
	if allowed, err := gateway.allowed(owner.ID); err != nil || !allowed {
		t.Fatalf("separate authorized chat owner was rejected: %v", err)
	}
	if allowed, err := gateway.allowed(listener.ID); err != nil || allowed {
		t.Fatalf("listener without permission was accepted: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE mcp_settings SET acting_user_id=?`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if allowed, err := gateway.allowed(owner.ID); err != nil || !allowed {
		t.Fatalf("authorized identity rejected: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE mcp_settings SET acting_user_id=NULL`); err != nil {
		t.Fatal(err)
	}
	if allowed, err := gateway.allowed(owner.ID); err != nil || allowed {
		t.Fatalf("disabled MCP was accepted: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE mcp_settings SET acting_user_id=?`, listener.ID); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/agent/capabilities", nil)
	request = request.WithContext(context.WithValue(request.Context(), userContextKey, owner.ID))
	response := httptest.NewRecorder()
	gateway.capabilities(response, request)
	var result map[string]bool
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || !result["canUseAgent"] {
		t.Fatal("missing capability")
	}
	if _, err := store.db.Exec(`DELETE FROM role_permissions WHERE permission_id=(SELECT id FROM permissions WHERE code=?)`, permissionAgentUse); err != nil {
		t.Fatal(err)
	}
	if allowed, err := gateway.allowed(owner.ID); err != nil || allowed {
		t.Fatalf("revoked chat access was accepted: %v", err)
	}
}

func TestAgentInternalAccessRejectsAppCredentials(t *testing.T) {
	store := newTestTrackStore(t)
	defer store.db.Close()
	gateway, err := newAgentGateway(store, "http://127.0.0.1:8312", strings.Repeat("x", 32))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/agent-internal/users/1", nil)
	request.Header.Set("Authorization", "Bearer unrelated-app-token")
	response := httptest.NewRecorder()
	gateway.internalAccess(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestAgentChatOwnerCanReviewMCPAccountSubmission(t *testing.T) {
	service, actor := newMCPTestService(t)
	gateway, err := newAgentGateway(service.store, "http://127.0.0.1:8312", strings.Repeat("s", 32))
	if err != nil {
		t.Fatal(err)
	}
	const ownerID int64 = 1
	if allowed, err := gateway.allowed(ownerID); err != nil || !allowed {
		t.Fatalf("chat owner access: %v", err)
	}
	submission := callMCP(t, service, "submit_author", map[string]any{"requestId": "separate-chat-owner", "currentName": "Agent Review Identity Test"})
	identifier := mcpID(submission, "submissionId")
	if _, err := service.store.acquireCatalogReviewLease(actor.ID, actor.ID); !errors.Is(err, errCatalogSelfReview) {
		t.Fatalf("self review error: %v", err)
	}
	lease, err := service.store.acquireCatalogReviewLease(ownerID, actor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.store.approveCatalogSubmission(ownerID, identifier, lease.LeaseToken); err != nil {
		t.Fatalf("chat owner could not approve agent submission: %v", err)
	}
	result := callMCP(t, service, "get_submission", map[string]any{"submissionId": identifier})
	if result["status"] != "approved" {
		t.Fatalf("unexpected review outcome: %v", result["status"])
	}
}
