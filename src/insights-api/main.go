package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type server struct {
	mcp *mcpClient

	namespace     string
	environment   string
	lookback      time.Duration
	lookbackLabel string

	// configured is false when no MCP gateway address was injected. The service
	// still starts and still serves the catalogue in that state, because a
	// readiness failure here would make the cause much harder to see than an
	// explicit message on the page.
	configured bool
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// mcpEndpoint builds the JSON-RPC endpoint from what the ai-mcp-federation
// trait injects. The trait sets MCP_GATEWAY_URL to a bare http://host:port, so
// a path is appended -- but if an operator has already included one, it is
// respected rather than doubled.
//
// Parsed with net/url rather than string surgery. The hand-rolled version of
// this had a bug that a permissive test server hid: it checked for an existing
// path with strings.Contains(base, "/") after trimming only the "https://"
// prefix, so on an http:// URL the "//" of the scheme itself matched, the
// function returned early, and the path was silently dropped. Against a real
// gateway that is a 404 on every call.
func mcpEndpoint() (string, bool) {
	raw := strings.TrimSpace(os.Getenv("MCP_GATEWAY_URL"))
	if raw == "" {
		return "", false
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		log.Printf("MCP_GATEWAY_URL=%q is not a usable URL: %v", raw, err)
		return "", false
	}

	// Only a genuine path counts as "already specified"; "" and "/" do not.
	if p := strings.Trim(u.Path, "/"); p != "" {
		u.Path = "/" + p
		return u.String(), true
	}

	path := env("MCP_GATEWAY_PATH", "/mcp")
	u.Path = "/" + strings.Trim(path, "/")
	return u.String(), true
}

func main() {
	port := env("PORT", "8080")

	lookback := 24 * time.Hour
	label := "24 hours"
	if raw := strings.TrimSpace(os.Getenv("INSIGHTS_LOOKBACK")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			log.Printf("INSIGHTS_LOOKBACK=%q is not a duration, using 24h: %v", raw, err)
		} else {
			lookback, label = parsed, raw
		}
	}

	endpoint, configured := mcpEndpoint()
	srv := &server{
		namespace:     env("INSIGHTS_NAMESPACE", "boutique"),
		environment:   env("INSIGHTS_ENVIRONMENT", "development"),
		lookback:      lookback,
		lookbackLabel: label,
		configured:    configured,
	}
	if configured {
		component := env("MCP_COMPONENT_NAME", "insights-api")
		srv.mcp = newMCPClient(endpoint, component)
		log.Printf("MCP endpoint: %s (x-openchoreo-component: %s)", endpoint, component)
	} else {
		log.Printf("MCP_GATEWAY_URL is not set: questions will report the gateway as unconfigured")
	}
	log.Printf("scope: namespace=%s environment=%s lookback=%s", srv.namespace, srv.environment, label)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", srv.handleHealth)
	mux.HandleFunc("/api/questions", srv.handleQuestions)
	mux.HandleFunc("/api/ask", srv.handleAsk)
	mux.HandleFunc("/api/debug/tools", srv.handleDebugTools)

	httpSrv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("insights-api listening on :%s", port)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server stopped: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeErr(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// handleHealth never touches the gateway. Liveness must not depend on a
// downstream that is allowed to be absent.
func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *server) handleQuestions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	type public struct {
		ID          string `json:"id"`
		Label       string `json:"label"`
		Description string `json:"description"`
	}
	out := make([]public, 0, 5)
	for _, q := range s.catalogue() {
		out = append(out, public{ID: q.ID, Label: q.Label, Description: q.Description})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleAsk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}

	var body struct {
		QuestionID string `json:"questionId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "body must be JSON with a questionId")
		return
	}
	if body.QuestionID == "" {
		writeErr(w, http.StatusBadRequest, "questionId is required")
		return
	}

	// The fixed catalogue IS the authorization boundary. This service is reached
	// from a public, anonymous page, so an id that is not in the catalogue is
	// refused rather than interpreted -- there is no path from visitor input to
	// an arbitrary tool call or query.
	var found *question
	for _, q := range s.catalogue() {
		if q.ID == body.QuestionID {
			qq := q
			found = &qq
			break
		}
	}
	if found == nil {
		writeErr(w, http.StatusNotFound, "unknown questionId %q", body.QuestionID)
		return
	}

	if !s.configured {
		writeErr(w, http.StatusServiceUnavailable,
			"the MCP gateway address is not configured: MCP_GATEWAY_URL is unset, "+
				"which means the ai-mcp-federation trait is not attached to this component")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	result, err := found.run(s, ctx)
	if err != nil {
		// The tool name is in the error on purpose: when a question fails, the
		// useful fact is which federated tool refused, not that "ask" failed.
		log.Printf("question %s failed: %v", found.ID, err)
		writeErr(w, http.StatusBadGateway, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleDebugTools answers the first question anyone has in-cluster: is the
// gateway reachable, and what is it actually fronting? Read-only and it names
// no data, only tool names.
func (s *server) handleDebugTools(w http.ResponseWriter, r *http.Request) {
	if !s.configured {
		writeErr(w, http.StatusServiceUnavailable, "MCP_GATEWAY_URL is not set")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	names, err := s.mcp.listTools(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(names), "tools": names})
}
