package main

import (
	"context"
	"encoding/json"
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

func TestAgentAccessRequiresPermissionAndMatchingMCPUser(t *testing.T) {
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
	if allowed, err := gateway.allowed(owner.ID); err != nil || allowed {
		t.Fatalf("different MCP identity was accepted: %v", err)
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
	request := httptest.NewRequest(http.MethodGet, "/api/agent/capabilities", nil)
	request = request.WithContext(context.WithValue(request.Context(), userContextKey, owner.ID))
	response := httptest.NewRecorder()
	gateway.capabilities(response, request)
	var result map[string]bool
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || !result["canUseAgent"] {
		t.Fatal("missing capability")
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
