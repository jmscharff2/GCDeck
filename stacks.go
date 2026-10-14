package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// init's guesses about an app come from two questions answered separately: where it runs (Kamal, Docker Compose on a
// VM, GKE, pm2 on a VM, Cloud Run), which says how to get logs and status and how to run a command inside the
// deployed app; and what it is (Rails, Django, Node, Python), which says which commands are worth a key.

// target is where the app runs.
type target struct {
	name   string
	logs   string
	status string
	// version prints the commit the server runs.
	version string
	// exec runs a command inside the deployed app; tty does it with a terminal, for consoles. nil when the target
	// cannot (Cloud Run has no way in).
	exec, tty func(cmd string) string
	deploy    string
	vars      map[string]string
	notes     []string
}

// framework is what the app is.
type framework struct {
	name       string
	console    string
	migrations string // read-only migration status
	migrate    string // applies migrations; always asks first
}

// stackGuess is everything init derives from the repo and the project's resources.
type stackGuess struct {
	target    target
	framework framework
	health    string
	version   string
	actions   []Action
	deploy    *Deploy
	vars      map[string]string
	notes     []string
}

// discovered holds what the gcloud listings found beyond the resources written to the config.
type discovered struct {
	runURLs    map[string]string // Cloud Run service → URL
	runJobs    []Resource        // Cloud Run jobs, offered as actions
	vmInternal map[string]bool   // VMs with no external IP, reached over IAP
}

func guessStack(dir string, e Env, d discovered) stackGuess {
	var g stackGuess
	g.framework = detectFramework(dir)
	g.target = detectTarget(dir, e, d, g.framework)
	g.vars = g.target.vars
	g.version = g.target.version
	g.notes = append(g.notes, g.target.notes...)

	t, f := g.target, g.framework
	add := func(a Action) {
		if a.Command != "" {
			g.actions = append(g.actions, a)
		}
	}
	add(Action{Key: "l", Label: "logs", Command: t.logs})
	add(Action{Key: "i", Label: "status", Command: t.status})
	if t.tty != nil {
		add(Action{Key: "x", Label: "shell", Command: t.tty("sh"), Interactive: true})
		if f.console != "" {
			add(Action{Key: "c", Label: "console", Command: t.tty(f.console), Interactive: true})
		}
	}
	if t.exec != nil {
		if f.migrations != "" {
			add(Action{Key: "m", Label: "migrations", Command: t.exec(f.migrations)})
		}
		if f.migrate != "" {
			add(Action{Key: "M", Label: "migrate", Command: t.exec(f.migrate), Confirm: true})
		}
	} else if f.migrate != "" && t.name == "cloudrun" {
		g.notes = append(g.notes, "Cloud Run has no shell to run "+f.migrate+" in: make it a Cloud Run job and gcdeck will offer it")
	}
	if vm := first(e.Resources, "vm"); vm != nil && t.name != "kamal" {
		add(Action{Key: "s", Label: "ssh", Command: sshCmd(d, vm.Name), Interactive: true})
	}
	for i, job := range d.runJobs {
		if i >= len(jobKeys) {
			g.notes = append(g.notes, fmt.Sprintf("%d more Cloud Run jobs have no key; add them under actions", len(d.runJobs)-i))
			break
		}
		region := or(job.Region, e.Region)
		add(Action{Key: string(jobKeys[i]), Label: "job " + job.Name, Confirm: true,
			Command: fmt.Sprintf("gcloud run jobs execute %s --region=%s --project={project} --wait", job.Name, region)})
	}
	if t.deploy != "" {
		g.deploy = &Deploy{Command: t.deploy}
	}
	g.health = guessHealth(dir, e, d, t, f)
	if g.health == "" {
		g.notes = append(g.notes, "found no public URL: set health_url to your app's health check")
	}
	return g
}

// Keys for Cloud Run jobs, which run something rather than show it.
const jobKeys = "uvyz"

func first(rs []Resource, typ string) *Resource {
	for i := range rs {
		if rs[i].Type == typ {
			return &rs[i]
		}
	}
	return nil
}

func sshCmd(d discovered, vm string) string {
	cmd := "gcloud compute ssh " + vm + " --zone={zone} --project={project}"
	if d.vmInternal[vm] {
		cmd += " --tunnel-through-iap"
	}
	return cmd
}

// ---- where it runs ----

