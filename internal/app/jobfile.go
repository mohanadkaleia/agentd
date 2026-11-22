package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"agentd/internal/assignments"
	"agentd/internal/policy"
	"agentd/internal/watchdog"
)

// MaxJobFile caps the size of a job file.
const MaxJobFile = 256 << 10

// ParseJob reads a job file by its extension: .md (a header with the spec and the
// objective as its body) or .json.
func ParseJob(name string, data []byte) (Spec, error) {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown":
		return ParseMarkdown(data)
	case ".json":
		return ParseJSON(data)
	}
	return Spec{}, fmt.Errorf("%s: a job file must end in .md or .json", name)
}

// ParseJSON reads a spec as JSON, refusing unknown fields so a misspelt key is an error.
func ParseJSON(data []byte) (Spec, error) {
	if len(data) > MaxJobFile {
		return Spec{}, fmt.Errorf("the file is larger than %d KB", MaxJobFile>>10)
	}
	var s Spec
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, fmt.Errorf("%w: %v", assignments.ErrInvalid, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return s, fmt.Errorf("%w: trailing data after the spec", assignments.ErrInvalid)
	}
	return s, s.checkModes(nil)
}

// LineError is a problem in a job file, with the line it is on.
type LineError struct {
	Line int
	Msg  string
}

func (e *LineError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("line %d: %s", e.Line, e.Msg)
	}
	return e.Msg
}

func (e *LineError) Unwrap() error { return assignments.ErrInvalid }

func lineErr(line int, format string, args ...any) error {
	return &LineError{Line: line, Msg: fmt.Sprintf(format, args...)}
}

// ParseMarkdown reads a job file: a header between two --- lines holding the spec's keys,
// exactly as in JSON, then the objective as plain text. The header is a strict subset of
// YAML: no anchors, aliases, merge keys, tags or second documents, and no duplicate or
// unknown keys. The only shorthand is a tool rule given as a bare mode (gmail_search: allow).
func ParseMarkdown(data []byte) (Spec, error) {
	if len(data) > MaxJobFile {
		return Spec{}, lineErr(0, "the file is larger than %d KB", MaxJobFile>>10)
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return Spec{}, lineErr(1, "a job file starts with a --- line, then the header, then another --- line")
	}
	lines := strings.Split(text, "\n")
	end := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return Spec{}, lineErr(1, "the header has no closing --- line")
	}
	header := strings.Join(lines[1:end], "\n")
	body := strings.TrimSpace(strings.Join(lines[end+1:], "\n"))

	dec := yaml.NewDecoder(strings.NewReader(header))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return Spec{}, yamlError(err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return Spec{}, lineErr(extra.Line+1, "only one header document is allowed")
	} else if !errors.Is(err, io.EOF) {
		return Spec{}, yamlError(err)
	}
	value := map[string]any{}
	if len(doc.Content) > 0 {
		root := doc.Content[0]
		if root.Kind != yaml.MappingNode {
			return Spec{}, lineErr(root.Line+1, "the header must be a set of keys")
		}
		v, err := plain(root, nil)
		if err != nil {
			return Spec{}, err
		}
		value = v.(map[string]any)
	}
	if body != "" {
		if _, ok := value["objective"]; ok {
			return Spec{}, lineErr(keyLine(&doc, "objective"), "the objective is the text after the header; remove the objective key or the text")
		}
		value["objective"] = body
	}
	raw, _ := json.Marshal(value)
	var s Spec
	jd := json.NewDecoder(bytes.NewReader(raw))
	jd.DisallowUnknownFields()
	if err := jd.Decode(&s); err != nil {
		return s, jsonError(&doc, err)
	}
	return s, s.checkModes(&doc)
}

var unknownField = regexp.MustCompile(`unknown field "([^"]+)"`)
var fieldPath = regexp.MustCompile(`Go struct field [A-Za-z]+\.([A-Za-z0-9_.]+) of type (\S+)`)

// jsonError points a decoding error at the header line of the key it is about.
func jsonError(doc *yaml.Node, err error) error {
	msg := err.Error()
	if m := unknownField.FindStringSubmatch(msg); m != nil {
		return lineErr(keyLine(doc, m[1]), "unknown key %q", m[1])
	}
	if m := fieldPath.FindStringSubmatch(msg); m != nil {
		parts := strings.Split(m[1], ".")
		key := parts[len(parts)-1]
		return lineErr(keyLine(doc, key), "%s has the wrong type; it should be %s", m[1], m[2])
	}
	return lineErr(0, "%v", strings.TrimPrefix(msg, "json: "))
}

