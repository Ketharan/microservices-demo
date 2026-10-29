package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// maxResponseBytes caps how much of a backend response we are willing to read.
// The backend is trusted to be well behaved, but a UI served to anonymous
// visitors should not be a memory amplifier for a misbehaving dependency.
const maxResponseBytes = 2 << 20 // 2 MiB

// Question is one entry of the fixed catalogue served by insights-api.
type Question struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// askRequest is the body of POST /api/ask.
type askRequest struct {
	QuestionID string `json:"questionId"`
}

// askResponse mirrors the backend contract. Data is kept as raw JSON so that a
// shape we do not expect (null, absent, an object, a series) cannot fail the
// decode of the whole response.
type askResponse struct {
	Answer  string          `json:"answer"`
	Data    json.RawMessage `json:"data"`
	Sources []string        `json:"sources"`
	AsOf    string          `json:"asOf"`
	Error   string          `json:"error"`
}

// tableData is the tabular shape of askResponse.Data that this UI can render.
type tableData struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

// client talks to insights-api. A zero baseURL means the backend address was
// never configured; every call then fails with errNotConfigured.
type client struct {
	baseURL string
	http    *http.Client
}

var errNotConfigured = errors.New("INSIGHTS_API_ADDR is not set, so there is no insights-api to query")

// resolveBackendAddr normalises the value injected by the platform. It accepts
// either a bare host:port ("insights-api:8080") or a full URL
// ("http://insights-api:8080"), and never falls back to a hardcoded address.
func resolveBackendAddr(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("cannot parse %q as a backend address: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("backend address %q has unsupported scheme %q", raw, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("backend address %q has no host", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func (c *client) configured() bool { return c.baseURL != "" }

// Questions fetches the catalogue from GET /api/questions.
func (c *client) Questions(ctx context.Context) ([]Question, error) {
	if !c.configured() {
		return nil, errNotConfigured
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/questions", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	body, status, err := c.do(req)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("insights-api returned %s for /api/questions%s", strconv.Itoa(status), errorSuffix(body))
	}
	var questions []Question
	if err := json.Unmarshal(body, &questions); err != nil {
		return nil, fmt.Errorf("could not decode the question catalogue: %w", err)
	}
	return questions, nil
}

// Ask runs one pre-defined question via POST /api/ask.
func (c *client) Ask(ctx context.Context, questionID string) (*askResponse, error) {
	if !c.configured() {
		return nil, errNotConfigured
	}
	payload, err := json.Marshal(askRequest{QuestionID: questionID})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/ask", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	body, status, err := c.do(req)
	if err != nil {
		return nil, err
	}

	var out askResponse
	decodeErr := json.Unmarshal(body, &out)

	if status != http.StatusOK {
		if decodeErr == nil && strings.TrimSpace(out.Error) != "" {
			return nil, fmt.Errorf("insights-api could not answer that question: %s", out.Error)
		}
		return nil, fmt.Errorf("insights-api returned %s for /api/ask%s", strconv.Itoa(status), errorSuffix(body))
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("could not decode the answer from insights-api: %w", decodeErr)
	}
	// A 200 that still carries an error field is treated as an error.
	if strings.TrimSpace(out.Error) != "" && strings.TrimSpace(out.Answer) == "" {
		return nil, fmt.Errorf("insights-api could not answer that question: %s", out.Error)
	}
	return &out, nil
}

func (c *client) do(req *http.Request) ([]byte, int, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("could not reach insights-api: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("could not read the response from insights-api: %w", err)
	}
	return body, resp.StatusCode, nil
}

// errorSuffix pulls {"error": "..."} out of a non-200 body when it is there, so
// the page can show the backend's own wording.
func errorSuffix(body []byte) string {
	var wire struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &wire); err == nil {
		if msg := strings.TrimSpace(wire.Error); msg != "" {
			return ": " + msg
		}
	}
	return ""
}

// parseTable interprets askResponse.Data defensively. Absent, null, an
// unexpected shape or zero rows all come back as a nil table rather than an
// error, so the page can say "no rows returned" instead of breaking.
func parseTable(raw json.RawMessage) *tableData {
	if len(raw) == 0 {
		return nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var t tableData
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil
	}
	if len(t.Columns) == 0 && len(t.Rows) == 0 {
		return nil
	}
	return &t
}

// formatCell renders one JSON value as display text. Numbers keep their shape,
// everything unexpected falls back to its JSON encoding.
func formatCell(v any) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case bool:
		return strconv.FormatBool(val)
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	case json.Number:
		return val.String()
	default:
		b, err := json.Marshal(val)
		if err != nil {
			return fmt.Sprintf("%v", val)
		}
		return string(b)
	}
}
