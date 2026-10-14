package main

import (
	"errors"
	"fmt"
	neturl "net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Panel keys besides "res<N>" for the env's Nth resource.
const (
	pHealth   = "health"
	pDeploys  = "deploys"
	pPipeline = "pipeline"
	pVersion  = "version"
	pCompare  = "compare"
)

type panel struct {
	val     any
	err     error
	loading bool
	at      time.Time // when it last loaded
}

type dataMsg struct {
	env int
	key string
	val any
	err error
}

type actionMsg struct {
	text    string
	err     error
	refresh bool
}

type tickMsg struct{}

// refreshNowMsg refreshes once, outside the tick's schedule.
type refreshNowMsg struct{}

type model struct {
	cfg     Config
	panels  []map[string]*panel
	cur     int
	sel     int
	w, h    int
	flash   string
	isErr   bool
	confirm *confirmation
	help    bool
	stack   []subview // full-screen views over the dashboard, top last
	// notified remembers pipeline events already announced, as "env|sha|event".
	notified map[string]bool
}

func newModel(cfg Config) model {
	m := model{cfg: cfg, panels: make([]map[string]*panel, len(cfg.Envs)), notified: map[string]bool{}}
	for i := range m.panels {
		m.panels[i] = map[string]*panel{}
	}
	return m
}

func (m model) env() Env { return m.cfg.Envs[m.cur] }

func (m model) hasGitHub() bool { return m.cfg.GitHubRepo != "" }

func (m model) Init() tea.Cmd { return tea.Batch(m.refresh(m.cur), m.tick()) }

func load(env int, key string, f func() (any, error)) tea.Cmd {
	return func() tea.Msg {
		v, err := f()
		return dataMsg{env: env, key: key, val: v, err: err}
	}
}

// refresh starts every fetch for env i and marks its panels loading.
func (m model) refresh(i int) tea.Cmd {
	cfg, e := m.cfg, m.cfg.Envs[i]
	var cmds []tea.Cmd
	add := func(key string, f func() (any, error)) {
		p := m.panels[i][key]
		if p == nil {
			p = &panel{}
			m.panels[i][key] = p
		}
		p.loading = true
		cmds = append(cmds, load(i, key, f))
	}
	if e.HealthURL != "" {
		add(pHealth, func() (any, error) {
			c, err := fetchHealth(e.HealthURL)
			return cleanCard(c), err
		})
	}
	for n, r := range e.Resources {
		r := r
		add(fmt.Sprintf("res%d", n), func() (any, error) { return fetchResource(e, r) })
	}
	if m.hasGitHub() {
		if e.Deploy != nil && e.Deploy.Workflow != "" {
			add(pDeploys, func() (any, error) { return fetchDeploys(cfg, e) })
		}
		if e.Branch != "" && (cfg.CIWorkflow != "" || e.Deploy != nil && e.Deploy.Workflow != "") {
			add(pPipeline, func() (any, error) { return fetchPipeline(cfg, e) })
		}
		if cfg.Promote != nil {
			add(pCompare, func() (any, error) { return fetchCompare(cfg.GitHubRepo, cfg.Promote) })
		}
	}
	if e.VersionCommand != "" && m.wantVersion(i) {
		dir, cmd := cfg.AppDir, e.expand(e.VersionCommand)
		add(pVersion, func() (any, error) { return fetchVersion(dir, cmd) })
	}
	return tea.Batch(cmds...)
}

// wantVersion limits asking the server what it runs, which goes over SSH: on first load, while a commit is on its
// way, while the server is behind, and every five minutes otherwise. A watch screen asks on its own.
func (m model) wantVersion(i int) bool {
	if len(m.stack) > 0 {
		return false
	}
	v := m.panels[i][pVersion]
	if v == nil {
		return true
	}
	if v.loading {
		return false
	}
	ver, _ := v.val.(string)
	if p, ok := m.pipeline(i); ok && (p.active() || !sameCommit(ver, p.Head.SHA)) {
		return time.Since(v.at) > 8*time.Second
	}
	return time.Since(v.at) > 5*time.Minute
}

func (m model) pipeline(i int) (pipeline, bool) {
	if p := m.panels[i][pPipeline]; p != nil {
		pl, ok := p.val.(pipeline)
		return pl, ok
	}
	return pipeline{}, false
}

// tick refreshes faster while a deploy is running, so its progress shows without pressing r.
func (m model) tick() tea.Cmd {
	every := time.Duration(m.cfg.RefreshSeconds) * time.Second
	for _, r := range m.runs() {
		if r.Status != "completed" {
			every = 8 * time.Second
		}
	}
	if p, ok := m.pipeline(m.cur); ok && p.active() {
		every = 8 * time.Second
	}
	return tea.Tick(every, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m model) runs() []Run {
	if p := m.panels[m.cur][pDeploys]; p != nil {
		r, _ := p.val.([]Run)
		return r
	}
	return nil
}

// authExpired names the CLI whose login has lapsed, if any.
func (m model) authExpired() string {
	who := ""
	for _, p := range m.panels[m.cur] {
		if p.err != nil && errors.Is(p.err, errAuth) {
			if strings.HasPrefix(p.err.Error(), "gcloud") {
				return "gcloud"
			}
			who = "gh"
		}
	}
	return who
}

func (m *model) say(text string, isErr bool) { m.flash, m.isErr = text, isErr }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case pushViewMsg:
		m.stack = append(m.stack, msg.v)
		return m, msg.v.Init()
	case viewMsg:
		for _, v := range m.stack {
			if v.ID() == msg.viewID() {
				open, cmd := v.Update(msg)
				if !open {
					m = m.closeView(v)
				}
				return m, cmd
			}
		}
		return m, nil
	case tea.KeyMsg:
		if len(m.stack) == 0 {
			break
		}
		if msg.String() == "ctrl+c" {
			for _, v := range m.stack {
				v.Close()
			}
			return m, tea.Quit
		}
		top := m.stack[len(m.stack)-1]
		open, cmd := top.Update(msg)
		if !open {
			m = m.closeView(top)
		}
		return m, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case tickMsg:
		// The env on screen in full, the others' pipelines only, so a merge elsewhere is still announced.
		cmds := []tea.Cmd{m.refresh(m.cur), m.tick()}
		for i, e := range m.cfg.Envs {
			if i != m.cur && m.hasGitHub() && e.Branch != "" && m.panels[i][pPipeline] != nil {
				cfg, e := m.cfg, e
				cmds = append(cmds, load(i, pPipeline, func() (any, error) { return fetchPipeline(cfg, e) }))
			}
		}
		return m, tea.Batch(cmds...)
	case refreshNowMsg:
		return m, m.refresh(m.cur)
	case dataMsg:
		p := m.panels[msg.env][msg.key]
		if p == nil {
			p = &panel{}
			m.panels[msg.env][msg.key] = p
		}
		before := p.val
		p.loading, p.at = false, time.Now()
		if msg.err != nil {
			p.err = msg.err // keep the last good value on screen beside the error
		} else {
			p.val, p.err = msg.val, nil
		}
		if n := len(m.runs()); m.sel >= n && n > 0 {
			m.sel = n - 1
		}
		if msg.err == nil {
			return m, m.announce(msg.env, msg.key, before)
		}
	case actionMsg:
		if msg.err != nil {
			m.say(errText(msg.err), true)
		} else {
			m.say(msg.text, false)
		}
		if msg.refresh {
			// GitHub takes a moment to list a dispatched run. A one-off refresh: the tick keeps its own schedule.
			return m, tea.Tick(3*time.Second, func(time.Time) tea.Msg { return refreshNowMsg{} })
		}
	case execMsg:
		return m, execHere(msg)
	case tea.KeyMsg:
		if m.confirm != nil {
			return m.updateConfirm(msg)
		}
		return m.updateKey(msg)
	}
	return m, nil
}

