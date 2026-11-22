package overview

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"agentd/internal/assignments"
	"agentd/internal/outcomes"
	"agentd/internal/runtime"
	"agentd/internal/system"
	"agentd/internal/watchdog"
)

type Overview struct {
	Assignment     assignments.Assignment
	SpentMicros    int64
	ReservedMicros int64
	LimitMicros    int64
	Criteria       []outcomes.Progress
	Obligations    []watchdog.Health
	OpenTasks      []Task
	Recent         []Activity
	Next           Expected
	Undelivered    int
	Blocked        int
	PendingAsk     int
}

type Task struct {
	ID        string
	Title     string
	Status    string
	NextRunAt time.Time
	Waiting   string
}

type Activity struct {
	At        time.Time
	Actor     string
	Operation string
	ObjectID  string
}

type Expected struct {
	At   time.Time
	What string
}

func (e Expected) String() string {
	if e.At.IsZero() {
		return e.What
	}
	return fmt.Sprintf("%s (%s)", e.What, e.At.Local().Format("2006-01-02 15:04"))
}

const recentLimit = 10

func Get(ctx context.Context, rt *runtime.Runtime, id string) (Overview, error) {
	var o Overview
	a, err := rt.Assignments.Get(ctx, id)
	if err != nil {
		return o, err
	}
	o.Assignment = a
	b, err := rt.Budget(ctx, a)
	if err != nil {
		return o, err
	}
	o.SpentMicros, o.ReservedMicros, o.LimitMicros = b.SpentMicros, b.ReservedMicros, b.LimitMicros
	if o.Criteria, err = outcomes.Criteria(ctx, rt.DB, a); err != nil {
		return o, err
	}
	if o.Obligations, err = watchdog.Status(ctx, rt.DB, id, rt.Now()); err != nil {
		return o, err
	}
	if o.OpenTasks, err = openTasks(ctx, rt.DB, id); err != nil {
		return o, err
	}
	if o.Recent, err = recent(ctx, rt.DB, id); err != nil {
		return o, err
	}
	if err := rt.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM notifications WHERE assignment_id = ? AND status = 'pending'`, id).
		Scan(&o.Undelivered); err != nil {
		return o, err
	}
	var askTool, askResource sql.NullString
	if err := rt.DB.QueryRowContext(ctx, `SELECT COUNT(*), MIN(a.tool_name), MIN(a.resource) FROM approvals ap
		JOIN actions a ON a.id = ap.action_id WHERE a.assignment_id = ? AND ap.status = 'pending'`, id).
		Scan(&o.PendingAsk, &askTool, &askResource); err != nil {
		return o, err
	}
	nextFire, nextSchedule, err := nextSchedule(ctx, rt.DB, id)
	if err != nil {
		return o, err
	}
	unresolved, err := rt.Gateway.UnresolvedActions(ctx, id)
	if err != nil {
		return o, err
	}
	var firstUnresolved string
	if len(unresolved) > 0 {
		firstUnresolved = fmt.Sprintf("%s happened (action %s)", unresolved[0], unresolved[0].ID)
	}
	if err := rt.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE assignment_id = ? AND status = 'blocked'
		AND updated_at > ?`, id, rt.Now().Add(-7*24*time.Hour).UnixMilli()).Scan(&o.Blocked); err != nil {
		return o, err
	}
	halted, err := system.Get(ctx, rt.DB)
	if err != nil {
		return o, err
	}
	o.Next = next(a, halted, firstUnresolved, o.Blocked, o.OpenTasks, o.PendingAsk, askTool.String, askResource.String, nextFire, nextSchedule, o.Obligations, rt.Now())
	return o, nil
}

