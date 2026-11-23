package chat

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"agentd/internal/ask"
	"agentd/internal/assignments"
	"agentd/internal/overview"
	"agentd/internal/runtime"
	"agentd/internal/system"
)

type command struct {
	usage string
	help  string
	// starts says the command would start work, so it is refused during an emergency stop.
	starts bool
	run    func(s *Service, ctx context.Context, c call) (string, error)
}

type call struct {
	conv, channel string
	args          []string
	rest          string
}

var commands map[string]command

func init() {
	commands = map[string]command{
		"help":    {usage: "/help", help: "this list", run: (*Service).cmdHelp},
		"status":  {usage: "/status", help: "assignments, their next step, and what waits on you", run: (*Service).cmdStatus},
		"show":    {usage: "/show <assignment>", help: "one assignment in detail, as agentd show", run: (*Service).cmdShow},
		"waiting": {usage: "/waiting", help: "approvals, unknown outcomes and blocked tasks", run: (*Service).cmdWaiting},
		"approve": {usage: "/approve <n or id>", help: "approve an item", run: decideWith(true)},
		"reject":  {usage: "/reject <n or id>", help: "reject an item", run: decideWith(false)},
		"pause":   {usage: "/pause <assignment>", help: "stop new work on an assignment", run: (*Service).cmdPause},
		"resume":  {usage: "/resume <assignment>", help: "let a paused assignment work again", run: (*Service).cmdResume},
		"halt":    {usage: "/halt [reason]", help: "emergency stop: nothing runs", run: (*Service).cmdHalt},
		"unhalt":  {usage: "/unhalt", help: "lift the emergency stop", run: (*Service).cmdUnhalt},
		"retry":   {usage: "/retry <task> [note]", help: "continue a blocked task", starts: true, run: (*Service).cmdRetry},
		"usage":   {usage: "/usage", help: "spend per assignment", run: (*Service).cmdUsage},
		"new":     {usage: "/new", help: "start a new conversation", run: (*Service).cmdNew},
		"mcp":     {usage: "/mcp", help: "connected MCP servers and how to add one", run: (*Service).cmdMCP},
		"model":   {usage: "/model [task] [model|tier|auto]", help: "see or choose the model for this conversation or a task", run: (*Service).cmdModel},
		"answer":  {usage: "/answer <n or id> <text>", help: "answer a question a task asked you", run: (*Service).cmdAnswer},
		"tell":    {usage: "/tell <task or assignment> <text>", help: "send a note to running work, word for word", run: (*Service).cmdTell},
		"edit":    {usage: "/edit <n> <job file or json>", help: "replace a proposal with your edited version, which takes effect at once", run: (*Service).cmdEdit},
	}
}

// CommandHelp is one slash command as a client shows it in a menu.
type CommandHelp struct{ Name, Usage, Help string }

// Commands lists the slash commands in name order.
func Commands() []CommandHelp {
	out := make([]CommandHelp, 0, len(commands))
	for name, c := range commands {
		out = append(out, CommandHelp{Name: name, Usage: c.usage, Help: c.help})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Command runs one owner slash command. It never calls a model, so it works while the
// model is down, the front desk is paused or out of budget, and (unless it starts work)
// during an emergency stop.
func (s *Service) Command(ctx context.Context, conv, channel, line string) (string, error) {
	fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "/"))
	if len(fields) == 0 {
		return s.cmdHelp(ctx, call{})
	}
	name := strings.ToLower(fields[0])
	cmd, ok := commands[name]
	if !ok {
		return "", fmt.Errorf("unknown command /%s%s", name, suggest(name))
	}
	if cmd.starts {
		if st, err := system.Get(ctx, s.DB); err != nil {
			return "", err
		} else if st.Halted {
			return "", fmt.Errorf("the emergency stop is on (%s); /unhalt first", st.Reason)
		}
	}
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "/")), fields[0]))
	return cmd.run(s, ctx, call{conv: conv, channel: channel, args: fields[1:], rest: rest})
}

func suggest(name string) string {
	var close []string
	for c := range commands {
		if strings.HasPrefix(c, name) || strings.HasPrefix(name, c) || distance(c, name) <= 2 {
			close = append(close, "/"+c)
		}
	}
	sort.Strings(close)
	if len(close) == 0 {
		return "; /help lists the commands"
	}
	return "; did you mean " + strings.Join(close, " or ") + "?"
}