// announce tells you when a pipeline moves on: a new commit on the branch, CI or a deploy failing, the server going
// live on the newest commit. Each happens once per commit, and only for changes seen while gcdeck runs.
func (m *model) announce(i int, key string, before any) tea.Cmd {
	e := m.cfg.Envs[i]
	p, ok := m.pipeline(i)
	if !ok {
		return nil
	}
	once := func(event string) bool {
		k := e.Name + "|" + p.Head.SHA + "|" + event
		if m.notified[k] {
			return false
		}
		m.notified[k] = true
		return true
	}
	switch key {
	case pPipeline:
		prev, had := before.(pipeline)
		if !had {
			// First load: remember what has already happened, so it is not announced as news.
			once("ci-failed")
			once("deploy-failed")
			return nil
		}
		if prev.Head.SHA != p.Head.SHA {
			m.say(fmt.Sprintf("new commit on %s: %s %s · w to watch", e.Branch, short(p.Head.SHA), truncate(p.Head.Message, 40)), false)
		}
		if p.CI.State == stageFailed && once("ci-failed") {
			return notify(fmt.Sprintf("CI failed on %s", e.Branch), short(p.Head.SHA)+" "+p.Head.Message, false)
		}
		if p.Deploy.State == stageFailed && once("deploy-failed") {
			return notify(fmt.Sprintf("%s deploy failed", e.Name), short(p.Head.SHA)+" "+p.Head.Message, false)
		}
		if e.VersionCommand == "" && p.Deploy.State == stageDone && prev.Deploy.State == stageRunning && once("live") {
			return notify(fmt.Sprintf("%s deployed", e.Name), short(p.Head.SHA)+" "+p.Head.Message, true)
		}
	case pVersion:
		prevVer, _ := before.(string)
		ver, _ := m.panels[i][pVersion].val.(string)
		if prevVer != "" && !sameCommit(prevVer, p.Head.SHA) && sameCommit(ver, p.Head.SHA) && once("live") {
			return notify(fmt.Sprintf("%s is live", e.Name), short(p.Head.SHA)+" "+p.Head.Message, true)
		}
	}
	return nil
}

