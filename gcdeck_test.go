package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func j(name, status, conclusion string) job {
	t := time.Date(2026, 10, 13, 13, 0, 0, 0, time.UTC)
	return job{Name: name, Status: status, Conclusion: conclusion, StartedAt: t, CompletedAt: t.Add(5 * time.Minute)}
}

// The job shapes below are from a real workflow: a gate job, then one reusable-workflow call per env, where the
// env not chosen is skipped and keeps its bare name.
func TestClassify(t *testing.T) {
	cases := []struct {
		name       string
		run        Run
		jobs       []job
		want       bool
		pending    bool
		conclusion string
	}{
		{"this env deployed", Run{Status: "completed", Conclusion: "success"},
			[]job{j("gate", "completed", "success"), j("production / deploy", "completed", "success"), j("staging", "completed", "skipped")},
			true, false, "success"},
		{"other env deployed", Run{Status: "completed", Conclusion: "success"},
			[]job{j("gate", "completed", "success"), j("staging / deploy", "completed", "success"), j("production", "completed", "skipped")},
			false, false, ""},
		{"everything skipped", Run{Status: "completed", Conclusion: "skipped"},
			[]job{j("gate", "completed", "skipped"), j("staging", "completed", "skipped"), j("production", "completed", "skipped")},
			false, false, ""},
		{"gate blocked the deploy", Run{Status: "completed", Conclusion: "failure"},
			[]job{j("gate", "completed", "failure"), j("staging", "completed", "skipped"), j("production", "completed", "skipped")},
			true, false, "failure"},
		{"gate still running", Run{Status: "in_progress"},
			[]job{j("gate", "in_progress", ""), j("staging", "queued", ""), j("production", "queued", "")},
			true, true, ""},
		{"this env's deploy failing", Run{Status: "completed", Conclusion: "failure"},
			[]job{j("gate", "completed", "success"), j("production / deploy", "completed", "failure"), j("staging", "completed", "skipped")},
			true, false, "failure"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := c.run
			got := classify(&r, c.jobs, "production", []string{"staging"})
			if got != c.want {
				t.Fatalf("classify = %v, want %v", got, c.want)
			}
			if got && r.Pending != c.pending {
				t.Errorf("pending = %v, want %v", r.Pending, c.pending)
			}
			if got && !c.pending && r.Conclusion != c.conclusion {
				t.Errorf("conclusion = %q, want %q", r.Conclusion, c.conclusion)
			}
		})
	}
}

func TestClassifyTakesDurationFromEnvJob(t *testing.T) {
	r := Run{Status: "completed", Conclusion: "success"}
	classify(&r, []job{j("gate", "completed", "success"), j("production / deploy", "completed", "success")}, "production", nil)
	if r.Duration != 5*time.Minute {
		t.Errorf("duration = %v, want 5m", r.Duration)
	}
}

func TestMatchMode(t *testing.T) {
	cfg := Config{Envs: []Env{
		{Name: "prod", Deploy: &Deploy{Workflow: "deploy.yml"}},
		{Name: "stg", Deploy: &Deploy{Workflow: "deploy.yml"}},
		{Name: "preview", Deploy: &Deploy{Workflow: "preview.yml"}},
		{Name: "jobs", Deploy: &Deploy{Workflow: "deploy.yml", Job: "jobs"}},
	}}
	want := []string{"branch", "branch", "all", "job"}
	for i, e := range cfg.Envs {
		if got := matchMode(cfg, e); got != want[i] {
			t.Errorf("%s: matchMode = %s, want %s", e.Name, got, want[i])
		}
	}
}

func writeConfig(t *testing.T, v any) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, localConfigName)
	data, _ := json.Marshal(v)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigDefaults(t *testing.T) {
	p := writeConfig(t, map[string]any{
		"github_repo": "o/r",
		"envs": []any{map[string]any{"name": "prod", "project": "my-project", "branch": "main",
			"deploy": map[string]any{"workflow": "deploy.yml"}}},
	})
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppDir != filepath.Dir(p) {
		t.Errorf("app_dir = %s, want the config's directory", cfg.AppDir)
	}
	if cfg.RefreshSeconds != 30 {
		t.Errorf("refresh = %d", cfg.RefreshSeconds)
	}
	if cfg.Envs[0].Deploy.Ref != "main" {
		t.Errorf("deploy ref = %q, want the branch", cfg.Envs[0].Deploy.Ref)
	}
}

