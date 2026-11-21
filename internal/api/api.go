package api

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agentd/internal/app"
	"agentd/internal/assignments"
	"agentd/internal/gateway"
	"agentd/internal/notify"
	"agentd/internal/outcomes"
	"agentd/internal/overview"
	"agentd/internal/policy"
	"agentd/internal/runtime"
	"agentd/internal/scheduler"
	"agentd/internal/system"
	"agentd/internal/tasks"
)

const maxBody = 1 << 20

type Server struct {
	App        *app.App
	Token      string
	Log        *slog.Logger
	StreamPoll time.Duration
}

type handler func(w http.ResponseWriter, r *http.Request) error

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	routes := map[string]handler{
		"GET /api/assignments":                                    s.listAssignments,
		"POST /api/assignments":                                   s.createAssignment,
		"GET /api/assignments/{id}":                               s.getAssignment,
		"POST /api/assignments/{id}/{action}":                     s.assignmentAction,
		"POST /api/assignments/{id}/criteria/{criterion}/confirm": s.confirmCriterion,
		"PUT /api/assignments/{id}/policy":                        s.setPolicy,
		"GET /api/tasks":                                          s.listTasks,
		"GET /api/tasks/{id}":                                     s.getTask,
		"POST /api/tasks/{id}/retry":                              s.retryTask,
		"GET /api/actions/unresolved":                             s.listUnresolved,
		"POST /api/actions/{id}/resolve":                          s.resolveAction,
		"GET /api/approvals":                                      s.listApprovals,
		"POST /api/approvals/{id}/{decision}":                     s.decide,
		"GET /api/notifications":                                  s.listNotifications,
		"POST /api/notifications/{id}/delivered":                  s.markDelivered,
		"POST /api/events":                                        s.ingestEvent,
		"GET /api/events/stream":                                  s.stream,
		"GET /api/activity":                                       s.activity,
		"GET /api/usage":                                          s.usage,
		"GET /api/system":                                         s.systemStatus,
		"POST /api/system/emergency-stop":                         s.emergencyStop,
		"POST /api/system/resume":                                 s.systemResume,
	}
	for pattern, h := range routes {
		mux.HandleFunc(pattern, s.wrap(h))
	}
	return s.authenticate(mux)
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.Token == "" || !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="agentd"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusError struct {
	code int
	msg  string
}

func (e statusError) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return statusError{http.StatusBadRequest, fmt.Sprintf(format, args...)}
}

