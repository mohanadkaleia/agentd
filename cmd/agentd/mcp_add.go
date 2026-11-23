package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"agentd/internal/integrations/mcp"
	"agentd/internal/system"
)

// defaultMCPConfig is the config file `agentd mcp add` writes and `agentd serve` loads
// when no other is named: mcp.json next to the database.
func defaultMCPConfig(dataDir string) string { return filepath.Join(dataDir, "mcp.json") }

// mcpConsole is how add and remove talk to the owner; tests replace it.
type mcpConsole struct {
	in          *bufio.Reader
	out         io.Writer
	interactive bool
	// secret asks for a value without showing it.
	secret func(prompt string) (string, error)
	// open shows a URL to the owner, for OAuth sign-in.
	open func(url string) error
}

func terminalConsole() mcpConsole {
	fd := int(os.Stdin.Fd())
	return mcpConsole{
		in: bufio.NewReader(os.Stdin), out: os.Stdout,
		interactive: term.IsTerminal(fd) && term.IsTerminal(int(os.Stdout.Fd())),
		secret: func(prompt string) (string, error) {
			if !term.IsTerminal(fd) {
				return "", errors.New("a secret must be typed at a terminal, so it is never on a command line or in a pipe")
			}
			fmt.Fprint(os.Stdout, prompt)
			b, err := term.ReadPassword(fd)
			fmt.Fprintln(os.Stdout)
			return strings.TrimSpace(string(b)), err
		},
		open: func(url string) error { return exec.Command("/usr/bin/open", url).Run() },
	}
}

func (c mcpConsole) ask(prompt string) string {
	fmt.Fprint(c.out, prompt)
	line, _ := c.in.ReadString('\n')
	return strings.ToLower(strings.TrimSpace(line))
}

type mcpAddOptions struct {
	name, url    string
	command      []string
	env          []string
	token, oauth bool
	clientID     string
	clientSecret bool
	scopes       []string
	redirectPort int
	noReview     bool
	config       string
	preset       *mcpPreset
}

// mcpPreset is what agentd knows about a hosted service, so `agentd mcp add <name>` needs
// only the owner's own OAuth client. Details come from docs/mcp.md, where their check date is.
type mcpPreset struct {
	about, setup string
	server       mcp.ServerConfig
}

var mcpPresets = map[string]mcpPreset{
	"slack": {
		about: "Slack's hosted server: read channels, search, post",
		setup: "Create a Slack app, add the redirect URL http://127.0.0.1:53682/callback to it, then:\n  agentd mcp add slack -client-id <id> -client-secret",
		server: mcp.ServerConfig{URL: "https://mcp.slack.com/mcp", MaxConcurrent: 2, Auth: &mcp.Auth{Type: "oauth", RedirectPort: 53682,
			Scopes: []string{"channels:history", "chat:write", "search:read.public"}}},
	},
	"gmail": {
		about: "Google's hosted Gmail server: search and read threads, create drafts",
		setup: "In a Google Cloud project, enable gmail.googleapis.com and gmailmcp.googleapis.com and create a Desktop OAuth client, then:\n  agentd mcp add gmail -client-id <id> -client-secret",
		server: mcp.ServerConfig{URL: "https://gmailmcp.googleapis.com/mcp/v1", Auth: &mcp.Auth{Type: "oauth",
			Scopes: []string{"https://www.googleapis.com/auth/gmail.readonly", "https://www.googleapis.com/auth/gmail.compose"}},
			Tools: mcp.ToolFilter{Include: []string{"gmail.search_threads", "gmail.get_thread", "gmail.create_draft"},
				Meta: map[string]mcp.ToolMeta{
					"gmail.search_threads": {Name: "search_threads", Effect: "read"},
					"gmail.get_thread":     {Name: "get_thread", Effect: "read"},
					"gmail.create_draft":   {Name: "create_draft", Effect: "write"},
				}}},
	},
}

func mcpPresetList(w io.Writer) {
	names := make([]string, 0, len(mcpPresets))
	for name := range mcpPresets {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		fmt.Fprintf(w, "%s: %s\n  %s\n\n", name, mcpPresets[name].about, strings.ReplaceAll(mcpPresets[name].setup, "\n", "\n  "))
	}
	fmt.Fprintln(w, "Any other server: agentd mcp add <name> <https-url>, or agentd mcp add <name> -- <command> [args...]")
}