func TestLoadConfigRejects(t *testing.T) {
	env := func(extra map[string]any) map[string]any {
		e := map[string]any{"name": "prod", "project": "my-project"}
		for k, v := range extra {
			e[k] = v
		}
		return e
	}
	cases := map[string]map[string]any{
		"no envs":          {"envs": []any{}},
		"unknown field":    {"envs": []any{env(nil)}, "colour": "red"},
		"no project":       {"envs": []any{map[string]any{"name": "prod"}}},
		"duplicate env":    {"envs": []any{env(nil), env(nil)}},
		"bad type":         {"envs": []any{env(map[string]any{"resources": []any{map[string]any{"type": "lambda", "name": "x"}}})}},
		"images no repo":   {"envs": []any{env(map[string]any{"resources": []any{map[string]any{"type": "images"}}})}},
		"reserved key":     {"envs": []any{env(nil)}, "actions": []any{map[string]any{"key": "d", "label": "x", "command": "x"}}},
		"long key":         {"envs": []any{env(nil)}, "actions": []any{map[string]any{"key": "ll", "label": "x", "command": "x"}}},
		"deploy no ref":    {"envs": []any{env(map[string]any{"deploy": map[string]any{"workflow": "d.yml"}})}},
		"deploy empty":     {"envs": []any{env(map[string]any{"deploy": map[string]any{}})}},
		"promote no shape": {"envs": []any{env(nil)}, "promote": map[string]any{"from": "a", "to": "b"}},
		"flag as a name": {"envs": []any{env(map[string]any{"resources": []any{
			map[string]any{"type": "vm", "name": "--impersonate-service-account=x@y.iam.gserviceaccount.com"}}})}},
		"flag as a project": {"envs": []any{map[string]any{"name": "prod", "project": "--account=x"}}},
		"branch with ..":    {"envs": []any{env(map[string]any{"branch": "main/../../users"})}},
		"branch with ?":     {"envs": []any{env(map[string]any{"branch": "main?per_page=100"})}},
		"repo with shell":   {"github_repo": "acme/app$(id)", "envs": []any{env(nil)}},
		"health not http":   {"envs": []any{env(map[string]any{"health_url": "file:///etc/passwd"})}},
		"workflow path":     {"envs": []any{env(map[string]any{"branch": "main", "deploy": map[string]any{"workflow": "../x.yml"}})}},
		"input name":        {"envs": []any{env(map[string]any{"branch": "main", "deploy": map[string]any{"workflow": "d.yml", "inputs": map[string]any{"a b": "1"}}})}},
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadConfig(writeConfig(t, v)); err == nil {
				t.Error("loaded, want an error")
			}
		})
	}
}

func TestEnvActionsOverrideShared(t *testing.T) {
	cfg := Config{Actions: []Action{{Key: "l", Label: "logs", Command: "shared"}, {Key: "s", Label: "ssh", Command: "ssh"}}}
	e := Env{Actions: []Action{{Key: "l", Label: "logs", Command: "mine"}}}
	got := cfg.actions(e)
	if len(got) != 2 || got[0].Command != "mine" || got[1].Key != "s" {
		t.Errorf("actions = %+v", got)
	}
}

func TestExpand(t *testing.T) {
	e := Env{Name: "stg", Project: "p1", Zone: "z1", Region: "r1",
		Resources: []Resource{{Type: "vm", Name: "web"}, {Type: "vm", Name: "worker"}, {Type: "cloudrun", Name: "api"}},
		Vars:      map[string]string{"dest": " -d stg"}}
	got := e.expand("ssh {vm} {zone} {project} {cloudrun}{dest} ${HOME} {unknown}")
	want := "ssh web z1 p1 api -d stg ${HOME} {unknown}"
	if got != want {
		t.Errorf("expand = %q, want %q", got, want)
	}
}

func TestParseGitHubRemote(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/o/r.git":            "o/r",
		"https://github.com/o/r":                "o/r",
		"git@github.com:o/r.git":                "o/r",
		"ssh://git@github.com/o/r":              "o/r",
		"https://gitlab.com/o/r.git":            "",
		"git@github.com:acme/app$(id).git":      "",
		"https://github.com/acme/app;id>x;.git": "",
	} {
		if got := parseGitHubRemote(in); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}

