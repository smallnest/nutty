package app

import (
	"errors"
	"os"
	"testing"
	"time"

	"nutty/internal/settings"

	"github.com/egoist/mygo/plugins/terminal"
)

// TestParseProcs reads rows as ps prints them, skipping what is not one.
func TestParseProcs(t *testing.T) {
	rows := parseProcs("    1     0     1      0 ??      \n" +
		"72350 72330 72350  72359 ttys012 \n" +
		"ps: not a row\n" +
		"72359 72350 72359  72359 ttys012 \n")
	want := []proc{
		{1, 0, 1, 0, "??"},
		{72350, 72330, 72350, 72359, "ttys012"},
		{72359, 72350, 72359, 72359, "ttys012"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %+v", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
}

// newWatch is an empty shellWatch with the terminals given waiting.
func newWatch(waiting ...*terminal.Terminal) *shellWatch {
	w := &shellWatch{
		pids:    map[*terminal.Terminal]int{},
		waiting: map[*terminal.Terminal]*waitingShell{},
		seen:    map[int]bool{},
	}
	for _, t := range waiting {
		w.waiting[t] = &waitingShell{since: time.Now()}
	}
	return w
}

// The process table of the tests: this process is 100, started in a
// terminal of its own, with a child that is no shell.
const self = 100

var baseTable = []proc{
	{1, 0, 1, 0, "??"},
	{self, 50, self, self, "ttys000"},
	{110, self, 110, 0, "??"},          // a child without a terminal, as open
	{111, self, self, self, "ttys000"}, // a child on this process's terminal, as git
}

// shellRow is a shell on its own terminal, a child of this process.
func shellRow(pid int, tty string) proc { return proc{pid, self, pid, pid, tty} }

// TestShellWatchFindsTheShell checks that the one new session leader on a
// terminal of its own is taken for the program of the one terminal
// waiting, and that other children never are.
func TestShellWatchFindsTheShell(t *testing.T) {
	term := new(terminal.Terminal)
	w := newWatch(term)
	w.match(baseTable, self)
	if len(w.pids) != 0 || len(w.waiting) != 1 {
		t.Fatal("no shell has started: the terminal should still wait")
	}
	w.match(append(baseTable[:len(baseTable):len(baseTable)], shellRow(500, "ttys001")), self)
	if w.pids[term] != 500 {
		t.Fatalf("pid = %d, want 500", w.pids[term])
	}
	if len(w.waiting) != 0 {
		t.Fatal("the terminal should wait no more")
	}
}

// TestShellWatchRefusesToGuess checks that two terminals waiting at once
// are never told apart, also when their shells appear one by one, and that
// neither shell is then taken for a third terminal's.
func TestShellWatchRefusesToGuess(t *testing.T) {
	one, two, three := new(terminal.Terminal), new(terminal.Terminal), new(terminal.Terminal)
	w := newWatch(one, two)
	table := baseTable[:len(baseTable):len(baseTable)]
	table = append(table, shellRow(500, "ttys001"))
	w.match(table, self)
	table = append(table, shellRow(501, "ttys002"))
	w.match(table, self)
	if len(w.pids) != 0 {
		t.Fatalf("two terminals waited at once, yet a shell was given to one: %v", w.pids)
	}
	// A third terminal, made while the two are still within their window,
	// cannot be sure of its shell either.
	w.waiting[three] = &waitingShell{since: time.Now()}
	w.match(append(table, shellRow(502, "ttys003")), self)
	if len(w.pids) != 0 {
		t.Fatalf("a shell was given out while others were unaccounted for: %v", w.pids)
	}

	// A shell nobody waits for is not kept for the next terminal.
	w = newWatch()
	w.match(table, self)
	w.waiting[three] = &waitingShell{since: time.Now()}
	w.match(table, self)
	if len(w.pids) != 0 {
		t.Fatal("a shell already seen was given to a later terminal")
	}
	w.match(append(table, shellRow(502, "ttys003")), self)
	if w.pids[three] != 502 {
		t.Fatalf("pid = %d, want 502", w.pids[three])
	}
}

// processApp makes an app with one tab holding a real terminal, on a
// connection that never ends, whose shell is known as pid 500; table is
// what the process table says next, or tableErr when it fails.
func processApp(t *testing.T, mode string) (a *app, s *session, table *[]proc, tableErr *error) {
	t.Helper()
	term, err := terminal.New(terminal.Options{Conn: &scriptConn{data: []byte("$ "), hold: true}})
	if err != nil {
		t.Fatal(err)
	}
	shells.mu.Lock()
	shells.pids[term] = 500
	shells.mu.Unlock()
	old := processTable
	table, tableErr = new([]proc), new(error)
	processTable = func() ([]proc, error) { return *table, *tableErr }
	t.Cleanup(func() {
		processTable = old
		shells.mu.Lock()
		delete(shells.pids, term)
		shells.mu.Unlock()
		_ = term.Close()
	})
	st := settings.Default()
	st.ConfirmCloseTab = mode
	p := &pane{term: term}
	s = &session{id: 1, dir: "/tmp/nutty-process", root: &node{term: p}, panes: []*pane{p}}
	a = &app{settings: st, sessions: []*session{s}, activePane: p, agent: &demoAgent{}}
	return a, s, table, tableErr
}

// TestClosingTabProcess checks the Closing Tab setting's "process": Close
// Tab asks while something runs in the tab and closes an idle one at once.
// "process" used to ask every time, as "always" does.
func TestClosingTabProcess(t *testing.T) {
	me := os.Getpid()
	shell := proc{500, me, 500, 500, "ttys001"}
	busyShell := proc{500, me, 500, 600, "ttys001"} // another group has the terminal
	for name, c := range map[string]struct {
		mode  string
		table []proc
		err   error
		asks  bool
	}{
		"an idle shell closes at once":       {"process", []proc{shell}, nil, false},
		"a shell that is gone closes":        {"process", []proc{}, nil, false},
		"a foreground job asks":              {"process", []proc{busyShell, {600, 500, 600, 600, "ttys001"}}, nil, true},
		"a background job asks":              {"process", []proc{shell, {700, 500, 700, 500, "ttys001"}}, nil, true},
		"a process table that fails asks":    {"process", nil, errors.New("ps: no answer"), true},
		"always asks about an idle shell":    {"always", []proc{shell}, nil, true},
		"never closes a busy shell at once":  {"never", []proc{busyShell}, nil, false},
		"an unknown setting asks, as before": {"sometimes", []proc{shell}, nil, true},
	} {
		a, s, table, tableErr := processApp(t, c.mode)
		*table, *tableErr = c.table, c.err
		a.closeTab()
		if asked := a.pendingClose == s; asked != c.asks {
			t.Errorf("%s: asked = %v, want %v", name, asked, c.asks)
		}
		if closed := len(a.sessions) == 0; closed == c.asks {
			t.Errorf("%s: closed = %v, want %v", name, closed, !c.asks)
		}
	}
}

// TestClosingTabProcessUnknown checks that "process" asks about a tab
// whose shell was never identified, rather than close something unseen,
// and closes one that has no terminal at all.
func TestClosingTabProcessUnknown(t *testing.T) {
	a, s, table, _ := processApp(t, "process")
	*table = []proc{{500, os.Getpid(), 500, 500, "ttys001"}}
	shells.mu.Lock()
	delete(shells.pids, s.panes[0].term)
	shells.mu.Unlock()
	a.closeTab()
	if a.pendingClose != s {
		t.Fatal("a tab whose shell is unknown should ask before it closes")
	}

	a.pendingClose = nil
	failed := &pane{} // a pane whose terminal could not start
	a.sessions = []*session{{id: 2, root: &node{term: failed}, panes: []*pane{failed}}}
	a.active, a.activePane = 0, failed
	a.closeTab()
	if len(a.sessions) != 0 {
		t.Fatal("a tab without a terminal has nothing running: it should close at once")
	}
}

// TestShellWatchRealShell starts a real shell and checks the whole path:
// the shell is found, an idle one is not busy and one running a command
// is. It starts a process, so it runs only when asked to:
// NUTTY_SHELL_TEST=1 go test -run TestShellWatchRealShell ./internal/app
func TestShellWatchRealShell(t *testing.T) {
	if os.Getenv("NUTTY_SHELL_TEST") == "" {
		t.Skip("set NUTTY_SHELL_TEST=1 to start a real shell")
	}
	term, err := watchedTerminal(terminal.Options{Command: []string{"/bin/sh"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = term.Close() }()
	known := func() bool {
		shells.mu.Lock()
		defer shells.mu.Unlock()
		return shells.pids[term] != 0
	}
	for deadline := time.Now().Add(5 * time.Second); !known(); {
		if time.Now().After(deadline) {
			t.Fatal("the shell was not found")
		}
		time.Sleep(20 * time.Millisecond)
	}
	terms := []*terminal.Terminal{term}
	if shells.busy(terms) {
		t.Fatal("an idle shell is reported busy")
	}
	term.Send([]byte("sleep 30\n"))
	time.Sleep(500 * time.Millisecond)
	if !shells.busy(terms) {
		t.Fatal("a shell running a command is reported idle")
	}
	term.Send([]byte{3}) // Ctrl+C
	term.Send([]byte("sleep 30 &\n"))
	time.Sleep(500 * time.Millisecond)
	if !shells.busy(terms) {
		t.Fatal("a shell with a background job is reported idle")
	}
}
