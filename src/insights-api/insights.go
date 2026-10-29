package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// maxRows caps what a single answer renders. The tools are happy to return
// thousands of records; a demo page is not.
const maxRows = 50

type table struct {
	Columns   []string   `json:"columns"`
	Rows      [][]string `json:"rows"`
	Truncated bool       `json:"truncated,omitempty"`
}

type answer struct {
	Answer  string   `json:"answer"`
	Data    *table   `json:"data"`
	Sources []string `json:"sources"`
	AsOf    string   `json:"asOf"`
}

type question struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
	// run is not serialised; the catalogue endpoint returns the three fields
	// above. Receiver first, because catalogue() assigns method expressions.
	run func(*server, context.Context) (*answer, error) `json:"-"`
}

// ---------------------------------------------------------------------------
// Turning a tool's JSON into something renderable
// ---------------------------------------------------------------------------

// records pulls the list out of a tool response WITHOUT knowing that tool's
// schema. The observer's handlers marshal their own response types, so shapes
// differ per tool -- costs return {"items":[...]}, others return a bare array
// or an object of named sections. Hardcoding each one would mean this service
// breaks every time an unrelated response type gains a field, so instead:
//
//	{"items":[...]}         -> that array
//	{"anything":[...]}      -> the first array-valued key, when there is one
//	[...]                   -> itself
//	{...}                   -> a single record
//
// This is deliberately generic. The demo's claim is that the answer was
// composed from federated MCP tools, which `sources` evidences; it is not a
// claim to have hand-modelled every response type.
func records(raw json.RawMessage) ([]map[string]any, error) {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("tool returned text that is not JSON: %w", err)
	}

	toRecords := func(list []any) []map[string]any {
		out := make([]map[string]any, 0, len(list))
		for _, item := range list {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
				continue
			}
			out = append(out, map[string]any{"value": item})
		}
		return out
	}

	switch v := decoded.(type) {
	case []any:
		return toRecords(v), nil
	case map[string]any:
		if items, ok := v["items"].([]any); ok {
			return toRecords(items), nil
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if list, ok := v[k].([]any); ok {
				return toRecords(list), nil
			}
		}
		return []map[string]any{v}, nil
	default:
		return []map[string]any{{"value": decoded}}, nil
	}
}

// preferredFirst keeps the columns a human scans for on the left, rather than
// in whatever order the map iterated.
var preferredFirst = []string{
	"project", "component", "environment", "namespace", "name",
	"deployedAt", "timestamp", "startTime", "endTime",
}

func asTable(recs []map[string]any, drop ...string) *table {
	dropped := map[string]bool{}
	for _, d := range drop {
		dropped[d] = true
	}

	seen := map[string]bool{}
	var rest []string
	for _, r := range recs {
		for k := range r {
			if dropped[k] || seen[k] {
				continue
			}
			seen[k] = true
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)

	var cols []string
	for _, p := range preferredFirst {
		if seen[p] {
			cols = append(cols, p)
			dropped[p] = true
		}
	}
	for _, c := range rest {
		if !dropped[c] {
			cols = append(cols, c)
		}
	}

	t := &table{Columns: cols, Rows: [][]string{}}
	for i, r := range recs {
		if i >= maxRows {
			t.Truncated = true
			break
		}
		row := make([]string, 0, len(cols))
		for _, c := range cols {
			row = append(row, stringify(r[c]))
		}
		t.Rows = append(t.Rows, row)
	}
	return t
}

func stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		// Whole numbers read better without a decimal tail; money and ratios
		// are cut to two places rather than printed at float precision.
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', 2, 64)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprint(x)
		}
		return string(b)
	}
}

// num reads a numeric field defensively: absent and non-numeric both mean "no
// number", never a zero that would silently skew a total.
func num(r map[string]any, key string) (float64, bool) {
	v, ok := r[key].(float64)
	return v, ok
}

