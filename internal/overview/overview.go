package overview

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"agentd/internal/assignments"
	"agentd/internal/runtime"
)

type Overview struct {
	Assignment  assignments.Assignment
	SpentMicros int64
	LimitMicros int64
	OpenTasks   []Task
	Recent      []Activity
	Next        Expected
	Undelivered int
	PendingAsk  int
}

type Task struct {
	ID        string
	Title     string
	Status    string
	NextRunAt time.Time
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
	if o.SpentMicros, o.LimitMicros, err = rt.Spend(ctx, a); err != nil {
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
	o.Next = next(a, o.OpenTasks, o.PendingAsk, askTool.String, askResource.String, nextFire, nextSchedule, rt.Now())
	return o, nil
}

func next(a assignments.Assignment, open []Task, asks int, askTool, askResource string, fire time.Time, schedule string, now time.Time) Expected {
	switch a.Status {
	case assignments.Paused:
		return Expected{What: "paused: " + firstNonEmpty(a.StatusReason, "by "+a.PausedBy)}
	case assignments.Completed, assignments.Cancelled, assignments.Revoked:
		return Expected{What: "nothing; " + string(a.Kind) + " is " + a.Status}
	}
	if asks > 0 {
		return Expected{What: fmt.Sprintf("waiting for your approval: %s %s", askTool, askResource)}
	}
	var timed []Expected
	for _, t := range open {
		switch {
		case t.Status == "running":
			return Expected{At: now, What: "working on: " + t.Title}
		case t.Status == "ready" || t.Status == "retryable":
			at := t.NextRunAt
			if at.Before(now) {
				at = now
			}
			timed = append(timed, Expected{At: at, What: "run task: " + t.Title})
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
	rows, err := db.QueryContext(ctx, `SELECT id, title, status, COALESCE(next_run_at, 0) FROM tasks
		WHERE assignment_id = ? AND status NOT IN ('completed', 'blocked', 'cancelled') ORDER BY created_at, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		var next int64
		if err := rows.Scan(&t.ID, &t.Title, &t.Status, &next); err != nil {
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