func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

func (s *Service) cmdHelp(context.Context, call) (string, error) {
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "%-22s %s\n", commands[name].usage, commands[name].help)
	}
	return b.String(), nil
}

func (s *Service) cmdStatus(ctx context.Context, _ call) (string, error) {
	var b strings.Builder
	if st, err := system.Get(ctx, s.DB); err != nil {
		return "", err
	} else if st.Halted {
		fmt.Fprintf(&b, "EMERGENCY STOP is on (%s). /unhalt to lift it.\n", st.Reason)
	}
	ids, err := s.liveAssignments(ctx)
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		b.WriteString("No active projects or responsibilities.\n")
	}
	for _, id := range ids {
		o, err := overview.Get(ctx, s.Runtime, id)
		if err != nil {
			return "", err
		}
		a := o.Assignment
		fmt.Fprintf(&b, "%s  %s (%s, %s)\n    next: %s\n", a.ID, a.Name, a.Kind, a.Status, o.Next)
	}
	waiting, err := s.waitingCounts(ctx)
	if err != nil {
		return "", err
	}
	b.WriteString(waiting)
	return b.String(), nil
}

func (s *Service) liveAssignments(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM assignments WHERE builtin IS NULL AND status IN ('active', 'paused')
		ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	return strings1(rows)
}

func (s *Service) waitingCounts(ctx context.Context) (string, error) {
	var approvals, unresolved, blocked int
	if err := s.DB.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM approvals WHERE status = 'pending'),
		(SELECT COUNT(*) FROM actions WHERE status = 'needs_reconciliation'),
		(SELECT COUNT(*) FROM tasks t JOIN assignments a ON a.id = t.assignment_id
			WHERE t.status = 'blocked' AND a.builtin IS NULL AND a.status IN ('active', 'paused'))`).
		Scan(&approvals, &unresolved, &blocked); err != nil {
		return "", err
	}
	if approvals+unresolved+blocked == 0 {
		return "Nothing is waiting on you.\n", nil
	}
	return fmt.Sprintf("Waiting on you: %d approval(s), %d unknown outcome(s), %d blocked task(s). /waiting lists them.\n",
		approvals, unresolved, blocked), nil
}

func (s *Service) cmdShow(ctx context.Context, c call) (string, error) {
	if len(c.args) != 1 {
		return "", errors.New("usage: /show <assignment>")
	}
	id, err := s.resolveAssignment(ctx, c.args[0])
	if err != nil {
		return "", err
	}
	o, err := overview.Get(ctx, s.Runtime, id)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := overview.Write(&b, o); err != nil {
		return "", err
	}
	return b.String(), nil
}

func (s *Service) cmdWaiting(ctx context.Context, c call) (string, error) {
	var b strings.Builder
	approvals, err := s.Runtime.Gateway.PendingApprovals(ctx)
	if err != nil {
		return "", err
	}
	for _, p := range approvals {
		label := p.ID
		if card, err := s.findCard(ctx, c.conv, p.ID); err == nil && card.ConversationID == c.conv {
			label = fmt.Sprintf("[%d]", card.Card().N)
		}
		fmt.Fprintf(&b, "%s approve? %s %s (expires %s)\n", label, p.ToolName, p.Resource, p.ExpiresAt.Local().Format("01-02 15:04"))
	}
	questions, err := ask.OpenFor(ctx, s.DB, "")
	if err != nil {
		return "", err
	}
	for _, q := range questions {
		label := q.ID
		if card, err := s.findCard(ctx, c.conv, q.ID); err == nil && card.ConversationID == c.conv {
			label = fmt.Sprintf("[%d]", card.Card().N)
		}
		fmt.Fprintf(&b, "%s question: %s (/answer %s <text>)\n", label, excerpt(q.Question, 200), strings.Trim(label, "[]"))
	}
	unknown, err := s.Runtime.Gateway.UnresolvedActions(ctx, "")
	if err != nil {
		return "", err
	}
	for _, u := range unknown {
		fmt.Fprintf(&b, "%s did it happen? %s %s (agentd resolve %s happened|not-happened)\n", u.ID, u.ToolName, u.Resource, u.ID)
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT t.id, t.title, COALESCE(json_extract(t.result_json, '$.error'), '')
		FROM tasks t JOIN assignments a ON a.id = t.assignment_id
		WHERE t.status = 'blocked' AND a.builtin IS NULL AND a.status IN ('active', 'paused') ORDER BY t.updated_at DESC`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var id, title, cause string
		if err := rows.Scan(&id, &title, &cause); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s blocked: %s (%s) /retry %s\n", id, title, excerpt(cause, 200), id)
	}
	if b.Len() == 0 {
		return "Nothing is waiting on you.", rows.Err()
	}
	return b.String(), rows.Err()
}

