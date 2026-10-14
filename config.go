package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Config describes the environments one app deploys to. It usually lives in the app's repo as .gcdeck.json.
type Config struct {
	// AppDir is where commands and actions run. Defaults to the directory holding .gcdeck.json.
	AppDir string `json:"app_dir,omitempty"`
	// GitHubRepo is owner/name. Defaults to the origin remote of AppDir. Empty and undetectable hides GitHub panels.
	GitHubRepo string `json:"github_repo,omitempty"`
	// CIWorkflow is shown per env for the env's branch, e.g. "ci.yml".
	CIWorkflow     string   `json:"ci_workflow,omitempty"`
	RefreshSeconds int      `json:"refresh_seconds,omitempty"`
	Promote        *Promote `json:"promote,omitempty"`
	// Actions apply to every env; an env's own action with the same key replaces one of these.
	Actions []Action `json:"actions,omitempty"`
	Envs    []Env    `json:"envs"`

	path string
	hash string // of the bytes parsed, for the trust check
}

type Env struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	Region  string `json:"region,omitempty"` // default for resources that take a region
	Zone    string `json:"zone,omitempty"`   // default for VMs
	Branch  string `json:"branch,omitempty"` // the branch that deploys here
	// Protected makes deploying ask you to type the env's name.
	Protected bool `json:"protected,omitempty"`
	// HealthURL is fetched each refresh; any 2xx is up.
	HealthURL string `json:"health_url,omitempty"`
	// VersionCommand prints the commit the server runs (kamal app version, an image tag), so gcdeck can tell when a
	// deploy has reached it.
	VersionCommand string     `json:"version_command,omitempty"`
	Resources      []Resource `json:"resources,omitempty"`
	Deploy         *Deploy    `json:"deploy,omitempty"`
	Actions        []Action   `json:"actions,omitempty"`
	// Vars are extra {placeholders} for this env's action commands.
	Vars map[string]string `json:"vars,omitempty"`
}

// Resource is one thing to show a status card for.
type Resource struct {
	Type     string `json:"type"` // vm, sql, cloudrun, gke, images
	Name     string `json:"name,omitempty"`
	Zone     string `json:"zone,omitempty"`     // vm
	Region   string `json:"region,omitempty"`   // cloudrun, images
	Location string `json:"location,omitempty"` // gke: region or zone
	// Repo is an Artifact Registry repository or image: "repo", "repo/image", or a full
	// "<region>-docker.pkg.dev/<project>/<repo>[/<image>]" path.
	Repo string `json:"repo,omitempty"`
}

// Deploy is how d ships an env: a GitHub Actions workflow, or a command run in a pane.
type Deploy struct {
	Workflow string            `json:"workflow,omitempty"`
	Ref      string            `json:"ref,omitempty"` // defaults to the env's branch
	Inputs   map[string]string `json:"inputs,omitempty"`
	// Job matches runs to this env by job name prefix, for one workflow that deploys several envs. Without it,
	// runs are matched by branch when envs share a workflow, and all runs count when the workflow is the env's own.
	Job     string `json:"job,omitempty"`
	Command string `json:"command,omitempty"`
}

// Promote moves one branch onto another (staging to main, say), by workflow or command.
type Promote struct {
	From     string            `json:"from"`
	To       string            `json:"to"`
	Workflow string            `json:"workflow,omitempty"`
	Ref      string            `json:"ref,omitempty"` // defaults to From
	Inputs   map[string]string `json:"inputs,omitempty"`
	Command  string            `json:"command,omitempty"`
}

// Action is a key that runs a command. Its output shows inside gcdeck, streaming, unless it is interactive (a console,
// a shell), which gets a real terminal: a Herdr pane beside gcdeck, or this terminal outside Herdr.
type Action struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Command     string `json:"command"`
	Interactive bool   `json:"interactive,omitempty"`
	// Confirm asks before running, for commands that change things (a migration, a job). A protected env asks you to
	// type its name.
	Confirm bool `json:"confirm,omitempty"`
	// Pane opens it in a Herdr pane beside gcdeck even though it is not interactive, to keep it running in view.
	Pane bool `json:"pane,omitempty"`
}