func detectTarget(dir string, e Env, d discovered, f framework) target {
	name := filepath.Base(dir)
	vm, gke, run := first(e.Resources, "vm"), first(e.Resources, "gke"), first(e.Resources, "cloudrun")
	switch {
	case fileExists(filepath.Join(dir, "config", "deploy.yml")):
		return kamalTarget(dir)
	case composeFile(dir) != "" && vm != nil:
		return composeTarget(dir, name, vm.Name, d)
	case gke != nil:
		return gkeTarget(e, gke, name)
	case vm != nil && pm2Config(dir) != "":
		return pm2Target(name, vm.Name, d)
	case run != nil:
		return cloudRunTarget(dir, run)
	case vm != nil:
		return target{name: "vm", notes: []string{"found a VM but no Kamal, Compose or pm2 config: add logs and status actions for how the app runs there"}}
	}
	return target{name: "none", notes: []string{"could not tell where the app runs: add logs and status actions yourself"}}
}

func kamalTarget(dir string) target {
	k := "kamal"
	if fileExists(filepath.Join(dir, "bin", "kamal")) {
		k = "bin/kamal"
	}
	return target{
		name:    "kamal",
		version: k + " app version",
		logs:    k + " app logs -f",
		status:  k + " details",
		exec:    func(c string) string { return k + ` app exec --reuse "` + c + `"` },
		tty:     func(c string) string { return k + ` app exec --interactive --reuse "` + c + `"` },
	}
}

// composeFile prefers a production compose file over the plain one, which is often for development.
func composeFile(dir string) string {
	for _, n := range []string{"compose.production.yaml", "compose.production.yml", "compose.prod.yaml", "compose.prod.yml",
		"docker-compose.production.yml", "docker-compose.prod.yml", "docker-compose.prod.yaml",
		"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"} {
		if fileExists(filepath.Join(dir, n)) {
			return n
		}
	}
	return ""
}

var composeService = regexp.MustCompile(`(?m)^  ([A-Za-z0-9_.-]+):\s*$`)

// appService picks the compose service that runs the app: one named like an app, else the first that builds.
func appService(dir, file string) string {
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return "web"
	}
	text := string(data)
	if i := strings.Index(text, "\nservices:"); i >= 0 {
		text = text[i:]
	}
	var services []string
	for _, m := range composeService.FindAllStringSubmatch(text, -1) {
		services = append(services, m[1])
	}
	for _, want := range []string{"web", "app", "api", "server", "backend", "django", "node", "rails"} {
		if contains(services, want) {
			return want
		}
	}
	if len(services) > 0 {
		return services[0]
	}
	return "web"
}

func composeTarget(dir, name, vm string, d discovered) target {
	file := composeFile(dir)
	compose := "docker compose"
	if !strings.HasPrefix(file, "compose.y") && !strings.HasPrefix(file, "docker-compose.y") {
		compose += " -f " + file
	}
	ssh := sshCmd(d, vm)
	remote := func(args string) string { return ssh + ` --command 'cd {compose_dir} && ` + compose + " " + args + `'` }
	return target{
		name: "compose",
		// Assumes the VM's checkout is what runs; a deploy that only pulls images needs its own version_command.
		version: ssh + ` --command 'cd {compose_dir} && git rev-parse HEAD'`,
		logs:    remote("logs -f --tail=200"),
		status:  remote("ps"),
		exec:    func(c string) string { return remote("exec -T {service} " + c) },
		tty: func(c string) string {
			return ssh + ` -- -t 'cd {compose_dir} && ` + compose + " exec {service} " + c + `'`
		},
		vars: map[string]string{"compose_dir": "~/" + name, "service": appService(dir, file)},
		notes: []string{fmt.Sprintf("Compose runs on %s from %s: check vars.compose_dir is where the app lives on the VM, "+
			"and vars.service is its app service", vm, file)},
	}
}

func gkeTarget(e Env, c *Resource, name string) target {
	loc := or(c.Location, or(e.Region, e.Zone))
	// A kubeconfig of gcdeck's own, so fetching credentials never switches the context your kubectl is on.
	// umask 077: the kubeconfig names the cluster and how to get its credentials, so only you can read it.
	kube := fmt.Sprintf(`umask 077; export KUBECONFIG="$HOME/.cache/gcdeck/%s-%s.kubeconfig"; mkdir -p "$HOME/.cache/gcdeck"; `+
		`[ -s "$KUBECONFIG" ] || gcloud container clusters get-credentials %s --location=%s --project=%s --quiet >/dev/null 2>&1; kubectl`,
		e.Project, c.Name, c.Name, loc, e.Project)
	return target{
		name:    "gke",
		version: "{kubectl} get deploy/{deployment} -o jsonpath='{.spec.template.spec.containers[0].image}'",
		logs:    "{kubectl} logs -f deploy/{deployment} --all-containers --prefix --tail=200",
		status:  "{kubectl} get pods -o wide",
		exec:    func(cmd string) string { return "{kubectl} exec deploy/{deployment} -- " + cmd },
		tty:     func(cmd string) string { return "{kubectl} exec -it deploy/{deployment} -- " + cmd },
		vars:    map[string]string{"kubectl": kube, "deployment": name},
		notes: []string{"check vars.deployment is the app's Kubernetes Deployment. kubectl needs gke-gcloud-auth-plugin " +
			"(gcloud components install gke-gcloud-auth-plugin)"},
	}
}

