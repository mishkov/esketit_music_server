package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const permissionAgentUse = "agent.use"

func migrateAgentPermission(ctx context.Context, transaction *sql.Tx) error {
	_, err := transaction.ExecContext(ctx, `INSERT OR IGNORE INTO permissions (code, description, created_at) VALUES (?, ?, ?)`, permissionAgentUse, "Use agent conversations", formatSQLiteTime(time.Now().UTC()))
	// Deliberately grant nobody access automatically. Assign agent.use through
	// existing access-control management to the configured MCP acting user.
	return err
}

type agentGateway struct {
	store *trackStore
	proxy http.Handler
	token string
}

func newAgentGateway(store *trackStore, serviceURL, token string) (*agentGateway, error) {
	gateway := &agentGateway{store: store, token: token}
	if serviceURL == "" {
		return gateway, nil
	}
	target, err := url.Parse(serviceURL)
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") || target.User != nil || target.RawQuery != "" || target.Fragment != "" || strings.Trim(target.Path, "/") != "" {
		return nil, errors.New("AGENT_SERVICE_URL must be an HTTP(S) origin")
	}
	if len(token) < 32 {
		return nil, errors.New("AGENT_SERVICE_TOKEN must contain at least 32 characters")
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(target)
			request.Out.URL.Path = "/internal" + strings.TrimPrefix(request.In.URL.Path, "/api/agent")
			request.Out.Header = make(http.Header)
			request.Out.Header.Set("Content-Type", "application/json")
			request.Out.Header.Set("X-Agent-Device-Token", request.In.Header.Get("X-Agent-Device-Token"))
			request.Out.Header.Set("Authorization", "Bearer "+token)
			userID, _ := userIDFromContext(request.In.Context())
			request.Out.Header.Set("X-Esketit-User-Id", strconv.FormatInt(userID, 10))
		},
		Transport: &http.Transport{ResponseHeaderTimeout: 20 * time.Second},
		ErrorHandler: func(writer http.ResponseWriter, request *http.Request, err error) {
			writeSentryInternalError(writer, request, err, "agent service unavailable", "agent", "agent.gateway")
		},
	}
	gateway.proxy = proxy
	return gateway, nil
}

func (gateway *agentGateway) allowed(userID int64) (bool, error) {
	allowed, err := gateway.store.userHasPermission(userID, permissionAgentUse)
	if err != nil || !allowed {
		return false, err
	}
	var actingUserID *int64
	err = gateway.store.withinReadTransaction(func(ctx context.Context, repositories domainRepositories) error {
		settings, err := repositories.mcp.GetMCPSettings(ctx)
		actingUserID = settings.ActingUserID
		return err
	})
	return err == nil && actingUserID != nil && *actingUserID == userID, err
}

func (gateway *agentGateway) capabilities(writer http.ResponseWriter, request *http.Request) {
	userID, _ := userIDFromContext(request.Context())
	allowed, err := gateway.allowed(userID)
	if err != nil {
		writeSentryInternalError(writer, request, err, "failed to read agent access", "agent", "agent.access")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"canUseAgent": allowed && gateway.proxy != nil})
}

func (gateway *agentGateway) serve(writer http.ResponseWriter, request *http.Request) {
	userID, _ := userIDFromContext(request.Context())
	allowed, err := gateway.allowed(userID)
	if err != nil || !allowed {
		http.Error(writer, "agent access denied", http.StatusForbidden)
		return
	}
	if gateway.proxy == nil {
		http.Error(writer, "agent service is not configured", http.StatusServiceUnavailable)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	gateway.proxy.ServeHTTP(writer, request)
}

func (gateway *agentGateway) internalAccess(writer http.ResponseWriter, request *http.Request) {
	credential := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	if len(gateway.token) < 32 || subtle.ConstantTimeCompare([]byte(credential), []byte(gateway.token)) != 1 {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	userID, err := strconv.ParseInt(request.PathValue("userId"), 10, 64)
	if err != nil {
		http.Error(writer, "invalid user", http.StatusBadRequest)
		return
	}
	allowed, err := gateway.allowed(userID)
	if err != nil || !allowed {
		http.Error(writer, "agent access denied", http.StatusForbidden)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"allowed": true})
}