// notify shows a Herdr notification, or rings the terminal bell outside Herdr.
func notify(title, body string, good bool) tea.Cmd {
	return func() tea.Msg {
		if inHerdr() {
			sound := "request"
			if good {
				sound = "done"
			}
			_, _ = run("herdr", "notification", "show", title, "--body", body, "--sound", sound)
		} else {
			fmt.Print("\a")
		}
		return actionMsg{text: title + ": " + body, err: nil}
	}
}

func (m model) closeView(v subview) model {
	v.Close()
	for i, s := range m.stack {
		if s == v {
			m.stack = append(m.stack[:i:i], m.stack[i+1:]...)
			break
		}
	}
	return m
}

func (m model) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	done, ok := m.confirm.key(msg)
	if !done {
		return m, nil
	}
	cmd := m.confirm.cmd
	m.confirm = nil
	if !ok {
		m.say("cancelled", false)
		return m, nil
	}
	return m, cmd
}

// dispatchWorkflow runs `gh workflow run` and reports back.
func dispatchWorkflow(desc string, args []string) tea.Cmd {
	return func() tea.Msg {
		if _, err := run("gh", args...); err != nil {
			return actionMsg{err: fmt.Errorf("%s: %w", desc, err)}
		}
		return actionMsg{text: desc + " started", refresh: true}
	}
}