func pm2Config(dir string) string {
	for _, n := range []string{"ecosystem.config.js", "ecosystem.config.cjs", "ecosystem.config.json", "ecosystem.json"} {
		if fileExists(filepath.Join(dir, n)) {
			return n
		}
	}
	return ""
}

func pm2Target(name, vm string, d discovered) target {
	ssh := sshCmd(d, vm)
	return target{
		name:    "pm2",
		version: ssh + ` --command 'cd {app_dir} && git rev-parse HEAD'`,
		logs:    ssh + ` --command 'pm2 logs --raw --lines 200'`,
		status:  ssh + ` --command 'pm2 status'`,
		exec:    func(c string) string { return ssh + ` --command 'cd {app_dir} && ` + c + `'` },
		tty:     func(c string) string { return ssh + ` -- -t 'cd {app_dir} && ` + c + `'` },
		vars:    map[string]string{"app_dir": "~/" + name},
		notes:   []string{fmt.Sprintf("pm2 runs on %s: check vars.app_dir is where the app lives there", vm)},
	}
}

func cloudRunTarget(dir string, svc *Resource) target {
	t := target{
		name: "cloudrun",
		// gcdeck streams these itself: gcloud's own tail needs the beta component and asks to install it.
		logs: "{gcdeck} cloudrun-logs {cloudrun} --region={region} --project={project}",
	}
	if svc.Region != "" {
		t.logs = "{gcdeck} cloudrun-logs {cloudrun} --region=" + svc.Region + " --project={project}"
	}
	// The image of the revision serving; it names the commit when images are tagged with it.
	t.version = "gcloud run services describe {cloudrun} --region=" + or(svc.Region, "{region}") +
		" --project={project} --format='value(spec.template.spec.containers[0].image)'"
	for _, n := range []string{"Dockerfile", "package.json", "requirements.txt", "pyproject.toml", "go.mod", "Gemfile"} {
		if fileExists(filepath.Join(dir, n)) {
			t.deploy = "gcloud run deploy {cloudrun} --source . --region=" + or(svc.Region, "{region}") + " --project={project}"
			break
		}
	}
	return t
}

// ---- what it is ----

func detectFramework(dir string) framework {
	has := func(p ...string) bool { return fileExists(filepath.Join(append([]string{dir}, p...)...)) }
	switch {
	case has("config", "application.rb"):
		return framework{name: "Rails", console: "bin/rails console", migrations: "bin/rails db:migrate:status", migrate: "bin/rails db:migrate"}
	case has("manage.py"):
		return framework{name: "Django", console: "python manage.py shell", migrations: "python manage.py showmigrations", migrate: "python manage.py migrate"}
	case has("package.json"):
		return nodeFramework(dir)
	case has("alembic.ini"):
		return framework{name: "Python (Alembic)", console: "python", migrations: "alembic current", migrate: "alembic upgrade head"}
	case has("pyproject.toml"), has("requirements.txt"):
		return framework{name: "Python", console: "python"}
	}
	return framework{}
}

func nodeFramework(dir string) framework {
	f := framework{name: "Node", console: "node"}
	var pkg struct {
		Scripts         map[string]string `json:"scripts"`
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if data, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		_ = json.Unmarshal(data, &pkg)
	}
	dep := func(n string) bool { _, a := pkg.Dependencies[n]; _, b := pkg.DevDependencies[n]; return a || b }
	for _, fw := range []struct{ dep, name string }{{"next", "Next.js"}, {"@nestjs/core", "NestJS"}, {"express", "Express"},
		{"fastify", "Fastify"}, {"@remix-run/node", "Remix"}, {"nuxt", "Nuxt"}} {
		if dep(fw.dep) {
			f.name = "Node (" + fw.name + ")"
			break
		}
	}
	switch {
	case dep("prisma") || dep("@prisma/client") || fileExists(filepath.Join(dir, "prisma", "schema.prisma")):
		f.migrations, f.migrate = "npx prisma migrate status", "npx prisma migrate deploy"
	case dep("drizzle-kit"):
		f.migrate = "npx drizzle-kit migrate"
	default:
		for _, s := range []string{"migrate", "db:migrate", "migrate:deploy", "migrate:latest"} {
			if _, ok := pkg.Scripts[s]; ok {
				f.migrate = "npm run " + s
				break
			}
		}
		if _, ok := pkg.Scripts["migrate:status"]; ok {
			f.migrations = "npm run migrate:status"
		}
	}
	return f
}

