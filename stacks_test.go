package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repo writes files into a temp dir: path → contents.
func repo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "myapp")
	for p, body := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func keys(actions []Action) string {
	var b strings.Builder
	for _, a := range actions {
		b.WriteString(a.Key)
	}
	return b.String()
}

func action(t *testing.T, g stackGuess, key string) Action {
	t.Helper()
	for _, a := range g.actions {
		if a.Key == key {
			return a
		}
	}
	t.Fatalf("no action %q in %q", key, keys(g.actions))
	return Action{}
}

var testEnv = Env{Project: "p", Region: "us-central1", Zone: "us-central1-a"}

func withResources(rs ...Resource) Env {
	e := testEnv
	e.Resources = rs
	return e
}

func noDiscovery() discovered {
	return discovered{runURLs: map[string]string{}, vmInternal: map[string]bool{}}
}

func TestStackRailsKamal(t *testing.T) {
	dir := repo(t, map[string]string{
		"config/deploy.yml":     "proxy:\n  ssl: true\n  hosts: [ example.com, www.example.com ]\n",
		"config/application.rb": "",
		"bin/kamal":             "",
		"config/routes.rb":      `get "up" => "rails/health#show"`,
	})
	g := guessStack(dir, withResources(Resource{Type: "vm", Name: "web"}), noDiscovery())
	if g.target.name != "kamal" || g.framework.name != "Rails" {
		t.Fatalf("%s on %s", g.framework.name, g.target.name)
	}
	if got := keys(g.actions); got != "lixcmM" {
		t.Errorf("keys = %s, want lixcmM (no ssh: Kamal has its own way in)", got)
	}
	if c := action(t, g, "c").Command; c != `bin/kamal app exec --interactive --reuse "bin/rails console"` {
		t.Errorf("console = %s", c)
	}
	if !action(t, g, "M").Confirm {
		t.Error("migrate must ask first")
	}
	if g.health != "https://example.com/up" {
		t.Errorf("health = %s", g.health)
	}
}

func TestStackDjangoCompose(t *testing.T) {
	dir := repo(t, map[string]string{
		"manage.py": "",
		"docker-compose.yml": "services:\n  db:\n    image: postgres\n  web:\n    build: .\n" +
			"    labels:\n      - \"traefik.http.routers.web.rule=Host(`shop.example.org`)\"\n",
		"shop/urls.py": `urlpatterns = [path("healthz/", views.health)]`,
	})
	d := noDiscovery()
	d.vmInternal["box"] = true
	g := guessStack(dir, withResources(Resource{Type: "vm", Name: "box"}), d)
	if g.target.name != "compose" || g.framework.name != "Django" {
		t.Fatalf("%s on %s", g.framework.name, g.target.name)
	}
	if got := keys(g.actions); got != "lixcmMs" {
		t.Errorf("keys = %s", got)
	}
	if g.vars["service"] != "web" || g.vars["compose_dir"] != "~/myapp" {
		t.Errorf("vars = %v", g.vars)
	}
	e := withResources(Resource{Type: "vm", Name: "box"})
	e.Vars = g.vars
	logs := e.expand(action(t, g, "l").Command)
	if want := "gcloud compute ssh box --zone=us-central1-a --project=p --tunnel-through-iap --command 'cd ~/myapp && docker compose logs -f --tail=200'"; logs != want {
		t.Errorf("logs =\n%s\nwant\n%s", logs, want)
	}
	if m := e.expand(action(t, g, "m").Command); !strings.Contains(m, "docker compose exec -T web python manage.py showmigrations") {
		t.Errorf("migrations = %s", m)
	}
	if c := e.expand(action(t, g, "c").Command); !strings.Contains(c, "-- -t 'cd ~/myapp && docker compose exec web python manage.py shell'") {
		t.Errorf("console = %s", c)
	}
	if g.health != "https://shop.example.org/healthz/" {
		t.Errorf("health = %s", g.health)
	}
}

func TestStackComposeWithoutVMIsLocalOnly(t *testing.T) {
	dir := repo(t, map[string]string{"package.json": `{}`, "compose.yaml": "services:\n  app:\n    build: .\n"})
	g := guessStack(dir, withResources(Resource{Type: "cloudrun", Name: "api"}), noDiscovery())
	if g.target.name != "cloudrun" {
		t.Errorf("target = %s: a compose file with no VM is for local development", g.target.name)
	}
}

