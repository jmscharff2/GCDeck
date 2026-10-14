package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// runInit writes (or adds an env to) .gcdeck.json by looking at a Google Cloud project and the repo in the current
// directory.
func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	project := fs.String("project", "", "Google Cloud project id (default: gcloud's configured project)")
	envName := fs.String("env", "production", "name of the env to add")
	branch := fs.String("branch", "", "branch that deploys to this env (default: the repo's default branch)")
	out := fs.String("out", localConfigName, "config file to write")
	force := fs.Bool("force", false, "replace an env of the same name")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "Usage: gcdeck init [flags]\n\nRun it in your app's repo. Run it again with -env and -project to add more envs.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *project == "" {
		*project = gcloudValue("core/project")
		if *project == "" {
			return errors.New("no project: pass -project or run `gcloud config set project <id>`")
		}
	}

	var cfg Config
	// The written file is trusted only if what was there before was: adding an env must not vouch for someone
	// else's commands already in the file.
	vouch := initMayVouch(*out)
	if data, err := os.ReadFile(*out); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("%s: %w", *out, err)
		}
		for i, e := range cfg.Envs {
			if e.Name == *envName {
				if !*force {
					return fmt.Errorf("%s already has an env named %s (use -force to replace it, or -env for another name)", *out, *envName)
				}
				cfg.Envs = append(cfg.Envs[:i], cfg.Envs[i+1:]...)
				break
			}
		}
	}

	dir, _ := os.Getwd()
	fmt.Printf("Looking at %s in project %s…\n", dir, *project)
	env, found, notes, err := discover(*project, dir)
	if err != nil {
		return err
	}
	env.Name = *envName
	env.Branch = *branch
	if env.Branch == "" {
		env.Branch = defaultBranch(dir)
	}
	env.Protected = strings.Contains(*envName, "prod")

	g := guessStack(dir, env, found)
	env.HealthURL, env.Actions, env.Vars, env.VersionCommand = g.health, g.actions, g.vars, g.version
	notes = append(notes, g.notes...)

	if cfg.GitHubRepo == "" && detectGitHubRepo(dir) == "" {
		notes = append(notes, "no GitHub remote found: CI and deploy panels are off until you set github_repo")
	}
	ci, deploy := findWorkflows(dir)
	if cfg.CIWorkflow == "" {
		cfg.CIWorkflow = ci
	}
	if deploy != "" {
		env.Deploy = &Deploy{Workflow: deploy}
		if env.Branch != "" && len(cfg.Envs) > 0 {
			notes = append(notes, fmt.Sprintf("several envs deploy with %s: if it is one workflow for all of them, set "+
				`deploy.job to the job name each env runs, and deploy.inputs to what workflow_dispatch expects`, deploy))
		} else {
			notes = append(notes, fmt.Sprintf("deploy uses %s; add deploy.inputs if its workflow_dispatch takes any", deploy))
		}
	} else if g.deploy != nil {
		env.Deploy = g.deploy
		notes = append(notes, "no deploy workflow found, so d runs this here: "+g.deploy.Command)
	} else {
		notes = append(notes, "no deploy workflow found: set deploy.workflow, or deploy.command to run something locally")
	}
	cfg.Envs = append(cfg.Envs, env)

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		return err
	}
	if vouch {
		written := Config{path: *out, hash: digest(data)}
		if err := saveTrust(written.absPath(), written.hash); err != nil {
			fmt.Fprintln(os.Stderr, "gcdeck init: could not record trust:", err)
		}
	}

	fmt.Printf("\nWrote env %q to %s:\n", env.Name, *out)
	app := or(g.framework.name, "an app")
	fmt.Printf("  %-9s %s, run by %s\n", "app", app, map[string]string{"kamal": "Kamal", "compose": "Docker Compose on a VM",
		"gke": "GKE", "pm2": "pm2 on a VM", "cloudrun": "Cloud Run", "vm": "a VM", "none": "nothing gcdeck recognized"}[g.target.name])
	if env.HealthURL != "" {
		fmt.Printf("  %-9s %s\n", "health", env.HealthURL)
	}
	for _, r := range env.Resources {
		fmt.Printf("  %-9s %s\n", r.Type, or(r.Name, r.Repo))
	}
	for _, a := range env.Actions {
		note := ""
		if a.Confirm {
			note = "  (asks first)"
		}
		fmt.Printf("  %-9s %s: %s%s\n", "key "+a.Key, a.Label, env.expand(a.Command), note)
	}
	if len(notes) > 0 {
		fmt.Println("\nCheck:")
		for _, n := range notes {
			fmt.Println("  -", n)
		}
	}
	fmt.Println("\nEdit it as you like (README.md lists every field), check it with `gcdeck check`, then run gcdeck.")
	return nil
}