func (m model) updateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	e, cfg := m.env(), m.cfg
	key := msg.String()
	switch key {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "?":
		m.help = !m.help
	case "tab", "right", "L":
		return m.switchEnv((m.cur + 1) % len(cfg.Envs))
	case "shift+tab", "left", "H":
		return m.switchEnv((m.cur + len(cfg.Envs) - 1) % len(cfg.Envs))
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		if i := int(key[0] - '1'); i < len(cfg.Envs) {
			return m.switchEnv(i)
		}
	case "r":
		m.say("refreshing "+e.Name, false)
		return m, m.refresh(m.cur)
	case "j", "down":
		if m.sel < len(m.runs())-1 {
			m.sel++
		}
	case "k", "up":
		if m.sel > 0 {
			m.sel--
		}
	case "enter":
		if runs := m.runs(); m.sel < len(runs) {
			return m, pushView(newRunView(cfg.GitHubRepo, cfg.AppDir, runs[m.sel].ID))
		}
	case "w":
		if m.hasGitHub() && e.Branch != "" {
			return m, pushView(newWatchView(cfg, e))
		}
	case "o":
		if runs := m.runs(); m.sel < len(runs) {
			return m, openURL(runs[m.sel].URL)
		}
	case "g":
		return m, openURL("https://console.cloud.google.com/home/dashboard?project=" + e.Project)
	case "d":
		return m.startDeploy(e)
	case "p":
		return m.startPromote()
	case "a":
		cmd := "gcloud auth login"
		if m.authExpired() == "gh" {
			cmd = "gh auth login"
		}
		return m, openPane("sign in", cfg.AppDir, cmd+" && echo 'Signed in. Press r in gcdeck.'", true)
	case "e":
		return m, openPane("edit config", cfg.AppDir, "${EDITOR:-vi} "+shellQuote(cfg.path), true)
	default:
		for _, a := range cfg.actions(e) {
			if a.Key != key {
				continue
			}
			title, cmd := e.Name+" "+a.Label, e.expand(a.Command)
			var run tea.Cmd
			switch {
			case a.Interactive || a.Pane:
				run = openPane(title, cfg.AppDir, cmd, a.Interactive)
			default:
				run = viewAction(title, cfg.AppDir, cmd, a.Confirm)
			}
			if a.Confirm {
				m.confirm = newConfirmation(fmt.Sprintf("Run %s on %s: %s?", a.Label, e.Name, truncate(cmd, 60)), &e, run)
				return m, nil
			}
			return m, run
		}
	}
	return m, nil
}

func (m model) startDeploy(e Env) (tea.Model, tea.Cmd) {
	d := e.Deploy
	if d == nil {
		m.say("no deploy configured for "+e.Name, true)
		return m, nil
	}
	c := newConfirmation("", &e, nil)
	if d.Command != "" {
		cmd := e.expand(d.Command)
		c.prompt = fmt.Sprintf("Deploy %s: %s?", e.Name, truncate(cmd, 60))
		c.cmd = viewAction("deploy "+e.Name, m.cfg.AppDir, cmd, true)
	} else {
		if !m.hasGitHub() {
			m.say("no GitHub repo: set github_repo in the config", true)
			return m, nil
		}
		c.prompt = fmt.Sprintf("Deploy %s → %s with %s?", d.Ref, e.Name, d.Workflow)
		c.cmd = dispatchWorkflow("deploy "+e.Name, workflowArgs(m.cfg.GitHubRepo, d.Workflow, d.Ref, d.Inputs))
	}
	m.confirm = c
	return m, nil
}

func (m model) startPromote() (tea.Model, tea.Cmd) {
	p := m.cfg.Promote
	if p == nil {
		m.say("no promote configured", true)
		return m, nil
	}
	c := newConfirmation(fmt.Sprintf("Promote %s → %s?", p.From, p.To), nil, nil)
	c.require = "promote"
	if p.Command != "" {
		c.cmd = viewAction("promote", m.cfg.AppDir, p.Command, true)
	} else {
		c.cmd = dispatchWorkflow("promote", workflowArgs(m.cfg.GitHubRepo, p.Workflow, p.Ref, p.Inputs))
	}
	m.confirm = c
	return m, nil
}

