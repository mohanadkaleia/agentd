package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"agentd/internal/system"
)

func TestStopServerStopsTheRecordedProcess(t *testing.T) {
	w := newChatWorld(t, false)
	if was, err := stopServer(w.ctx, w.db); err != nil || was {
		t.Fatalf("with no server: %v, %v", was, err)
	}
	proc := exec.Command("sleep", "60")
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { proc.Wait(); close(exited) }()
	t.Cleanup(func() { proc.Process.Kill() })
	if err := system.Beat(w.ctx, w.db, system.Liveness{PID: proc.Process.Pid, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := serverCommand(w.ctx, w.db, "stop", nil, &out); err != nil || !strings.Contains(out.String(), "Stopped") {
		t.Fatalf("stop: %q, %v", out.String(), err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the process is still running")
	}
	if _, alive, _ := system.Alive(w.ctx, w.db, time.Now()); alive {
		t.Fatal("the liveness record outlived the process")
	}
	if line := serverLine(w.ctx, w.db); !strings.Contains(line, "not running") {
		t.Fatalf("server line: %s", line)
	}
}

func TestServerLineNamesAnOlderBuild(t *testing.T) {
	w := newChatWorld(t, false)
	l := system.Liveness{PID: os.Getpid(), At: time.Now(), Listen: "127.0.0.1:7777", Exe: buildExe, Build: "older",
		Started: time.Now().Add(-time.Minute), Models: []system.ModelInfo{{ID: "ollama/x"}}}
	if err := system.Beat(w.ctx, w.db, l); err != nil {
		t.Fatal(err)
	}
	line := serverLine(w.ctx, w.db)
	for _, want := range []string{"running", "ollama/x", "http://127.0.0.1:7777", "not the build of this command"} {
		if !strings.Contains(line, want) {
			t.Fatalf("server line lacks %q: %s", want, line)
		}
	}
}

func TestLogsPrintsTheTail(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var out bytes.Buffer
	if err := serverCommand(t.Context(), nil, "logs", nil, &out); err == nil || !strings.Contains(err.Error(), "no log") {
		t.Fatalf("with no log: %v", err)
	}
	os.MkdirAll(stateDir(), 0o700)
	os.WriteFile(filepath.Join(stateDir(), "serve.log"), []byte("one\ntwo\nthree \x1b[2Jfour\n"), 0o600)
	if err := serverCommand(t.Context(), nil, "logs", []string{"-n", "2"}, &out); err != nil || out.String() != "two\nthree four\n" {
		t.Fatalf("logs -n 2: %q, %v", out.String(), err)
	}
}

func TestAMistypedCommandGetsASuggestion(t *testing.T) {
	for typed, want := range map[string]string{"stat": "status", "restrat": "restart", "log": "logs", "aprove": "approve", "zzzzzz": ""} {
		if got := nearestCommand(typed); got != want {
			t.Fatalf("nearest to %q = %q, want %q", typed, got, want)
		}
	}
	if !knownCommand("serve") || knownCommand("bogus") {
		t.Fatal("known commands are wrong")
	}
	for _, c := range commands {
		if !strings.Contains(usage, "\n  "+c) && !strings.Contains(usage, "| "+c) && !strings.Contains(help, c) {
			t.Fatalf("command %q is in neither help text", c)
		}
	}
	if !strings.HasPrefix(version(), "agentd ") {
		t.Fatalf("version: %s", version())
	}
}

func TestNewChatHelpIsAlignedAndFits(t *testing.T) {
	ui := tuiFor(t, 70)
	list := ui.commandList()
	for _, want := range []string{"/approve <n or id>", "/restart", "/quit"} {
		if !strings.Contains(list, want) {
			t.Fatalf("/help lacks %q:\n%s", want, list)
		}
	}
	for _, line := range strings.Split(list, "\n") {
		if lipgloss.Width(line) > 69 {
			t.Fatalf("a /help line is %d wide:\n%s", lipgloss.Width(line), list)
		}
	}
}

// serviceWorld sets up a home directory with the login service installed and a launchctl
// that only records what it was asked.
func serviceWorld(t *testing.T) *[]string {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENTD_DB", "")
	t.Chdir(t.TempDir())
	var calls []string
	loaded := false
	old, oldDB := launchctl, dbFile
	t.Cleanup(func() { launchctl, dbFile = old, oldDB })
	launchctl = func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "print":
			if !loaded {
				return "not found", os.ErrNotExist
			}
		case "bootstrap":
			loaded = true
		case "bootout":
			loaded = false
		}
		return "", nil
	}
	if serviceInstalled() {
		t.Fatal("a fresh home has the service installed")
	}
	if got := defaultDB(); !strings.HasSuffix(got, filepath.Join(".local", "share", "agentd", "agentd.db")) {
		t.Fatalf("default database without the service: %s", got)
	}
	os.MkdirAll(filepath.Dir(servicePlist()), 0o755)
	os.MkdirAll(filepath.Dir(serviceBinary()), 0o755)
	os.WriteFile(servicePlist(), []byte("<plist/>"), 0o644)
	os.WriteFile(serviceDB(), nil, 0o600)
	return &calls
}

func TestTheLoginServiceIsTheOneAgent(t *testing.T) {
	calls := serviceWorld(t)
	if got := defaultDB(); got != serviceDB() {
		t.Fatalf("default database with the service installed: %s", got)
	}
	dbFile = filepath.Join(t.TempDir(), "other.db")
	os.WriteFile(dbFile, nil, 0o600)
	if serviceManaged() {
		t.Fatal("another database counts as the service's")
	}
	dbFile = serviceDB()
	if !serviceManaged() {
		t.Fatal("the service's own database does not count as managed")
	}

	// Starting gives the service this build, then loads it; nothing else is run.
	old := time.Now().Add(-time.Hour)
	os.WriteFile(serviceBinary(), []byte("old build"), 0o755)
	os.Chtimes(serviceBinary(), old, old)
	l, err := startServer(t.Context(), nil, "")
	if err != nil || l.log != serviceLog() {
		t.Fatalf("start: %+v, %v", l, err)
	}
	if fileMark(serviceBinary()) != buildMark() {
		t.Fatal("the service was not given this build")
	}
	if last := (*calls)[len(*calls)-1]; !strings.HasPrefix(last, "bootstrap gui/") || !strings.HasSuffix(last, servicePlist()) {
		t.Fatalf("launchctl calls: %v", *calls)
	}
	// Loaded but not serving: it is kicked, not loaded twice.
	if _, err := startServer(t.Context(), nil, ""); err != nil || !strings.HasPrefix((*calls)[len(*calls)-1], "kickstart gui/") {
		t.Fatalf("second start: %v, %v", *calls, err)
	}
}

func TestStoppingTheLoginServiceUnloadsIt(t *testing.T) {
	calls := serviceWorld(t)
	w := newChatWorld(t, false)
	dbFile = serviceDB()
	launchctl("bootstrap")
	proc := exec.Command("sleep", "60")
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	go proc.Wait()
	t.Cleanup(func() { proc.Process.Kill() })
	if err := system.Beat(w.ctx, w.db, system.Liveness{PID: proc.Process.Pid, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// launchd, not a signal from here, ends the process: stand in for it.
	real := launchctl
	launchctl = func(args ...string) (string, error) {
		if args[0] == "bootout" {
			proc.Process.Kill()
		}
		return real(args...)
	}
	var out bytes.Buffer
	if err := serverCommand(w.ctx, w.db, "stop", nil, &out); err != nil || !strings.Contains(out.String(), "login service") {
		t.Fatalf("stop: %q, %v", out.String(), err)
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "bootout gui/") {
		t.Fatalf("the service was not unloaded: %v", *calls)
	}
}

func TestServeRefusesASecondServerOnOneDatabase(t *testing.T) {
	// The guard is in run(); here its condition: a live record whose process exists.
	w := newChatWorld(t, false)
	if err := system.Beat(w.ctx, w.db, system.Liveness{PID: os.Getppid(), At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	l, alive, _ := system.Alive(w.ctx, w.db, time.Now())
	proc, _ := os.FindProcess(l.PID)
	if !alive || proc.Signal(syscall.Signal(0)) != nil {
		t.Fatal("a live server with a running process is not seen")
	}
}