func yamlError(err error) error {
	msg := strings.TrimPrefix(err.Error(), "yaml: ")
	if m := regexp.MustCompile(`^line (\d+): (.*)$`).FindStringSubmatch(msg); m != nil {
		var n int
		fmt.Sscan(m[1], &n)
		return lineErr(n+1, "%s", m[2])
	}
	return lineErr(0, "%s", msg)
}

// keyLine finds the file line of the first key with this name in the header.
func keyLine(n *yaml.Node, key string) int {
	if n == nil {
		return 0
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == key {
				return n.Content[i].Line + 1
			}
		}
	}
	for _, c := range n.Content {
		if l := keyLine(c, key); l > 0 {
			return l
		}
	}
	return 0
}

// plain turns a node into JSON-shaped values, refusing what the format does not allow.
// path is the chain of keys above it, which places the tool-rule shorthand.
func plain(n *yaml.Node, path []string) (any, error) {
	line := n.Line + 1
	if n.Anchor != "" {
		return nil, lineErr(line, "anchors (&%s) are not allowed", n.Anchor)
	}
	if n.Style&yaml.TaggedStyle != 0 {
		return nil, lineErr(line, "tags (%s) are not allowed", n.Tag)
	}
	switch n.Kind {
	case yaml.AliasNode:
		return nil, lineErr(line, "aliases (*%s) are not allowed", n.Value)
	case yaml.MappingNode:
		out := map[string]any{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Kind != yaml.ScalarNode || k.Tag == "!!merge" || k.Value == "<<" {
				return nil, lineErr(k.Line+1, "only plain keys are allowed")
			}
			if _, dup := out[k.Value]; dup {
				return nil, lineErr(k.Line+1, "key %q appears twice", k.Value)
			}
			child := append(append([]string{}, path...), k.Value)
			if len(path) == 2 && path[0] == "policy" && path[1] == "tools" && v.Kind == yaml.ScalarNode {
				out[k.Value] = map[string]any{"mode": v.Value}
				continue
			}
			val, err := plain(v, child)
			if err != nil {
				return nil, err
			}
			out[k.Value] = val
		}
		return out, nil
	case yaml.SequenceNode:
		out := []any{}
		for _, c := range n.Content {
			val, err := plain(c, path)
			if err != nil {
				return nil, err
			}
			out = append(out, val)
		}
		return out, nil
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!str":
			return n.Value, nil
		case "!!null":
			return nil, nil
		}
		var v any
		if err := n.Decode(&v); err != nil {
			return nil, lineErr(line, "%v", err)
		}
		return v, nil
	}
	return nil, lineErr(line, "unexpected YAML")
}

// checkModes refuses unknown policy modes, with a line when the spec came from a header.
func (s Spec) checkModes(doc *yaml.Node) error {
	valid := func(m policy.Mode) bool { return m == policy.Allow || m == policy.RequireApproval || m == policy.Deny }
	for name, rule := range s.Policy.Tools {
		if !valid(rule.Mode) {
			return lineErr(keyLine(doc, name), "tool %q: mode %q is not one of allow, approval, deny", name, rule.Mode)
		}
		if rule.Otherwise != "" && !valid(rule.Otherwise) {
			return lineErr(keyLine(doc, name), "tool %q: otherwise %q is not one of allow, approval, deny", name, rule.Otherwise)
		}
	}
	return nil
}

// Markdown writes a spec as a job file, the form proposal cards and `agentd export` show.
// It is a rendering: an approval always binds the canonical JSON arguments, never this text.
func (s Spec) Markdown() (string, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	var header map[string]any
	json.Unmarshal(raw, &header)
	delete(header, "objective")
	prune(header)
	if p, ok := header["policy"].(map[string]any); ok {
		if tools, ok := p["tools"].(map[string]any); ok {
			for name, rule := range tools {
				if r, ok := rule.(map[string]any); ok && len(r) == 1 && r["mode"] != nil {
					tools[name] = r["mode"]
				}
			}
		}
	}
	var b strings.Builder
	b.WriteString("---\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(ordered(map[string]any(header))); err != nil {
		return "", err
	}
	enc.Close()
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(s.Objective))
	b.WriteString("\n")
	return b.String(), nil
}

// prune drops empty values so an exported file holds only what was set.
func prune(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			if prune(c) {
				delete(x, k)
			}
		}
		return len(x) == 0
	case []any:
		for _, c := range x {
			prune(c)
		}
		return len(x) == 0
	case string:
		return x == "" || x == "0s"
	case nil:
		return true
	}
	return false
}

