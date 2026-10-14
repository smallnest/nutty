package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"nutty/internal/settings"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
	"github.com/smallnest/uix/components/split"
)

// session is one tab: a running shell, split into panes.
type session struct {
	id      int
	name    string // the user's name; empty follows the shell's title
	title   string // the latest title the shell set
	dir     string
	started time.Time // when the shell started, for the info panel
	root    *node
	panes   []*pane
}

// displayTitle is the session's title: the user's name when set, else the
// shell's title, else the shell's name. The tabs show tabLabel instead.
func (s *session) displayTitle() string {
	if s.name != "" {
		return s.name
	}
	if s.title != "" {
		return s.title
	}
	return shellName()
}

// tabLabel is what the rail and the tab bar draw on a tab: the user's
// name when the tab was renamed, else the directory the shell runs in.
// A renamed tab keeps its name when the shell changes directory, as
// otty pins the name the user gives a tab.
func tabLabel(s *session) string {
	if s.name != "" {
		return s.name
	}
	return homeShown(s.dir)
}

// node is the pane tree of a session: a terminal (leaf) or a split.
type node struct {
	term  *pane
	split *splitNode
}

// splitNode is an internal node: two children with a divider.
type splitNode struct {
	vertical bool
	size     *float32
	room     float32 // what the two panes shared in the last frame
	a, b     *node
}

// pane is a terminal with the handle that gives it focus.
type pane struct {
	term   *terminal.Terminal
	err    error // why there is no terminal, for the pane to say
	handle ui.Handle
	dead   bool
	w, h   float32 // the pane's size in the last frame, for a split to halve
}

// newTerminal is terminal.New, a seam for tests that must not start a
// shell. The shell it starts is watched for (see shellprocs.go).
var newTerminal = watchedTerminal

var sessionIDs int

// newSession opens a session in dir, starting a shell there.
func (a *app) newSession(dir string) *session {
	sessionIDs++
	s := &session{id: sessionIDs, dir: dir, started: time.Now()}
	// A tab opened without a directory starts where the default profile
	// says, else at home. The session takes the directory here, not the
	// terminal in newTerm, so the tab, the title and the info panel show
	// where the shell really starts, and Duplicate Tab and a split, which
	// pass the session's directory on, are not sent back to the profile's.
	if pr := a.settings.Profile(); s.dir == "" && pr != nil {
		s.dir = expandHome(pr.CWD)
	}
	if s.dir == "" {
		s.dir = a.startDir()
	}
	p := &pane{}
	p.term = a.newTerm(s, p)
	s.root = &node{term: p}
	s.panes = []*pane{p}
	// Auto and after-current open the new tab beside the active one, as
	// otty's New Tab Position setting says; End appends it.
	if (a.settings.NewTabPosition == "auto" || a.settings.NewTabPosition == "after-current") &&
		a.active < len(a.sessions) {
		a.sessions = append(a.sessions[:a.active+1],
			append([]*session{s}, a.sessions[a.active+1:]...)...)
	} else {
		a.sessions = append(a.sessions, s)
	}
	a.activate(s)
	return s
}

// expandHome writes out the "~" a directory typed into the settings starts
// with. The shell is started in the directory as given, with no shell
// before it to expand the "~": left in, the start fails for want of a
// directory of that name.
func expandHome(dir string) string {
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		return filepath.Join(homeDir(), dir[1:])
	}
	return dir
}

// newTerm starts a terminal running a shell in the session's directory.
// Its callbacks run on the terminal's goroutine, so they cross to the UI
// thread before touching app state. p is the pane that shows the
// terminal, whose dead flag the exit callback sets.
func (a *app) newTerm(s *session, p *pane) *terminal.Terminal {
	theme, darkTheme := a.termThemes()
	dir := s.dir
	command := a.settings.Command
	env := a.settings.EnvList()
	// The default profile overrides what new sessions run, as otty's
	// profiles do; where they start is newSession's, and their themes
	// termThemes's, which the panes already open follow too.
	if pr := a.settings.Profile(); pr != nil {
		if pr.Command != "" {
			command = pr.Command
		}
		env = append(append([]string{}, env...), settings.ValidEnv(pr.Env)...)
	}
	term, err := newTerminal(terminal.Options{
		Command:     settings.ShellArgs(command),
		Dir:         dir,
		Env:         a.shellEnv(command, env),
		Font:        a.termFont(),
		Theme:       theme,
		DarkTheme:   darkTheme,
		Cursor:      a.cursorStyle(),
		NoBlink:     a.settings.CursorBlinkStyle == "default_off" || a.settings.CursorBlinkStyle == "always_off",
		Scrollback:  a.scrollbackBytes(),
		OptionAsAlt: a.optionAsAlt(),
		Transparent: a.settings.TerminalOpacity < 1,
		OnTitle: func(title string) {
			a.win.Update(func() {
				s.title = title
			})
		},
		OnExit: func(code int) {
			a.win.Update(func() {
				a.paneExited(p)
			})
		},
		OpenLink: func(link string) bool { return a.openLink(p, link) },
	})
	if err != nil {
		// Without its library, or the program the settings name, the
		// terminal cannot run; show the pane as closed, with the reason,
		// rather than crash.
		p.err = err
		return nil
	}
	a.runAfter(term)
	return term
}

