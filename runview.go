package main

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type step struct {
	Name        string    `json:"name"`
	Number      int       `json:"number"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
}

type jobDetail struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	Steps       []step    `json:"steps"`
	URL         string    `json:"html_url"`
}

type runDetail struct {
	Status     string      `json:"status"`
	Conclusion string      `json:"conclusion"`
	SHA        string      `json:"head_sha"`
	Branch     string      `json:"head_branch"`
	Event      string      `json:"event"`
	Title      string      `json:"display_title"`
	Workflow   string      `json:"name"`
	Attempt    int         `json:"run_attempt"`
	Created    time.Time   `json:"created_at"`
	Updated    time.Time   `json:"updated_at"`
	URL        string      `json:"html_url"`
	Jobs       []jobDetail `json:"-"`
}

type runDataMsg struct {
	id  int
	d   *runDetail
	err error
}

func (m runDataMsg) viewID() int { return m.id }

type runTickMsg struct{ id int }

func (m runTickMsg) viewID() int { return m.id }

type runActionMsg struct {
	id   int
	text string
	err  error
}

func (m runActionMsg) viewID() int { return m.id }

// runView shows one GitHub Actions run: its jobs and their steps, live while it runs, with each job's log a key away.
type runView struct {
	id      int
	repo    string
	runID   int64
	dir     string
	d       *runDetail
	err     error
	loading bool
	sel     int
	flash   string
	flashOK bool
	ask     *confirmation // a pending question
}

func newRunView(repo, dir string, runID int64) *runView {
	return &runView{id: nextViewID(), repo: repo, dir: dir, runID: runID}
}

func (v *runView) ID() int { return v.id }
func (v *runView) Close()  {}

func (v *runView) Init() tea.Cmd { return v.fetch() }

func (v *runView) fetch() tea.Cmd {
	v.loading = true
	id, repo, runID := v.id, v.repo, v.runID
	return func() tea.Msg {
		var d runDetail
		err := ghGet(fmt.Sprintf("repos/%s/actions/runs/%d", repo, runID), &d)
		if err == nil {
			d.Jobs, err = runJobDetails(repo, runID)
		}
		if err != nil {
			return runDataMsg{id: id, err: err}
		}
		d.Status, d.Conclusion, d.SHA, d.Branch = clean(d.Status), clean(d.Conclusion), clean(d.SHA), clean(d.Branch)
		d.Event, d.Title, d.Workflow, d.URL = clean(d.Event), clean(d.Title), clean(d.Workflow), clean(d.URL)
		return runDataMsg{id: id, d: &d}
	}
}

func (v *runView) ghAction(desc string, args ...string) tea.Cmd {
	id := v.id
	return func() tea.Msg {
		if _, err := run("gh", args...); err != nil {
			return runActionMsg{id: id, err: fmt.Errorf("%s: %w", desc, err)}
		}
		return runActionMsg{id: id, text: desc}
	}
}

func (v *runView) Update(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case runDataMsg:
		v.loading = false
		if msg.err != nil {
			v.err = msg.err
		} else {
			v.d, v.err = msg.d, nil
			if v.sel >= len(v.d.Jobs) {
				v.sel = max(0, len(v.d.Jobs)-1)
			}
		}
		// Poll while anything is still moving.
		if v.d == nil || v.d.Status != "completed" {
			id := v.id
			return true, tea.Tick(4*time.Second, func(time.Time) tea.Msg { return runTickMsg{id} })
		}
	case runTickMsg:
		return true, v.fetch()
	case runActionMsg:
		if msg.err != nil {
			v.flash, v.flashOK = errText(msg.err), false
		} else {
			v.flash, v.flashOK = msg.text, true
		}
		id := v.id
		return true, tea.Tick(2*time.Second, func(time.Time) tea.Msg { return runTickMsg{id} })
	case tea.KeyMsg:
		return v.key(msg)
	}
	return true, nil
}

func (v *runView) key(msg tea.KeyMsg) (bool, tea.Cmd) {
	if v.ask != nil {
		done, ok := v.ask.key(msg)
		if !done {
			return true, nil
		}
		cmd := v.ask.cmd
		v.ask = nil
		if ok {
			return true, cmd
		}
		v.flash, v.flashOK = "cancelled", true
		return true, nil
	}
	jobs := []jobDetail{}
	if v.d != nil {
		jobs = v.d.Jobs
	}
	switch msg.String() {
	case "esc", "q":
		return false, nil
	case "up", "k":
		v.sel = max(0, v.sel-1)
	case "down", "j":
		v.sel = max(0, min(len(jobs)-1, v.sel+1))
	case "enter", "l":
		if v.sel < len(jobs) {
			j := jobs[v.sel]
			if j.Status != "completed" {
				v.flash, v.flashOK = "GitHub serves a job's log once it finishes; its steps update here meanwhile", false
				return true, nil
			}
			lv := newCommandView(fmt.Sprintf("%s · %s", v.d.Workflow, j.Name), v.dir, "")
			lv.argv = []string{"gh", "run", "view", "-R", v.repo, "--job", fmt.Sprint(j.ID), "--log"}
			lv.ingest = jobLogIngest(j.Steps)
			lv.scrollBack = 0
			return true, pushView(lv)
		}
	case "o":
		if v.d != nil {
			url := v.d.URL
			if v.sel < len(jobs) && jobs[v.sel].URL != "" {
				url = jobs[v.sel].URL
			}
			return true, openURL(url)
		}
	case "r":
		return true, v.fetch()
	case "R":
		if v.d != nil && v.d.Status == "completed" && v.d.Conclusion != "success" {
			v.ask = newConfirmation("Re-run the failed jobs of this run?", nil,
				v.ghAction("re-run of failed jobs requested", "run", "rerun", fmt.Sprint(v.runID), "-R", v.repo, "--failed"))
		}
	case "X":
		if v.d != nil && v.d.Status != "completed" {
			v.ask = newConfirmation("Cancel this run?", nil,
				v.ghAction("cancel requested", "run", "cancel", fmt.Sprint(v.runID), "-R", v.repo))
		}
	}
	return true, nil
}

func (v *runView) View(w, h int) string {
	var b strings.Builder
	if v.d == nil {
		b.WriteString(sTitle.Render(fmt.Sprintf("Run %d", v.runID)) + "\n\n")
		if v.err != nil {
			b.WriteString(sBad.Render(" "+errText(v.err)) + "\n")
		} else {
			b.WriteString(sMuted.Render(" loading…") + "\n")
		}
		return b.String() + strings.Repeat("\n", max(0, h-5)) + keyHelp([][2]string{{"esc", "back"}})
	}
	d := v.d
	head := sTitle.Render(d.Workflow) + "  " + runIcon(Run{Status: d.Status, Conclusion: d.Conclusion})
	if v.loading {
		head += sMuted.Render(" ⟳")
	}
	b.WriteString(head + "\n")
	event := map[string]string{"workflow_run": "after CI", "workflow_dispatch": "by hand", "push": "on push"}[d.Event]
	meta := []string{short(d.SHA), or(event, d.Event), "started " + age(d.Created) + " ago"}
	if d.Attempt > 1 {
		meta = append(meta, fmt.Sprintf("attempt %d", d.Attempt))
	}
	if d.Status == "completed" {
		meta = append(meta, "took "+d.Updated.Sub(d.Created).Round(time.Second).String())
	}
	b.WriteString(sMuted.Render(" "+strings.Join(meta, " · ")) + "\n\n")

	// Every job, with the chosen one's steps opened out beneath it.
	var lines []string
	selLine := 0
	for i, j := range d.Jobs {
		cursor := "  "
		if i == v.sel {
			cursor = sTitle.Render("▸ ")
			selLine = len(lines)
		}
		lines = append(lines, fmt.Sprintf("%s%s %s%s", cursor, jobIcon(j.Status, j.Conclusion), sBold.Render(j.Name),
			sMuted.Render(liveDuration(j.StartedAt, j.CompletedAt, j.Status))))
		if i != v.sel {
			continue
		}
		for _, s := range j.Steps {
			lines = append(lines, fmt.Sprintf("      %s %s%s", stepIcon(s), s.Name,
				sMuted.Render(liveDuration(s.StartedAt, s.CompletedAt, s.Status))))
		}
	}
	bodyH := max(1, h-5)
	start := 0
	if selLine >= bodyH {
		start = selLine - bodyH/3
	}
	end := min(len(lines), start+bodyH)
	for _, l := range lines[start:end] {
		b.WriteString(l + "\n")
	}
	for i := end - start; i < bodyH; i++ {
		b.WriteString("\n")
	}

	switch {
	case v.ask != nil:
		b.WriteString(v.ask.view() + "\n")
	case v.flash != "":
		style := sBad
		if v.flashOK {
			style = sGood
		}
		b.WriteString(style.Render(" "+v.flash) + "\n")
	default:
		b.WriteString("\n")
	}
	keys := [][2]string{{"↑↓", "job"}, {"enter", "log"}, {"o", "open on GitHub"}}
	if d.Status == "completed" && d.Conclusion != "success" {
		keys = append(keys, [2]string{"R", "re-run failed"})
	}
	if d.Status != "completed" {
		keys = append(keys, [2]string{"X", "cancel run"})
	}
	keys = append(keys, [2]string{"esc", "back"})
	b.WriteString(keyHelp(keys))
	return b.String()
}

func jobIcon(status, conclusion string) string {
	return stepIcon(step{Status: status, Conclusion: conclusion})
}

func stepIcon(s step) string {
	switch {
	case s.Status == "in_progress":
		return sWarn.Render("◐")
	case s.Status != "completed":
		return sMuted.Render("○")
	case s.Conclusion == "success":
		return sGood.Render("✓")
	case s.Conclusion == "skipped":
		return sMuted.Render("–")
	default:
		return sBad.Render("✗")
	}
}

func liveDuration(start, end time.Time, status string) string {
	if start.IsZero() {
		return ""
	}
	if status != "completed" || end.IsZero() || end.Before(start) {
		end = time.Now()
	}
	return "  " + end.Sub(start).Round(time.Second).String()
}
