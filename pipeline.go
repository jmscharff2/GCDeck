package main

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// A pipeline follows the newest commit on an env's branch to the server: its CI run, its deploy run, and whether the
// server reports running it. It answers "I just merged; where is it?"

type commitInfo struct {
	SHA     string    `json:"sha"`
	Message string    `json:"message"`
	Author  string    `json:"author"`
	Date    time.Time `json:"date"`
}

type stageState int

const (
	stageNone    stageState = iota // nothing to show (no CI configured, say)
	stageWaiting                   // not started yet
	stageRunning
	stageDone
	stageFailed
	stageSkipped
)

type stage struct {
	State    stageState
	Run      *Run
	Detail   string // the job and step running now, or why it has not started
	Duration time.Duration
}

type pipeline struct {
	Head   commitInfo
	CI     stage
	Deploy stage
}

// active reports whether something is still moving, which makes gcdeck poll faster.
func (p pipeline) active() bool {
	return p.CI.State == stageRunning || p.Deploy.State == stageRunning ||
		(p.CI.State == stageDone && p.Deploy.State == stageWaiting)
}

func fetchHead(repo, branch string) (commitInfo, error) {
	var resp struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Name string    `json:"name"`
				Date time.Time `json:"date"`
			} `json:"author"`
		} `json:"commit"`
		Author *struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	if err := ghGet(fmt.Sprintf("repos/%s/commits/%s", repo, url.PathEscape(branch)), &resp); err != nil {
		return commitInfo{}, err
	}
	c := commitInfo{SHA: resp.SHA, Author: resp.Commit.Author.Name, Date: resp.Commit.Author.Date}
	c.Message, _, _ = strings.Cut(resp.Commit.Message, "\n")
	if resp.Author != nil && resp.Author.Login != "" {
		c.Author = resp.Author.Login
	}
	c.SHA, c.Message, c.Author = clean(c.SHA), clean(c.Message), clean(c.Author)
	return c, nil
}

func fetchPipeline(cfg Config, e Env) (pipeline, error) {
	var p pipeline
	head, err := fetchHead(cfg.GitHubRepo, e.Branch)
	if err != nil {
		return p, err
	}
	p.Head = head

	if cfg.CIWorkflow != "" {
		runs, err := fetchRuns(cfg.GitHubRepo, cfg.CIWorkflow, e.Branch, 10)
		if err != nil {
			return p, err
		}
		// A merged commit can have a pull_request run as well as its push run; the push run is the one that counts.
		var ci *Run
		for i := range runs {
			if runs[i].SHA == head.SHA && (ci == nil || runs[i].Event == "push") {
				ci = &runs[i]
			}
		}
		p.CI = stageFor(cfg.GitHubRepo, ci, "")
		if ci == nil {
			p.CI.Detail = "not started"
		}
	}

	if d := e.Deploy; d != nil && d.Workflow != "" {
		deploys, err := fetchDeploys(cfg, e)
		if err != nil {
			return p, err
		}
		var run *Run
		for i := range deploys {
			if deploys[i].SHA == head.SHA {
				run = &deploys[i]
				break
			}
		}
		p.Deploy = stageFor(cfg.GitHubRepo, run, d.Job)
		if run == nil {
			switch p.CI.State {
			case stageRunning, stageWaiting:
				p.Deploy.Detail = "starts when CI passes"
			case stageFailed:
				p.Deploy.State, p.Deploy.Detail = stageSkipped, "CI failed, so this commit will not deploy"
			default:
				p.Deploy.Detail = "not started"
			}
		}
	}
	return p, nil
}

// stageFor reads a run's state and, while it runs, which job and step it is on. job narrows that to the env's job
// when one workflow deploys several envs.
func stageFor(repo string, r *Run, job string) stage {
	if r == nil {
		return stage{State: stageWaiting}
	}
	s := stage{Run: r, Duration: r.Duration}
	switch {
	case r.Pending:
		s.State, s.Detail = stageRunning, "deciding which env to deploy"
	case r.Status != "completed":
		s.State = stageRunning
	case r.Conclusion == "success":
		s.State = stageDone
	case r.Conclusion == "skipped" || r.Conclusion == "cancelled":
		s.State, s.Detail = stageSkipped, r.Conclusion
	default:
		s.State, s.Detail = stageFailed, r.Conclusion
	}
	if s.State != stageRunning && s.State != stageFailed {
		return s
	}
	jobs, err := runJobDetails(repo, r.ID)
	if err != nil {
		return s
	}
	var started time.Time
	for _, j := range jobs {
		if job != "" && !strings.HasPrefix(j.Name, job) {
			continue
		}
		if !j.StartedAt.IsZero() && (started.IsZero() || j.StartedAt.Before(started)) {
			started = j.StartedAt
		}
		want := j.Status == "in_progress"
		if s.State == stageFailed {
			want = j.Conclusion == "failure"
		}
		if !want {
			continue
		}
		s.Detail = j.Name
		for _, st := range j.Steps {
			if (s.State == stageRunning && st.Status == "in_progress") || (s.State == stageFailed && st.Conclusion == "failure") {
				s.Detail += " · " + st.Name
				break
			}
		}
		break
	}
	if s.State == stageRunning && !started.IsZero() {
		s.Duration = time.Since(started)
	}
	return s
}

