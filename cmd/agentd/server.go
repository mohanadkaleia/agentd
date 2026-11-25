package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"agentd/internal/system"
)

// Starting, stopping and reading the log of the background server, for the chat and for
// the stop, restart and logs commands.

const (
	serverListen   = "127.0.0.1:7777"
	serverStopWait = 20 * time.Second
)

// serveCommand is this executable with the same global flags and the serve command; it is
// run to start the server. serveHasModel says whether those flags name a model.
var (
	serveCommand  []string
	serveHasModel bool
)

// dbFile is the database in use.
var dbFile string

// errNoModel means no server can be started until the owner picks a model.
var errNoModel = errors.New("no model is set")

// buildExe is this executable. buildMark identifies its contents, so that a server
// running another build can be told apart from this one wherever its copy is kept.
var buildExe = func() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return exe
}()

var buildMark = sync.OnceValue(func() string { return fileMark(buildExe) })

func fileMark(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// otherBuild reports whether a running server is a different build from this command.
// A server that recorded no build predates the record, so it is older.
func otherBuild(l system.Liveness) bool {
	return l.Build != buildMark() && buildMark() != ""
}

// version describes this build in one line.
func version() string {
	rev, at, dirty := "development build", "", ""
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value[:min(len(s.Value), 12)]
			case "vcs.time":
				if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
					at = ", " + t.Local().Format("2006-01-02")
				}
			case "vcs.modified":
				if s.Value == "true" {
					dirty = " with local changes"
				}
			}
		}
	}
	return "agentd " + rev + dirty + at
}

// launched is a server just started in the background.
type launched struct {
	log string
	// from is where this start's output begins in the log.
	from int64
	// done is closed when the process exits.
	done chan struct{}
}

// failure says why a server that exited did not start, from what it wrote to its log.
func (l *launched) failure() string {
	f, err := os.Open(l.log)
	if err != nil {
		return ""
	}
	defer f.Close()
	f.Seek(l.from, io.SeekStart)
	last := ""
	lines := bufio.NewScanner(io.LimitReader(f, 1<<20))
	for lines.Scan() {
		if line := strings.TrimSpace(lines.Text()); line != "" && !strings.HasPrefix(line, "usage:") {
			last = line
		}
	}
	return sanitize(strings.TrimPrefix(last, "agentd: "))
}

func (l *launched) exited() bool {
	select {
	case <-l.done:
		return true
	default:
		return false
	}
}

func serverLog() string { return filepath.Join(stateDir(), "serve.log") }

// startServer runs `agentd serve` in the background, detached from this terminal: with
// the model just picked, else with this command's own model flags, else the way serve
// last ran. Without any of those it returns errNoModel.
func startServer(ctx context.Context, db *sql.DB, model string) (*launched, error) {
	if serviceManaged() {
		return startService()
	}
	if len(serveCommand) == 0 || stateDir() == "" {
		return nil, errors.New("cannot start the server from here; run `agentd serve` in another terminal")
	}
	args, dir := append([]string{}, serveCommand[1:]...), ""
	switch {
	case model != "":
		args = append(append(args[:len(args)-1], "-model", model), "serve")
		args = withListen(args)
	case serveHasModel:
		args = withListen(args)
	default:
		last, ok, err := system.LastLaunch(ctx, db)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errNoModel
		}
		args, dir = last.Args, last.Dir
	}
	args = withDB(args)
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return nil, err
	}
	log, err := os.OpenFile(serverLog(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	l := &launched{log: serverLog(), done: make(chan struct{})}
	if st, err := log.Stat(); err == nil {
		l.from = st.Size()
	}
	cmd := exec.Command(serveCommand[0], args...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		cmd.Wait()
		close(l.done)
	}()
	return l, nil
}

// withDB names the database in use, by its full path, in a server's arguments. The
// default database depends on the folder and on what is installed, so a server started
// from here is never left to work it out again.
func withDB(args []string) []string {
	path, err := filepath.Abs(dbFile)
	if dbFile == "" || err != nil {
		return args
	}
	out := []string{"-db", path}
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case (a == "-db" || a == "--db") && i+1 < len(args):
			i++
		case strings.HasPrefix(a, "-db=") || strings.HasPrefix(a, "--db="):
		default:
			out = append(out, a)
		}
	}
	return out
}

// withListen adds the local API address, which streaming and the web page need, when the
// address is free.
func withListen(args []string) []string {
	l, err := net.Listen("tcp", serverListen)
	if err != nil {
		return args
	}
	l.Close()
	return append(args, "-listen", serverListen)
}