// ---- health check ----

var (
	kamalHost   = regexp.MustCompile(`(?m)^\s*hosts?:\s*\[?\s*["']?([a-z0-9.-]+\.[a-z]{2,})`)
	traefikHost = regexp.MustCompile("Host\\(`([a-z0-9.-]+\\.[a-z]{2,})`\\)")
	caddyHost   = regexp.MustCompile(`(?m)^(?:https?://)?([a-z0-9-]+(?:\.[a-z0-9-]+)+)(?::\d+)?\s*(?:,[^{]*)?\{`)
	nginxHost   = regexp.MustCompile(`server_name\s+([a-z0-9-]+(?:\.[a-z0-9-]+)+)`)
	healthRoute = regexp.MustCompile("[\"'`]/?((?:api/)?(?:healthz|health|_health|up|ping))(/?)[\"'`]")
)

// guessHealth finds the public host (Kamal proxy, Cloud Run URL, a reverse proxy config in the repo) and the health
// route the code defines.
func guessHealth(dir string, e Env, d discovered, t target, f framework) string {
	host := ""
	read := func(p string) string { b, _ := os.ReadFile(filepath.Join(dir, p)); return string(b) }
	if m := kamalHost.FindStringSubmatch(read("config/deploy.yml")); m != nil {
		host = "https://" + m[1]
	}
	if host == "" {
		if svc := first(e.Resources, "cloudrun"); svc != nil {
			host = d.runURLs[svc.Name]
		}
	}
	if host == "" {
		if file := composeFile(dir); file != "" {
			if m := traefikHost.FindStringSubmatch(read(file)); m != nil {
				host = "https://" + m[1]
			}
		}
	}
	if host == "" {
		for _, p := range []string{"Caddyfile", "caddy/Caddyfile", "deploy/Caddyfile", "docker/Caddyfile"} {
			if m := caddyHost.FindStringSubmatch(read(p)); m != nil && m[1] != "localhost" {
				host = "https://" + m[1]
				break
			}
		}
	}
	if host == "" {
		for _, p := range []string{"nginx.conf", "nginx/nginx.conf", "nginx/default.conf", "deploy/nginx.conf", "docker/nginx.conf"} {
			if m := nginxHost.FindStringSubmatch(read(p)); m != nil && m[1] != "localhost" {
				host = "https://" + m[1]
				break
			}
		}
	}
	if host == "" {
		return ""
	}
	path := healthPath(dir)
	if path == "" && t.name == "kamal" && f.name == "Rails" {
		path = "/up" // Rails' own health route, which Kamal checks
	}
	return strings.TrimRight(host, "/") + or(path, "/")
}

// healthPath searches the app's source for a health route literal, preferring the most specific name.
func healthPath(dir string) string {
	skip := map[string]bool{"node_modules": true, ".git": true, "vendor": true, "venv": true, ".venv": true, "dist": true,
		"build": true, "tmp": true, "log": true, "public": true, "static": true, "__pycache__": true, ".next": true, "coverage": true}
	exts := map[string]bool{".rb": true, ".py": true, ".js": true, ".ts": true, ".mjs": true, ".cjs": true, ".go": true}
	found := map[string]string{} // route name → path as written
	files := 0
	_ = filepath.WalkDir(dir, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if de.IsDir() {
			if skip[de.Name()] || (strings.HasPrefix(de.Name(), ".") && p != dir) {
				return filepath.SkipDir
			}
			return nil
		}
		if files > 3000 {
			return filepath.SkipAll
		}
		if !exts[filepath.Ext(p)] || strings.Contains(p, "test") || strings.Contains(p, "spec") {
			return nil
		}
		if info, err := de.Info(); err != nil || info.Size() > 200_000 {
			return nil
		}
		files++
		data, _ := os.ReadFile(p)
		for _, m := range healthRoute.FindAllStringSubmatch(string(data), -1) {
			if _, ok := found[m[1]]; !ok {
				found[m[1]] = "/" + m[1] + m[2]
			}
		}
		return nil
	})
	for _, name := range []string{"healthz", "api/healthz", "health", "api/health", "_health", "up", "ping", "api/ping"} {
		if p, ok := found[name]; ok {
			return p
		}
	}
	keys := make([]string, 0, len(found))
	for k := range found {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		return found[keys[0]]
	}
	return ""
}
