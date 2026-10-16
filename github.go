package main

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

type Run struct {
	ID         int64     `json:"id"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	Branch     string    `json:"head_branch"`
	SHA        string    `json:"head_sha"`
	Event      string    `json:"event"`
	Title      string    `json:"display_title"`
	Created    time.Time `json:"created_at"`
	URL        string    `json:"html_url"`

	// Pending is set when the run has not reached the env's job yet, so it may or may not deploy this env.
	Pending  bool          `json:"-"`
	Duration time.Duration `json:"-"`
}

type job struct {
	Name        string
	Status      string
	Conclusion  string
	StartedAt   time.Time
	CompletedAt time.Time
}

type Compare struct {
	Ahead  int `json:"ahead_by"`
	Behind int `json:"behind_by"`
}

func fetchRuns(repo, workflow, branch string, limit int) ([]Run, error) {
	path := fmt.Sprintf("repos/%s/actions/workflows/%s/runs?per_page=%d", repo, url.PathEscape(workflow), limit)
	if branch != "" {
		path += "&branch=" + url.QueryEscape(branch)
	}
	var resp struct {
		Runs []Run `json:"workflow_runs"`
	}
	err := ghGet(path, &resp)
	for i := range resp.Runs {
		cleanRun(&resp.Runs[i])
	}
	return resp.Runs, err
}

// matchMode decides how a deploy run is attributed to env e.
func matchMode(cfg Config, e Env) string {
	if e.Deploy.Job != "" {
		return "job"
	}
	for _, other := range cfg.Envs {
		if other.Name != e.Name && other.Deploy != nil && other.Deploy.Workflow == e.Deploy.Workflow {
			return "branch"
		}
	}
	return "all"
}

// Finished runs never change, so their jobs are fetched once.
var (
	jobsMu    sync.Mutex
	jobsCache = map[int64][]job{}
)

func runJobs(repo string, r Run) ([]job, error) {
	jobsMu.Lock()
	cached, ok := jobsCache[r.ID]
	jobsMu.Unlock()
	if ok {
		return cached, nil
	}
	details, err := runJobDetails(repo, r.ID)
	if err != nil {
		return nil, err
	}
	jobs := make([]job, len(details))
	for i, d := range details {
		jobs[i] = job{Name: d.Name, Status: d.Status, Conclusion: d.Conclusion, StartedAt: d.StartedAt, CompletedAt: d.CompletedAt}
	}
	if r.Status == "completed" {
		jobsMu.Lock()
		jobsCache[r.ID] = jobs
		jobsMu.Unlock()
	}
	return jobs, nil
}

// fetchDeploys returns the recent deploy runs for env e, newest first.
func fetchDeploys(cfg Config, e Env) ([]Run, error) {
	d := e.Deploy
	switch matchMode(cfg, e) {
	case "all":
		return fetchRuns(cfg.GitHubRepo, d.Workflow, "", 6)
	case "branch":
		return fetchRuns(cfg.GitHubRepo, d.Workflow, e.Branch, 6)
	}

	// One workflow deploys several envs: read each run's jobs to see which env's job ran.
	runs, err := fetchRuns(cfg.GitHubRepo, d.Workflow, "", 20)
	if err != nil {
		return nil, err
	}
	var others []string
	for _, o := range cfg.Envs {
		if o.Name != e.Name && o.Deploy != nil && o.Deploy.Workflow == d.Workflow && o.Deploy.Job != "" {
			others = append(others, o.Deploy.Job)
		}
	}
	out := make([]*Run, len(runs))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for i := range runs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := runs[i]
			jobs, err := runJobs(cfg.GitHubRepo, r)
			if err != nil {
				mu.Lock()
				firstErr = err
				mu.Unlock()
				return
			}
			if classify(&r, jobs, d.Job, others) {
				out[i] = &r
			}
		}(i)
	}
	wg.Wait()
	var result []Run
	for _, r := range out {
		if r != nil {
			result = append(result, *r)
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Created.After(result[j].Created) })
	if len(result) > 6 {
		result = result[:6]
	}
	if len(result) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return result, nil
}

// classify reports whether run r belongs to the env whose jobs start with mine, and if so takes its status from that
// job. A run where another env's job ran is not ours. A run where no env's job ran is ours to show only while it is
// still deciding, or when it failed before getting there (a gate that blocked the deploy); one with every job skipped
// deployed nothing.
func classify(r *Run, jobs []job, mine string, others []string) bool {
	allSkipped := len(jobs) > 0
	for _, j := range jobs {
		allSkipped = allSkipped && j.Conclusion == "skipped"
	}
	if allSkipped || (len(jobs) == 0 && r.Conclusion == "skipped") {
		return false
	}
	// A job that is queued has not been decided on yet; only one that started or finished unskipped counts.
	ran := func(j job) bool {
		return j.Conclusion != "skipped" && j.Status != "queued" && j.Status != "waiting" && j.Status != "pending"
	}
	for _, j := range jobs {
		if strings.HasPrefix(j.Name, mine) && ran(j) {
			r.Status, r.Conclusion = j.Status, j.Conclusion
			r.Duration = jobDuration(j)
			return true
		}
	}
	for _, j := range jobs {
		for _, o := range others {
			if strings.HasPrefix(j.Name, o) && ran(j) && !strings.HasPrefix(j.Name, mine) {
				return false
			}
		}
	}
	if r.Status != "completed" {
		r.Pending = true
		return true
	}
	return r.Conclusion == "failure"
}

func jobDuration(j job) time.Duration {
	if j.StartedAt.IsZero() {
		return 0
	}
	end := j.CompletedAt
	if end.IsZero() || end.Before(j.StartedAt) {
		end = time.Now()
	}
	return end.Sub(j.StartedAt)
}

func fetchCompare(repo string, p *Promote) (Compare, error) {
	var c Compare
	// One commit per page is enough: only the counts are read.
	err := ghGet(fmt.Sprintf("repos/%s/compare/%s...%s?per_page=1", repo, url.PathEscape(p.To), url.PathEscape(p.From)), &c)
	return c, err
}

// workflowArgs builds `gh workflow run` for a workflow, ref and inputs.
func workflowArgs(repo, workflow, ref string, inputs map[string]string) []string {
	args := []string{"workflow", "run", workflow, "-R", repo, "--ref", ref}
	keys := make([]string, 0, len(inputs))
	for k := range inputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-f", k+"="+inputs[k])
	}
	return args
}
