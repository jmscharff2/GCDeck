package main

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type pipelineMsg struct {
	id  int
	p   pipeline
	err error
}

func (m pipelineMsg) viewID() int { return m.id }

type versionMsg struct {
	id      int
	version string
	err     error
}

func (m versionMsg) viewID() int { return m.id }

type watchTickMsg struct {
	id   int
	what string // "pipeline" or "version"
}

func (m watchTickMsg) viewID() int { return m.id }

type jobsMsg struct {
	id   int
	jobs []jobDetail
}

func (m jobsMsg) viewID() int { return m.id }

// watchView follows an env's newest commit to its server: the pipeline at the top, the running job's steps under it,
// and the server's logs streaming below, so a new container can be seen starting.
type watchView struct {
	id   int
	cfg  Config
	env  Env
	p    *pipeline
	perr error
	jobs []jobDetail // of the run that is moving: the deploy, else CI

	version  string
	verr     error
	checking bool

	logs *commandView // nil when the env has no logs action
}

func newWatchView(cfg Config, e Env) *watchView {
	v := &watchView{id: nextViewID(), cfg: cfg, env: e}
	for _, a := range cfg.actions(e) {
		if a.Label == "logs" && !a.Interactive {
			v.logs = newCommandView("server logs", cfg.AppDir, e.expand(a.Command))
			v.logs.id = v.id // its output arrives addressed to this view, which hands it on
			break
		}
	}
	return v
}

func (v *watchView) ID() int { return v.id }

func (v *watchView) Close() {
	if v.logs != nil {
		v.logs.Close()
	}
}

func (v *watchView) Init() tea.Cmd {
	cmds := []tea.Cmd{v.fetchPipeline(), v.fetchVersion()}
	if v.logs != nil {
		cmds = append(cmds, v.logs.Init())
	}
	return tea.Batch(cmds...)
}

func (v *watchView) fetchPipeline() tea.Cmd {
	id, cfg, e := v.id, v.cfg, v.env
	return func() tea.Msg {
		p, err := fetchPipeline(cfg, e)
		return pipelineMsg{id: id, p: p, err: err}
	}
}

func (v *watchView) fetchVersion() tea.Cmd {
	if v.env.VersionCommand == "" {
		return nil
	}
	v.checking = true
	id, dir, cmd := v.id, v.cfg.AppDir, v.env.expand(v.env.VersionCommand)
	return func() tea.Msg {
		ver, err := fetchVersion(dir, cmd)
		return versionMsg{id: id, version: ver, err: err}
	}
}

// moving is the run worth showing steps for: a running or failed deploy, else a running or failed CI run.
func (v *watchView) moving() *Run {
	if v.p == nil {
		return nil
	}
	for _, s := range []stage{v.p.Deploy, v.p.CI} {
		if s.Run != nil && (s.State == stageRunning || s.State == stageFailed) {
			return s.Run
		}
	}
	if v.p.Deploy.Run != nil {
		return v.p.Deploy.Run
	}
	return v.p.CI.Run
}

func (v *watchView) fetchJobs() tea.Cmd {
	r := v.moving()
	if r == nil {
		return nil
	}
	id, repo, runID := v.id, v.cfg.GitHubRepo, r.ID
	return func() tea.Msg {
		jobs, _ := runJobDetails(repo, runID)
		return jobsMsg{id: id, jobs: jobs}
	}
}

func (v *watchView) Update(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case linesMsg:
		if v.logs != nil {
			_, cmd := v.logs.Update(msg)
			return true, cmd
		}
	case pipelineMsg:
		if msg.err != nil {
			v.perr = msg.err
		} else {
			v.p, v.perr = &msg.p, nil
		}
		every := 15 * time.Second
		if v.p != nil && v.p.active() {
			every = 4 * time.Second
		}
		id := v.id
		return true, tea.Batch(v.fetchJobs(), tea.Tick(every, func(time.Time) tea.Msg { return watchTickMsg{id, "pipeline"} }))
	case jobsMsg:
		v.jobs = msg.jobs
	case versionMsg:
		v.checking = false
		if msg.err != nil {
			v.verr = msg.err
		} else {
			v.version, v.verr = msg.version, nil
		}
		// Ask the server again while a deploy is on its way; it answers in a few seconds over SSH.
		every := time.Minute
		if v.p == nil || v.p.active() || !sameCommit(v.version, v.p.Head.SHA) {
			every = 10 * time.Second
		}
		id := v.id
		return true, tea.Tick(every, func(time.Time) tea.Msg { return watchTickMsg{id, "version"} })
	case watchTickMsg:
		if msg.what == "version" {
			return true, v.fetchVersion()
		}
		return true, v.fetchPipeline()
	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "q":
			if v.logs != nil && (v.logs.typing || v.logs.filter != "") {
				break
			}
			v.Close()
			return false, nil
		case "enter":
			if r := v.moving(); r != nil {
				return true, pushView(newRunView(v.cfg.GitHubRepo, v.cfg.AppDir, r.ID))
			}
			return true, nil
		case "o":
			if r := v.moving(); r != nil {
				return true, openURL(r.URL)
			}
			return true, nil
		}
		if v.logs != nil {
			_, cmd := v.logs.key(msg)
			return true, cmd
		}
	}
	return true, nil
}