func TestWorkflowArgsSortsInputs(t *testing.T) {
	got := workflowArgs("o/r", "d.yml", "main", map[string]string{"b": "2", "a": "1"})
	want := []string{"workflow", "run", "d.yml", "-R", "o/r", "--ref", "main", "-f", "a=1", "-f", "b=2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%v", got)
	}
}

func TestFirstTag(t *testing.T) {
	for in, want := range map[string]string{
		`["latest","abc123"]`: "abc123",
		`"latest,abc123"`:     "abc123",
		`["latest"]`:          "latest",
		`[]`:                  "untagged",
		`""`:                  "untagged",
	} {
		if got := firstTag(json.RawMessage(in)); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}

func TestImagePath(t *testing.T) {
	e := Env{Project: "p", Region: "us-east1"}
	if got := imagePath(e, Resource{Repo: "repo/img"}); got != "us-east1-docker.pkg.dev/p/repo/img" {
		t.Error(got)
	}
	full := "europe-west1-docker.pkg.dev/other/repo"
	if got := imagePath(e, Resource{Repo: full}); got != full {
		t.Error(got)
	}
}

func TestParseDiscovery(t *testing.T) {
	internal := map[string]bool{}
	vms, _ := parseVMs([]byte(`[{"name":"web","zone":"https://x/zones/us-central1-a","networkInterfaces":[{"accessConfigs":[{"natIP":"1.2.3.4"}]}]},
		{"name":"worker","zone":"z","networkInterfaces":[{}]},{"name":"gke-pool-1","zone":"z"}]`), internal)
	if len(vms) != 2 || vms[0].Zone != "us-central1-a" || internal["web"] || !internal["worker"] {
		t.Errorf("vms = %+v, internal = %v", vms, internal)
	}
	repos, _ := parseRepos([]byte(`[{"name":"projects/p/locations/us-central1/repositories/app","format":"DOCKER"},
		{"name":"projects/p/locations/us-central1/repositories/gcf-artifacts","format":"DOCKER"},
		{"name":"projects/p/locations/us-central1/repositories/npm","format":"NPM"}]`))
	if len(repos) != 1 || repos[0].Repo != "app" || repos[0].Region != "us-central1" {
		t.Errorf("repos = %+v", repos)
	}
	urls := map[string]string{}
	run, _ := parseCloudRun([]byte(`[{"metadata":{"name":"api","labels":{"cloud.googleapis.com/location":"europe-west1"}},"status":{"url":"https://api-x.run.app"}}]`), urls)
	if len(run) != 1 || run[0].Region != "europe-west1" || urls["api"] != "https://api-x.run.app" {
		t.Errorf("cloudrun = %+v %v", run, urls)
	}
}

func TestFindWorkflows(t *testing.T) {
	dir := t.TempDir()
	wf := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(n, body string) { _ = os.WriteFile(filepath.Join(wf, n), []byte(body), 0o644) }
	write("ci.yml", "on: push")
	write("deploy-to.yml", "on:\n  workflow_call:\n")
	write("deploy.yml", "on:\n  workflow_dispatch:\n")
	write("lint.yml", "on: push")
	ci, deploy := findWorkflows(dir)
	if ci != "ci.yml" || deploy != "deploy.yml" {
		t.Errorf("ci=%s deploy=%s, want ci.yml deploy.yml", ci, deploy)
	}
	_ = os.Remove(filepath.Join(wf, "deploy.yml"))
	if _, deploy := findWorkflows(dir); deploy != "" {
		t.Errorf("picked reusable %s", deploy)
	}
}

func TestFormatJSONLog(t *testing.T) {
	got := formatLogLine(`2026-10-13T21:45:21.292807605Z {"time":"x","level":"INFO","msg":"Request","request":{"method":"GET","path":"/up"},"status":200,"request_id":"abc"}`)
	if got.prefix == "" {
		t.Error("docker timestamp not turned into a prefix")
	}
	if want := "INFO  Request method=GET path=/up status=200 request_id=abc"; got.text != want {
		t.Errorf("text = %q\nwant   %q", got.text, want)
	}
	if bad := formatLogLine(`{"level":"INFO","msg":"Request","status":503}`); bad.style.GetForeground() != sBad.GetForeground() {
		t.Error("a 5xx should be styled as bad")
	}
	if plain := formatLogLine("not json {"); plain.text != "not json {" {
		t.Errorf("plain line changed: %q", plain.text)
	}
}

func TestJobLogIngestAddsStepHeaders(t *testing.T) {
	t0 := time.Date(2026, 10, 13, 13, 17, 30, 0, time.UTC)
	steps := []step{
		{Name: "Set up job", Status: "completed", Conclusion: "success", StartedAt: t0, CompletedAt: t0.Add(3 * time.Second)},
		{Name: "Deploy", Status: "completed", Conclusion: "failure", StartedAt: t0.Add(3 * time.Second), CompletedAt: t0.Add(9 * time.Second)},
	}
	ingest := jobLogIngest(steps)
	var out []string
	for _, l := range []string{
		stripANSI("deploy\tUNKNOWN STEP\t\ufeff2026-10-13T13:17:30.5000000Z Current runner version"),
		"deploy\tUNKNOWN STEP\t2026-10-13T13:17:31.0000000Z more setup",
		"deploy\tUNKNOWN STEP\t2026-10-13T13:17:34.2000000Z ##[error]boom",
	} {
		out = append(out, ingest(l)...)
	}
	var headers []string
	for _, l := range out {
		if strings.HasPrefix(l, markHeader) {
			headers = append(headers, l)
		}
	}
	if !strings.HasPrefix(out[0], "") || strings.HasPrefix(out[0], "2026") || !strings.HasPrefix(out[1], markHeader) {
		t.Errorf("the first line should come after its step header: %q", out[:2])
	}
	if len(headers) != 2 || !strings.Contains(headers[0], "Set up job") || !strings.Contains(headers[1], "Deploy") {
		t.Errorf("headers = %q", headers)
	}
}

func TestWrapStyled(t *testing.T) {
	rows := wrapStyled(styled{prefix: "12:00:00 ", text: strings.Repeat("x", 25)}, 19)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	if !strings.HasPrefix(stripANSI(rows[1]), strings.Repeat(" ", 9)) {
		t.Errorf("continuation not indented under the prefix: %q", stripANSI(rows[1]))
	}
	if got := wrapStyled(styled{text: ""}, 20); len(got) != 1 {
		t.Errorf("empty line should still take a row, got %d", len(got))
	}
}

func TestStripANSI(t *testing.T) {
	if got := stripANSI("\ufeff\x1b[31mred\x1b[0m plain"); got != "red plain" {
		t.Errorf("%q", got)
	}
}

func TestTableReflow(t *testing.T) {
	v := newCommandView("t", ".", "true")
	v.lastWidth = 80
	in := []string{
		"App Host: 10.0.0.1",
		"CONTAINER ID   IMAGE                                                                          COMMAND                  CREATED       STATUS       PORTS     NAMES",
		"cfed41329ab1   us-central1-docker.pkg.dev/p/app/app:f0e37dfb746100cf7a7dc272e1dedcafbff8ba78   \"/rails/bin/docker-e…\"   8 hours ago   Up 8 hours   80/tcp    app-web-f0e37dfb746100cf7a7dc272e1dedcafbff8ba78",
		"0123456789ab   redis:7                                                                        \"redis\"                  2 days ago    Exited (1)             app-redis",
		"  INFO [445379a9] Finished",
	}
	var out []string
	for _, l := range in {
		out = append(out, v.tablePush(l)...)
	}
	out = append(out, v.tableFlush()...)
	if len(out) != 5 {
		t.Fatalf("got %d lines: %q", len(out), out)
	}
	if out[1][:2] != tableHeaderRow || out[2][:2] != tableGoodRow || out[3][:2] != tableBadRow {
		t.Errorf("row marks: %q", out[1:4])
	}
	for _, l := range out[1:4] {
		if w := len([]rune(l)) - 2; w > 80 {
			t.Errorf("row is %d wide, want ≤ 80: %q", w, l)
		}
	}
	if !strings.Contains(out[2], "app:f0e37dfb7461") || strings.Contains(out[2], "docker.pkg.dev") {
		t.Errorf("image not shortened: %q", out[2])
	}
	if out[4] != "  INFO [445379a9] Finished" {
		t.Errorf("line after the table: %q", out[4])
	}
}
