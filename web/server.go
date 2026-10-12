// Package web serves the expense app and its JSON API.
package web

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"

	"github.com/temporal-sa/temporal-demo-expense-approval/expense"
)

//go:embed static
var static embed.FS

// Demo personas. One browser plays every role so a presenter can drive the
// whole story; each browser session still gets its own isolated set of expenses.
const (
	employee           = "Alex Rivera"
	approver           = "Sam Chen"
	escalationApprover = "Dana Ortiz"

	sessionCookie      = "expense_session"
	maxRunningPerUser  = 10
	listPageSize       = 20
	temporalCallBudget = 5 * time.Second
)

var (
	sessionPattern   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	expenseIDPattern = regexp.MustCompile(`^[0-9a-f]{8}$`)
)

// Config is everything the server needs from the environment.
type Config struct {
	Client        client.Client
	TaskQueue     string
	TemporalUIURL string // namespace base URL, e.g. http://localhost:8233/namespaces/default
	EscalateAfter time.Duration
	ExpireAfter   time.Duration
	Logger        *slog.Logger
}

type Server struct {
	cfg Config

	// closed caches finished executions. Their history never changes, so this
	// only saves repeat history reads; Temporal remains the source of truth.
	mu     sync.Mutex
	closed map[string]*Expense
}

func NewServer(cfg Config) *Server {
	return &Server{cfg: cfg, closed: map[string]*Expense{}}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	assets, _ := fs.Sub(static, "static")
	mux.Handle("GET /", http.FileServerFS(assets))
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /api/config", s.config)
	mux.HandleFunc("GET /api/source", s.source)
	mux.HandleFunc("GET /api/expenses", s.listExpenses)
	mux.HandleFunc("POST /api/expenses", s.submitExpense)
	mux.HandleFunc("GET /api/expenses/{id}", s.getExpense)
	mux.HandleFunc("POST /api/expenses/{id}/decision", s.decide)
	mux.HandleFunc("POST /api/session/reset", s.resetSession)
	return mux
}

// health proves the app can reach Temporal without starting or changing work.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if _, err := s.cfg.Client.WorkflowService().GetSystemInfo(ctx, &workflowservice.GetSystemInfoRequest{}); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "temporal": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	s.session(w, r)
	writeJSON(w, http.StatusOK, map[string]any{
		"taskQueue":          s.cfg.TaskQueue,
		"temporalUI":         s.cfg.TemporalUIURL,
		"escalateAfterSec":   s.cfg.EscalateAfter.Seconds(),
		"expireAfterSec":     s.cfg.ExpireAfter.Seconds(),
		"employee":           employee,
		"approver":           approver,
		"escalationApprover": escalationApprover,
	})
}

func (s *Server) source(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(expense.WorkflowSource))
}

type submitRequest struct {
	Description     string `json:"description"`
	Category        string `json:"category"`
	AmountCents     int64  `json:"amountCents"`
	PaymentFailures int    `json:"paymentFailures"`
}

func (s *Server) submitExpense(w http.ResponseWriter, r *http.Request) {
	session := s.session(w, r)
	var in submitRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request")
		return
	}
	in.Description = strings.TrimSpace(in.Description)
	switch {
	case in.Description == "" || len(in.Description) > 80:
		writeError(w, http.StatusBadRequest, "Describe the expense in 80 characters or fewer")
		return
	case in.AmountCents <= 0 || in.AmountCents > 100_000_00:
		writeError(w, http.StatusBadRequest, "Enter an amount between $0.01 and $100,000")
		return
	case in.PaymentFailures < 0 || in.PaymentFailures > 5:
		writeError(w, http.StatusBadRequest, "Payment failures must be between 0 and 5")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), temporalCallBudget)
	defer cancel()
	if n, err := s.countRunning(ctx, session); err == nil && n >= maxRunningPerUser {
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("You already have %d open requests. Decide on some first", n))
		return
	}

	id := randomHex(4)
	req := expense.ExpenseRequest{
		ExpenseID:          id,
		Employee:           employee,
		Description:        in.Description,
		Category:           strings.TrimSpace(in.Category),
		AmountCents:        in.AmountCents,
		Approver:           approver,
		EscalationApprover: escalationApprover,
		EscalateAfter:      s.cfg.EscalateAfter,
		ExpireAfter:        s.cfg.ExpireAfter,
		Demo:               expense.DemoOptions{PaymentFailures: in.PaymentFailures},
	}
	run, err := s.cfg.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        s.workflowID(session, id),
		TaskQueue: s.cfg.TaskQueue,
	}, expense.ExpenseApproval, req)
	if err != nil {
		s.cfg.Logger.Error("start workflow failed", "error", err)
		writeError(w, http.StatusBadGateway, "Couldn't reach Temporal. Try again in a moment")
		return
	}
	s.cfg.Logger.Info("expense submitted", "workflowId", run.GetID(), "amount", expense.FormatCents(req.AmountCents))
	writeJSON(w, http.StatusAccepted, map[string]string{"id": id, "workflowId": run.GetID(), "runId": run.GetRunID()})
}