func decideWith(approve bool) func(*Service, context.Context, call) (string, error) {
	return func(s *Service, ctx context.Context, c call) (string, error) {
		if len(c.args) != 1 {
			return "", errors.New("usage: /approve <n or id> or /reject <n or id>")
		}
		out, err := s.Decide(ctx, c.conv, c.args[0], approve, c.channel)
		if errors.Is(err, ErrNotPending) || errors.Is(err, ErrNoSuchCard) || errors.Is(err, ErrStale) {
			fresh, ferr := s.cmdWaiting(ctx, c)
			if ferr != nil {
				return "", errors.Join(err, ferr)
			}
			return "", fmt.Errorf("%w\n\nWaiting now:\n%s", err, fresh)
		}
		return out, err
	}
}

func (s *Service) cmdPause(ctx context.Context, c call) (string, error) {
	if len(c.args) != 1 {
		return "", errors.New("usage: /pause <assignment>")
	}
	id, err := s.resolveAssignment(ctx, c.args[0])
	if err != nil {
		return "", err
	}
	if err := s.Assignments.Pause(ctx, id, assignments.PausedByUser, "", "owner:"+c.channel); err != nil {
		return "", err
	}
	return "Paused " + c.args[0] + ". /resume to continue.", nil
}

func (s *Service) cmdResume(ctx context.Context, c call) (string, error) {
	if len(c.args) != 1 {
		return "", errors.New("usage: /resume <assignment>")
	}
	id, err := s.resolveAssignment(ctx, c.args[0])
	if err != nil {
		return "", err
	}
	if err := s.Assignments.Resume(ctx, id, "owner:"+c.channel); err != nil {
		return "", err
	}
	return "Resumed " + c.args[0] + ".", nil
}

func (s *Service) cmdHalt(ctx context.Context, c call) (string, error) {
	reason := c.rest
	if reason == "" {
		reason = "emergency stop from chat"
	}
	if err := system.Halt(ctx, s.DB, s.Now(), "owner:"+c.channel, reason); err != nil {
		return "", err
	}
	return "Emergency stop on: no task starts, no tool runs, no model is called. /unhalt to lift it.", nil
}

func (s *Service) cmdUnhalt(ctx context.Context, c call) (string, error) {
	if err := system.Resume(ctx, s.DB, s.Now(), "owner:"+c.channel); err != nil {
		return "", err
	}
	if s.Wake != nil {
		s.Wake()
	}
	return "Emergency stop lifted.", nil
}

func (s *Service) cmdRetry(ctx context.Context, c call) (string, error) {
	if len(c.args) < 1 {
		return "", errors.New("usage: /retry <task> [note]")
	}
	note := strings.TrimSpace(strings.TrimPrefix(c.rest, c.args[0]))
	err := s.Runtime.Retry(ctx, c.args[0], "owner:"+c.channel, note)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("no task %s", c.args[0])
	}
	if errors.Is(err, runtime.ErrNotBlocked) {
		return "", fmt.Errorf("task %s is not blocked", c.args[0])
	}
	if err != nil {
		return "", err
	}
	return "Task " + c.args[0] + " will continue.", nil
}