func (s *Server) wrap(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		err := h(w, r)
		if err == nil {
			return
		}
		code, msg := classify(err)
		if code >= 500 {
			s.Log.Error("api request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		}
		writeJSON(w, code, map[string]string{"error": msg})
	}
}

func classify(err error) (int, string) {
	var se statusError
	switch {
	case errors.As(err, &se):
		return se.code, se.msg
	case errors.Is(err, assignments.ErrInvalid):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, assignments.ErrNotFound), errors.Is(err, tasks.ErrNotFound), errors.Is(err, sql.ErrNoRows),
		errors.Is(err, outcomes.ErrUnknownCriterion):
		return http.StatusNotFound, "not found"
	case errors.Is(err, assignments.ErrWrongKind), errors.Is(err, assignments.ErrTerminated),
		errors.Is(err, assignments.ErrNotPaused), errors.Is(err, gateway.ErrApprovalNotPending),
		errors.Is(err, gateway.ErrApprovalExpired), errors.Is(err, notify.ErrNotPending),
		errors.Is(err, runtime.ErrNotBlocked), errors.Is(err, runtime.ErrUnresolvedWork), errors.Is(err, gateway.ErrNotUnresolved):
		return http.StatusConflict, err.Error()
	}
	return http.StatusInternalServerError, "internal error"
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid JSON body: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return badRequest("invalid JSON body: trailing data")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func ok(w http.ResponseWriter, v any) error {
	writeJSON(w, http.StatusOK, v)
	return nil
}

type assignmentSummary struct {
	ID           string           `json:"id"`
	Kind         assignments.Kind `json:"kind"`
	Name         string           `json:"name"`
	Status       string           `json:"status"`
	StatusReason string           `json:"status_reason,omitempty"`
}

func (s *Server) listAssignments(w http.ResponseWriter, r *http.Request) error {
	rows, err := s.App.DB.QueryContext(r.Context(), `SELECT id, kind, name, status, COALESCE(status_reason, '')
		FROM assignments ORDER BY created_at, id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []assignmentSummary{}
	for rows.Next() {
		var a assignmentSummary
		if err := rows.Scan(&a.ID, &a.Kind, &a.Name, &a.Status, &a.StatusReason); err != nil {
			return err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return ok(w, out)
}

func (s *Server) createAssignment(w http.ResponseWriter, r *http.Request) error {
	var spec app.Spec
	if err := decode(r, &spec); err != nil {
		return err
	}
	id, err := s.App.Create(r.Context(), spec)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
	return nil
}

type assignmentDetail struct {
	assignmentSummary
	Objective     string                          `json:"objective"`
	Policy        policy.Policy                   `json:"policy"`
	PolicyVersion int64                           `json:"policy_version"`
	Completion    *assignments.Completion         `json:"completion,omitempty"`
	Notifications *assignments.NotificationPolicy `json:"notifications,omitempty"`
	State         json.RawMessage                 `json:"state"`
	SpentUSD      float64                         `json:"spent_usd"`
	ReservedUSD   float64                         `json:"reserved_usd,omitempty"`
	BudgetUSD     float64                         `json:"budget_usd,omitempty"`
	BudgetPeriod  string                          `json:"budget_period,omitempty"`
	Next          expected                        `json:"next"`
	OpenTasks     []openTask                      `json:"open_tasks"`
	Recent        []activityRow                   `json:"recent"`
	Approvals     int                             `json:"pending_approvals"`
	Undelivered   int                             `json:"undelivered_notifications"`
	Criteria      []criterionRow                  `json:"criteria,omitempty"`
	Obligations   []obligationRow                 `json:"obligations,omitempty"`
}

type expected struct {
	At   *time.Time `json:"at,omitempty"`
	What string     `json:"what"`
}

type openTask struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Status    string     `json:"status"`
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (s *Server) getAssignment(w http.ResponseWriter, r *http.Request) error {
	o, err := overview.Get(r.Context(), s.App.Runtime, r.PathValue("id"))
	if err != nil {
		return err
	}
	a := o.Assignment
	d := assignmentDetail{
		assignmentSummary: assignmentSummary{ID: a.ID, Kind: a.Kind, Name: a.Name, Status: a.Status, StatusReason: a.StatusReason},
		Objective:         a.Objective, Policy: a.Policy, PolicyVersion: a.PolicyVersion,
		Completion: a.Completion, Notifications: a.Notifications, State: a.State,
		SpentUSD: float64(o.SpentMicros) / 1e6, ReservedUSD: float64(o.ReservedMicros) / 1e6, BudgetUSD: float64(o.LimitMicros) / 1e6,
		Next:      expected{At: timePtr(o.Next.At), What: o.Next.What},
		OpenTasks: []openTask{}, Recent: []activityRow{},
		Approvals: o.PendingAsk, Undelivered: o.Undelivered,
	}
	if p := a.Policy.Limits.BudgetPeriod.Duration; p > 0 {
		d.BudgetPeriod = p.String()
	}
	for _, c := range o.Criteria {
		row := criterionRow{ID: c.Criterion.ID, Description: c.Criterion.Description, Verified: c.Verified,
			Evidence: c.Kind, EventID: c.EventID, By: c.Actor, Note: c.Note}
		if c.Verified {
			row.VerifiedAt = &c.ObservedAt
		}
		d.Criteria = append(d.Criteria, row)
	}
	for _, ob := range o.Obligations {
		row := obligationRow{ID: ob.ID, Description: ob.Description, Every: ob.Every.String(), Evidence: ob.Evidence.String(),
			Overdue: ob.Overdue, MissedIntervals: ob.Missed, DueAt: ob.DueAt}
		if !ob.LastEvidence.IsZero() {
			row.LastEvidenceAt = &ob.LastEvidence
		}
		d.Obligations = append(d.Obligations, row)
	}
	for _, t := range o.OpenTasks {
		d.OpenTasks = append(d.OpenTasks, openTask{ID: t.ID, Title: t.Title, Status: t.Status, NextRunAt: timePtr(t.NextRunAt)})
	}
	for _, act := range o.Recent {
		d.Recent = append(d.Recent, activityRow{At: act.At, Actor: act.Actor, Operation: act.Operation, ObjectID: act.ObjectID})
	}
	return ok(w, d)
}

func (s *Server) assignmentAction(w http.ResponseWriter, r *http.Request) error {
	ctx, id, store := r.Context(), r.PathValue("id"), s.App.Assignments
	var err error
	switch r.PathValue("action") {
	case "pause":
		err = store.Pause(ctx, id, assignments.PausedByUser, "", "api")
	case "resume":
		err = store.Resume(ctx, id, "api")
	case "complete":
		err = store.Complete(ctx, id, "api")
	case "cancel":
		err = store.Cancel(ctx, id, "api")
	case "revoke":
		err = store.Revoke(ctx, id, "api")
	default:
		return statusError{http.StatusNotFound, "unknown action"}
	}
	if err != nil {
		return err
	}
	a, err := store.Get(ctx, id)
	if err != nil {
		return err
	}
	return ok(w, assignmentSummary{ID: a.ID, Kind: a.Kind, Name: a.Name, Status: a.Status, StatusReason: a.StatusReason})
}

func (s *Server) setPolicy(w http.ResponseWriter, r *http.Request) error {
	var p policy.Policy
	if err := decode(r, &p); err != nil {
		return err
	}
	version, err := s.App.Assignments.SetPolicy(r.Context(), r.PathValue("id"), p, "api")
	if err != nil {
		return err
	}
	return ok(w, map[string]int64{"policy_version": version})
}

type taskRow struct {
	ID           string          `json:"id"`
	AssignmentID string          `json:"assignment_id"`
	ParentTaskID string          `json:"parent_task_id,omitempty"`
	Title        string          `json:"title"`
	Status       string          `json:"status"`
	Attempts     int             `json:"attempts"`
	NextRunAt    *time.Time      `json:"next_run_at,omitempty"`
	Result       json.RawMessage `json:"result,omitempty"`
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) error {
	q := `SELECT id, assignment_id, COALESCE(parent_task_id, ''), title, status, attempt_count, COALESCE(next_run_at, 0), result_json
		FROM tasks`
	var args []any
	var where []string
	if v := r.URL.Query().Get("assignment"); v != "" {
		where, args = append(where, "assignment_id = ?"), append(args, v)
	}
	if v := r.URL.Query().Get("status"); v != "" {
		where, args = append(where, "status = ?"), append(args, v)
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	limit, err := limitParam(r, 100, 500)
	if err != nil {
		return err
	}
	q += " ORDER BY created_at DESC, id LIMIT ?"
	args = append(args, limit)
	rows, err := s.App.DB.QueryContext(r.Context(), q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []taskRow{}
	for rows.Next() {
		var t taskRow
		var next int64
		var result sql.NullString
		if err := rows.Scan(&t.ID, &t.AssignmentID, &t.ParentTaskID, &t.Title, &t.Status, &t.Attempts, &next, &result); err != nil {
			return err
		}
		if next > 0 {
			t.NextRunAt = timePtr(time.UnixMilli(next))
		}
		if result.Valid {
			t.Result = json.RawMessage(result.String)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return ok(w, out)
}

type transcriptMessage struct {
	Role       string          `json:"role"`
	Content    string          `json:"content,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
}

type taskDetail struct {
	taskRow
	Objective  string              `json:"objective"`
	Skills     []string            `json:"skills,omitempty"`
	Transcript []transcriptMessage `json:"transcript"`
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	t, err := s.App.Tasks.Get(ctx, r.PathValue("id"))
	if err != nil {
		return err
	}
	d := taskDetail{
		taskRow: taskRow{ID: t.ID, AssignmentID: t.AssignmentID, ParentTaskID: t.ParentTaskID, Title: t.Title,
			Status: t.Status, Attempts: t.AttemptCount, Result: t.Result},
		Objective: t.Objective, Skills: t.Skills, Transcript: []transcriptMessage{},
	}
	rows, err := s.App.DB.QueryContext(ctx, `SELECT m.message_json FROM run_messages m
		WHERE m.run_id = (SELECT id FROM runs WHERE task_id = ? ORDER BY started_at DESC LIMIT 1) ORDER BY m.seq`, t.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return err
		}
		var m transcriptMessage
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			return err
		}
		d.Transcript = append(d.Transcript, m)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return ok(w, d)
}

func (s *Server) retryTask(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Note string `json:"note"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &body); err != nil {
			return err
		}
	}
	if err := s.App.Runtime.Retry(r.Context(), r.PathValue("id"), "api", body.Note); err != nil {
		return err
	}
	return ok(w, map[string]string{"status": "ready"})
}

type unresolvedRow struct {
	ID           string          `json:"id"`
	TaskID       string          `json:"task_id"`
	AssignmentID string          `json:"assignment_id"`
	Tool         string          `json:"tool"`
	Resource     string          `json:"resource,omitempty"`
	Arguments    json.RawMessage `json:"arguments"`
	Detail       json.RawMessage `json:"detail,omitempty"`
}

func (s *Server) listUnresolved(w http.ResponseWriter, r *http.Request) error {
	actions, err := s.App.Gateway.UnresolvedActions(r.Context(), r.URL.Query().Get("assignment"))
	if err != nil {
		return err
	}
	out := []unresolvedRow{}
	for _, u := range actions {
		row := unresolvedRow{ID: u.ID, TaskID: u.TaskID, AssignmentID: u.AssignmentID, Tool: u.ToolName,
			Resource: u.Resource, Arguments: json.RawMessage(u.Arguments)}
		if u.Detail != "" {
			row.Detail = json.RawMessage(u.Detail)
		}
		out = append(out, row)
	}
	return ok(w, out)
}

func (s *Server) resolveAction(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Happened *bool  `json:"happened"`
		Note     string `json:"note"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	if body.Happened == nil {
		return badRequest("happened (true or false) is required")
	}
	if err := s.App.Gateway.Resolve(r.Context(), r.PathValue("id"), *body.Happened, "api", body.Note); err != nil {
		return err
	}
	return ok(w, map[string]bool{"happened": *body.Happened})
}

type approvalRow struct {
	ID           string          `json:"id"`
	ActionID     string          `json:"action_id"`
	AssignmentID string          `json:"assignment_id"`
	Tool         string          `json:"tool"`
	Arguments    json.RawMessage `json:"arguments"`
	ExpiresAt    time.Time       `json:"expires_at"`
}

func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) error {
	pending, err := s.App.Gateway.PendingApprovals(r.Context())
	if err != nil {
		return err
	}
	out := []approvalRow{}
	for _, p := range pending {
		out = append(out, approvalRow{ID: p.ID, ActionID: p.ActionID, AssignmentID: p.AssignmentID, Tool: p.ToolName,
			Arguments: json.RawMessage(p.Arguments), ExpiresAt: p.ExpiresAt})
	}
	return ok(w, out)
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) error {
	var approve bool
	switch r.PathValue("decision") {
	case "approve":
		approve = true
	case "reject":
	default:
		return statusError{http.StatusNotFound, "unknown decision"}
	}
	if err := s.App.Gateway.Decide(r.Context(), r.PathValue("id"), approve, "api"); err != nil {
		return err
	}
	return ok(w, map[string]string{"status": r.PathValue("decision") + "d"})
}

