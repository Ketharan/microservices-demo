package main

import (
	"bytes"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"
)

// pageData is the whole model for the single page this service renders.
type pageData struct {
	// Notice is a configuration problem: the backend address is missing or bad.
	Notice string
	// Error is a runtime problem: the backend was unreachable or said no.
	Error string
	// Questions is the catalogue, when it could be fetched.
	Questions []Question
	// Answer is set only on a successful /ask.
	Answer *answerView
}

// answerView is an askResponse flattened for rendering. Cells are already
// strings so the template does no formatting of its own.
type answerView struct {
	QuestionID  string
	Label       string
	Description string
	Prose       string
	Columns     []string
	Rows        [][]string
	HasRows     bool
	Sources     []string
	AsOf        string
}

func newAnswerView(questionID string, q *Question, resp *askResponse) *answerView {
	view := &answerView{
		QuestionID: questionID,
		Label:      questionID,
		Prose:      strings.TrimSpace(resp.Answer),
		Sources:    cleanStrings(resp.Sources),
		AsOf:       formatAsOf(resp.AsOf),
	}
	if q != nil {
		if strings.TrimSpace(q.Label) != "" {
			view.Label = q.Label
		}
		view.Description = q.Description
	}
	if view.Prose == "" {
		view.Prose = "insights-api returned no prose for this question."
	}

	if table := parseTable(resp.Data); table != nil {
		view.Columns = table.Columns
		for _, row := range table.Rows {
			cells := make([]string, 0, len(row))
			for _, cell := range row {
				cells = append(cells, formatCell(cell))
			}
			view.Rows = append(view.Rows, cells)
		}
		// Columns alone, or rows alone, are both survivable: the table renders
		// what it has and the header falls back to positional names.
		if len(view.Rows) > 0 && len(view.Columns) == 0 {
			widest := 0
			for _, row := range view.Rows {
				if len(row) > widest {
					widest = len(row)
				}
			}
			for i := 0; i < widest; i++ {
				view.Columns = append(view.Columns, "")
			}
		}
		view.HasRows = len(view.Rows) > 0
	}
	return view
}

func cleanStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// formatAsOf prefers a readable UTC stamp but never discards a value it cannot
// parse.
func formatAsOf(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format("2006-01-02 15:04:05 UTC")
	}
	return s
}