func next(a assignments.Assignment, halted system.Status, unresolved string, blocked int, open []Task, asks int, askTool, askResource string, fire time.Time, schedule string, obligations []watchdog.Health, now time.Time) Expected {
	if halted.Halted && a.Status != assignments.Completed && a.Status != assignments.Cancelled && a.Status != assignments.Revoked {
		return Expected{What: "halted (emergency stop): " + halted.Reason}
	}
	switch a.Status {
	case assignments.Paused:
		return Expected{What: "paused: " + firstNonEmpty(a.StatusReason, "by "+a.PausedBy)}
	case assignments.Completed, assignments.Cancelled, assignments.Revoked:
		return Expected{What: "nothing; " + string(a.Kind) + " is " + a.Status}
	}
	if unresolved != "" {
		return Expected{What: "needs you: confirm whether " + unresolved}
	}
	if blocked > 0 {
		return Expected{What: fmt.Sprintf("needs you: %d blocked task(s) to retry or drop", blocked)}
	}
	if asks > 0 {
		return Expected{What: fmt.Sprintf("waiting for your approval: %s %s", askTool, askResource)}
	}
	var timed []Expected
	for _, ob := range obligations {
		if ob.Overdue {
			return Expected{At: now, What: "catching up on an overdue obligation: " + ob.Description}
		}
		timed = append(timed, Expected{At: ob.DueAt, What: "due: " + ob.Description})
	}
	for _, t := range open {
		switch {
		case t.Status == "running":
			return Expected{At: now, What: "working on: " + t.Title}
		case t.Status == "ready" || t.Status == "retryable":
			at := t.NextRunAt
			if at.Before(now) {
				at = now
			}
			what := "run task: " + t.Title
			if t.Waiting != "" {
				what = "waiting (" + t.Waiting + "): " + t.Title
			}
			timed = append(timed, Expected{At: at, What: what})
		}
	}
	if !fire.IsZero() {
		timed = append(timed, Expected{At: fire, What: "scheduled check: " + schedule})
	}
	if len(timed) == 0 {
		for _, t := range open {
			if t.Status == "waiting_event" {
				return Expected{What: "waiting on workers or events: " + t.Title}
			}
		}
		return Expected{What: "idle until the next matching event"}
	}
	sort.Slice(timed, func(i, j int) bool { return timed[i].At.Before(timed[j].At) })
	return timed[0]
}

func openTasks(ctx context.Context, db *sql.DB, id string) ([]Task, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, title, status, COALESCE(next_run_at, 0),
		COALESCE(CASE WHEN json_valid(result_json) THEN json_extract(result_json, '$.waiting') END, '') FROM tasks
		WHERE assignment_id = ? AND status NOT IN ('completed', 'blocked', 'cancelled') ORDER BY created_at, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		var next int64
		if err := rows.Scan(&t.ID, &t.Title, &t.Status, &next, &t.Waiting); err != nil {
			return nil, err
		}
		if next > 0 {
			t.NextRunAt = time.UnixMilli(next)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func recent(ctx context.Context, db *sql.DB, id string) ([]Activity, error) {
	rows, err := db.QueryContext(ctx, `SELECT created_at, actor, operation, COALESCE(object_id, '') FROM audit_log
		WHERE assignment_id = ? AND operation NOT IN ('task.leased', 'policy.evaluated', 'action.dispatching')
		ORDER BY id DESC LIMIT ?`, id, recentLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Activity
	for rows.Next() {
		var a Activity
		var at int64
		if err := rows.Scan(&at, &a.Actor, &a.Operation, &a.ObjectID); err != nil {
			return nil, err
		}
		a.At = time.UnixMilli(at)
		out = append(out, a)
	}
	return out, rows.Err()
}

func nextSchedule(ctx context.Context, db *sql.DB, id string) (time.Time, string, error) {
	var at int64
	var title string
	err := db.QueryRowContext(ctx, `SELECT next_fire_at, title FROM schedules WHERE assignment_id = ? AND enabled = 1
		ORDER BY next_fire_at LIMIT 1`, id).Scan(&at, &title)
	if err == sql.ErrNoRows {
		return time.Time{}, "", nil
	}
	if err != nil {
		return time.Time{}, "", err
	}
	return time.UnixMilli(at), title, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func StateSummary(state json.RawMessage) string {
	if len(state) == 0 || string(state) == "{}" {
		return "(none yet)"
	}
	return string(state)
}
