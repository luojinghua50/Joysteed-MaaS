package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/rbac"
)

type catalogModel struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type catalogResponse struct {
	Models []catalogModel `json:"models"`
	Total  int            `json:"total"`
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r, rbac.PermissionBillingRead, false); !ok {
		return
	}
	baseURL := strings.TrimRight(strings.TrimSpace(s.cfg.GatewayInternalURL), "/")
	if baseURL == "" || strings.TrimSpace(s.cfg.GatewayInternalToken) == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "gateway model catalog is not configured"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/maas/internal/models", nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot build model catalog request"})
		return
	}
	req.Header.Set("X-MaaS-Internal-Token", s.cfg.GatewayInternalToken)
	resp, err := s.gatewayHTTPClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "gateway model catalog is unavailable"})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("gateway model catalog returned HTTP %d", resp.StatusCode)})
		return
	}
	var payload catalogResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "gateway returned an invalid model catalog"})
		return
	}
	payload.Total = len(payload.Models)
	writeJSON(w, http.StatusOK, payload)
}