func TestStackNodeCloudRun(t *testing.T) {
	dir := repo(t, map[string]string{
		"package.json":            `{"dependencies":{"next":"15","@prisma/client":"6"}}`,
		"app/api/health/route.ts": `export const GET = () => Response.json({ok: true}) // "/api/health"`,
	})
	d := noDiscovery()
	d.runURLs["web"] = "https://web-abc-uc.a.run.app"
	d.runJobs = []Resource{{Type: "runjob", Name: "migrate"}}
	g := guessStack(dir, withResources(Resource{Type: "cloudrun", Name: "web"}), d)
	if g.framework.name != "Node (Next.js)" || g.target.name != "cloudrun" {
		t.Fatalf("%s on %s", g.framework.name, g.target.name)
	}
	if got := keys(g.actions); got != "lu" {
		t.Errorf("keys = %s, want l (logs) and u (the migrate job)", got)
	}
	if !strings.Contains(action(t, g, "l").Command, "{gcdeck} cloudrun-logs {cloudrun}") {
		t.Errorf("logs = %s", action(t, g, "l").Command)
	}
	if j := action(t, g, "u"); !j.Confirm || !strings.Contains(j.Command, "gcloud run jobs execute migrate --region=us-central1") {
		t.Errorf("job = %+v", j)
	}
	if g.deploy == nil || !strings.Contains(g.deploy.Command, "gcloud run deploy {cloudrun} --source .") {
		t.Errorf("deploy = %+v", g.deploy)
	}
	if g.health != "https://web-abc-uc.a.run.app/api/health" {
		t.Errorf("health = %s", g.health)
	}
	var noted bool
	for _, n := range g.notes {
		noted = noted || strings.Contains(n, "Cloud Run job")
	}
	if !noted {
		t.Errorf("expected a note that Prisma migrations need a Cloud Run job: %q", g.notes)
	}
}

func TestStackNodePm2(t *testing.T) {
	dir := repo(t, map[string]string{
		"package.json":        `{"dependencies":{"express":"5"},"scripts":{"migrate":"knex migrate:latest"}}`,
		"ecosystem.config.js": "module.exports = {}",
		"nginx/default.conf":  "server { server_name api.example.net; }",
	})
	g := guessStack(dir, withResources(Resource{Type: "vm", Name: "node-1"}), noDiscovery())
	if g.target.name != "pm2" || g.framework.name != "Node (Express)" {
		t.Fatalf("%s on %s", g.framework.name, g.target.name)
	}
	if got := keys(g.actions); got != "lixcMs" {
		t.Errorf("keys = %s", got)
	}
	if m := action(t, g, "M").Command; !strings.Contains(m, "cd {app_dir} && npm run migrate") {
		t.Errorf("migrate = %s", m)
	}
	if g.health != "https://api.example.net/" {
		t.Errorf("health = %s", g.health)
	}
}

func TestStackPythonGKE(t *testing.T) {
	dir := repo(t, map[string]string{"pyproject.toml": "", "alembic.ini": "", "app/main.py": `@app.get("/healthz")`})
	g := guessStack(dir, withResources(Resource{Type: "gke", Name: "prod", Location: "europe-west4"}), noDiscovery())
	if g.target.name != "gke" || g.framework.name != "Python (Alembic)" {
		t.Fatalf("%s on %s", g.framework.name, g.target.name)
	}
	e := withResources()
	e.Vars = g.vars
	m := e.expand(action(t, g, "m").Command)
	if !strings.Contains(m, `KUBECONFIG="$HOME/.cache/gcdeck/p-prod.kubeconfig"`) ||
		!strings.Contains(m, "get-credentials prod --location=europe-west4 --project=p") ||
		!strings.HasSuffix(m, "kubectl exec deploy/myapp -- alembic current") {
		t.Errorf("migrations = %s", m)
	}
	if g.health != "" {
		t.Errorf("health = %q: GKE gives no public URL to guess", g.health)
	}
}

func TestExpandNestedVars(t *testing.T) {
	e := Env{Project: "p", Vars: map[string]string{"ssh": "ssh {host}", "host": "box.{project}", "loop": "{loop}"}}
	if got := e.expand("{ssh} {loop}"); got != "ssh box.p {loop}" {
		t.Errorf("%q", got)
	}
}

func TestSourcePrefixes(t *testing.T) {
	c := formatLogLine("web-1  | GET /health 200")
	if c.prefix != "web-1 " || c.text != "GET /health 200" {
		t.Errorf("compose: %+v", c)
	}
	k := formatLogLine(`[pod/api-7d9f/app] {"level":"ERROR","msg":"boom"}`)
	if k.prefix != "app " || k.text != "ERROR boom" {
		t.Errorf("kubectl: %+v", k)
	}
}

func TestPrintEntriesSkipsSeen(t *testing.T) {
	e := func(id, ts string) logEntry { return logEntry{InsertID: id, Timestamp: ts, TextPayload: id} }
	last, seen := printEntries([]logEntry{e("a", "t1"), e("b", "t2"), e("c", "t2")}, "", nil)
	if last != "t2" || !seen["b"] || !seen["c"] || seen["a"] {
		t.Fatalf("last=%s seen=%v", last, seen)
	}
	// The next poll asks for timestamp >= t2, so b and c come back and must not print again.
	last, seen = printEntries([]logEntry{e("b", "t2"), e("c", "t2"), e("d", "t3")}, last, seen)
	if last != "t3" || !seen["d"] || seen["b"] {
		t.Errorf("last=%s seen=%v", last, seen)
	}
}