func (m model) switchEnv(i int) (tea.Model, tea.Cmd) {
	m.cur, m.sel = i, 0
	m.say("", false)
	return m, m.refresh(i)
}

// openURL opens a web page in the browser. Only https: `open` would also launch a file or an app given a file: or
// other URL, and some of these come from data gcdeck fetched.
func openURL(link string) tea.Cmd {
	return func() tea.Msg {
		u, err := neturl.Parse(link)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return actionMsg{err: fmt.Errorf("not opening %q: only https links open", clean(link))}
		}
		url := u.String()
		opener := "xdg-open"
		if runtime.GOOS == "darwin" {
			opener = "open"
		}
		if err := exec.Command(opener, url).Run(); err != nil {
			return actionMsg{err: fmt.Errorf("open %s: %w", url, err)}
		}
		return actionMsg{text: "opened in browser"}
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ---- view ----

var (
	cAccent = lipgloss.AdaptiveColor{Light: "#5A3FD6", Dark: "#A08CFF"}
	cMuted  = lipgloss.AdaptiveColor{Light: "#6B6B76", Dark: "#8A8A96"}
	cGood   = lipgloss.AdaptiveColor{Light: "#18794E", Dark: "#4CC38A"}
	cWarn   = lipgloss.AdaptiveColor{Light: "#A35200", Dark: "#F5A524"}
	cBad    = lipgloss.AdaptiveColor{Light: "#C62A2F", Dark: "#FF6369"}
	cBorder = lipgloss.AdaptiveColor{Light: "#D4D4DA", Dark: "#3A3A44"}

	sMuted  = lipgloss.NewStyle().Foreground(cMuted)
	sGood   = lipgloss.NewStyle().Foreground(cGood)
	sWarn   = lipgloss.NewStyle().Foreground(cWarn)
	sBad    = lipgloss.NewStyle().Foreground(cBad)
	sBold   = lipgloss.NewStyle().Bold(true)
	sTitle  = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	sTabOn  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(cAccent).Padding(0, 1)
	sTabOff = lipgloss.NewStyle().Foreground(cMuted).Padding(0, 1)
	sCard   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cBorder).Padding(0, 1)
)

const minCardWidth = 34

func (m model) View() string {
	if m.w == 0 {
		return "loading…"
	}
	if n := len(m.stack); n > 0 {
		return m.stack[n-1].View(m.w, m.h)
	}
	e := m.env()
	var b strings.Builder

	tabs := []string{sTitle.Render("gcdeck") + "  "}
	for i, env := range m.cfg.Envs {
		style := sTabOff
		if i == m.cur {
			style = sTabOn
		}
		tabs = append(tabs, style.Render(fmt.Sprintf("%d %s", i+1, env.Name)))
	}
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, tabs...) + "\n")
	where := []string{e.Project}
	if e.Branch != "" {
		where = append(where, "branch "+e.Branch)
	}
	if loc := or(e.Region, e.Zone); loc != "" {
		where = append(where, loc)
	}
	b.WriteString(sMuted.Render(" "+strings.Join(where, " · ")) + "\n")
	if who := m.authExpired(); who != "" {
		b.WriteString(sBad.Render(fmt.Sprintf(" ⚠ %s login expired. Press a to sign in, then r.", who)) + "\n")
	}

	var cards []string
	if e.HealthURL != "" {
		cards = append(cards, m.cardView("Health  "+strings.TrimPrefix(e.HealthURL, "https://"), pHealth))
	}
	for n, r := range e.Resources {
		cards = append(cards, m.cardView(resourceTitle(r), fmt.Sprintf("res%d", n)))
	}
	b.WriteString(m.grid(cards))

	if m.hasGitHub() && (e.Deploy != nil && e.Deploy.Workflow != "" || m.cfg.CIWorkflow != "" || m.cfg.Promote != nil) {
		b.WriteString(m.pipelineView(m.w-2) + "\n")
	}
	b.WriteString(m.footer())
	return b.String()
}

