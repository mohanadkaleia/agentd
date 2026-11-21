package gateway

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"agentd/internal/assignments"
	"agentd/internal/audit"
	"agentd/internal/ids"
	"agentd/internal/model"
	"agentd/internal/policy"
	"agentd/internal/system"
	"agentd/internal/tasks"
	"agentd/internal/tools"
)

const (
	Prepared            = "prepared"
	PendingApproval     = "pending_approval"
	Dispatching         = "dispatching"
	Succeeded           = "succeeded"
	Failed              = "failed"
	Denied              = "denied"
	NeedsReconciliation = "needs_reconciliation"
)

var ErrConflict = errors.New("action changed concurrently")

type Kind int

const (
	Continue Kind = iota
	WaitApproval
	WaitEvent
	Paused
	Blocked
)

type Outcome struct {
	Kind    Kind
	Content string
}

type Call struct {
	RunID        string
	TaskID       string
	AssignmentID string
	Seq          int
	Call         model.ToolCall
	Lease        tasks.Lease
}

type Action struct {
	ID             string
	RunID          string
	TaskID         string
	AssignmentID   string
	ToolName       string
	Arguments      json.RawMessage
	ArgumentsHash  string
	Resource       string
	Status         string
	IdempotencyKey string
	Result         json.RawMessage
}

type Gateway struct {
	DB           *sql.DB
	Tools        tools.Registry
	Assignments  *assignments.Store
	Tasks        *tasks.Store
	Now          func() time.Time
	ApprovalTTL  time.Duration
	AfterExecute func(Action)
}

func (g *Gateway) Handle(ctx context.Context, c Call) (Outcome, error) {
	key := fmt.Sprintf("%s:%d:%s", c.RunID, c.Seq, c.Call.ID)
	a, err := g.loadByKey(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		a, err = g.prepare(ctx, c, key)
	}
	if err != nil {
		return Outcome{}, err
	}
	tool, _ := g.Tools.Lookup(a.ToolName)

	if a.Status == Dispatching {
		if err := g.recover(ctx, &a, tool); err != nil {
			return Outcome{}, err
		}
	}
	switch a.Status {
	case Succeeded, Failed, Denied:
		return Outcome{Kind: Continue, Content: string(a.Result)}, nil
	case NeedsReconciliation:
		return Outcome{Kind: Blocked, Content: string(a.Result)}, nil
	}
	return g.authorizeAndDispatch(ctx, &a, tool, c.Lease)
}