type notificationRow struct {
	ID           string    `json:"id"`
	AssignmentID string    `json:"assignment_id"`
	Severity     string    `json:"severity"`
	Title        string    `json:"title"`
	Body         string    `json:"body"`
	CreatedAt    time.Time `json:"created_at"`
	Attempts     int       `json:"delivery_attempts"`
	LastError    string    `json:"last_error,omitempty"`
	Failed       bool      `json:"delivery_failed"`
}

func (s *Server) listNotifications(w http.ResponseWriter, r *http.Request) error {
	rows, err := s.App.DB.QueryContext(r.Context(), `SELECT id, assignment_id, severity, title, body, created_at,
		attempts, COALESCE(last_error, ''), failed_at IS NOT NULL
		FROM notifications WHERE status = 'pending' ORDER BY created_at, id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []notificationRow{}
	for rows.Next() {
		var n notificationRow
		var at int64
		if err := rows.Scan(&n.ID, &n.AssignmentID, &n.Severity, &n.Title, &n.Body, &at, &n.Attempts, &n.LastError, &n.Failed); err != nil {
			return err
		}
		n.CreatedAt = time.UnixMilli(at)
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return ok(w, out)
}

type criterionRow struct {
	ID          string     `json:"id"`
	Description string     `json:"description"`
	Verified    bool       `json:"verified"`
	Evidence    string     `json:"evidence,omitempty"`
	EventID     string     `json:"event_id,omitempty"`
	By          string     `json:"by,omitempty"`
	Note        string     `json:"note,omitempty"`
	VerifiedAt  *time.Time `json:"verified_at,omitempty"`
}

type obligationRow struct {
	ID              string     `json:"id"`
	Description     string     `json:"description"`
	Every           string     `json:"every"`
	Evidence        string     `json:"evidence"`
	Overdue         bool       `json:"overdue"`
	MissedIntervals int        `json:"missed_intervals,omitempty"`
	DueAt           time.Time  `json:"due_at"`
	LastEvidenceAt  *time.Time `json:"last_evidence_at,omitempty"`
}

type confirmBody struct {
	Note string `json:"note"`
}

func (s *Server) confirmCriterion(w http.ResponseWriter, r *http.Request) error {
	var b confirmBody
	if r.ContentLength != 0 {
		if err := decode(r, &b); err != nil {
			return err
		}
	}
	if err := s.App.Outcomes.Confirm(r.Context(), r.PathValue("id"), r.PathValue("criterion"), "api", b.Note); err != nil {
		return err
	}
	return ok(w, map[string]string{"status": "verified"})
}

func (s *Server) markDelivered(w http.ResponseWriter, r *http.Request) error {
	if err := s.App.Outbox.MarkDelivered(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	return ok(w, map[string]string{"status": "delivered"})
}

type eventBody struct {
	Source     string          `json:"source"`
	Kind       string          `json:"kind"`
	ExternalID string          `json:"external_id"`
	Payload    json.RawMessage `json:"payload"`
}

func (s *Server) ingestEvent(w http.ResponseWriter, r *http.Request) error {
	var e eventBody
	if err := decode(r, &e); err != nil {
		return err
	}
	if e.Source == "" || e.Kind == "" {
		return badRequest("source and kind are required")
	}
	if len(e.Payload) > 0 && e.Payload[0] != '{' {
		return badRequest("payload must be a JSON object")
	}
	id, dup, err := s.App.Scheduler.Ingest(r.Context(), scheduler.Event{Source: e.Source, Kind: e.Kind, ExternalID: e.ExternalID, Payload: e.Payload})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "duplicate": dup})
	return nil
}

type activityRow struct {
	ID           int64           `json:"id,omitempty"`
	At           time.Time       `json:"at"`
	AssignmentID string          `json:"assignment_id,omitempty"`
	Actor        string          `json:"actor"`
	Operation    string          `json:"operation"`
	ObjectID     string          `json:"object_id,omitempty"`
	Payload      json.RawMessage `json:"payload,omitempty"`
}

func (s *Server) activitySince(r *http.Request, after int64, limit int) ([]activityRow, error) {
	q := `SELECT id, created_at, COALESCE(assignment_id, ''), actor, operation, COALESCE(object_id, ''), payload_json
		FROM audit_log WHERE id > ?`
	args := []any{after}
	if v := r.URL.Query().Get("assignment"); v != "" {
		q, args = q+" AND assignment_id = ?", append(args, v)
	}
	q, args = q+" ORDER BY id LIMIT ?", append(args, limit)
	rows, err := s.App.DB.QueryContext(r.Context(), q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []activityRow{}
	for rows.Next() {
		var a activityRow
		var at int64
		var payload string
		if err := rows.Scan(&a.ID, &at, &a.AssignmentID, &a.Actor, &a.Operation, &a.ObjectID, &payload); err != nil {
			return nil, err
		}
		a.At, a.Payload = time.UnixMilli(at), json.RawMessage(payload)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Server) activity(w http.ResponseWriter, r *http.Request) error {
	after, err := int64Param(r, "after", 0)
	if err != nil {
		return err
	}
	limit, err := limitParam(r, 100, 1000)
	if err != nil {
		return err
	}
	rows, err := s.activitySince(r, after, limit)
	if err != nil {
		return err
	}
	return ok(w, rows)
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) error {
	flusher, okFlush := w.(http.Flusher)
	if !okFlush {
		return statusError{http.StatusInternalServerError, "streaming unsupported"}
	}
	after, err := int64Param(r, "after", -1)
	if err != nil {
		return err
	}
	if id := r.Header.Get("Last-Event-ID"); id != "" {
		if after, err = strconv.ParseInt(id, 10, 64); err != nil {
			return badRequest("invalid Last-Event-ID")
		}
	}
	if after < 0 {
		if err := s.App.DB.QueryRowContext(r.Context(), `SELECT COALESCE(MAX(id), 0) FROM audit_log`).Scan(&after); err != nil {
			return err
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	poll := s.StreamPoll
	if poll == 0 {
		poll = time.Second
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	idle := 0
	for {
		rows, err := s.activitySince(r, after, 200)
		if err != nil {
			if r.Context().Err() != nil {
				return nil
			}
			s.Log.Error("activity stream", "err", err)
			return nil
		}
		for _, row := range rows {
			body, _ := json.Marshal(row)
			fmt.Fprintf(w, "id: %d\nevent: activity\ndata: %s\n\n", row.ID, body)
			after = row.ID
		}
		if len(rows) > 0 {
			idle = 0
			flusher.Flush()
		} else if idle++; idle*int(poll/time.Millisecond) >= 15000 {
			idle = 0
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			return nil
		case <-ticker.C:
		}
	}
}

type usageRow struct {
	AssignmentID string  `json:"assignment_id"`
	Name         string  `json:"name"`
	SpentUSD     float64 `json:"spent_usd"`
	ReservedUSD  float64 `json:"reserved_usd,omitempty"`
	BudgetUSD    float64 `json:"budget_usd,omitempty"`
	BudgetPeriod string  `json:"budget_period,omitempty"`
	LifetimeUSD  float64 `json:"lifetime_usd"`
	ModelCalls   int     `json:"model_calls"`
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	rows, err := s.App.DB.QueryContext(ctx, `SELECT a.id, COALESCE(SUM(u.cost_micros), 0), COUNT(u.id)
		FROM assignments a LEFT JOIN model_usage u ON u.assignment_id = a.id GROUP BY a.id ORDER BY a.created_at, a.id`)
	if err != nil {
		return err
	}
	type total struct {
		id    string
		micro int64
		calls int
	}
	var totals []total
	for rows.Next() {
		var t total
		if err := rows.Scan(&t.id, &t.micro, &t.calls); err != nil {
			rows.Close()
			return err
		}
		totals = append(totals, t)
	}
	rows.Close()
	out := []usageRow{}
	for _, t := range totals {
		a, err := s.App.Assignments.Get(ctx, t.id)
		if err != nil {
			return err
		}
		b, err := s.App.Runtime.Budget(ctx, a)
		if err != nil {
			return err
		}
		u := usageRow{AssignmentID: a.ID, Name: a.Name, SpentUSD: float64(b.SpentMicros) / 1e6, ReservedUSD: float64(b.ReservedMicros) / 1e6,
			BudgetUSD:   float64(b.LimitMicros) / 1e6,
			LifetimeUSD: float64(t.micro) / 1e6, ModelCalls: t.calls}
		if p := a.Policy.Limits.BudgetPeriod.Duration; p > 0 {
			u.BudgetPeriod = p.String()
		}
		out = append(out, u)
	}
	return ok(w, out)
}

func (s *Server) systemStatus(w http.ResponseWriter, r *http.Request) error {
	st, err := system.Get(r.Context(), s.App.DB)
	if err != nil {
		return err
	}
	return ok(w, st)
}

func (s *Server) emergencyStop(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &body); err != nil {
			return err
		}
	}
	if err := system.Halt(r.Context(), s.App.DB, s.App.Runtime.Now(), "api", body.Reason); err != nil {
		return err
	}
	return s.systemStatus(w, r)
}

func (s *Server) systemResume(w http.ResponseWriter, r *http.Request) error {
	if err := system.Resume(r.Context(), s.App.DB, s.App.Runtime.Now(), "api"); err != nil {
		return err
	}
	return s.systemStatus(w, r)
}

func limitParam(r *http.Request, def, max int) (int, error) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > max {
		return 0, badRequest("limit must be between 1 and %d", max)
	}
	return n, nil
}

func int64Param(r *http.Request, name string, def int64) (int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, badRequest("%s must be a non-negative integer", name)
	}
	return n, nil
}