// render writes the page. Every value reaches the browser through
// html/template, so backend-supplied text cannot inject markup.
func (a *app) render(w http.ResponseWriter, status int, page *pageData) {
	var buf bytes.Buffer
	if err := pageTemplate.Execute(&buf, page); err != nil {
		// Render into a buffer first so a template failure cannot leave a
		// half-written page behind a 200.
		log.Printf("business-portal: template execution failed: %v", err)
		http.Error(w, "the page could not be rendered", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if _, err := buf.WriteTo(w); err != nil {
		log.Printf("business-portal: writing response failed: %v", err)
	}
}

var pageTemplate = template.Must(template.New("page").Parse(pageHTML))

const pageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Business Portal — estate insights</title>
<style>
:root {
  --bg: #f5f6f8;
  --card: #ffffff;
  --ink: #16191d;
  --muted: #5c6670;
  --line: #dde1e6;
  --accent: #1f5f8b;
  --accent-ink: #ffffff;
  --warn-bg: #fff6e0;
  --warn-line: #e0b457;
  --err-bg: #fdecec;
  --err-line: #d9807c;
  --tool-bg: #eaf2f8;
}
* { box-sizing: border-box; }
body {
  margin: 0;
  padding: 0 1rem 4rem;
  background: var(--bg);
  color: var(--ink);
  font: 16px/1.55 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
}
.wrap { max-width: 56rem; margin: 0 auto; }
header { padding: 2.25rem 0 1.25rem; border-bottom: 1px solid var(--line); margin-bottom: 1.75rem; }
h1 { margin: 0 0 .35rem; font-size: 1.65rem; letter-spacing: -0.01em; }
header p { margin: 0; color: var(--muted); font-size: .95rem; }
h2 { font-size: 1.2rem; margin: 0 0 .5rem; }
h3 { font-size: .85rem; text-transform: uppercase; letter-spacing: .07em; color: var(--muted); margin: 1.5rem 0 .6rem; }
a { color: var(--accent); }
.banner { border-radius: 6px; padding: .85rem 1rem; margin-bottom: 1.5rem; font-size: .95rem; }
.banner strong { display: block; margin-bottom: .2rem; }
.banner-warn { background: var(--warn-bg); border: 1px solid var(--warn-line); }
.banner-error { background: var(--err-bg); border: 1px solid var(--err-line); }
.banner code { word-break: break-all; }
.card {
  background: var(--card);
  border: 1px solid var(--line);
  border-radius: 8px;
  padding: 1.25rem 1.35rem;
  margin-bottom: 1.5rem;
}
.answer-prose {
  font-size: 1.1rem;
  line-height: 1.5;
  margin: .25rem 0 0;
  white-space: pre-wrap;
}
.answer-desc { color: var(--muted); font-size: .9rem; margin: 0 0 .9rem; }
.sources {
  margin-top: 1.5rem;
  background: var(--tool-bg);
  border: 1px solid #c3d8e6;
  border-left: 4px solid var(--accent);
  border-radius: 6px;
  padding: .9rem 1rem 1rem;
}
.sources h3 { margin: 0 0 .5rem; color: var(--accent); }
.tool-list { list-style: none; display: flex; flex-wrap: wrap; gap: .45rem; margin: 0; padding: 0; }
.tool-list li {
  background: var(--card);
  border: 1px solid #b9cfdf;
  border-radius: 999px;
  padding: .2rem .7rem;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: .85rem;
}
.table-scroll { overflow-x: auto; }
table { border-collapse: collapse; width: 100%; font-size: .92rem; }
th, td { text-align: left; padding: .5rem .7rem; border-bottom: 1px solid var(--line); vertical-align: top; }
th { background: #f0f2f5; font-weight: 600; white-space: nowrap; }
tbody tr:last-child td { border-bottom: none; }
.muted { color: var(--muted); font-size: .92rem; }
.asof { margin: 1.25rem 0 0; color: var(--muted); font-size: .82rem; }
.grid { display: grid; gap: 1rem; grid-template-columns: repeat(auto-fit, minmax(16rem, 1fr)); }
.q-card {
  background: var(--card);
  border: 1px solid var(--line);
  border-radius: 8px;
  padding: 1rem 1.1rem 1.1rem;
  display: flex;
  flex-direction: column;
}
.q-card h4 { margin: 0 0 .4rem; font-size: 1rem; line-height: 1.35; }
.q-card p { margin: 0 0 1rem; color: var(--muted); font-size: .9rem; flex: 1 1 auto; }
.q-card form { margin: 0; }
button {
  font: inherit;
  font-size: .92rem;
  background: var(--accent);
  color: var(--accent-ink);
  border: 1px solid var(--accent);
  border-radius: 5px;
  padding: .45rem 1rem;
  cursor: pointer;
}
button:hover { background: #17486a; }
button:focus-visible { outline: 3px solid #9ec6de; outline-offset: 2px; }
.backlink { display: inline-block; margin-bottom: 1.25rem; font-size: .92rem; }
footer { margin-top: 2.5rem; padding-top: 1.25rem; border-top: 1px solid var(--line); color: var(--muted); font-size: .85rem; }
</style>
</head>
<body>
<div class="wrap">
<header>
  <h1>Business Portal</h1>
  <p>Read-only estate insights. Every answer is composed by <code>insights-api</code> from federated MCP tools, and names the tools it used.</p>
</header>

{{if .Notice}}
<div class="banner banner-warn">
  <strong>The backend is not configured.</strong>
  {{.Notice}}
</div>
{{end}}

{{if .Error}}
<div class="banner banner-error">
  <strong>Could not get an answer from insights-api.</strong>
  {{.Error}}
</div>
{{end}}

{{with .Answer}}
<a class="backlink" href="/">&larr; All questions</a>
<section class="card">
  <h2>{{.Label}}</h2>
  {{if .Description}}<p class="answer-desc">{{.Description}}</p>{{end}}
  <p class="answer-prose">{{.Prose}}</p>

  <div class="sources">
    <h3>Answered using</h3>
    {{if .Sources}}
    <ul class="tool-list">
      {{range .Sources}}<li>{{.}}</li>{{end}}
    </ul>
    {{else}}
    <p class="muted">insights-api did not report which tools it used for this answer.</p>
    {{end}}
  </div>

  <h3>Supporting data</h3>
  {{if .HasRows}}
  <div class="table-scroll">
    <table>
      <thead>
        <tr>{{range $i, $c := .Columns}}<th scope="col">{{if $c}}{{$c}}{{else}}Column {{$i}}{{end}}</th>{{end}}</tr>
      </thead>
      <tbody>
        {{range .Rows}}<tr>{{range .}}<td>{{.}}</td>{{end}}</tr>{{end}}
      </tbody>
    </table>
  </div>
  {{else}}
  <p class="muted">no rows returned</p>
  {{end}}

  {{if .AsOf}}<p class="asof">Data as of {{.AsOf}}</p>{{end}}
</section>
{{end}}

{{if .Questions}}
<h3>{{if .Answer}}Ask another question{{else}}Pick a question{{end}}</h3>
<div class="grid">
  {{range .Questions}}
  <div class="q-card">
    <h4>{{.Label}}</h4>
    <p>{{.Description}}</p>
    <form method="post" action="/ask">
      <input type="hidden" name="q" value="{{.ID}}">
      <button type="submit">Show me</button>
    </form>
  </div>
  {{end}}
</div>
{{else if not .Notice}}
<p class="muted">No questions are available right now.</p>
{{end}}

<footer>
  This page runs only the pre-defined questions above. There is no free-text input in v1 &mdash;
  it is served publicly to anonymous visitors, so only known queries are offered.
</footer>
</div>
</body>
</html>
`