// awaitServer waits until the server just started is serving, and says why when it is not.
func awaitServer(ctx context.Context, db *sql.DB, l *launched, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		if _, alive, err := system.Alive(ctx, db, time.Now()); err != nil {
			return err
		} else if alive {
			return nil
		}
		if l.exited() {
			return startFailure(l)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the server is still not up after %s; its log is %s", limit, l.log)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func startFailure(l *launched) error {
	if why := l.failure(); why != "" {
		return fmt.Errorf("the server could not start: %s\n(log: %s)", why, l.log)
	}
	return fmt.Errorf("the server could not start; its log is %s", l.log)
}

// stopServer asks the running server to stop and waits until it has. It reports false
// when none was running.
func stopServer(ctx context.Context, db *sql.DB) (bool, error) {
	if serviceManaged() && serviceLoaded() {
		return true, stopService(ctx, db)
	}
	l, alive, err := system.Alive(ctx, db, time.Now())
	if err != nil || !alive {
		return false, err
	}
	proc, err := os.FindProcess(l.PID)
	if err != nil {
		return false, err
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return false, system.Stopped(ctx, db)
		}
		return false, fmt.Errorf("cannot stop the server (pid %d): %w; it may belong to another account", l.PID, err)
	}
	deadline := time.Now().Add(serverStopWait)
	for time.Now().Before(deadline) {
		if now, alive, err := system.Alive(ctx, db, time.Now()); err != nil {
			return true, err
		} else if !alive || now.PID != l.PID {
			return true, nil
		}
		// A server that was killed leaves its record behind; the process is what counts.
		if proc.Signal(syscall.Signal(0)) != nil {
			return true, system.Stopped(ctx, db)
		}
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	return true, fmt.Errorf("the server (pid %d) did not stop within %s", l.PID, serverStopWait)
}

// serverCommand runs agentd stop, restart and logs.
func serverCommand(ctx context.Context, db *sql.DB, name string, args []string, out io.Writer) error {
	switch name {
	case "stop":
		was, err := stopServer(ctx, db)
		if err != nil {
			return err
		}
		if !was {
			fmt.Fprintln(out, "No server was running.")
			return nil
		}
		fmt.Fprintln(out, "Stopped. Jobs wait until agentd runs again; nothing is lost.")
		if serviceManaged() {
			fmt.Fprintln(out, "It runs as a login service: it starts again when you log in, or with `agentd start`.")
		}
		return nil
	case "restart", "start":
		if name == "start" {
			if l, alive, err := system.Alive(ctx, db, time.Now()); err != nil {
				return err
			} else if alive {
				fmt.Fprintf(out, "Already running (pid %d).\n", l.PID)
				return nil
			}
		} else if _, err := stopServer(ctx, db); err != nil {
			return err
		}
		// A service manager may have started it again already.
		time.Sleep(300 * time.Millisecond)
		if _, alive, _ := system.Alive(ctx, db, time.Now()); !alive {
			l, err := startServer(ctx, db, "")
			if errors.Is(err, errNoModel) {
				return errors.New("no model is set yet: run agentd once to pick one, or name it: agentd -model ollama/qwen2.5:7b " + name)
			}
			if err != nil {
				return err
			}
			if err := awaitServer(ctx, db, l, time.Minute); err != nil {
				return err
			}
		}
		fmt.Fprintln(out, serverLine(ctx, db))
		return nil
	case "logs":
		f := flag.NewFlagSet("logs", flag.ContinueOnError)
		follow := f.Bool("f", false, "keep printing as the server writes")
		n := f.Int("n", 40, "lines to print")
		if err := f.Parse(args); err != nil {
			return &exitError{code: 1, msg: err.Error()}
		}
		path := serverLog()
		if serviceManaged() {
			path = serviceLog()
		}
		return tailLog(ctx, path, *n, *follow, out)
	}
	return fmt.Errorf("unknown command %q", name)
}

// serverLine is one line about the server: running or not, and how.
func serverLine(ctx context.Context, db *sql.DB) string {
	l, alive, err := system.Alive(ctx, db, time.Now())
	if err != nil {
		return "Server: " + err.Error()
	}
	if !alive {
		return "Server: not running. `agentd` or `agentd start` starts it."
	}
	parts := []string{fmt.Sprintf("Server: running, pid %d", l.PID)}
	if len(l.Models) > 0 {
		parts = append(parts, "model "+l.Models[0].ID)
	}
	if l.Listen != "" {
		parts = append(parts, "http://"+l.Listen)
	}
	if !l.Started.IsZero() {
		parts = append(parts, "up "+time.Since(l.Started).Round(time.Second).String())
	}
	if serviceManaged() {
		parts = append(parts, "login service")
	}
	if otherBuild(l) {
		parts = append(parts, "not the build of this command (agentd restart loads this one)")
	}
	return strings.Join(parts, ", ")
}

// tailLog prints the last n lines of the server's log, and what follows when asked.
func tailLog(ctx context.Context, path string, n int, follow bool, out io.Writer) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no log at %s: the log exists once agentd has started a server itself; a server you run with `agentd serve` writes to its own terminal", path)
	}
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	const window = 256 << 10
	if st.Size() > window {
		f.Seek(-window, io.SeekEnd)
	}
	body, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for _, line := range lines {
		fmt.Fprintln(out, sanitize(line))
	}
	if !follow {
		return nil
	}
	r := bufio.NewReader(f)
	for ctx.Err() == nil {
		line, err := r.ReadString('\n')
		if line != "" {
			fmt.Fprint(out, sanitize(strings.TrimRight(line, "\n"))+"\n")
		}
		if err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(300 * time.Millisecond):
			}
		}
	}
	return nil
}