const mcpAddUsage = `usage: agentd mcp add <name> <https-url> [-token | -oauth [-client-id ID] [-client-secret] [-scopes a,b] [-redirect-port N]]
       agentd mcp add <name> [-env KEY | -env KEY=value]... -- <command> [args...]
       agentd mcp add <preset> -client-id ID [-client-secret]      (agentd mcp presets lists them)

  -env KEY          ask for the value without showing it, and keep it in the keychain
  -env KEY=value    a plain value that is not a secret
  -token            a hosted server that takes a bearer token (asked for, kept in the keychain)
  -oauth            a hosted server you sign in to in the browser
  -client-secret    ask for the OAuth client secret (needs -client-id)
  -no-review        add the server and leave every tool for agentd mcp review`

func parseMCPAdd(args []string) (mcpAddOptions, error) {
	var o mcpAddOptions
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value\n%s", a, mcpAddUsage)
			}
			i++
			return args[i], nil
		}
		var err error
		switch strings.Replace(a, "--", "-", 1) {
		case "-":
			o.command = args[i+1:]
			i = len(args)
		case "-env":
			var v string
			if v, err = value(); err == nil {
				o.env = append(o.env, v)
			}
		case "-token":
			o.token = true
		case "-oauth":
			o.oauth = true
		case "-client-secret":
			o.clientSecret = true
		case "-no-review":
			o.noReview = true
		case "-client-id":
			o.clientID, err = value()
		case "-config":
			o.config, err = value()
		case "-scopes":
			var v string
			if v, err = value(); err == nil {
				o.scopes = strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
			}
		case "-redirect-port":
			var v string
			if v, err = value(); err == nil {
				o.redirectPort, err = strconv.Atoi(v)
			}
		default:
			if strings.HasPrefix(a, "-") {
				return o, fmt.Errorf("unknown flag %s\n%s", a, mcpAddUsage)
			}
			positional = append(positional, a)
		}
		if err != nil {
			return o, err
		}
	}
	if len(positional) == 0 {
		return o, errors.New(mcpAddUsage)
	}
	o.name = positional[0]
	if preset, ok := mcpPresets[o.name]; ok && len(positional) == 1 && len(o.command) == 0 {
		if o.clientID == "" {
			return o, fmt.Errorf("%s needs your own OAuth client. %s", o.name, preset.setup)
		}
		o.preset, o.url, o.oauth = &preset, preset.server.URL, true
		return o, nil
	}
	switch {
	case len(o.command) > 0 && len(positional) == 1:
		if o.token || o.oauth {
			return o, errors.New("-token and -oauth are for hosted servers; give a local server its credentials with -env")
		}
	case len(o.command) == 0 && len(positional) == 2:
		o.url = positional[1]
		if len(o.env) > 0 {
			return o, errors.New("-env is for local servers; a hosted server takes -token or -oauth")
		}
		if o.token && o.oauth {
			return o, errors.New("choose one of -token and -oauth")
		}
	default:
		return o, errors.New(mcpAddUsage)
	}
	return o, nil
}

var secretAccount = regexp.MustCompile(`[^a-z0-9]+`)

// storeSecret asks for a secret, keeps it in the keychain and returns the reference the
// config file holds in its place.
func storeSecret(c mcpConsole, server, what, prompt string, stored *[]string) (mcp.Secret, error) {
	account := strings.Trim(secretAccount.ReplaceAllString(strings.ToLower(server+"-"+what), "-"), "-")
	value, err := c.secret(prompt)
	if err != nil {
		return mcp.Secret{}, err
	}
	if value == "" {
		return mcp.Secret{}, fmt.Errorf("no value given for %s", what)
	}
	if err := keychainStore("agentd", account, value); err != nil {
		return mcp.Secret{}, err
	}
	*stored = append(*stored, account)
	return mcp.Secret{Keychain: "agentd/" + account}, nil
}

