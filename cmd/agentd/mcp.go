package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"agentd/internal/integrations/mcp"
)

func mcpCommand(ctx context.Context, db *sql.DB, dataDir string, args []string) error {
	usage := fmt.Errorf("usage: agentd mcp list | tools <server> | review [server] | accept <server> (<tool>... | -all) |\n" +
		"       auth <server> | test <server> | restart <server> | check   (auth, test and check read -config FILE or AGENTD_MCP_CONFIG)")
	if len(args) == 0 {
		return usage
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	path := fs.String("config", envOr("AGENTD_MCP_CONFIG", ""), "the MCP config file agentd serve uses")
	all := fs.Bool("all", false, "accept every new or changed tool of the server (each is printed first)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rest := fs.Args()
	config := func() (mcp.Config, error) {
		if *path == "" {
			return mcp.Config{}, fmt.Errorf("this command needs the MCP config: -config FILE or AGENTD_MCP_CONFIG")
		}
		return mcp.Load(*path)
	}
	server := func() (string, mcp.ServerConfig, error) {
		if len(rest) != 1 {
			return "", mcp.ServerConfig{}, usage
		}
		cfg, err := config()
		if err != nil {
			return "", mcp.ServerConfig{}, err
		}
		c, ok := cfg.Servers[rest[0]]
		if !ok {
			return "", mcp.ServerConfig{}, fmt.Errorf("no server %q in %s", rest[0], *path)
		}
		return rest[0], c, nil
	}
	switch args[0] {
	case "list":
		return mcpList(ctx, db, os.Stdout)
	case "tools":
		if len(rest) != 1 {
			return usage
		}
		var meta map[string]mcp.ToolMeta
		if cfg, err := config(); err == nil {
			meta = cfg.Servers[rest[0]].Tools.Meta
		}
		return mcpTools(ctx, db, os.Stdout, rest[0], meta, time.Now())
	case "review":
		name := ""
		if len(rest) > 0 {
			name = rest[0]
		}
		return mcpReview(ctx, db, os.Stdout, name)
	case "accept":
		if len(rest) == 0 || (len(rest) == 1) != *all {
			return fmt.Errorf("usage: agentd mcp accept <server> <tool>... | agentd mcp accept -all <server>")
		}
		return mcpAccept(ctx, db, os.Stdout, rest[0], rest[1:], *all)
	case "auth":
		name, c, err := server()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		scopes, err := mcp.SignIn(ctx, name, c, mcpVault(), func(url string) error {
			return exec.Command("/usr/bin/open", url).Run()
		}, os.Stdout)
		if err != nil {
			return err
		}
		return mcp.RecordScopes(ctx, db, time.Now(), name, scopes)
	case "test":
		name, c, err := server()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		pins, err := mcp.Probe(ctx, name, c, mcp.Options{DataDir: dataDir, DB: db, Vault: mcpVault(),
			Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		fmt.Printf("%s: connected, initialized and listed %d tool(s); no tool was called\n", name, len(pins))
		for _, p := range pins {
			fmt.Printf("  %s__%s\t%s\n", p.Server, p.Tool, p.Status)
		}
		return nil
	case "restart":
		if len(rest) != 1 {
			return usage
		}
		if err := mcp.RequestRestart(ctx, db, rest[0]); err != nil {
			return fmt.Errorf("no server %q has run yet: %w", rest[0], err)
		}
		fmt.Printf("asked the running agentd to restart %s (within a few seconds)\n", rest[0])
		return nil
	case "check":
		cfg, err := config()
		if err != nil {
			return err
		}
		var problems []string
		for name, c := range cfg.Servers {
			for _, err := range mcp.Check(name, c, mcpVault()) {
				problems = append(problems, err.Error())
			}
		}
		if len(problems) > 0 {
			slices.Sort(problems)
			return fmt.Errorf("%s has problems:\n  %s", *path, strings.Join(problems, "\n  "))
		}
		fmt.Printf("%s is valid: %d server(s), every secret resolves\n", *path, len(cfg.Servers))
		return nil
	}
	return usage
}

func mcpList(ctx context.Context, db *sql.DB, w io.Writer) error {
	servers, err := mcp.ListServers(ctx, db)
	if err != nil {
		return err
	}
	if len(servers) == 0 {
		fmt.Fprintln(w, "no MCP server has run yet")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVER\tSTATE\tTRANSPORT\tTOOLS\tSCOPES\tDETAIL")
	for _, s := range servers {
		counts := []string{}
		for _, status := range []string{mcp.StatusAccepted, mcp.StatusNew, mcp.StatusChanged, mcp.StatusRemoved} {
			if n := s.Tools[status]; n > 0 {
				counts = append(counts, fmt.Sprintf("%d %s", n, status))
			}
		}
		detail := s.LastError
		if s.NextRetryAt != nil {
			detail += " (retry " + s.NextRetryAt.Local().Format("15:04:05") + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Server, s.State, s.Transport, strings.Join(counts, ", "), s.Scopes, detail)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	polls, err := mcp.ListPolls(ctx, db)
	if err != nil || len(polls) == 0 {
		return err
	}
	fmt.Fprintln(w, "\nPOLLS")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVER\tPOLL\tLAST SUCCESS\tFAILURES IN A ROW\tLAST ERROR")
	for _, p := range polls {
		last := "never"
		if p.LastSuccessAt != nil {
			last = p.LastSuccessAt.Local().Format("2006-01-02 15:04:05")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", p.Server, p.Poll, last, p.Failures, p.LastError)
	}
	return tw.Flush()
}

func mcpTools(ctx context.Context, db *sql.DB, w io.Writer, server string, meta map[string]mcp.ToolMeta, now time.Time) error {
	pins, err := mcp.ListPins(ctx, db, server)
	if err != nil {
		return err
	}
	if len(pins) == 0 {
		return fmt.Errorf("no tools recorded for %q", server)
	}
	since := now.Add(-30 * 24 * time.Hour).UnixMilli()
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TOOL\tSTATUS\tEFFECT\tRESOURCE\tSERVER HINTS (not trusted)\tCALLS 30D\tRESOLVED BY HAND 30D")
	for _, p := range pins {
		name := p.Server + "__" + p.Tool
		effect, resource := "write", "-"
		if m, ok := meta[p.Tool]; ok && m.Name != "" {
			name = p.Server + "__" + m.Name + " (" + p.Tool + ")"
		}
		if m, ok := meta[p.Tool]; ok {
			if m.Effect != "" {
				effect = m.Effect
			}
			if m.Resource != "" {
				resource = m.Resource
			}
		}
		var d mcp.Definition
		json.Unmarshal(p.Definition, &d)
		hints := "-"
		if len(d.Annotations) > 0 {
			hints = string(d.Annotations)
		}
		var calls, resolved int
		db.QueryRowContext(ctx, `SELECT COUNT(*) FROM actions WHERE tool_name = ? AND created_at > ?`, name, since).Scan(&calls)
		db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log l JOIN actions a ON a.id = l.object_id
			WHERE l.operation LIKE 'action.resolved_%' AND a.tool_name = ? AND l.created_at > ?`, name, since).Scan(&resolved)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\n", name, p.Status, effect, resource, hints, calls, resolved)
	}
	return tw.Flush()
}

func mcpReview(ctx context.Context, db *sql.DB, w io.Writer, server string) error {
	pins, err := mcp.ListPins(ctx, db, server)
	if err != nil {
		return err
	}
	servers := map[string]bool{}
	pending := 0
	for _, p := range pins {
		servers[p.Server] = true
		if p.Status == mcp.StatusNew || p.Status == mcp.StatusChanged {
			printPin(w, p)
			pending++
		}
	}
	for name := range servers {
		if text, err := mcp.Instructions(ctx, db, name); err == nil && strings.TrimSpace(text) != "" {
			fmt.Fprintf(w, "server %q instructions (shown to you only, never sent to the model):\n%s\n\n", name, indent(text))
		}
	}
	if pending == 0 {
		fmt.Fprintln(w, "nothing to review")
		return nil
	}
	fmt.Fprintf(w, "%d tool(s) await review. They do not run until accepted:\n  agentd mcp accept <server> <tool>...\n", pending)
	return nil
}

func mcpAccept(ctx context.Context, db *sql.DB, w io.Writer, server string, names []string, all bool) error {
	pins, err := mcp.ListPins(ctx, db, server)
	if err != nil {
		return err
	}
	accepted := 0
	for _, p := range pins {
		wanted := slices.Contains(names, p.Tool) || all && (p.Status == mcp.StatusNew || p.Status == mcp.StatusChanged)
		if !wanted {
			continue
		}
		printPin(w, p)
		if err := mcp.Accept(ctx, db, time.Now(), p.Server, p.Tool, p.Hash, "cli"); err != nil {
			return err
		}
		fmt.Fprintf(w, "accepted %s__%s\n\n", p.Server, p.Tool)
		accepted++
	}
	if accepted == 0 {
		return fmt.Errorf("no matching tools to accept on server %q (see: agentd mcp review %s)", server, server)
	}
	return nil
}

func mcpVault() mcp.Vault {
	return mcp.Vault{
		Keychain: keychainSecret,
		Load:     func(account string) ([]byte, bool) { return keychainLoadHex("agentd", account) },
		Store:    func(account string, value []byte) error { return keychainStoreHex("agentd", account, value) },
	}
}

func printPin(w io.Writer, p mcp.Pin) {
	fmt.Fprintf(w, "%s__%s  [%s]  definition %s\n", p.Server, p.Tool, p.Status, p.Hash[:12])
	printDefinition(w, p.Definition)
	if p.Status == mcp.StatusChanged && p.Accepted != nil {
		by := p.AcceptedBy
		if p.AcceptedAt != nil {
			by += " on " + p.AcceptedAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(w, "  previously accepted (%s):\n", by)
		printDefinition(w, p.Accepted)
	}
	fmt.Fprintln(w)
}

func printDefinition(w io.Writer, raw json.RawMessage) {
	var d mcp.Definition
	if err := json.Unmarshal(raw, &d); err != nil {
		fmt.Fprintf(w, "    (unreadable definition: %v)\n", err)
		return
	}
	description := mcp.Visible(d.Description)
	if strings.TrimSpace(description) == "" {
		description = "(none)"
	}
	fmt.Fprintf(w, "    description:\n%s\n", indent(description))
	fmt.Fprintf(w, "    input schema:\n%s\n", indent(pretty(d.InputSchema)))
	if len(d.Annotations) > 0 {
		fmt.Fprintf(w, "    server's own hints (not trusted): %s\n", d.Annotations)
	}
}

func pretty(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Indent(&b, raw, "", "  ") != nil {
		return string(raw)
	}
	return b.String()
}

func indent(s string) string {
	return "      " + strings.ReplaceAll(s, "\n", "\n      ")
}