// grid lays cards out in as many equal columns as fit, every card in a row the same height.
func (m model) grid(cards []string) string {
	if len(cards) == 0 {
		return ""
	}
	cols := m.w / minCardWidth
	if cols < 1 {
		cols = 1
	}
	if cols > len(cards) {
		cols = len(cards)
	}
	if cols == 3 && len(cards) == 4 {
		cols = 2
	}
	width := m.w/cols - 2
	var b strings.Builder
	for i := 0; i < len(cards); i += cols {
		row := cards[i:min(i+cols, len(cards))]
		height := 0
		for _, c := range row {
			// The card's horizontal padding takes two cells of its width.
			height = max(height, lipgloss.Height(lipgloss.NewStyle().Width(width-2).Render(c)))
		}
		rendered := make([]string, len(row))
		for j, c := range row {
			rendered[j] = sCard.Width(width).Height(height).Render(c)
		}
		b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, rendered...) + "\n")
	}
	return b.String()
}

func (m model) cardView(title, key string) string {
	p := m.panels[m.cur][key]
	head := sBold.Render(title)
	if p != nil && p.loading {
		head += sMuted.Render(" ⟳")
	}
	lines := []string{head}
	c, ok := card{}, false
	if p != nil {
		c, ok = p.val.(card)
	}
	switch {
	case ok:
		if c.Status != "" {
			style := map[level]lipgloss.Style{lvGood: sGood, lvWarn: sWarn, lvBad: sBad}[c.Level]
			lines = append(lines, style.Render("● "+c.Status))
		}
		for _, row := range c.Rows {
			lines = append(lines, sMuted.Render(pad(row[0], 10))+" "+row[1])
		}
		for _, l := range c.Lines {
			name, when, _ := strings.Cut(l, "\t")
			lines = append(lines, name+"  "+sMuted.Render(when))
		}
		if c.Warn != "" {
			lines = append(lines, sWarn.Render(c.Warn))
		}
	case p == nil || p.err == nil:
		lines = append(lines, sMuted.Render("loading…"))
	}
	if p != nil && p.err != nil {
		lines = append(lines, sBad.Render(errText(p.err)))
	}
	return strings.Join(lines, "\n")
}

func (m model) pipelineView(width int) string {
	e, cfg := m.env(), m.cfg
	var lines []string

	if p := m.panels[m.cur][pPipeline]; p != nil {
		if pl, ok := p.val.(pipeline); ok {
			lines = append(lines, headLine(pl))
			for _, l := range []string{stageLine("CI", pl.CI), stageLine("Deploy", pl.Deploy)} {
				if l != "" {
					lines = append(lines, l)
				}
			}
			if e.VersionCommand != "" {
				ver, verr, checking := "", error(nil), false
				if vp := m.panels[m.cur][pVersion]; vp != nil {
					ver, _ = vp.val.(string)
					verr, checking = vp.err, vp.loading
				}
				lines = append(lines, serverLine(pl, ver, verr, checking))
			}
			if pl.active() {
				lines = append(lines, sMuted.Render("  w to watch it reach the server"))
			}
		} else if p.err == nil {
			lines = append(lines, sMuted.Render("loading the newest commit…"))
		}
		if p.err != nil {
			lines = append(lines, sBad.Render(truncate(errText(p.err), width-4)))
		}
	}

	if p := m.panels[m.cur][pCompare]; p != nil && cfg.Promote != nil {
		if c, ok := p.val.(Compare); ok {
			if c.Ahead == 0 {
				lines = append(lines, sMuted.Render(fmt.Sprintf("%s and %s are in step", cfg.Promote.From, cfg.Promote.To)))
			} else {
				lines = append(lines, sWarn.Render(fmt.Sprintf("%s is %d commit(s) ahead of %s", cfg.Promote.From, c.Ahead, cfg.Promote.To))+
					sMuted.Render(" · p to promote"))
			}
		} else if p.err != nil {
			lines = append(lines, sBad.Render(truncate(errText(p.err), width-4)))
		}
	}

	if p := m.panels[m.cur][pDeploys]; p != nil {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, sBold.Render("Deploys → "+e.Name)+sMuted.Render("  "+e.Deploy.Workflow))
		runs := m.runs()
		for i, r := range runs {
			cursor := "  "
			if i == m.sel {
				cursor = sTitle.Render("▸ ")
			}
			event := map[string]string{"workflow_run": "after CI", "workflow_dispatch": "by hand", "push": "on push"}[r.Event]
			detail := ""
			switch {
			case r.Pending:
				detail = sWarn.Render("not started")
			case r.Duration > 0:
				detail = sMuted.Render("took " + r.Duration.Round(time.Second).String())
			}
			lines = append(lines, fmt.Sprintf("%s%s  %s  %-9s %s  %s", cursor, runIcon(r), short(r.SHA), or(event, r.Event),
				pad(detail, 14), sMuted.Render(age(r.Created)+" ago")))
		}
		if len(runs) == 0 && p.err == nil && !p.loading {
			lines = append(lines, sMuted.Render("  no deploys yet"))
		}
		if p.err != nil {
			lines = append(lines, sBad.Render("  "+truncate(errText(p.err), width-4)))
		}
	}
	return sCard.Width(width).Render(strings.Join(lines, "\n"))
}