func (s *Server) listExpenses(w http.ResponseWriter, r *http.Request) {
	session := s.session(w, r)
	ctx, cancel := context.WithTimeout(r.Context(), temporalCallBudget)
	defer cancel()

	// Visibility is eventually consistent: a new expense can take a moment to
	// appear here. The UI shows it immediately from the submit response.
	resp, err := s.cfg.Client.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
		Query:    s.sessionQuery(session),
		PageSize: listPageSize,
	})
	if err != nil {
		s.cfg.Logger.Warn("list workflows failed", "error", err)
		writeError(w, http.StatusBadGateway, "Couldn't reach Temporal")
		return
	}

	prefix := s.workflowID(session, "")
	items := make([]*Expense, 0, len(resp.GetExecutions()))
	for _, ex := range resp.GetExecutions() {
		wfID := ex.GetExecution().GetWorkflowId()
		e, err := s.load(ctx, strings.TrimPrefix(wfID, prefix), wfID, ex.GetExecution().GetRunId())
		if err != nil {
			s.cfg.Logger.Warn("load expense failed", "workflowId", wfID, "error", err)
			continue
		}
		items = append(items, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"expenses": items})
}

func (s *Server) getExpense(w http.ResponseWriter, r *http.Request) {
	session := s.session(w, r)
	id := r.PathValue("id")
	if !expenseIDPattern.MatchString(id) {
		writeError(w, http.StatusNotFound, "Expense not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), temporalCallBudget)
	defer cancel()
	e, err := s.load(ctx, id, s.workflowID(session, id), "")
	if err != nil {
		writeTemporalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"expense": e, "temporalUrl": s.executionURL(e)})
}

type decisionRequest struct {
	Approved bool   `json:"approved"`
	As       string `json:"as"` // approver | escalation
	Comment  string `json:"comment"`
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	session := s.session(w, r)
	id := r.PathValue("id")
	var in decisionRequest
	if !expenseIDPattern.MatchString(id) || json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil {
		writeError(w, http.StatusBadRequest, "Invalid request")
		return
	}
	by := approver
	if in.As == "escalation" {
		by = escalationApprover
	}
	if len(in.Comment) > 140 {
		in.Comment = in.Comment[:140]
	}

	ctx, cancel := context.WithTimeout(r.Context(), temporalCallBudget)
	defer cancel()
	wfID := s.workflowID(session, id)

	// Check first so the approver gets a clear answer. The Workflow enforces the
	// same rules itself and ignores anything that slips past this check.
	e, err := s.load(ctx, id, wfID, "")
	if err != nil {
		writeTemporalError(w, err)
		return
	}
	if !e.AwaitingDecision || !contains(e.CanDecide, by) {
		writeError(w, http.StatusConflict, "This request isn't waiting for "+by)
		return
	}

	// A Signal is accepted by Temporal even when no worker is running. The
	// Workflow acts on it as soon as a worker is available.
	d := expense.Decision{Approved: in.Approved, By: by, Comment: strings.TrimSpace(in.Comment), DecidedAt: time.Now().UTC()}
	if err := s.cfg.Client.SignalWorkflow(ctx, wfID, "", expense.DecisionSignal, d); err != nil {
		writeTemporalError(w, err)
		return
	}
	s.cfg.Logger.Info("decision recorded", "workflowId", wfID, "approved", d.Approved, "by", d.By)
	writeJSON(w, http.StatusAccepted, map[string]any{"recorded": true})
}

// resetSession gives this browser a fresh, empty session. Earlier requests
// keep running to completion or expiry; nobody else's session is affected.
func (s *Server) resetSession(w http.ResponseWriter, r *http.Request) {
	s.setSessionCookie(w, r, randomHex(16))
	writeJSON(w, http.StatusOK, map[string]bool{"reset": true})
}

// load reads one execution's history and pending state and builds its view.
func (s *Server) load(ctx context.Context, id, workflowID, runID string) (*Expense, error) {
	s.mu.Lock()
	if e, ok := s.closed[workflowID]; ok {
		s.mu.Unlock()
		return e, nil
	}
	s.mu.Unlock()

	desc, err := s.cfg.Client.DescribeWorkflowExecution(ctx, workflowID, runID)
	if err != nil {
		return nil, err
	}
	runID = desc.GetWorkflowExecutionInfo().GetExecution().GetRunId()

	var events []*historypb.HistoryEvent
	iter := s.cfg.Client.GetWorkflowHistory(ctx, workflowID, runID, false, enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		ev, err := iter.Next()
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}

	e, err := buildExpense(id, events, desc, time.Now())
	if err != nil {
		return nil, err
	}
	e.WorkflowID, e.RunID = workflowID, runID
	if !e.Running {
		s.mu.Lock()
		if len(s.closed) > 5000 {
			s.closed = map[string]*Expense{}
		}
		s.closed[workflowID] = e
		s.mu.Unlock()
	}
	return e, nil
}

func (s *Server) countRunning(ctx context.Context, session string) (int64, error) {
	resp, err := s.cfg.Client.CountWorkflow(ctx, &workflowservice.CountWorkflowExecutionsRequest{
		Query: s.sessionQuery(session) + " AND ExecutionStatus = 'Running'",
	})
	return resp.GetCount(), err
}

// workflowID scopes every execution to the task queue and the browser session.
// Crashable workspaces share one Temporal namespace, so the task queue keeps
// one workspace's expenses apart from another's.
func (s *Server) workflowID(session, expenseID string) string {
	return fmt.Sprintf("%s-%s-%s", s.cfg.TaskQueue, session, expenseID)
}

func (s *Server) sessionQuery(session string) string {
	return fmt.Sprintf("WorkflowType = 'ExpenseApproval' AND TaskQueue = '%s' AND WorkflowId STARTS_WITH '%s'",
		escapeQuery(s.cfg.TaskQueue), escapeQuery(s.workflowID(session, "")))
}

func (s *Server) executionURL(e *Expense) string {
	return fmt.Sprintf("%s/workflows/%s/%s/history", s.cfg.TemporalUIURL, url.PathEscape(e.WorkflowID), url.PathEscape(e.RunID))
}

// session returns this browser's opaque session ID, issuing one if needed. The
// ID is 128 random bits, so it can't be guessed, and it only ever selects the
// caller's own Workflow IDs.
func (s *Server) session(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil && sessionPattern.MatchString(c.Value) {
		return c.Value
	}
	id := randomHex(16)
	s.setSessionCookie(w, r, id)
	return id
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, id string) {
	// No Domain attribute: the cookie stays on the host that served it, which
	// keeps each crashable workspace hostname separate.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		MaxAge:   7 * 24 * 3600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
	// Requests later in this handler must see the new session.
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: id})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeTemporalError(w http.ResponseWriter, err error) {
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		writeError(w, http.StatusNotFound, "Expense not found")
		return
	}
	writeError(w, http.StatusBadGateway, "Couldn't reach Temporal")
}

func escapeQuery(s string) string { return strings.ReplaceAll(s, "'", "") }

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