func runJobDetails(repo string, id int64) ([]jobDetail, error) {
	var resp struct {
		Jobs []jobDetail `json:"jobs"`
	}
	err := ghGet(fmt.Sprintf("repos/%s/actions/runs/%d/jobs?per_page=100", repo, id), &resp)
	cleanJobs(resp.Jobs)
	return resp.Jobs, err
}

// ---- what the server runs ----

var (
	wholeHexLine = regexp.MustCompile(`(?m)^\s*([0-9a-f]{7,40})\s*$`)
	taggedHex    = regexp.MustCompile(`[:@-]([0-9a-f]{7,40})\b`)
	anyHex       = regexp.MustCompile(`\b([0-9a-f]{12,40})\b`)
)

// fetchVersion runs the env's version_command and pulls the commit out of what it prints: a line that is only a
// commit, else a commit tagging an image or a container name, else any long hex.
func fetchVersion(dir, command string) (string, error) {
	out, err := runIn(dir, "sh", "-c", command)
	if err != nil {
		return "", err
	}
	return clean(parseVersion(string(out))), nil
}

func parseVersion(out string) string {
	if m := wholeHexLine.FindAllStringSubmatch(out, -1); m != nil {
		return m[len(m)-1][1]
	}
	if m := taggedHex.FindAllStringSubmatch(out, -1); m != nil {
		return m[len(m)-1][1]
	}
	if m := anyHex.FindAllStringSubmatch(out, -1); m != nil {
		return m[len(m)-1][1]
	}
	return strings.TrimSpace(out)
}

// sameCommit compares a full SHA with a possibly shortened one.
func sameCommit(a, b string) bool {
	n := min(len(a), len(b))
	return n >= 7 && strings.EqualFold(a[:n], b[:n])
}

// ---- drawing ----

func stageLine(name string, s stage) string {
	icon, label := "", ""
	switch s.State {
	case stageNone:
		return ""
	case stageWaiting:
		icon, label = sMuted.Render("○"), sMuted.Render("waiting")
	case stageRunning:
		icon, label = sWarn.Render("◐"), sWarn.Render("running")
	case stageDone:
		icon, label = sGood.Render("✓"), sGood.Render("passed")
		if name == "Deploy" {
			label = sGood.Render("deployed")
		}
	case stageFailed:
		icon, label = sBad.Render("✗"), sBad.Render("failed")
	case stageSkipped:
		icon, label = sMuted.Render("–"), sMuted.Render("skipped")
	}
	line := fmt.Sprintf("  %s %s %s", pad(name, 7), icon, pad(label, 9))
	if s.Duration > 0 {
		line += sMuted.Render(pad(s.Duration.Round(time.Second).String(), 8))
	} else {
		line += pad("", 8)
	}
	if s.Detail != "" {
		line += " " + s.Detail
	}
	return line
}

// serverLine says what the server runs compared with the branch head.
func serverLine(p pipeline, version string, verr error, checking bool) string {
	head := "  " + pad("Server", 7) + " "
	switch {
	case version == "" && verr != nil:
		return head + sBad.Render("? ") + sMuted.Render(truncate(errText(verr), 70))
	case version == "" && checking:
		return head + sMuted.Render("○ checking…")
	case version == "":
		return head + sMuted.Render("○ unknown")
	case sameCommit(version, p.Head.SHA):
		return head + sGood.Render("✓ live") + "    " + short(version) + sMuted.Render(" is the newest commit")
	case p.Deploy.State == stageRunning || p.Deploy.State == stageWaiting:
		return head + sMuted.Render("○ still on ") + short(version) + sMuted.Render(" until the deploy finishes")
	default:
		return head + sWarn.Render("! runs ") + short(version) + sWarn.Render(", not the newest commit")
	}
}

func headLine(p pipeline) string {
	c := p.Head
	who := ""
	if c.Author != "" {
		who = " by " + c.Author
	}
	return sBold.Render("Newest commit ") + short(c.SHA) + "  " + truncate(c.Message, 60) + sMuted.Render(who+" · "+age(c.Date)+" ago")
}
