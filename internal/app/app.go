package app

import (
	"slices"
	"sync"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/smallnest/uix/components/command"

	"nutty/internal/settings"
)

// panelMode is the panel on the right of the window: the agent chat or
// the info panel, one at a time, or none.
type panelMode int

const (
	panelNone panelMode = iota
	panelInfo
	panelChat
)

// app is one window's state: its sessions, chrome and agent chat. Each
// window owns its own app; the menu dispatches to the focused window's.
type app struct {
	win *mygo.Window

	sessions   []*session
	active     int
	activePane *pane
	// focusActive asks the next frame to give activePane the keyboard
	// focus: a command chose the pane (focusPane), and the pane that had
	// the focus until then must not take activePane back as it draws.
	focusActive bool

	sideOpen bool
	panel    panelMode
	info     infoState

	paletteOpen bool
	palette     command.State

	rename renameState

	settings     *settings.Settings
	settingsWin  *mygo.Window // the single settings window, when open
	winSizeW     int          // the window size the settings last applied
	winSizeH     int
	pendingClose *session // the tab awaiting the confirm dialog, or nil

	chat  chatState
	agent Agent
}

// renameState is the small "Rename Tab" dialog.
type renameState struct {
	open   bool
	buf    string
	target *session
	input  ui.Handle
}

var (
	appsMu sync.Mutex
	apps   = map[int]*app{} // window ID -> app
)

// shared is the one set of settings of the process, loaded on first use.
var shared *settings.Settings

// sharedSettings is the settings every window and the menu use. A copy per
// window let them drift apart: a change made in one window never reached
// the others, and the next save from any of them wrote its stale copy over
// the change. It is called on the UI thread only.
func sharedSettings() *settings.Settings {
	if shared == nil {
		shared = settings.Load()
	}
	return shared
}

// newWindow opens a window with one session in it. The title bar is
// hidden so the view draws its own under the traffic lights, as otty
// does.
func newWindow() *app {
	s := sharedSettings()
	a := &app{
		settings: s,
		sideOpen: s.SidebarVisible,
		agent:    &demoAgent{},
	}
	a.winSizeW, a.winSizeH = int(a.settings.WindowWidth), int(a.settings.WindowHeight)
	a.win = mygo.NewWindow(mygo.WindowOptions{
		Title:                "Nutty",
		Width:                int(a.settings.WindowWidth),
		Height:               int(a.settings.WindowHeight),
		Opacity:              a.settings.WindowOpacity,
		MinWidth:             680,
		MinHeight:            440,
		TitleBarStyle:        mygo.TitleBarHiddenInset,
		TrafficLightPosition: &mygo.Point{X: 14, Y: 7},
		Content:              ui.View(a.view),
	})
	appsMu.Lock()
	apps[a.win.ID()] = a
	appsMu.Unlock()
	a.win.OnClosed(func() {
		for _, s := range a.sessions {
			for _, p := range s.panes {
				if p.term != nil {
					_ = p.term.Close()
				}
			}
		}
		a.agent.Stop()
		appsMu.Lock()
		delete(apps, a.win.ID())
		appsMu.Unlock()
	})
	// A change of appearance re-applies the terminals' colors, so the
	// "follow system" terminals keep blending with the strip. The listener
	// holds the app, so it ends with the window: one left behind kept a
	// closed window's sessions reachable and restyled them on each change.
	a.win.OnClosed(mygo.Theme.OnUpdated(func() { a.applyTheme() }))
	a.newSession("")
	return a
}

// appOf is the app of the window, or nil.
func appOf(win *mygo.Window) *app {
	if win == nil {
		return nil
	}
	appsMu.Lock()
	defer appsMu.Unlock()
	return apps[win.ID()]
}

// currentSession is the active session, or nil.
func (a *app) currentSession() *session {
	if a.active < 0 || a.active >= len(a.sessions) {
		return nil
	}
	return a.sessions[a.active]
}

func (a *app) sessionOf(p *pane) *session {
	for _, s := range a.sessions {
		for _, x := range s.panes {
			if x == p {
				return s
			}
		}
	}
	return nil
}

// activate makes s the active session: its first pane takes the focus.
func (a *app) activate(s *session) {
	idx := slices.Index(a.sessions, s)
	if idx < 0 {
		return
	}
	a.active = idx
	a.activePane = s.panes[0]
	a.refreshTitle()
	// The info panel follows the active session, whose Git and Files
	// sections hold the session's working directory; mark them stale.
	a.infoDirChanged()
}

// refreshTitle follows the active session's title, as otty names its
// window user@host:dir.
func (a *app) refreshTitle() {
	if a.win == nil {
		return
	}
	a.win.SetTitle(a.titleText())
}

// --- actions the menus and the palette run --------------------------------

func (a *app) newTab() { a.newSession("") }

func (a *app) closeTab() { a.requestClose(a.currentSession()) }

// requestClose closes the tab s, asking first when the settings say to:
// the menu's and the palette's Close Tab and a tab's close button all come
// through here, so none of them skips the question. A request made while
// the dialog is up answers it, closing the tab the dialog names rather
// than s: the active tab may have changed under the dialog, and the tab
// that closes must be the one the user was asked about.
func (a *app) requestClose(s *session) {
	if p := a.pendingClose; p != nil {
		a.pendingClose = nil
		a.closeSession(p)
		return
	}
	if s == nil {
		return
	}
	if a.closingAsks(s) {
		a.pendingClose = s
		return
	}
	a.closeSession(s)
}