func (s *Service) cmdMCP(ctx context.Context, c call) (string, error) {
	if len(c.args) > 0 {
		return "", errors.New("servers are added and removed in a terminal, where secrets can be typed safely: agentd mcp add <name> ..., agentd mcp remove <name>")
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT s.server, s.state, s.last_error,
		(SELECT COUNT(*) FROM mcp_tools t WHERE t.server = s.server AND t.status = 'accepted'),
		(SELECT COUNT(*) FROM mcp_tools t WHERE t.server = s.server AND t.status IN ('new', 'changed'))
		FROM mcp_servers s WHERE s.state != 'removed' ORDER BY s.server`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var name, state, detail string
		var accepted, waiting int
		if err := rows.Scan(&name, &state, &detail, &accepted, &waiting); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%-16s %-14s %d tool(s) accepted", name, state, accepted)
		if waiting > 0 {
			fmt.Fprintf(&b, ", %d to review (agentd mcp review %s)", waiting, name)
		}
		if detail != "" && state != "needs_review" {
			fmt.Fprintf(&b, "  %s", detail)
		}
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if b.Len() == 0 {
		b.WriteString("No MCP server is connected.\n")
	}
	b.WriteString("Add one in a terminal: agentd mcp add <name> <url>, or agentd mcp add <name> -- <command>. agentd mcp presets lists known services.\n")
	return b.String(), nil
}

func (s *Service) cmdUsage(ctx context.Context, _ call) (string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM assignments WHERE status IN ('active', 'paused') ORDER BY created_at, id`)
	if err != nil {
		return "", err
	}
	ids, err := strings1(rows)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, id := range ids {
		a, err := s.Assignments.Get(ctx, id)
		if err != nil {
			return "", err
		}
		budget, err := s.Runtime.Budget(ctx, a)
		if err != nil {
			return "", err
		}
		limit := "no limit"
		if budget.LimitMicros > 0 {
			limit = fmt.Sprintf("$%.2f", float64(budget.LimitMicros)/1e6)
			if p := a.Policy.Limits.BudgetPeriod.Duration; p > 0 {
				limit += " per " + p.String()
			}
		}
		name := a.Name
		if a.Builtin != "" {
			name += " (this conversation)"
		}
		fmt.Fprintf(&b, "%-32s $%.4f of %s\n", name, float64(budget.SpentMicros)/1e6, limit)
		byModel, err := s.Runtime.SpendByModel(ctx, a)
		if err != nil {
			return "", err
		}
		models := make([]string, 0, len(byModel))
		for m := range byModel {
			models = append(models, m)
		}
		sort.Strings(models)
		for _, m := range models {
			fmt.Fprintf(&b, "    %-28s $%.4f\n", m, float64(byModel[m])/1e6)
		}
	}
	return b.String(), nil
}

func (s *Service) cmdEdit(ctx context.Context, c call) (string, error) {
	if len(c.args) < 2 || s.Edit == nil {
		return "", errors.New("usage: /edit <n> <the edited proposal arguments as JSON>")
	}
	card, err := s.findCard(ctx, c.conv, c.args[0])
	if err != nil {
		return "", err
	}
	if card.Kind != KindProposal {
		return "", fmt.Errorf("[%d] is not a proposal", card.Card().N)
	}
	args := strings.TrimSpace(strings.TrimPrefix(c.rest, c.args[0]))
	if !json.Valid([]byte(args)) && !strings.HasPrefix(args, "---") {
		return "", errors.New("the edited proposal must be JSON or a job file starting with ---")
	}
	return s.Edit(ctx, card.RefID, json.RawMessage(args), "owner:"+c.channel)
}

func (s *Service) cmdNew(ctx context.Context, _ call) (string, error) {
	id, err := s.NewConversation(ctx, "")
	if err != nil {
		return "", err
	}
	return "Started conversation " + id + ".", nil
}

// resolveAssignment accepts an id or a unique name, never the front desk.
func (s *Service) resolveAssignment(ctx context.Context, ref string) (string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM assignments WHERE builtin IS NULL AND (id = ? OR name = ?)
		ORDER BY id = ? DESC, status IN ('active', 'paused') DESC, created_at DESC`, ref, ref, ref)
	if err != nil {
		return "", err
	}
	ids, err := strings1(rows)
	if err != nil {
		return "", err
	}
	switch {
	case len(ids) == 0:
		return "", fmt.Errorf("no project or responsibility named %q", ref)
	case len(ids) > 1 && ids[0] != ref:
		return "", fmt.Errorf("%q matches %s; use the id", ref, strings.Join(ids, ", "))
	}
	return ids[0], nil
}