func mcpAdd(ctx context.Context, db *sql.DB, dataDir, path string, o mcpAddOptions, c mcpConsole) (err error) {
	cfg, err := mcp.LoadOrEmpty(path)
	if err != nil {
		return err
	}
	if _, exists := cfg.Servers[o.name]; exists {
		return fmt.Errorf("a server named %q is already in %s; remove it first: agentd mcp remove %s", o.name, path, o.name)
	}
	var stored []string
	defer func() {
		if err != nil {
			for _, account := range stored {
				keychainDelete("agentd", account)
			}
		}
	}()
	var s mcp.ServerConfig
	suggested := map[string]string{}
	if o.preset != nil {
		s = o.preset.server
		auth := *s.Auth
		auth.ClientID = o.clientID
		if len(o.scopes) > 0 {
			auth.Scopes = o.scopes
		}
		if o.redirectPort != 0 {
			auth.RedirectPort = o.redirectPort
		}
		s.Auth = &auth
		meta := map[string]mcp.ToolMeta{}
		for tool, m := range s.Tools.Meta {
			meta[tool] = m
			suggested[tool] = "w"
			if m.Effect == "read" {
				suggested[tool] = "r"
			}
		}
		s.Tools.Meta = meta
		if o.clientSecret {
			ref, err := storeSecret(c, o.name, "client-secret", "OAuth client secret for "+o.name+" (not shown): ", &stored)
			if err != nil {
				return err
			}
			s.Auth.ClientSecret = &ref
		}
	} else if o.url != "" {
		s.URL = o.url
		switch {
		case o.token:
			ref, err := storeSecret(c, o.name, "token", "Token for "+o.name+" (not shown): ", &stored)
			if err != nil {
				return err
			}
			s.Auth = &mcp.Auth{Type: "bearer", Token: &ref}
		case o.oauth:
			s.Auth = &mcp.Auth{Type: "oauth", ClientID: o.clientID, Scopes: o.scopes, RedirectPort: o.redirectPort}
			if o.clientSecret {
				ref, err := storeSecret(c, o.name, "client-secret", "OAuth client secret for "+o.name+" (not shown): ", &stored)
				if err != nil {
					return err
				}
				s.Auth.ClientSecret = &ref
			}
		}
	} else {
		found, err := exec.LookPath(o.command[0])
		if err != nil {
			return fmt.Errorf("cannot find the program %q: %w", o.command[0], err)
		}
		if s.Command, err = filepath.Abs(found); err != nil {
			return err
		}
		s.Args = o.command[1:]
		for _, e := range o.env {
			key, value, literal := strings.Cut(e, "=")
			if s.Env == nil {
				s.Env = map[string]mcp.Secret{}
			}
			if literal {
				s.Env[key] = mcp.Secret{Literal: value}
				continue
			}
			ref, err := storeSecret(c, o.name, key, key+" for "+o.name+" (not shown): ", &stored)
			if err != nil {
				return err
			}
			s.Env[key] = ref
		}
	}
	if err := (mcp.Config{Servers: map[string]mcp.ServerConfig{o.name: s}}).Check(); err != nil {
		return err
	}
	opts := mcp.Options{DataDir: dataDir, DB: db, Vault: mcpVault(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if o.oauth {
		signIn, cancel := context.WithTimeout(ctx, 10*time.Minute)
		scopes, err := mcp.SignIn(signIn, o.name, s, opts.Vault, c.open, c.out)
		cancel()
		if err != nil {
			return fmt.Errorf("sign-in to %s: %w", o.name, err)
		}
		if err := mcp.RecordScopes(ctx, db, time.Now(), o.name, scopes); err != nil {
			return err
		}
	}
	fmt.Fprintf(c.out, "Connecting to %s...\n", o.name)
	survey := func() ([]mcp.Pin, []string, error) {
		probe, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		return mcp.Survey(probe, o.name, s, opts)
	}
	pins, offered, err := survey()
	if err != nil {
		return fmt.Errorf("%s did not answer, so nothing was saved: %w", o.name, err)
	}
	// A model cannot be offered a name with dots or one that is too long, so give those a local name.
	renamed := false
	for _, tool := range offered {
		if mcp.ValidToolName(o.name, tool) {
			continue
		}
		local := strings.Trim(regexp.MustCompile(`[^A-Za-z0-9_-]+`).ReplaceAllString(tool, "_"), "_")
		if room := 64 - len(o.name) - 2; len(local) > room && room > 0 {
			local = local[len(local)-room:]
		}
		if local == "" || !mcp.ValidToolName(o.name, local) {
			fmt.Fprintf(c.out, "  %s cannot be offered to models and has no usable short name; left out\n", mcp.Visible(tool))
			continue
		}
		if s.Tools.Meta == nil {
			s.Tools.Meta = map[string]mcp.ToolMeta{}
		}
		s.Tools.Meta[tool] = mcp.ToolMeta{Name: local}
		renamed = true
	}
	if renamed {
		if pins, _, err = survey(); err != nil {
			return fmt.Errorf("%s did not answer, so nothing was saved: %w", o.name, err)
		}
	}
	fmt.Fprintf(c.out, "%s answered with %d tool(s). No tool was called.\n\n", o.name, len(pins))
	accepted := 0
	if c.interactive && !o.noReview {
		if accepted, err = reviewTools(ctx, db, c, &s, pins, suggested); err != nil {
			return err
		}
	}
	cfg.Servers[o.name] = s
	if err := mcp.Save(path, cfg); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Added %s to %s.\n", o.name, path)
	waiting := 0
	for _, p := range pins {
		if p.Status == mcp.StatusNew || p.Status == mcp.StatusChanged {
			waiting++
		}
	}
	if waiting -= accepted; waiting > 0 {
		fmt.Fprintf(c.out, "%d tool(s) are not accepted and will not run. Review them with: agentd mcp review %s\n", waiting, o.name)
	}
	fmt.Fprintln(c.out, reloadNote(ctx, db, path, "It loads the next time agentd starts."))
	fmt.Fprintf(c.out, "Its tools are named %s__<tool>. A job can use one only when its policy has a rule for it;\nin the chat, ask for example: let <job> use %s.\n", o.name, o.name)
	return nil
}

// reviewTools asks, tool by tool, whether to accept it and whether it only reads. The
// server's own hints are shown and never applied without the owner's answer.
func reviewTools(ctx context.Context, db *sql.DB, c mcpConsole, s *mcp.ServerConfig, pins []mcp.Pin, suggested map[string]string) (int, error) {
	accepted := 0
	fmt.Fprintln(c.out, "For each tool: r accepts it as read-only, w accepts it as a tool that changes things,\ns skips it (it will not run), d shows its full definition, q skips the rest.")
	for _, p := range pins {
		if p.Status != mcp.StatusNew && p.Status != mcp.StatusChanged {
			continue
		}
		var d mcp.Definition
		json.Unmarshal(p.Definition, &d)
		var hints struct {
			ReadOnly    *bool `json:"readOnlyHint"`
			Destructive *bool `json:"destructiveHint"`
		}
		json.Unmarshal(d.Annotations, &hints)
		hint := ""
		switch {
		case hints.Destructive != nil && *hints.Destructive:
			hint = "  (the server says: destructive)"
		case hints.ReadOnly != nil && *hints.ReadOnly:
			hint = "  (the server says: read-only; not verified)"
		}
		summary, _, _ := strings.Cut(strings.TrimSpace(mcp.Visible(d.Description)), "\n")
		if len([]rune(summary)) > 100 {
			summary = string([]rune(summary)[:99]) + "…"
		}
		fallback := "s"
		if v := suggested[p.Tool]; v != "" {
			fallback = v
			hint += map[string]string{"r": "  (agentd's preset: read-only)", "w": "  (agentd's preset: changes things)"}[v]
		}
		fmt.Fprintf(c.out, "\n%s%s\n    %s\n", mcp.Visible(p.Tool), hint, summary)
		for answered := false; !answered; {
			answer := c.ask("    r / w / s / d / q? [" + fallback + "] ")
			if answer == "" {
				answer = fallback
			}
			switch answer {
			case "d":
				printPin(c.out, p)
			case "q":
				return accepted, nil
			case "r":
				if hints.Destructive != nil && *hints.Destructive {
					fmt.Fprintln(c.out, "    The server marks this tool destructive, so it cannot be read-only. Use w or s.")
					continue
				}
				if s.Tools.Meta == nil {
					s.Tools.Meta = map[string]mcp.ToolMeta{}
				}
				meta := s.Tools.Meta[p.Tool]
				meta.Effect = "read"
				s.Tools.Meta[p.Tool] = meta
				if err := mcp.Accept(ctx, db, time.Now(), p.Server, p.Tool, p.Hash, "cli"); err != nil {
					return accepted, err
				}
				accepted++
				answered = true
			case "w":
				if meta, ok := s.Tools.Meta[p.Tool]; ok && meta.Effect == "read" {
					meta.Effect = ""
					s.Tools.Meta[p.Tool] = meta
				}
				if err := mcp.Accept(ctx, db, time.Now(), p.Server, p.Tool, p.Hash, "cli"); err != nil {
					return accepted, err
				}
				accepted++
				answered = true
			default:
				answered = true
			}
		}
	}
	fmt.Fprintln(c.out)
	return accepted, nil
}

func mcpRemove(ctx context.Context, db *sql.DB, path, name string, yes bool, c mcpConsole) error {
	cfg, err := mcp.LoadOrEmpty(path)
	if err != nil {
		return err
	}
	s, ok := cfg.Servers[name]
	if !ok {
		return fmt.Errorf("no server %q in %s", name, path)
	}
	// Only secrets `agentd mcp add` stored for this server are deleted: agentd/<name>-...
	var accounts []string
	own := func(ref *mcp.Secret) {
		if ref != nil && strings.HasPrefix(ref.Keychain, "agentd/"+name+"-") {
			accounts = append(accounts, strings.TrimPrefix(ref.Keychain, "agentd/"))
		}
	}
	for _, ref := range s.Env {
		own(&ref)
	}
	if s.Auth != nil {
		own(s.Auth.Token)
		own(s.Auth.ClientSecret)
		if s.Auth.Type == "oauth" {
			accounts = append(accounts, mcp.CredentialAccount(name))
		}
	}
	slices.Sort(accounts)
	if !yes {
		if !c.interactive {
			return errors.New("removing a server deletes its stored secrets; confirm with -yes")
		}
		fmt.Fprintf(c.out, "Remove %s from %s", name, path)
		if len(accounts) > 0 {
			fmt.Fprintf(c.out, " and delete %d stored secret(s)", len(accounts))
		}
		if c.ask("? This cannot be undone. [y/N] ") != "y" {
			return errors.New("nothing was removed")
		}
	}
	delete(cfg.Servers, name)
	if err := mcp.Save(path, cfg); err != nil {
		return err
	}
	for _, account := range accounts {
		if err := keychainDelete("agentd", account); err != nil && account != mcp.CredentialAccount(name) {
			fmt.Fprintf(c.out, "could not delete the stored secret %s: %v\n", account, err)
		}
	}
	mcp.Removed(ctx, db, name)
	fmt.Fprintf(c.out, "Removed %s.\n", name)
	if note := reloadNote(ctx, db, path, ""); note != "" {
		fmt.Fprintln(c.out, note)
	}
	return nil
}

// reloadNote says how a config change reaches agentd: by itself when the running agentd
// watches this file, after a restart otherwise.
func reloadNote(ctx context.Context, db *sql.DB, path, stopped string) string {
	l, alive, _ := system.Alive(ctx, db, time.Now())
	if !alive {
		return stopped
	}
	if abs, err := filepath.Abs(path); err == nil && l.MCPConfig == abs {
		return "The running agentd picks this up by itself within a minute (it starts again once running tasks finish)."
	}
	return "The running agentd does not watch this file: restart it to apply the change."
}

// fileSignature identifies a file's content cheaply; "" means there is no file.
func fileSignature(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d-%d", info.ModTime().UnixNano(), info.Size())
}

// watchMCPConfig calls changed once when the MCP config file differs from the one serve
// loaded and is valid. It waits up to patience for running tasks to finish first, so a
// restart interrupts as little as it can; an interrupted task recovers as after any stop.
func watchMCPConfig(ctx context.Context, db *sql.DB, path, loaded string, every, patience time.Duration, log *slog.Logger, changed func()) {
	rejected := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
		now := fileSignature(path)
		if now == loaded || now == rejected {
			continue
		}
		if now != "" {
			if _, err := mcp.Load(path); err != nil {
				log.Error("the MCP config changed but is not valid; still running with the earlier one", "err", err)
				rejected = now
				continue
			}
		}
		log.Info("the MCP config changed; starting again once running tasks finish", "file", path)
		deadline := time.Now().Add(patience)
		for time.Now().Before(deadline) {
			var running int
			if db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE status = 'running'`).Scan(&running) != nil || running == 0 {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(every):
			}
		}
		changed()
		return
	}
}