var resourceTypeNames = []string{"vm", "sql", "cloudrun", "gke", "images"}

// reservedKeys are gcdeck's own; actions cannot take them.
var reservedKeys = map[string]bool{
	"q": true, "?": true, "r": true, "j": true, "k": true, "w": true, "o": true, "g": true, "d": true, "p": true,
	"a": true, "e": true, "H": true, "L": true, "1": true, "2": true, "3": true, "4": true, "5": true, "6": true,
	"7": true, "8": true, "9": true,
}

const localConfigName = ".gcdeck.json"

func globalConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "gcdeck", "config.json")
}

// findConfig picks the config: an explicit path, $GCDECK_CONFIG, .gcdeck.json here or in a parent, then the global file.
func findConfig(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if p := os.Getenv("GCDECK_CONFIG"); p != "" {
		return p, nil
	}
	if dir, err := os.Getwd(); err == nil {
		for {
			p := filepath.Join(dir, localConfigName)
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if p := globalConfigPath(); fileExists(p) {
		return p, nil
	}
	return "", errors.New("no config found: run `gcdeck init` in your app's directory to create " + localConfigName)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func loadConfig(path string) (Config, error) {
	// Ownership is checked on the file that is read, not on the path, so it cannot be swapped in between.
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Config{}, err
	}
	if err := checkOwnership(path, info); err != nil {
		return Config{}, err
	}
	data, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	cfg.path, cfg.hash = path, digest(data)
	if err := cfg.normalize(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// normalize fills defaults and rejects configs that would misbehave at runtime.
func (c *Config) normalize() error {
	if c.AppDir == "" {
		if filepath.Base(c.path) == localConfigName {
			c.AppDir = filepath.Dir(c.path)
		} else {
			c.AppDir, _ = os.Getwd()
		}
	}
	c.AppDir = expandHome(c.AppDir)
	if c.RefreshSeconds <= 0 {
		c.RefreshSeconds = 30
	}
	if c.GitHubRepo == "" {
		c.GitHubRepo = detectGitHubRepo(c.AppDir)
	}
	if err := checkPattern("github_repo", c.GitHubRepo, githubRepoPattern); err != nil {
		return err
	}
	if err := checkPattern("ci_workflow", c.CIWorkflow, workflowPattern); err != nil {
		return err
	}
	if len(c.Envs) == 0 {
		return errors.New("no envs")
	}
	if c.Promote != nil {
		if c.Promote.From == "" || c.Promote.To == "" {
			return errors.New("promote needs from and to")
		}
		if c.Promote.Workflow == "" && c.Promote.Command == "" {
			return errors.New("promote needs a workflow or a command")
		}
		if c.Promote.Ref == "" {
			c.Promote.Ref = c.Promote.From
		}
		for what, b := range map[string]string{"promote.from": c.Promote.From, "promote.to": c.Promote.To, "promote.ref": c.Promote.Ref} {
			if err := checkBranch(what, b); err != nil {
				return err
			}
		}
		if err := checkPattern("promote.workflow", c.Promote.Workflow, workflowPattern); err != nil {
			return err
		}
		if err := checkInputs("promote", c.Promote.Inputs); err != nil {
			return err
		}
	}
	if err := checkActions("actions", c.Actions); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i := range c.Envs {
		e := &c.Envs[i]
		if e.Name == "" || e.Project == "" {
			return fmt.Errorf("envs[%d] needs a name and a project", i)
		}
		if seen[e.Name] {
			return fmt.Errorf("env %q appears twice", e.Name)
		}
		seen[e.Name] = true
		for _, check := range []error{
			checkPattern("env name", e.Name, envNamePattern),
			checkPattern("env "+e.Name+": project", e.Project, projectPattern),
			checkPattern("env "+e.Name+": region", e.Region, locationPattern),
			checkPattern("env "+e.Name+": zone", e.Zone, locationPattern),
			checkBranch("env "+e.Name+": branch", e.Branch),
			checkHealthURL(*e),
		} {
			if check != nil {
				return check
			}
		}
		for j, r := range e.Resources {
			if !contains(resourceTypeNames, r.Type) {
				return fmt.Errorf("env %s: resources[%d]: type %q is not one of %s", e.Name, j, r.Type,
					strings.Join(resourceTypeNames, ", "))
			}
			if r.Type == "images" && r.Repo == "" {
				return fmt.Errorf("env %s: images needs repo", e.Name)
			}
			if r.Type != "images" && r.Name == "" {
				return fmt.Errorf("env %s: %s needs a name", e.Name, r.Type)
			}
			if r.Type == "images" && !imageRepoValid(r.Repo) {
				return fmt.Errorf("env %s: images repo %q is not valid", e.Name, r.Repo)
			}
			for _, check := range []error{
				checkPattern("env "+e.Name+": "+r.Type+" name", r.Name, namePattern),
				checkPattern("env "+e.Name+": zone", r.Zone, locationPattern),
				checkPattern("env "+e.Name+": region", r.Region, locationPattern),
				checkPattern("env "+e.Name+": location", r.Location, locationPattern),
			} {
				if check != nil {
					return check
				}
			}
		}
		if d := e.Deploy; d != nil {
			if d.Workflow == "" && d.Command == "" {
				return fmt.Errorf("env %s: deploy needs a workflow or a command", e.Name)
			}
			if d.Ref == "" {
				d.Ref = e.Branch
			}
			if d.Workflow != "" && d.Ref == "" {
				return fmt.Errorf("env %s: a workflow deploy needs the env's branch or deploy.ref", e.Name)
			}
			if err := checkBranch("env "+e.Name+": deploy.ref", d.Ref); err != nil {
				return err
			}
			if err := checkPattern("env "+e.Name+": deploy.workflow", d.Workflow, workflowPattern); err != nil {
				return err
			}
			if err := checkInputs("env "+e.Name+" deploy", d.Inputs); err != nil {
				return err
			}
		}
		if err := checkActions("env "+e.Name+" actions", e.Actions); err != nil {
			return err
		}
	}
	return nil
}

func checkHealthURL(e Env) error {
	if e.HealthURL == "" {
		return nil
	}
	u, err := url.Parse(e.HealthURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("env %s: health_url must be an http or https URL", e.Name)
	}
	return nil
}

func checkActions(where string, actions []Action) error {
	keys := map[string]bool{}
	for _, a := range actions {
		if len([]rune(a.Key)) != 1 {
			return fmt.Errorf("%s: key %q must be one character", where, a.Key)
		}
		if reservedKeys[a.Key] {
			return fmt.Errorf("%s: key %q is one of gcdeck's own", where, a.Key)
		}
		if keys[a.Key] {
			return fmt.Errorf("%s: key %q used twice", where, a.Key)
		}
		if a.Command == "" || a.Label == "" {
			return fmt.Errorf("%s: %q needs a label and a command", where, a.Key)
		}
		keys[a.Key] = true
	}
	return nil
}

// actions are the env's own plus the shared ones it does not override, sorted by key.
func (c Config) actions(e Env) []Action {
	byKey := map[string]Action{}
	for _, a := range c.Actions {
		byKey[a.Key] = a
	}
	for _, a := range e.Actions {
		byKey[a.Key] = a
	}
	out := make([]Action, 0, len(byKey))
	for _, a := range byKey {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

var placeholder = regexp.MustCompile(`\{([a-z_][a-z0-9_]*)\}`)

// expand fills {env}, {project}, {region}, {zone}, {branch}, {vm}, {sql}, {cloudrun}, {gke}, {gcdeck} (this
// program) and the env's vars, whose values may use placeholders too. Unknown placeholders are left as written, so
// shell braces survive.
func (e Env) expand(s string) string {
	self, err := os.Executable()
	if err != nil {
		self = "gcdeck"
	}
	vals := map[string]string{"env": e.Name, "project": e.Project, "region": e.Region, "zone": e.Zone, "branch": e.Branch,
		"gcdeck": shellQuote(self)}
	for _, r := range e.Resources {
		if _, ok := vals[r.Type]; !ok && r.Name != "" {
			vals[r.Type] = r.Name
		}
	}
	for k, v := range e.Vars {
		vals[k] = v
	}
	// A few passes, so a var can refer to another or to {project}; a var that refers to itself stops here.
	for i := 0; i < 4; i++ {
		next := placeholder.ReplaceAllStringFunc(s, func(m string) string {
			if v, ok := vals[m[1:len(m)-1]]; ok {
				return v
			}
			return m
		})
		if next == s {
			break
		}
		s = next
	}
	return s
}

var remotePattern = regexp.MustCompile(`github\.com[:/]([^/]+/[^/]+?)(?:\.git)?/?$`)

// Patterns for config values that become command arguments or URL paths. They reject a leading "-", which a CLI
// would read as a flag (a resource "named" --impersonate-service-account=…), and anything a shell or URL would treat
// specially.
var (
	githubRepoPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}$`)
	// Project ids, including legacy domain-scoped ones (example.com:my-project).
	projectPattern   = regexp.MustCompile(`^(?:[a-z0-9][a-z0-9.-]*[a-z0-9]:)?[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	locationPattern  = regexp.MustCompile(`^[a-z]+(?:-[a-z]+)*[0-9]+(?:-[a-z])?$`)
	namePattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)
	imageRepoPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(?:/[a-z0-9._-]+)*$`)
	// Branch names: a safe subset of what git allows.
	branchPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	workflowPattern = regexp.MustCompile(`^(?:[A-Za-z0-9][A-Za-z0-9._-]*\.ya?ml|[0-9]+)$`)
	inputKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	envNamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$`)
)

func checkBranch(what, b string) error {
	if b == "" {
		return nil
	}
	if !branchPattern.MatchString(b) || strings.Contains(b, "..") || strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".lock") {
		return fmt.Errorf("%s %q is not a branch name gcdeck accepts (letters, digits, . _ / -, not starting with -)", what, b)
	}
	return nil
}

func checkPattern(what, v string, p *regexp.Regexp) error {
	if v != "" && !p.MatchString(v) {
		return fmt.Errorf("%s %q is not valid", what, v)
	}
	return nil
}

func checkInputs(where string, inputs map[string]string) error {
	for k := range inputs {
		if !inputKeyPattern.MatchString(k) {
			return fmt.Errorf("%s: input name %q is not valid", where, k)
		}
	}
	return nil
}

// imageRepoValid accepts "repo", "repo/image", or a full <region>-docker.pkg.dev/<project>/<repo>[/<image>] path.
func imageRepoValid(repo string) bool {
	if host, rest, ok := strings.Cut(repo, "/"); ok && strings.HasSuffix(host, "-docker.pkg.dev") {
		region := strings.TrimSuffix(host, "-docker.pkg.dev")
		project, path, ok := strings.Cut(rest, "/")
		return ok && locationPattern.MatchString(region) && projectPattern.MatchString(project) && imageRepoPattern.MatchString(path)
	}
	return imageRepoPattern.MatchString(repo)
}

func detectGitHubRepo(dir string) string {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return parseGitHubRemote(strings.TrimSpace(string(out)))
}

// parseGitHubRemote returns owner/name, or "" when the remote is not GitHub or the name holds anything GitHub would
// not allow, shell syntax included.
func parseGitHubRemote(url string) string {
	if m := remotePattern.FindStringSubmatch(url); m != nil && githubRepoPattern.MatchString(m[1]) {
		return m[1]
	}
	return ""
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