// duplicateTab opens a new session in the same directory.
func (a *app) duplicateTab() {
	s := a.currentSession()
	if s == nil {
		return
	}
	a.newSession(s.dir)
}

// closeSession closes the session and its terminals, activating a neighbor.
func (a *app) closeSession(s *session) {
	idx := slices.Index(a.sessions, s)
	if idx < 0 {
		return
	}
	for _, p := range s.panes {
		if p.term != nil {
			_ = p.term.Close()
		}
	}
	a.sessions = append(a.sessions[:idx], a.sessions[idx+1:]...)
	// A dialog about the tab that just closed, as when its shell exits
	// under it, has nothing left to ask about or to rename.
	if a.pendingClose == s {
		a.pendingClose = nil
	}
	if a.rename.target == s {
		a.rename.target, a.rename.open = nil, false
	}
	if len(a.sessions) == 0 {
		a.active = 0
		a.activePane = nil
		a.refreshTitle()
		// The Git and Files panels described the session that closed:
		// without one they must not keep its repository and its files.
		a.infoDirChanged()
		return
	}
	if idx <= a.active && a.active > 0 {
		a.active--
	}
	if a.active >= len(a.sessions) {
		a.active = len(a.sessions) - 1
	}
	a.activate(a.sessions[a.active])
}

func (a *app) nextTab() {
	if n := len(a.sessions); n > 1 {
		a.activate(a.sessions[(a.active+1)%n])
	}
}

func (a *app) prevTab() {
	if n := len(a.sessions); n > 1 {
		a.activate(a.sessions[(a.active-1+n)%n])
	}
}

func (a *app) toggleSidebar() {
	a.sideOpen = !a.sideOpen
	if a.settings != nil {
		a.settings.SidebarVisible = a.sideOpen
		a.SaveSettings()
	}
}

// setPanel shows one right panel or none; opening the info panel starts
// the load of the section that is open.
func (a *app) setPanel(p panelMode) {
	a.panel = p
	if p == panelInfo {
		a.loadInfoTab()
	}
}

func (a *app) toggleChat() {
	if a.panel == panelChat {
		a.setPanel(panelNone)
	} else {
		a.setPanel(panelChat)
	}
}

func (a *app) toggleInfo() {
	if a.panel == panelInfo {
		a.setPanel(panelNone)
	} else {
		a.setPanel(panelInfo)
	}
}

func (a *app) splitRight() { a.splitPane(a.activePane, false) }

func (a *app) splitDown() { a.splitPane(a.activePane, true) }

func (a *app) focusNextPane() { a.focusRelative(1) }

func (a *app) focusPrevPane() { a.focusRelative(-1) }

func (a *app) focusRelative(delta int) {
	s := a.currentSession()
	if s == nil || a.activePane == nil {
		return
	}
	order := s.orderedPanes()
	if len(order) < 2 {
		return
	}
	i := slices.Index(order, a.activePane)
	if i < 0 {
		i = 0
	}
	a.focusPane(order[(i+delta+len(order))%len(order)])
}

func (a *app) focusPane(p *pane) {
	if p == nil || p.dead {
		return
	}
	a.activePane = p
	a.focusActive = true
}

func (a *app) clearScreen() {
	if p := a.activePane; p != nil && p.term != nil {
		p.term.Send([]byte{12}) // Ctrl+L, the shell's clear
	}
}

func (a *app) copyPath() {
	if s := a.currentSession(); s != nil {
		mygo.Clipboard.WriteText(s.dir)
	}
}

func (a *app) revealInFinder() {
	if s := a.currentSession(); s != nil {
		openWith(s.dir)
	}
}

// The font sizes the Settings page allows (its font-size setting). The
// zoom keeps to the same range: a narrower one made Increase Font Size
// shrink a size the page had set above it.
const (
	minFontSize = 6
	maxFontSize = 40
)

func (a *app) zoomFont(delta float32) {
	a.settings.FontSize = min(max(a.settings.FontSize+float64(delta), minFontSize), maxFontSize)
	a.ApplySettings()
	a.SaveSettings()
}

func (a *app) resetFont() {
	a.settings.FontSize = settings.Default().FontSize
	a.ApplySettings()
	a.SaveSettings()
}

func (a *app) applyFont() {
	for _, s := range a.sessions {
		for _, p := range s.panes {
			if p.term != nil {
				p.term.SetFont(a.termFont())
			}
		}
	}
}

// openSettings shows the one settings window, or focuses it when it is
// already open.
func (a *app) openSettings() {
	if a.settingsWin != nil && !a.settingsWin.IsDestroyed() {
		a.settingsWin.Focus()
		return
	}
	a.settingsWin = settings.NewWindow(a, a.settings)
	a.settingsWin.OnClosed(func() { a.settingsWin = nil })
}

// beginRename opens the rename dialog for the active session.
func (a *app) beginRename() {
	a.beginRenameSession(a.currentSession())
}

// beginRenameSession opens the rename dialog for a session. The tab's
// double click renames the tab it hit, the menu and palette the active
// session.
func (a *app) beginRenameSession(s *session) {
	if s == nil {
		return
	}
	a.rename.target = s
	a.rename.buf = s.name
	a.rename.open = true
}

func (a *app) commitRename() {
	if t := a.rename.target; t != nil {
		t.name = a.rename.buf
		a.refreshTitle()
	}
	a.rename.open = false
}