// initMayVouch says whether init can trust the file it writes: yes for a new file, or one already trusted as it is.
func initMayVouch(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	return trustState(Config{path: path, hash: digest(data)}) == "trusted"
}

func gcloudValue(prop string) string {
	out, err := exec.Command("gcloud", "config", "get-value", prop).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// discover lists what the project runs. A listing that fails (an API not enabled, say) is skipped with a note; an
// expired login stops everything, since nothing else would work either.
func discover(project, dir string) (Env, discovered, []string, error) {
	env := Env{Project: project, Region: gcloudValue("compute/region"), Zone: gcloudValue("compute/zone")}
	d := discovered{runURLs: map[string]string{}, vmInternal: map[string]bool{}}
	type listing struct {
		what string
		args []string
		into func([]byte) ([]Resource, error)
	}
	listings := []listing{
		{"VMs", []string{"compute", "instances", "list"}, func(b []byte) ([]Resource, error) { return parseVMs(b, d.vmInternal) }},
		{"Cloud SQL", []string{"sql", "instances", "list"}, parseSQL},
		{"Cloud Run", []string{"run", "services", "list"}, func(b []byte) ([]Resource, error) { return parseCloudRun(b, d.runURLs) }},
		{"Cloud Run jobs", []string{"run", "jobs", "list"}, parseRunJobs},
		{"GKE", []string{"container", "clusters", "list"}, parseGKE},
		{"Artifact Registry", []string{"artifacts", "repositories", "list"}, parseRepos},
	}
	found := make([][]Resource, len(listings))
	errs := make([]error, len(listings))
	var wg sync.WaitGroup
	for i, l := range listings {
		wg.Add(1)
		go func(i int, l listing) {
			defer wg.Done()
			out, err := run("gcloud", append(l.args, "--project="+project, "--format=json", "--quiet")...)
			if err == nil {
				found[i], err = l.into(out)
			}
			errs[i] = err
		}(i, l)
	}
	wg.Wait()

	var notes []string
	skipped := map[string]bool{}
	for i, err := range errs {
		if errors.Is(err, errAuth) {
			return env, d, nil, errors.New("gcloud login expired: run `gcloud auth login`, then try again")
		}
		// With the Cloud Run API off, listing jobs fails too, with a confusing permission error; one note is enough.
		if err != nil && listings[i].what == "Cloud Run jobs" && skipped["Cloud Run"] {
			continue
		}
		if err != nil {
			skipped[listings[i].what] = true
			msg := truncate(err.Error(), 100)
			if strings.Contains(msg, "has not been used") || strings.Contains(msg, "SERVICE_DISABLED") || strings.Contains(msg, "is not enabled") {
				msg = "its API is not enabled in this project"
			}
			notes = append(notes, fmt.Sprintf("skipped %s: %s", listings[i].what, msg))
		}
		for _, r := range found[i] {
			if r.Type == "runjob" {
				d.runJobs = append(d.runJobs, r) // offered as actions, not shown as cards
			} else {
				env.Resources = append(env.Resources, r)
			}
		}
	}
	if len(env.Resources) == 0 {
		notes = append(notes, "found no resources: add them under resources (types: "+strings.Join(resourceTypeNames, ", ")+")")
	}

	// Fill env defaults from what was found, so resources can leave out their zone and region.
	for _, r := range env.Resources {
		if env.Zone == "" && r.Zone != "" {
			env.Zone = r.Zone
		}
		if env.Region == "" && r.Region != "" {
			env.Region = r.Region
		}
	}
	if env.Region == "" && len(env.Zone) > 2 {
		env.Region = env.Zone[:strings.LastIndex(env.Zone, "-")]
	}
	for i := range env.Resources {
		r := &env.Resources[i]
		if r.Zone == env.Zone {
			r.Zone = ""
		}
		if r.Region == env.Region {
			r.Region = ""
		}
	}

	for i := range d.runJobs {
		if d.runJobs[i].Region == env.Region {
			d.runJobs[i].Region = ""
		}
	}
	return env, d, notes, nil
}

// parseVMs also records which VMs have no external IP, since SSH reaches those only through IAP.
func parseVMs(out []byte, internal map[string]bool) ([]Resource, error) {
	var raw []struct {
		Name, Zone        string
		NetworkInterfaces []struct {
			AccessConfigs []struct {
				NatIP string `json:"natIP"`
			} `json:"accessConfigs"`
		} `json:"networkInterfaces"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	var rs []Resource
	for _, v := range raw {
		// GKE node VMs show on the cluster's card instead.
		if strings.HasPrefix(v.Name, "gke-") {
			continue
		}
		external := false
		for _, ni := range v.NetworkInterfaces {
			for _, ac := range ni.AccessConfigs {
				external = external || ac.NatIP != ""
			}
		}
		internal[v.Name] = !external
		rs = append(rs, Resource{Type: "vm", Name: v.Name, Zone: path.Base(v.Zone)})
	}
	return rs, nil
}

func parseSQL(out []byte) ([]Resource, error) {
	var raw []struct{ Name, Region string }
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	var rs []Resource
	for _, v := range raw {
		rs = append(rs, Resource{Type: "sql", Name: v.Name, Region: v.Region})
	}
	return rs, nil
}

type runListItem struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Status struct {
		URL string `json:"url"`
	} `json:"status"`
}

func parseCloudRun(out []byte, urls map[string]string) ([]Resource, error) {
	var raw []runListItem
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	var rs []Resource
	for _, v := range raw {
		rs = append(rs, Resource{Type: "cloudrun", Name: v.Metadata.Name, Region: v.Metadata.Labels["cloud.googleapis.com/location"]})
		urls[v.Metadata.Name] = v.Status.URL
	}
	return rs, nil
}

func parseRunJobs(out []byte) ([]Resource, error) {
	var raw []runListItem
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	var rs []Resource
	for _, v := range raw {
		rs = append(rs, Resource{Type: "runjob", Name: v.Metadata.Name, Region: v.Metadata.Labels["cloud.googleapis.com/location"]})
	}
	return rs, nil
}

func parseGKE(out []byte) ([]Resource, error) {
	var raw []struct{ Name, Location string }
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	var rs []Resource
	for _, v := range raw {
		rs = append(rs, Resource{Type: "gke", Name: v.Name, Location: v.Location})
	}
	return rs, nil
}

func parseRepos(out []byte) ([]Resource, error) {
	var raw []struct {
		Name   string `json:"name"` // projects/<p>/locations/<l>/repositories/<r>
		Format string `json:"format"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	var rs []Resource
	for _, v := range raw {
		parts := strings.Split(v.Name, "/")
		// gcf-artifacts and similar are Google-managed build caches, not your images.
		if v.Format != "DOCKER" || len(parts) != 6 || strings.HasPrefix(parts[5], "gcf-") {
			continue
		}
		rs = append(rs, Resource{Type: "images", Repo: parts[5], Region: parts[3]})
	}
	return rs, nil
}

var ciName = regexp.MustCompile(`^(ci|test|tests|build|checks?)\.ya?ml$`)

// findWorkflows picks a CI workflow and a deploy workflow you can dispatch: deploy.yml if there is one, else the first
// deploy* file. Reusable workflows (workflow_call without workflow_dispatch) cannot be started by hand, so they are skipped.
func findWorkflows(dir string) (ci, deploy string) {
	wf := filepath.Join(dir, ".github", "workflows")
	entries, _ := os.ReadDir(wf)
	var names []string
	for _, e := range entries {
		if n := e.Name(); strings.HasSuffix(n, ".yml") || strings.HasSuffix(n, ".yaml") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	dispatchable := func(n string) bool {
		data, err := os.ReadFile(filepath.Join(wf, n))
		return err == nil && (strings.Contains(string(data), "workflow_dispatch") || !strings.Contains(string(data), "workflow_call"))
	}
	for _, n := range names {
		if ci == "" && ciName.MatchString(n) {
			ci = n
		}
		if (n == "deploy.yml" || n == "deploy.yaml") && dispatchable(n) {
			deploy = n
		}
	}
	for _, n := range names {
		if deploy == "" && strings.HasPrefix(n, "deploy") && dispatchable(n) {
			deploy = n
		}
	}
	return ci, deploy
}

func defaultBranch(dir string) string {
	out, err := exec.Command("git", "-C", dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "origin/")
}