func (m model) footer() string {
	if c := m.confirm; c != nil {
		return c.view()
	}
	var b strings.Builder
	if m.flash != "" {
		style := sGood
		if m.isErr {
			style = sBad
		}
		b.WriteString(style.Render(" "+m.flash) + "\n")
	}
	e := m.env()
	keys := [][2]string{{"tab", "env"}}
	if m.hasGitHub() && e.Branch != "" {
		keys = append(keys, [2]string{"w", "watch"})
	}
	if e.Deploy != nil {
		keys = append(keys, [2]string{"d", "deploy"})
	}
	if m.cfg.Promote != nil {
		keys = append(keys, [2]string{"p", "promote"})
	}
	if len(m.runs()) > 0 {
		keys = append(keys, [2]string{"↑↓ enter", "runs"})
	}
	for _, a := range m.cfg.actions(e) {
		keys = append(keys, [2]string{a.Key, a.Label})
	}
	keys = append(keys, [2]string{"r", "refresh"}, [2]string{"?", "more"}, [2]string{"q", "quit"})
	if m.help {
		where := "in a pane below"
		if !inHerdr() {
			where = "in this terminal"
		}
		b.WriteString(sMuted.Render(fmt.Sprintf(" Output shows here; consoles and shells open %s. Config: %s\n", where, m.cfg.path)))
		keys = [][2]string{{"o", "open run on GitHub"}, {"g", "open project in Cloud Console"},
			{"a", "sign in (gcloud/gh)"}, {"e", "edit config"}, {"1-9", "jump to env"}, {"?", "back"}}
	}
	var parts []string
	for _, k := range keys {
		parts = append(parts, sTitle.Render(k[0])+" "+sMuted.Render(k[1]))
	}
	b.WriteString(" " + strings.Join(parts, sMuted.Render(" · ")))
	return b.String()
}

func runIcon(r Run) string {
	switch {
	case r.Status != "completed":
		return sWarn.Render("◐ " + pad(strings.ReplaceAll(r.Status, "_", " "), 11))
	case r.Conclusion == "success":
		return sGood.Render("✓ " + pad("success", 11))
	case r.Conclusion == "skipped" || r.Conclusion == "cancelled" || r.Conclusion == "neutral":
		return sMuted.Render("– " + pad(r.Conclusion, 11))
	default:
		return sBad.Render("✗ " + pad(r.Conclusion, 11))
	}
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// pad right-pads to n visible cells, ignoring ANSI styling.
func pad(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

func truncate(s string, n int) string {
	if n < 4 {
		n = 4
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