// renderNode draws the pane tree of a session.
func (a *app) renderNode(c *ui.Context, n *node) {
	if n.split != nil {
		sn := n.split
		// The divider keeps its place in DIPs and the split clamps it only
		// while it is dragged, so a window made narrower, or a panel opened
		// beside the panes, would push the second pane out of its room:
		// keep the divider inside the room the split had.
		if sn.room > 0 {
			*sn.size = max(splitMinPane, min(*sn.size, sn.room-1-splitMinPane))
		}
		r := split.Split(c, split.Props{
			Size:     sn.size,
			Vertical: sn.vertical,
			First:    func(c *ui.Context) { a.renderNode(c, sn.a) },
			Second:   func(c *ui.Context) { a.renderNode(c, sn.b) },
		}).Fill().Bounds()
		sn.room = r.W
		if sn.vertical {
			sn.room = r.H
		}
		return
	}
	a.renderPane(c, n.term)
	// Remember the pane's size, which a split of it halves (see splitPane).
	if r := c.Resolve(n.term.handle).Bounds(); r.W > 0 && r.H > 0 {
		n.term.w, n.term.h = r.W, r.H
	}
}

// splitMinPane is the least a split leaves either pane, as the divider of
// the Split widget does when dragged.
const splitMinPane = 40

// renderPane draws one terminal, filling the pane with no ring around it —
// otty draws none, and a ring around a terminal reads as a border.
func (a *app) renderPane(c *ui.Context, p *pane) {
	// The pane with the focus is the active one, unless a command just
	// chose another (focusPane): that one takes the focus as it draws, and
	// the pane the focus is leaving does not claim activePane meanwhile.
	chosen := a.focusActive && p == a.activePane
	if chosen {
		a.focusActive = false
	} else if !a.focusActive && p.handle.Focused(c) {
		a.activePane = p
	}
	ui.Box(c).Fill().Clip().Children(func() {
		if p.term == nil {
			t := c.Theme()
			ui.Column(c).Fill().Center().Gap(8).Children(func() {
				ui.Text(c, tr("Terminal unavailable")).TextColor(t.TextMuted)
				// The reason is the terminal's own: the library that did
				// not load, or the command that was not found.
				why := tr("the terminal library could not be loaded")
				if p.err != nil {
					why = p.err.Error()
				}
				ui.Text(c, why).TextColor(t.TextMuted).FontSize(11)
			})
			return
		}
		tv := terminal.View(c, p.term)
		if chosen {
			tv.Focus()
		} else if p == a.activePane && a.panel == panelNone && !a.paletteOpen {
			tv.AutoFocus()
		}
		tv.Fill().Bind(&p.handle)
	})
}

// splitPane splits p with a new pane beside it and focuses the new pane.
func (a *app) splitPane(p *pane, vertical bool) {
	if p == nil {
		p = a.activePane
		if p == nil {
			return
		}
	}
	s := a.sessionOf(p)
	if s == nil {
		return
	}
	np := &pane{}
	np.term = a.newTerm(s, np)
	// Half of the pane being split, not of the window: the window's half is
	// wider than a pane that is already one side of a split, which left the
	// new pane no room at all.
	size := a.defaultSplitSize(vertical)
	if vertical && p.h > 0 {
		size = float32(int(p.h / 2))
	} else if !vertical && p.w > 0 {
		size = float32(int(p.w / 2))
	}
	split := &node{split: &splitNode{
		vertical: vertical,
		size:     &size,
		a:        nil,
		b:        &node{term: np},
	}}
	if s.root.term == p {
		split.split.a = s.root
		s.root = split
	} else {
		s.replaceChild(p, func(n *node) *node {
			split.split.a = n
			return split
		})
	}
	s.panes = append(s.panes, np)
	a.focusPane(np)
}

// defaultSplitSize puts the divider near the middle of the window.
func (a *app) defaultSplitSize(vertical bool) float32 {
	if a.win == nil {
		return 300
	}
	w, h := a.win.ContentSize()
	if vertical {
		if h > 80 {
			return float32(h)/2 - 20
		}
		return 200
	}
	if w > 80 {
		return float32(w)/2 - 20
	}
	return 300
}