func (g *Gateway) prepare(ctx context.Context, c Call, key string) (Action, error) {
	args := c.Call.Arguments
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	sum := sha256.Sum256(args)
	a := Action{
		ID:             ids.New("act"),
		RunID:          c.RunID,
		TaskID:         c.TaskID,
		AssignmentID:   c.AssignmentID,
		ToolName:       c.Call.Name,
		Arguments:      args,
		ArgumentsHash:  hex.EncodeToString(sum[:]),
		Status:         Prepared,
		IdempotencyKey: key,
	}
	tool, ok := g.Tools.Lookup(c.Call.Name)
	switch {
	case !ok:
		a.Status, a.Result = Denied, errorJSON(fmt.Sprintf("unknown tool %q", c.Call.Name))
	default:
		resource, err := tool.Resource(args)
		if err != nil {
			a.Status, a.Result = Failed, errorJSON("invalid arguments: "+err.Error())
		}
		a.Resource = resource
	}

	p, err := g.Assignments.Get(ctx, c.AssignmentID)
	if err != nil {
		return a, err
	}
	now := g.Now()
	tx, err := g.DB.BeginTx(ctx, nil)
	if err != nil {
		return a, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO actions (id, run_id, task_id, assignment_id, tool_name, arguments_json, arguments_hash,
		resource, policy_version, status, idempotency_key, result_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.RunID, a.TaskID, a.AssignmentID, a.ToolName, string(a.Arguments), a.ArgumentsHash,
		a.Resource, p.PolicyVersion, a.Status, a.IdempotencyKey, nullable(a.Result), now.UnixMilli(), now.UnixMilli()); err != nil {
		return a, err
	}
	if err := audit.Record(ctx, tx, now, a.AssignmentID, "agent", "action."+a.Status, a.ID,
		map[string]any{"tool": a.ToolName, "resource": a.Resource, "arguments_hash": a.ArgumentsHash}); err != nil {
		return a, err
	}
	return a, tx.Commit()
}

func (g *Gateway) authorizeAndDispatch(ctx context.Context, a *Action, tool tools.Tool, lease tasks.Lease) (Outcome, error) {
	p, err := g.Assignments.Get(ctx, a.AssignmentID)
	if err != nil {
		return Outcome{}, err
	}
	if p.Status != assignments.Active {
		return Outcome{Kind: Paused}, nil
	}
	if s, err := system.Get(ctx, g.DB); err != nil || s.Halted {
		return Outcome{Kind: Paused}, err
	}
	task, err := g.Tasks.Get(ctx, a.TaskID)
	if err != nil {
		return Outcome{}, err
	}
	d := policy.Evaluate(p.Policy, a.ToolName, a.Resource)
	if !task.Allows(a.ToolName) {
		d = policy.Decision{Mode: policy.Deny, Reason: fmt.Sprintf("tool %q is not in this task's allowlist", a.ToolName)}
	}
	if err := audit.Record(ctx, g.DB, g.Now(), a.AssignmentID, "policy", "policy.evaluated", a.ID, d); err != nil {
		return Outcome{}, err
	}
	switch d.Mode {
	case policy.Deny:
		return g.finish(ctx, a, a.Status, Denied, errorJSON("denied by policy: "+d.Reason), "")
	case policy.RequireApproval:
		out, approved, err := g.checkApproval(ctx, a)
		if err != nil || !approved {
			return out, err
		}
	}
	return g.dispatch(ctx, a, tool, p.Policy.Limits.ToolRates[a.ToolName], lease)
}

func (g *Gateway) dispatch(ctx context.Context, a *Action, tool tools.Tool, limit policy.RateLimit, lease tasks.Lease) (Outcome, error) {
	if reason, err := g.beginDispatch(ctx, a, limit, lease); err != nil || reason != "" {
		if errors.Is(err, errHalted) {
			return Outcome{Kind: Paused}, nil
		}
		if err != nil {
			return Outcome{}, err
		}
		return g.finish(ctx, a, a.Status, Denied, errorJSON(reason), "")
	}
	res, err := tool.Execute(ctx, g.invocation(*a))
	if g.AfterExecute != nil {
		g.AfterExecute(*a)
	}
	ctx = context.WithoutCancel(ctx)
	if err == nil {
		return g.finish(ctx, a, Dispatching, Succeeded, res.Output, res.ExternalID)
	}
	if errors.Is(err, tools.ErrNotReady) {
		if err := g.setStatus(ctx, a, Dispatching, Prepared); err != nil {
			return Outcome{}, err
		}
		return Outcome{Kind: WaitEvent}, nil
	}
	if errors.Is(err, tools.ErrNotExecuted) || tool.Effect() == tools.EffectRead {
		return g.finish(ctx, a, Dispatching, Failed, errorJSON(err.Error()), "")
	}
	res, found, rerr := tool.Reconcile(ctx, g.invocation(*a))
	switch {
	case rerr != nil:
		return g.finish(ctx, a, Dispatching, NeedsReconciliation, errorJSON(fmt.Sprintf("outcome unknown: %v; reconcile: %v", err, rerr)), "")
	case found:
		return g.finish(ctx, a, Dispatching, Succeeded, res.Output, res.ExternalID)
	default:
		return g.finish(ctx, a, Dispatching, Failed, errorJSON(err.Error()), "")
	}
}

var errHalted = errors.New("system is halted")

func (g *Gateway) beginDispatch(ctx context.Context, a *Action, limit policy.RateLimit, lease tasks.Lease) (string, error) {
	now := g.Now()
	tx, err := g.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if lease.Owner != "" {
		if err := tasks.HoldsLease(ctx, tx, lease); err != nil {
			return "", err
		}
	}
	var halted int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM system_state WHERE key = 'halted'`).Scan(&halted); err != nil {
		return "", err
	}
	if halted > 0 {
		return "", errHalted
	}
	if limit.Per.Duration > 0 {
		var used int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM actions
			WHERE assignment_id = ? AND tool_name = ? AND id != ? AND dispatched_at > ?
			  AND status IN ('dispatching', 'succeeded', 'needs_reconciliation')`,
			a.AssignmentID, a.ToolName, a.ID, now.Add(-limit.Per.Duration).UnixMilli()).Scan(&used); err != nil {
			return "", err
		}
		if used >= limit.Max {
			return fmt.Sprintf("rate limit reached: %d %s per %s", limit.Max, a.ToolName, limit.Per.Duration), nil
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE actions SET status = 'dispatching', dispatched_at = ?, updated_at = ?
		WHERE id = ? AND status = ?`, now.UnixMilli(), now.UnixMilli(), a.ID, a.Status)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrConflict
	}
	if err := audit.Record(ctx, tx, now, a.AssignmentID, "gateway", "action.dispatching", a.ID, nil); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	a.Status = Dispatching
	return "", nil
}

func (g *Gateway) recover(ctx context.Context, a *Action, tool tools.Tool) error {
	res, found, err := tool.Reconcile(ctx, g.invocation(*a))
	switch {
	case err != nil:
		_, ferr := g.finish(ctx, a, Dispatching, NeedsReconciliation, errorJSON("reconcile failed: "+err.Error()), "")
		return ferr
	case found:
		_, ferr := g.finish(ctx, a, Dispatching, Succeeded, res.Output, res.ExternalID)
		return ferr
	}
	return audit.Record(ctx, g.DB, g.Now(), a.AssignmentID, "gateway", "action.reconciled_not_found", a.ID, nil)
}

func (g *Gateway) finish(ctx context.Context, a *Action, from, to string, result json.RawMessage, externalID string) (Outcome, error) {
	now := g.Now()
	tx, err := g.DB.BeginTx(ctx, nil)
	if err != nil {
		return Outcome{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE actions SET status = ?, result_json = ?, external_id = NULLIF(?, ''), updated_at = ?
		WHERE id = ? AND status = ?`, to, string(result), externalID, now.UnixMilli(), a.ID, from)
	if err != nil {
		return Outcome{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Outcome{}, ErrConflict
	}
	if err := audit.Record(ctx, tx, now, a.AssignmentID, "gateway", "action."+to, a.ID, map[string]string{"external_id": externalID}); err != nil {
		return Outcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return Outcome{}, err
	}
	a.Status, a.Result = to, result
	if to == NeedsReconciliation {
		return Outcome{Kind: Blocked, Content: string(result)}, nil
	}
	return Outcome{Kind: Continue, Content: string(result)}, nil
}

func (g *Gateway) setStatus(ctx context.Context, a *Action, from, to string) error {
	now := g.Now()
	tx, err := g.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE actions SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
		to, now.UnixMilli(), a.ID, from)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 && from != to {
		return ErrConflict
	}
	if err := audit.Record(ctx, tx, now, a.AssignmentID, "gateway", "action."+to, a.ID, nil); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	a.Status = to
	return nil
}