func (v *watchView) View(w, h int) string {
	var lines []string
	title := sTitle.Render("Watching "+v.env.Name) + sMuted.Render("  branch "+v.env.Branch)
	lines = append(lines, title)
	switch {
	case v.p == nil && v.perr != nil:
		lines = append(lines, sBad.Render(" "+errText(v.perr)))
	case v.p == nil:
		lines = append(lines, sMuted.Render(" loading…"))
	default:
		p := *v.p
		lines = append(lines, " "+headLine(p))
		for _, l := range []string{stageLine("CI", p.CI), stageLine("Deploy", p.Deploy)} {
			if l != "" {
				lines = append(lines, l)
			}
		}
		if v.env.VersionCommand != "" {
			lines = append(lines, serverLine(p, v.version, v.verr, v.checking))
		}
		if v.perr != nil {
			lines = append(lines, sBad.Render(" "+truncate(errText(v.perr), w-2)))
		}
	}

	// The moving run's current job, step by step.
	if job := currentJob(v.jobs); job != nil {
		lines = append(lines, "", sBold.Render(" "+job.Name)+sMuted.Render(liveDuration(job.StartedAt, job.CompletedAt, job.Status)))
		steps := job.Steps
		// Keep the step that is running in view: the last few done, the current one, the next few.
		cur := len(steps) - 1
		for i, s := range steps {
			if s.Status != "completed" {
				cur = i
				break
			}
		}
		from, to := max(0, cur-3), min(len(steps), cur+4)
		if from > 0 {
			lines = append(lines, sMuted.Render(fmt.Sprintf("      … %d steps done", from)))
		}
		for _, s := range steps[from:to] {
			lines = append(lines, fmt.Sprintf("      %s %s%s", stepIcon(s), s.Name, sMuted.Render(liveDuration(s.StartedAt, s.CompletedAt, s.Status))))
		}
		if to < len(steps) {
			lines = append(lines, sMuted.Render(fmt.Sprintf("      … %d more", len(steps)-to)))
		}
	}
	lines = append(lines, "")

	logH := h - len(lines) - 1
	if v.logs != nil && logH >= 4 {
		lines = append(lines, v.logs.render(w, logH))
	} else {
		for i := 0; i < logH; i++ {
			lines = append(lines, "")
		}
		if v.logs == nil && logH >= 1 {
			lines[len(lines)-1] = sMuted.Render(" Add a logs action to see the server's logs here.")
		}
	}
	keys := [][2]string{}
	if v.moving() != nil {
		keys = append(keys, [2]string{"enter", "run details"}, [2]string{"o", "open on GitHub"})
	}
	if v.logs != nil {
		keys = append(keys, [2]string{"↑↓", "scroll logs"}, [2]string{"/", "filter"})
	}
	keys = append(keys, [2]string{"esc", "back"})
	return strings.Join(lines, "\n") + "\n" + keyHelp(keys)
}

// currentJob is the job running now, else one that failed, else the last that ran.
func currentJob(jobs []jobDetail) *jobDetail {
	var last *jobDetail
	for i := range jobs {
		j := &jobs[i]
		if j.Status == "in_progress" {
			return j
		}
		if j.Conclusion == "failure" {
			last = j
		}
	}
	if last != nil {
		return last
	}
	for i := len(jobs) - 1; i >= 0; i-- {
		if jobs[i].Conclusion != "skipped" && len(jobs[i].Steps) > 0 {
			return &jobs[i]
		}
	}
	return nil
}