// replaceChild replaces the node holding p (as a direct child of a split)
// with the result of fn.
func (s *session) replaceChild(p *pane, fn func(*node) *node) bool {
	var rec func(n *node) bool
	rec = func(n *node) bool {
		if n.split == nil {
			return false
		}
		if n.split.a.term == p {
			n.split.a = fn(n.split.a)
			return true
		}
		if n.split.b.term == p {
			n.split.b = fn(n.split.b)
			return true
		}
		return rec(n.split.a) || rec(n.split.b)
	}
	return rec(s.root)
}

// paneExited closes the pane whose shell ended, as typing exit in a
// terminal closes it: its split collapses to the sibling, and the tab
// closes with its last pane. A pane left in the tree showed "[Process
// exited]" for as long as the tab lived, with no command to close it, and
// stopped the focus from cycling past it.
func (a *app) paneExited(p *pane) {
	p.dead = true
	a.closePane(p)
}

// closePane closes p, collapsing its split to the sibling, or closing the
// whole session when it is the last pane.
func (a *app) closePane(p *pane) {
	if p == nil {
		return
	}
	s := a.sessionOf(p)
	if s == nil {
		return
	}
	if len(s.panes) == 1 {
		a.closeSession(s)
		return
	}
	s.removePane(p)
	if p.term != nil {
		_ = p.term.Close()
	}
	if a.activePane == p {
		// The focus was in the pane that went: give it to the one taking
		// its place, or nothing typed reaches a shell until a click.
		a.focusPane(s.panes[0])
	}
}

// removePane removes p from the tree, collapsing its split to the sibling.
func (s *session) removePane(p *pane) {
	var rec func(n *node) bool
	rec = func(n *node) bool {
		if n.split == nil {
			return false
		}
		if n.split.a.term == p {
			*n = *n.split.b
			return true
		}
		if n.split.b.term == p {
			*n = *n.split.a
			return true
		}
		return rec(n.split.a) || rec(n.split.b)
	}
	rec(s.root)
	s.panes = collectPanes(s.root)
}

// orderedPanes lists the panes left to right, top to bottom, for focus
// navigation.
func (s *session) orderedPanes() []*pane {
	return collectPanes(s.root)
}

func collectPanes(n *node) []*pane {
	if n == nil {
		return nil
	}
	if n.term != nil {
		return []*pane{n.term}
	}
	return append(collectPanes(n.split.a), collectPanes(n.split.b)...)
}

// openLink opens a file path the terminal printed, reporting whether it
// did; URLs open with the system on their own.
func (a *app) openLink(p *pane, link string) bool {
	// A relative path is the program's: it starts at the directory the
	// pane's shell reported, or the session's when it reported none.
	dir := ""
	if p != nil && p.term != nil {
		dir = p.term.Dir()
	}
	if s := a.sessionOf(p); s != nil && dir == "" {
		dir = s.dir
	}
	path := linkPath(link, dir)
	if path == "" {
		return false
	}
	openFile(path)
	return true
}

// openFile opens a file with the system, a seam for tests that must not.
// It waits for the child, which a bare Start would leave a zombie for as
// long as the app runs.
var openFile = func(path string) {
	go func() { _ = exec.Command("open", path).Run() }()
}

// lineSuffix is the :line or :line:col a compiler or grep prints after a
// file's name.
var lineSuffix = regexp.MustCompile(`:\d+(?::\d+)?$`)

// linkPath is the file a path the terminal printed names, or "" when
// there is none. The terminal passes the path as printed: from ~, relative
// to the program's directory dir, and with a :line:col suffix included, so
// each is resolved before the file is looked for; the app's own working
// directory is the wrong start for a relative one.
func linkPath(link, dir string) string {
	link = strings.TrimPrefix(link, "file://")
	// As printed first, for the rare file whose name ends in :digits.
	for _, name := range []string{link, lineSuffix.ReplaceAllString(link, "")} {
		switch {
		case name == "~" || strings.HasPrefix(name, "~/"):
			name = filepath.Join(homeDir(), name[1:])
		case !filepath.IsAbs(name):
			name = filepath.Join(dir, name)
		}
		if _, err := os.Stat(name); err == nil {
			return name
		}
	}
	return ""
}

func homeDir() string {
	d, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return d
}

func shellName() string {
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/zsh"
	}
	return filepath.Base(sh)
}

// sessionID is the sidebar's selection id of a session.
func sessionID(id int) string {
	return "session:" + strconv.Itoa(id)
}