// The login service: on a Mac, deploy/install.sh installs agentd as a launchd agent that
// starts at login and is kept running. When the database in use is that service's, the
// service is the server: start, stop and restart act on it instead of running a second one.

const serviceLabel = "local.agentd"

func serviceHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "agentd")
}

func serviceDB() string     { return filepath.Join(serviceHome(), "agentd.db") }
func serviceBinary() string { return filepath.Join(serviceHome(), "bin", "agentd") }
func serviceTarget() string { return fmt.Sprintf("gui/%d/%s", os.Getuid(), serviceLabel) }

func servicePlist() string {
	return filepath.Join(filepath.Dir(filepath.Dir(serviceHome())), "LaunchAgents", serviceLabel+".plist")
}

func serviceLog() string {
	return filepath.Join(filepath.Dir(filepath.Dir(serviceHome())), "Logs", "agentd", "agentd.log")
}

// serviceInstalled reports whether the login service is set up for this account.
func serviceInstalled() bool {
	if serviceHome() == "" {
		return false
	}
	_, plist := os.Stat(servicePlist())
	_, db := os.Stat(serviceDB())
	return plist == nil && db == nil
}

// serviceManaged reports whether the database in use belongs to the login service.
func serviceManaged() bool {
	if dbFile == "" || !serviceInstalled() {
		return false
	}
	mine, err := os.Stat(dbFile)
	if err != nil {
		return false
	}
	theirs, err := os.Stat(serviceDB())
	return err == nil && os.SameFile(mine, theirs)
}

var launchctl = func(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func serviceLoaded() bool {
	_, err := launchctl("print", serviceTarget())
	return err == nil
}

// stopService unloads the login service and waits until its process is gone. launchd
// starts a server that merely exits again, so the service itself has to be unloaded; it
// loads again at the next login or with startService.
func stopService(ctx context.Context, db *sql.DB) error {
	if out, err := launchctl("bootout", serviceTarget()); err != nil && serviceLoaded() {
		return fmt.Errorf("cannot stop the login service: %s", out)
	}
	deadline := time.Now().Add(2 * serverStopWait)
	for serviceLoaded() {
		if time.Now().After(deadline) {
			return fmt.Errorf("the login service did not stop within %s", 2*serverStopWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	// A server from before liveness was recorded, or one that was killed, leaves no clean record.
	return system.Stopped(ctx, db)
}

// startService loads the login service, first giving it this build when this one is newer.
func startService() (*launched, error) {
	l := &launched{log: serviceLog(), done: make(chan struct{})}
	if st, err := os.Stat(l.log); err == nil {
		l.from = st.Size()
	}
	if serviceLoaded() {
		// Loaded but not serving: launchd is between restarts, or was told to wait.
		if out, err := launchctl("kickstart", serviceTarget()); err != nil {
			return nil, fmt.Errorf("cannot start the login service: %s", out)
		}
		return l, nil
	}
	if err := serviceUpdate(); err != nil {
		return nil, err
	}
	if out, err := launchctl("bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), servicePlist()); err != nil {
		return nil, fmt.Errorf("cannot start the login service: %s", out)
	}
	return l, nil
}

// serviceUpdate replaces the service's copy of agentd with this executable when this one
// is newer and differs. It is only called while the service is stopped.
func serviceUpdate() error {
	mine, err := os.Stat(buildExe)
	if err != nil {
		return nil
	}
	theirs, err := os.Stat(serviceBinary())
	if err == nil && (os.SameFile(mine, theirs) || !mine.ModTime().After(theirs.ModTime()) || fileMark(serviceBinary()) == buildMark()) {
		return nil
	}
	src, err := os.Open(buildExe)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(serviceBinary()), 0o755); err != nil {
		return err
	}
	tmp := serviceBinary() + ".new"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(tmp)
		return err
	}
	if err := dst.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, serviceBinary())
}
