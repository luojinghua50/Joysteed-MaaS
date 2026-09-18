package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/rbac"
	"github.com/luojinghua50/Joysteed-MaaS/internal/requestlog"
	tenant "github.com/luojinghua50/Joysteed-MaaS/internal/tenantid"
	"gorm.io/gorm"
)

func parsePortalRequestLogPath(path string) (string, bool) {
	const prefix = "/api/portal/request-logs/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	id, err := url.PathUnescape(strings.TrimPrefix(path, prefix))
	return id, err == nil && id != "" && !strings.Contains(id, "/")
}

func parsePortalUsageDetailPath(path string) (string, bool) {
	const prefix = "/api/portal/usage/"
	const suffix = "/detail"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	id, err := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix))
	return id, err == nil && id != "" && !strings.Contains(id, "/")
}

func (s *Server) handlePortalRequestLogs(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.authorize(w, r, rbac.PermissionRequestLogRead, false)
	if !ok {
		return
	}
	limit := queryInt(r, "limit", 50)
	if limit > 100 {
		limit = 100
	}
	offset := queryInt(r, "offset", 0)
	if offset < 0 {
		offset = 0
	}
	keyIDs, err := s.tenantVirtualKeyIDs(r, claims.Principal.TenantID)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if len(keyIDs) == 0 {
		writeJSON(w, http.StatusOK, requestlog.ListResponse{Logs: []requestlog.ListItem{}, Limit: limit, Offset: offset})
		return
	}
	var result requestlog.ListResponse
	status, err := s.gatewayRequestLogs(r, "/maas/internal/logs", keyIDs, url.Values{
		"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)},
	}, &result)
	if err != nil {
		writeGatewayLogError(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handlePortalRequestLogDetail(w http.ResponseWriter, r *http.Request, id string) {
	claims, ok := s.authorize(w, r, rbac.PermissionRequestLogRead, false)
	if !ok {
		return
	}
	keyIDs, err := s.tenantVirtualKeyIDs(r, claims.Principal.TenantID)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if len(keyIDs) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "request log not found"})
		return
	}
	var detail requestlog.Detail
	status, err := s.gatewayRequestLogs(r, "/maas/internal/logs/"+url.PathEscape(id), keyIDs, nil, &detail)
	if err != nil {
		writeGatewayLogError(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handlePortalUsageDetail(w http.ResponseWriter, r *http.Request, id string) {
	claims, ok := s.authorize(w, r, rbac.PermissionRequestLogRead, false)
	if !ok {
		return
	}
	usage, err := s.billing.GetUsage(r.Context(), claims.Principal.TenantID, id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "usage event not found"})
		return
	}
	if err != nil {
		writeDBError(w, err)
		return
	}
	keyIDs, err := s.tenantVirtualKeyIDs(r, claims.Principal.TenantID)
	if err != nil {
		writeDBError(w, err)
		return
	}
	response := map[string]any{"usage": usage, "log_available": false, "reason": "request_log_expired_or_unavailable"}
	if len(keyIDs) == 0 || usage.EffectiveGatewayLogID() == "" {
		writeJSON(w, http.StatusOK, response)
		return
	}
	var detail requestlog.Detail
	status, gatewayErr := s.gatewayRequestLogs(r, "/maas/internal/logs/"+url.PathEscape(usage.EffectiveGatewayLogID()), keyIDs, nil, &detail)
	if gatewayErr != nil {
		if status == http.StatusNotFound {
			writeJSON(w, http.StatusOK, response)
			return
		}
		writeGatewayLogError(w, status, gatewayErr)
		return
	}
	response["log_available"] = true
	response["reason"] = ""
	response["log"] = detail
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) tenantVirtualKeyIDs(r *http.Request, tenantID tenant.ID) ([]string, error) {
	keys, err := s.keys.List(r.Context(), tenantID, 1000)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(keys))
	for _, key := range keys {
		ids = append(ids, key.ID)
	}
	return ids, nil
}

func queryInt(r *http.Request, key string, fallback int) int {
	value, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil {
		return fallback
	}
	return value
}

func (s *Server) gatewayRequestLogs(r *http.Request, path string, keyIDs []string, query url.Values, target any) (int, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(s.cfg.GatewayInternalURL), "/")
	if baseURL == "" || strings.TrimSpace(s.cfg.GatewayInternalToken) == "" {
		return http.StatusServiceUnavailable, errors.New("gateway request logs are not configured")
	}
	if query == nil {
		query = make(url.Values)
	}
	for _, id := range keyIDs {
		query.Add("virtual_key_id", id)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path+"?"+query.Encode(), nil)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	req.Header.Set("X-MaaS-Internal-Token", s.cfg.GatewayInternalToken)
	resp, err := s.gatewayHTTPClient.Do(req)
	if err != nil {
		return http.StatusBadGateway, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, fmt.Errorf("gateway returned HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(target); err != nil {
		return http.StatusBadGateway, fmt.Errorf("invalid gateway response: %w", err)
	}
	return http.StatusOK, nil
}

func writeGatewayLogError(w http.ResponseWriter, status int, err error) {
	if status == http.StatusNotFound {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "request log not found"})
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": "gateway request logs are unavailable", "detail": err.Error()})
}