func str(r map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := r[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ---------------------------------------------------------------------------
// The catalogue
// ---------------------------------------------------------------------------

func (s *server) catalogue() []question {
	return []question{
		{
			ID:          "cost",
			Label:       "What does this estate cost, and where is it going?",
			Description: "Per-component infrastructure cost for the window, plus the largest right-sizing saving available.",
			run:         (*server).answerCost,
		},
		{
			ID:          "recent-changes",
			Label:       "What changed recently?",
			Description: "The deployments that landed in the window, newest first, with their outcome and lead time.",
			run:         (*server).answerRecentChanges,
		},
		{
			ID:          "busiest",
			Label:       "What is the busiest path through the storefront?",
			Description: "HTTP request metrics by component, so the hot path is visible rather than assumed.",
			run:         (*server).answerBusiest,
		},
		{
			ID:          "headroom",
			Label:       "Which component is closest to its resource limits?",
			Description: "CPU and memory usage against requests and limits, as a headroom ranking.",
			run:         (*server).answerHeadroom,
		},
		{
			ID:          "cadence",
			Label:       "What is our delivery cadence?",
			Description: "DORA deployment frequency and lead time for the window, with data caveats stated.",
			run:         (*server).answerCadence,
		},
	}
}

// window returns the query range. Every tool in use takes start_time/end_time
// as required RFC3339 strings.
func (s *server) window() (string, string, time.Time) {
	now := time.Now().UTC()
	return now.Add(-s.lookback).Format(time.RFC3339), now.Format(time.RFC3339), now
}

func (s *server) scope(start, end string) map[string]any {
	return map[string]any{
		"namespace":   s.namespace,
		"environment": s.environment,
		"start_time":  start,
		"end_time":    end,
	}
}

func (s *server) answerCost(ctx context.Context) (*answer, error) {
	start, end, now := s.window()
	sources := []string{"query_costs"}

	raw, err := s.mcp.callTool(ctx, "query_costs", s.scope(start, end))
	if err != nil {
		return nil, err
	}
	recs, err := records(raw)
	if err != nil {
		return nil, err
	}

	var total float64
	var topName string
	var topCost float64
	for _, r := range recs {
		cpu, _ := num(r, "cpuCost")
		mem, _ := num(r, "memoryCost")
		c := cpu + mem
		total += c
		if c > topCost {
			topCost, topName = c, str(r, "component", "name")
		}
	}

	prose := fmt.Sprintf("Over the last %s, %d cost %s total %.2f across this estate.",
		s.lookbackLabel, len(recs), plural(len(recs), "record", "records"), total)
	if len(recs) == 0 {
		prose = fmt.Sprintf("No cost records were returned for namespace %q in %q over the last %s.",
			s.namespace, s.environment, s.lookbackLabel)
	} else if topName != "" {
		prose += fmt.Sprintf(" The largest single line is %s at %.2f.", topName, topCost)
	}

	// Recommendations are additive: a failure here still leaves a usable cost
	// answer, so it is reported in the prose rather than failing the request.
	if recRaw, recErr := s.mcp.callTool(ctx, "query_recommendations", s.scope(start, end)); recErr == nil {
		sources = append(sources, "query_recommendations")
		if recRecs, rErr := records(recRaw); rErr == nil {
			prose += fmt.Sprintf(" %d right-sizing %s available.",
				len(recRecs), plural(len(recRecs), "recommendation is", "recommendations are"))
		}
	} else {
		prose += " Right-sizing recommendations could not be read for this window."
	}

	return &answer{
		Answer:  prose,
		Data:    asTable(recs, "componentUid", "environmentUid", "projectUid"),
		Sources: sources,
		AsOf:    now.Format(time.RFC3339),
	}, nil
}

func (s *server) answerRecentChanges(ctx context.Context) (*answer, error) {
	start, end, now := s.window()
	args := map[string]any{
		"namespace":  s.namespace,
		"start_time": start,
		"end_time":   end,
		"sort_order": "desc",
		"limit":      maxRows,
	}
	raw, err := s.mcp.callTool(ctx, "query_dora_deployments", args)
	if err != nil {
		return nil, err
	}
	recs, err := records(raw)
	if err != nil {
		return nil, err
	}

	failed := 0
	for _, r := range recs {
		if strings.EqualFold(str(r, "outcome", "status", "result"), "failed") {
			failed++
		}
	}

	prose := fmt.Sprintf("%d %s landed in namespace %q over the last %s.",
		len(recs), plural(len(recs), "deployment", "deployments"), s.namespace, s.lookbackLabel)
	switch {
	case len(recs) == 0:
		prose = fmt.Sprintf("No deployments were recorded for namespace %q over the last %s.",
			s.namespace, s.lookbackLabel)
	case failed == 0:
		prose += " All of them succeeded."
	default:
		prose += fmt.Sprintf(" %d of them failed.", failed)
	}

	return &answer{
		Answer:  prose,
		Data:    asTable(recs, "componentUid", "environmentUid", "projectUid"),
		Sources: []string{"query_dora_deployments"},
		AsOf:    now.Format(time.RFC3339),
	}, nil
}

func (s *server) answerBusiest(ctx context.Context) (*answer, error) {
	start, end, now := s.window()
	args := s.scope(start, end)
	args["step"] = "5m"

	raw, err := s.mcp.callTool(ctx, "query_http_metrics", args)
	if err != nil {
		return nil, err
	}
	recs, err := records(raw)
	if err != nil {
		return nil, err
	}

	prose := fmt.Sprintf("HTTP metrics for %d %s in %q over the last %s.",
		len(recs), plural(len(recs), "series", "series"), s.environment, s.lookbackLabel)
	if len(recs) == 0 {
		prose = fmt.Sprintf("No HTTP metrics were returned for %q in %q over the last %s. "+
			"The traffic simulator drives this data, so an empty result usually means it is not running.",
			s.namespace, s.environment, s.lookbackLabel)
	}

	return &answer{
		Answer:  prose,
		Data:    asTable(recs, "componentUid", "environmentUid", "projectUid"),
		Sources: []string{"query_http_metrics"},
		AsOf:    now.Format(time.RFC3339),
	}, nil
}

func (s *server) answerHeadroom(ctx context.Context) (*answer, error) {
	start, end, now := s.window()
	args := s.scope(start, end)
	args["step"] = "5m"

	raw, err := s.mcp.callTool(ctx, "query_resource_metrics", args)
	if err != nil {
		return nil, err
	}
	recs, err := records(raw)
	if err != nil {
		return nil, err
	}

	prose := fmt.Sprintf("Resource metrics for %d %s in %q over the last %s. "+
		"Compare usage against requests and limits to find the tightest headroom.",
		len(recs), plural(len(recs), "series", "series"), s.environment, s.lookbackLabel)
	if len(recs) == 0 {
		prose = fmt.Sprintf("No resource metrics were returned for %q in %q over the last %s.",
			s.namespace, s.environment, s.lookbackLabel)
	}

	return &answer{
		Answer:  prose,
		Data:    asTable(recs, "componentUid", "environmentUid", "projectUid"),
		Sources: []string{"query_resource_metrics"},
		AsOf:    now.Format(time.RFC3339),
	}, nil
}

func (s *server) answerCadence(ctx context.Context) (*answer, error) {
	start, end, now := s.window()
	args := map[string]any{
		"namespace":  s.namespace,
		"start_time": start,
		"end_time":   end,
		"metrics":    []string{"deployment_frequency", "lead_time_for_changes"},
	}
	raw, err := s.mcp.callTool(ctx, "query_dora_metrics", args)
	if err != nil {
		return nil, err
	}
	recs, err := records(raw)
	if err != nil {
		return nil, err
	}

	// Stated, not hidden. Lead time is derived from commit-to-deploy provenance,
	// and on this estate every workload still reads spec.source: none, so the
	// figure is structurally absent rather than zero. An answer that showed a
	// confident 0 here would be worse than one that explains itself.
	prose := fmt.Sprintf("DORA metrics for namespace %q over the last %s. "+
		"Deployment frequency is derived from completed rollouts. "+
		"Lead time for changes has no data on this estate yet: it needs commit-to-deploy "+
		"provenance, and the workloads here are built without source provenance "+
		"(spec.source: none), so treat any lead-time figure as absent rather than fast.",
		s.namespace, s.lookbackLabel)
	if len(recs) == 0 {
		prose = fmt.Sprintf("No DORA metrics were returned for namespace %q over the last %s.",
			s.namespace, s.lookbackLabel)
	}

	return &answer{
		Answer:  prose,
		Data:    asTable(recs),
		Sources: []string{"query_dora_metrics"},
		AsOf:    now.Format(time.RFC3339),
	}, nil
}
