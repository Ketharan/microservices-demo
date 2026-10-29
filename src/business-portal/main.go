// Command business-portal serves a small read-only web UI for an OpenChoreo
// demo. It lists a fixed catalogue of business questions fetched from
// insights-api and renders the answer for whichever one the visitor picks.
//
// There is deliberately no free-text input: see README.md.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// backendTimeout bounds every outbound call to insights-api.
	backendTimeout = 10 * time.Second
	// maxFormBytes bounds the form body of a POST /ask.
	maxFormBytes = 4 << 10
	// maxQuestionIDLen rejects absurd ids before they reach the backend.
	maxQuestionIDLen = 128

	defaultPort = "8080"
)

type app struct {
	backend *client
	// configErr is set when INSIGHTS_API_ADDR was present but unusable.
	configErr error
}

func main() {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = defaultPort
	}

	raw := os.Getenv("INSIGHTS_API_ADDR")
	base, err := resolveBackendAddr(raw)
	if err != nil {
		// Not fatal: the UI must still load and explain itself.
		log.Printf("business-portal: INSIGHTS_API_ADDR is unusable: %v", err)
	}
	switch {
	case err != nil:
		log.Printf("business-portal: starting with no usable backend")
	case base == "":
		log.Printf("business-portal: INSIGHTS_API_ADDR is unset, starting with no backend")
	default:
		log.Printf("business-portal: using insights-api at %s", base)
	}

	a := &app{
		backend: &client{
			baseURL: base,
			http:    &http.Client{Timeout: backendTimeout},
		},
		configErr: err,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", a.handleHealthz)
	mux.HandleFunc("/ask", a.handleAsk)
	mux.HandleFunc("/", a.handleIndex)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("business-portal: listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("business-portal: server stopped: %v", err)
	}
}

// handleHealthz is deliberately independent of the backend: this is the
// liveness/readiness signal for the UI process itself.
func (a *app) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	page := a.newPage()
	if page.Notice == "" {
		ctx, cancel := context.WithTimeout(r.Context(), backendTimeout)
		defer cancel()

		questions, err := a.backend.Questions(ctx)
		if err != nil {
			log.Printf("business-portal: catalogue fetch failed: %v", err)
			page.Error = err.Error()
		} else {
			page.Questions = questions
			if len(questions) == 0 {
				page.Error = "insights-api returned an empty question catalogue."
			}
		}
	}

	// The index is always a 200: a missing or broken backend is reported on the
	// page, not by failing the page.
	a.render(w, http.StatusOK, page)
}

func (a *app) handleAsk(w http.ResponseWriter, r *http.Request) {
	var questionID string
	switch r.Method {
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		if err := r.ParseForm(); err != nil {
			a.renderNotice(w, "That request could not be read. Please pick a question again.")
			return
		}
		questionID = r.PostFormValue("q")
	case http.MethodGet:
		questionID = r.URL.Query().Get("q")
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	questionID = strings.TrimSpace(questionID)

	page := a.newPage()
	if page.Notice != "" {
		a.render(w, http.StatusOK, page)
		return
	}

	if questionID == "" || len(questionID) > maxQuestionIDLen {
		page.Error = "No question was selected. Pick one of the questions below."
		a.loadCatalogue(r.Context(), page)
		a.render(w, http.StatusOK, page)
		return
	}

	// Load the catalogue first: it supplies the label for the answer card, lets
	// the question list stay on the page, and lets an unknown id be rejected
	// before it reaches the backend. v1 only ever runs catalogue questions.
	catalogueErr := a.loadCatalogue(r.Context(), page)

	var selected *Question
	for i := range page.Questions {
		if page.Questions[i].ID == questionID {
			selected = &page.Questions[i]
			break
		}
	}
	if selected == nil && catalogueErr == nil && len(page.Questions) > 0 {
		page.Error = "That question is not in the catalogue. Pick one of the questions below."
		a.render(w, http.StatusOK, page)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), backendTimeout)
	defer cancel()

	resp, err := a.backend.Ask(ctx, questionID)
	if err != nil {
		log.Printf("business-portal: ask %q failed: %v", questionID, err)
		page.Error = err.Error()
		// Still a 200 with a full, readable page: the rest of the UI stays
		// usable and no proxy gets to replace the body with its own error page.
		a.render(w, http.StatusOK, page)
		return
	}

	page.Answer = newAnswerView(questionID, selected, resp)
	a.render(w, http.StatusOK, page)
}

// loadCatalogue fills page.Questions, recording a failure on the page as well
// as returning it.
func (a *app) loadCatalogue(parent context.Context, page *pageData) error {
	ctx, cancel := context.WithTimeout(parent, backendTimeout)
	defer cancel()

	questions, err := a.backend.Questions(ctx)
	if err != nil {
		log.Printf("business-portal: catalogue fetch failed: %v", err)
		if page.Error == "" {
			page.Error = err.Error()
		}
		return err
	}
	page.Questions = questions
	return nil
}

// newPage seeds a page with the backend-configuration notice, if any.
func (a *app) newPage() *pageData {
	page := &pageData{}
	switch {
	case a.configErr != nil:
		page.Notice = a.configErr.Error() + " Set INSIGHTS_API_ADDR to the insights-api endpoint, for example insights-api:8080."
	case !a.backend.configured():
		page.Notice = "INSIGHTS_API_ADDR is not set, so there is no insights-api to query. " +
			"The platform normally injects it from the component's dependencies.endpoints binding."
	}
	return page
}

func (a *app) renderNotice(w http.ResponseWriter, msg string) {
	page := a.newPage()
	page.Error = msg
	a.render(w, http.StatusOK, page)
}