// keyOrder is the reading order for keys at any level; others follow alphabetically.
var keyOrder = []string{"kind", "name", "policy", "tools", "mode", "resources", "otherwise", "limits", "max_llm_spend_usd",
	"budget_period", "tool_rate_limits", "models", "completion", "criteria", "id", "title", "description", "every", "grace",
	"source", "match", "coalesce_field", "evidence", "on_miss", "skills", "objective", "notifications", "min_severity",
	"dedup_window", "max_per_day", "schedules", "routes", "obligations"}

var longDuration = regexp.MustCompile(`^(\d+h)?(\d+m)?(\d+s)$`)

// ordered builds the header with keys in reading order and durations written short (24h).
func ordered(v any) *yaml.Node {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		rank := func(k string) int {
			for i, o := range keyOrder {
				if o == k {
					return i
				}
			}
			return len(keyOrder)
		}
		sort.Slice(keys, func(i, j int) bool {
			if rank(keys[i]) != rank(keys[j]) {
				return rank(keys[i]) < rank(keys[j])
			}
			return keys[i] < keys[j]
		})
		n := &yaml.Node{Kind: yaml.MappingNode}
		for _, k := range keys {
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, ordered(x[k]))
		}
		return n
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode}
		for _, c := range x {
			n.Content = append(n.Content, ordered(c))
		}
		return n
	case string:
		if d, err := time.ParseDuration(x); err == nil && longDuration.MatchString(x) {
			v = shortDuration(d)
		}
	}
	var n yaml.Node
	n.Encode(v)
	return &n
}

func shortDuration(d time.Duration) string {
	out := d.String()
	out = strings.TrimSuffix(out, "0s")
	out = strings.TrimSuffix(out, "0m")
	if out == "" {
		return "0s"
	}
	return out
}

// Export reads an assignment back as a spec, with its routes, schedules and obligations as
// they are now, so that creating it again gives an equivalent assignment.
func (a *App) Export(ctx context.Context, id string) (Spec, error) {
	got, err := a.Assignments.Get(ctx, id)
	if err != nil {
		return Spec{}, err
	}
	s := Spec{Kind: got.Kind, Name: got.Name, Objective: got.Objective, Policy: got.Policy, Notifications: got.Notifications}
	if got.Completion != nil {
		s.Completion = *got.Completion
	}
	routes, err := a.Scheduler.Routes(ctx, id)
	if err != nil {
		return s, err
	}
	for _, r := range routes {
		s.Routes = append(s.Routes, RouteSpec{Source: r.Source, Kind: r.Kind, Match: r.Match, CoalesceField: r.CoalesceField,
			Skills: r.Skills, Title: r.Title, Objective: r.Objective})
	}
	schedules, err := a.Scheduler.Schedules(ctx, id)
	if err != nil {
		return s, err
	}
	for _, sc := range schedules {
		s.Schedules = append(s.Schedules, ScheduleSpec{Every: policy.Duration{Duration: sc.Every}, Skills: sc.Skills,
			Title: sc.Title, Objective: sc.Objective})
	}
	obligations, err := watchdog.List(ctx, a.DB, id)
	if err != nil {
		return s, err
	}
	for _, o := range obligations {
		s.Obligations = append(s.Obligations, ObligationSpec{Description: o.Description, Every: policy.Duration{Duration: o.Every},
			Grace: policy.Duration{Duration: o.Grace}, Evidence: o.Evidence, OnMiss: o.OnMiss, Skills: o.Skills})
	}
	return s, nil
}

// ParsePolicy reads a policy file: JSON as `agentd policy` always took, or a job file whose
// header holds only a policy key and no text.
func ParsePolicy(name string, data []byte) (policy.Policy, error) {
	if strings.EqualFold(filepath.Ext(name), ".json") {
		var p policy.Policy
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			return p, fmt.Errorf("%w: %v", assignments.ErrInvalid, err)
		}
		return p, nil
	}
	s, err := ParseJob(name, data)
	if err != nil {
		return s.Policy, err
	}
	if s.Kind != "" || s.Name != "" || s.Objective != "" || s.Notifications != nil || len(s.Routes)+len(s.Schedules)+len(s.Obligations) > 0 ||
		len(s.Completion.Criteria) > 0 {
		return s.Policy, lineErr(0, "a policy file holds only the policy key")
	}
	return s.Policy, nil
}

// Check validates a spec as Create would, without creating anything.
func (a *App) Check(s Spec) error {
	tx, err := a.DB.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = a.CreateTx(context.Background(), tx, s)
	return err
}
