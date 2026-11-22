package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"agentd/internal/integrations/mcp"
)

func mcpCommand(ctx context.Context, db *sql.DB, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: agentd mcp review [server] | accept <server> (<tool>... | -all)")
	}
	switch args[0] {
	case "review":
		server := ""
		if len(args) > 1 {
			server = args[1]
		}
		return mcpReview(ctx, db, os.Stdout, server)
	case "accept":
		fs := flag.NewFlagSet("accept", flag.ContinueOnError)
		all := fs.Bool("all", false, "accept every new or changed tool of the server (each is printed first)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		rest := fs.Args()
		if len(rest) == 0 || (len(rest) == 1) != *all {
			return fmt.Errorf("usage: agentd mcp accept <server> <tool>... | agentd mcp accept -all <server>")
		}
		return mcpAccept(ctx, db, os.Stdout, rest[0], rest[1:], *all)
	}
	return fmt.Errorf("unknown mcp command %q", args[0])
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
