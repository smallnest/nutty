package app

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
)

// The Closing Tab setting's "process" asks before a tab closes only while
// something runs in it. The terminal does not say which process its shell
// is, so this file finds out: the terminal starts its program as a child of
// this process, leading a session of its own on a pseudo-terminal, and such
// a child appears in the process table shortly after the terminal is made.
// A new one is the program of the one terminal waiting for its own; when
// that cannot be told for sure, the terminal's process stays unknown and
// closing its tab asks, as "always" does.

// proc is a row of the process table: a process, its parent, its process
// group, the group in the foreground of its terminal, and the terminal.
type proc struct {
	pid, ppid, pgid, tpgid int
	tty                    string
}

// processTable lists the processes of the system, a seam for tests, which
// start no shells. A second is far more than ps takes; without an answer
// by then the callers do without one.
var processTable = func() ([]proc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,pgid=,tpgid=,tty=").Output()
	if err != nil {
		return nil, err
	}
	return parseProcs(string(out)), nil
}

// parseProcs reads the rows ps printed, leaving out what is not one.
func parseProcs(out string) []proc {
	var rows []proc
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		if len(f) != 5 {
			continue
		}
		var n [4]int
		ok := true
		for i := range n {
			v, err := strconv.Atoi(f[i])
			n[i], ok = v, ok && err == nil
		}
		if ok {
			rows = append(rows, proc{n[0], n[1], n[2], n[3], f[4]})
		}
	}
	return rows
}

const (
	// shellStartWindow is how long after a terminal is made its program
	// may still appear: the terminal starts it with its first frame, or
	// half a second later when no view shows it, so this is ample.
	shellStartWindow = 10 * time.Second
	// shellPollEvery is how often the process table is read while a
	// terminal waits for its program.
	shellPollEvery = 50 * time.Millisecond
)

// shellWatch knows the process of each terminal's program.
type shellWatch struct {
	mu sync.Mutex
	// pids are the programs found, by terminal.
	pids map[*terminal.Terminal]int
	// waiting are the terminals made within shellStartWindow whose
	// program is not found yet.
	waiting map[*terminal.Terminal]*waitingShell
	// seen are the programs of terminals in the last table read, found
	// or not, so each is new once.
	seen    map[int]bool
	polling bool
}

// waitingShell is a terminal waiting for its program. unsure marks one
// whose program can no longer be told from another's: it keeps waiting,
// so that its program, when it appears, is not taken for another
// terminal's.
type waitingShell struct {
	since  time.Time
	unsure bool
}

var shells = &shellWatch{
	pids:    map[*terminal.Terminal]int{},
	waiting: map[*terminal.Terminal]*waitingShell{},
	seen:    map[int]bool{},
}

// watchedTerminal is terminal.New, with the terminal's program watched for.
func watchedTerminal(o terminal.Options) (*terminal.Terminal, error) {
	t, err := terminal.New(o)
	if err == nil && o.Conn == nil {
		shells.watch(t)
	}
	return t, err
}

// watch waits for the program of t, a terminal just made, to appear, and
// forgets the terminal once its program ended.
func (w *shellWatch) watch(t *terminal.Terminal) {
	w.mu.Lock()
	w.waiting[t] = &waitingShell{since: time.Now()}
	if !w.polling {
		w.polling = true
		go w.poll()
	}
	w.mu.Unlock()
	go func() {
		<-t.Done()
		w.mu.Lock()
		delete(w.pids, t)
		// A terminal closed before its program was found may still have
		// started it: it waits on, unsure, so the program is not taken for
		// the next terminal's.
		if ws := w.waiting[t]; ws != nil {
			ws.unsure = true
		}
		w.mu.Unlock()
	}()
}

// poll reads the process table until no terminal waits for its program.
func (w *shellWatch) poll() {
	for {
		rows, err := processTable()
		now := time.Now()
		w.mu.Lock()
		if err == nil {
			w.match(rows, os.Getpid())
		}
		for t, ws := range w.waiting {
			if now.Sub(ws.since) > shellStartWindow {
				delete(w.waiting, t)
			}
		}
		if len(w.waiting) == 0 {
			w.polling = false
			w.mu.Unlock()
			return
		}
		w.mu.Unlock()
		time.Sleep(shellPollEvery)
	}
}

// match gives a program that is new in rows to the terminal waiting for
// one; w.mu is held. Only a single new program with a single terminal
// waiting is certain: anything else leaves the waiting terminals unsure.
func (w *shellWatch) match(rows []proc, self int) {
	// The programs of terminals are the children of this process that
	// lead a session on a terminal of their own: other children, as git
	// or open, have none, or share the one this process was started in.
	ownTTY := ""
	for _, r := range rows {
		if r.pid == self {
			ownTTY = r.tty
		}
	}
	var fresh []int
	seen := map[int]bool{}
	for _, r := range rows {
		if r.ppid != self || r.pid != r.pgid || noTTY(r.tty) || r.tty == ownTTY {
			continue
		}
		seen[r.pid] = true
		if !w.seen[r.pid] {
			fresh = append(fresh, r.pid)
		}
	}
	w.seen = seen
	if len(fresh) == 0 {
		return
	}
	if len(fresh) == 1 && len(w.waiting) == 1 {
		for t, ws := range w.waiting {
			if !ws.unsure {
				w.pids[t] = fresh[0]
				delete(w.waiting, t)
				return
			}
		}
	}
	for _, ws := range w.waiting {
		ws.unsure = true
	}
}

// noTTY reports whether ps printed no terminal for a process.
func noTTY(tty string) bool { return tty == "" || strings.Trim(tty, "?") == "" }

// busy reports whether something runs in any of the terminals besides
// their own programs: a shell with a child process, or one that is not in
// the foreground of its terminal. It is true as well when that cannot be
// told, so the caller asks rather than closes something unseen.
func (w *shellWatch) busy(terms []*terminal.Terminal) bool {
	var pids []int
	w.mu.Lock()
	for _, t := range terms {
		select {
		case <-t.Done():
			continue // its program ended: nothing runs in it
		default:
		}
		pid, ok := w.pids[t]
		if !ok {
			w.mu.Unlock()
			return true
		}
		pids = append(pids, pid)
	}
	w.mu.Unlock()
	if len(pids) == 0 {
		return false
	}
	rows, err := processTable()
	if err != nil {
		return true
	}
	self := os.Getpid()
	for _, pid := range pids {
		for _, r := range rows {
			// The shell itself, still this process's child, with another
			// group in the foreground of its terminal; or a child of it.
			if r.pid == pid && r.ppid == self && r.tpgid != r.pgid || r.ppid == pid {
				return true
			}
		}
	}
	return false
}

// closingAsks reports whether closing the tab s asks first, per the
// Closing Tab setting: always, never, or — "process" — only while
// something runs in the tab besides its shells.
func (a *app) closingAsks(s *session) bool {
	if !a.confirmClosingTab() {
		return false
	}
	if a.settings.ConfirmCloseTab != "process" {
		return true
	}
	var terms []*terminal.Terminal
	for _, p := range s.panes {
		if p.term != nil {
			terms = append(terms, p.term)
		}
	}
	return shells.busy(terms)
}