func (g *Gateway) loadByKey(ctx context.Context, key string) (Action, error) {
	var a Action
	var args string
	var result sql.NullString
	err := g.DB.QueryRowContext(ctx, `SELECT id, run_id, task_id, assignment_id, tool_name, arguments_json, arguments_hash,
		resource, status, idempotency_key, result_json FROM actions WHERE idempotency_key = ?`, key).
		Scan(&a.ID, &a.RunID, &a.TaskID, &a.AssignmentID, &a.ToolName, &args, &a.ArgumentsHash,
			&a.Resource, &a.Status, &a.IdempotencyKey, &result)
	a.Arguments = json.RawMessage(args)
	if result.Valid {
		a.Result = json.RawMessage(result.String)
	}
	return a, err
}

func (g *Gateway) invocation(a Action) tools.Invocation {
	return tools.Invocation{
		ActionID:       a.ID,
		AssignmentID:   a.AssignmentID,
		TaskID:         a.TaskID,
		RunID:          a.RunID,
		IdempotencyKey: a.IdempotencyKey,
		Name:           a.ToolName,
		Arguments:      a.Arguments,
	}
}

func errorJSON(msg string) json.RawMessage {
	return mustJSON(map[string]string{"error": msg})
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func nullable(b json.RawMessage) any {
	if b == nil {
		return nil
	}
	return string(b)
}
