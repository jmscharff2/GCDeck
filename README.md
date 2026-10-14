# gcdeck

A terminal dashboard for the Google Cloud environments your app deploys to. One screen per environment shows a
health check, your VMs, Cloud SQL, Cloud Run services, GKE clusters and Artifact Registry images, plus GitHub
Actions CI and deploys. You can deploy, promote, tail logs, open a console or SSH in without leaving the terminal.

Everything you read shows inside gcdeck: logs stream into a scrollable, filterable view, `kamal details`,
`docker ps` and `kubectl get` tables are redrawn to fit, and each GitHub run has its own screen with live jobs and
steps and each job's log. Only interactive programs (a Rails console, SSH) leave the dashboard: inside
Herdr they open in a pane beside it, elsewhere they borrow the terminal until
they exit.

gcdeck uses the `gcloud` and `gh` CLIs and the logins you already have. It stores no credentials and calls no
other services.

```
gcdeck   1 production  2 staging
 my-project · branch main · us-central1
╭─────────────────────────────╮╭─────────────────────────────╮╭─────────────────────────────╮
│ Health  example.com/up      ││ Cloud Run  api              ││ Cloud SQL  main-db          │
│ ● up  200                   ││ ● ready                     ││ ● runnable                  │
│ latency    54ms             ││ since      2h ago           ││ tier       db-g1-small      │
│ TLS cert   75d left         ││ revision   api-00042-xuz    ││ version    postgres_16      │
╰─────────────────────────────╯╰─────────────────────────────╯╰─────────────────────────────╯
╭───────────────────────────────────────────────────────────────────────────────────────────╮
│ CI on main  ✓ success      f0e37df  8h ago                                                │
│ Deploys → production  deploy.yml                                                          │
│ ▸ ✓ success      f0e37df  after CI  took 5m47s      8h ago                                │
│   ✗ failure      3e88be4  by hand   took 1m02s      2d ago                                │
╰───────────────────────────────────────────────────────────────────────────────────────────╯
 tab env · d deploy · ↑↓ enter runs · l logs · s ssh · r refresh · ? more · q quit
```

## Install

Needs Go 1.24+, the [gcloud CLI](https://cloud.google.com/sdk/docs/install), and the [GitHub CLI](https://cli.github.com)
for the CI and deploy panels.

```bash
go install github.com/jmscharff2/gcdeck@latest
```

### As a Herdr plugin

```bash
herdr plugin install jmscharff2/gcdeck      # builds it with Go; needs Herdr 0.9.3 or later
```

This adds two actions to Herdr: **gcdeck: open beside this pane** and **gcdeck: open in a new tab**. Each opens the
dashboard for the project in the pane you are working in, using that project's `.gcdeck.json`. To work on gcdeck
itself, `herdr plugin link <path to your checkout>` uses your local build instead.

To open it with a key, add a popup to Herdr's `config.toml` (gcdeck on your `PATH`, from `go install`):

```toml
[[keys.command]]
key = ["prefix+G"]
type = "popup"
command = "cd \"${HERDR_ACTIVE_PANE_CWD:-.}\" && exec gcdeck"
description = "Google Cloud dashboard"
width = "95%"
height = "90%"
```

## Set up

In your app's repo:

```bash
gcdeck init                                         # the gcloud-configured project, as env "production"
gcdeck init -env staging -project my-app-staging -branch staging   # add more envs the same way
gcdeck check                                        # show what the config resolves to
gcdeck
```

`init` lists the project's VMs, Cloud SQL instances, Cloud Run services and jobs, GKE clusters and Docker
repositories, finds your CI and deploy workflows in `.github/workflows`, and works out how your app runs (below) to
set up keys for logs, status, a shell, a console and migrations. It writes `.gcdeck.json`, which you can commit so
everyone on the team gets the same dashboard. Read through it once: `init` guesses, and the notes it prints say
where.

### What `init` recognizes

Two things are worked out separately and combined: **where the app runs**, which decides how logs, status and
commands reach it, and **what the app is**, which decides the commands.

| Where it runs | Recognized by | `l` logs | `i` status | Commands run through |
|---|---|---|---|---|
| Kamal | `config/deploy.yml` | `kamal app logs -f` | `kamal details` | `kamal app exec` |
| Docker Compose on a VM | a compose file and a VM in the project | `docker compose logs -f` over SSH | `docker compose ps` | `docker compose exec <service>` over SSH |
| GKE | a cluster in the project | `kubectl logs -f deploy/<name>` | `kubectl get pods` | `kubectl exec deploy/<name>` |
| pm2 on a VM | `ecosystem.config.js` and a VM | `pm2 logs` over SSH | `pm2 status` | SSH, in the app's directory |
| Cloud Run | a service in the project | streamed from Cloud Logging | the service's card | Cloud Run jobs (below) |

A compose file with no VM in the project is taken to be for local development. Production variants
(`compose.production.yaml`, `docker-compose.prod.yml` and so on) are preferred over the plain file.

| The app | Recognized by | `c` console | `m` migrations | `M` migrate (asks first) |
|---|---|---|---|---|
| Rails | `config/application.rb` | `bin/rails console` | `db:migrate:status` | `db:migrate` |
| Django | `manage.py` | `manage.py shell` | `showmigrations` | `migrate` |
| Node (Next.js, Express, NestJS, Fastify, Remix, Nuxt) | `package.json` | `node` | `prisma migrate status` with Prisma | `prisma migrate deploy`, `drizzle-kit migrate`, or a `migrate` script |
| Python with Alembic | `alembic.ini` | `python` | `alembic current` | `alembic upgrade head` |
| Python | `pyproject.toml` or `requirements.txt` | `python` | | |

Every target that can get into the app also gets `x` for a shell, and a VM gets `s` for SSH (through IAP when it has
no external IP). Cloud Run has no way into a running container, so each Cloud Run job becomes a key (`u`, `v`, `y`,
`z`) that runs it and waits; put migrations in a job to run them from gcdeck.

The health check comes from the first public host it can find (Kamal's proxy hosts, the Cloud Run URL, a Traefik
`Host()` rule in the compose file, a Caddyfile or an nginx `server_name`) and the health route your code defines
(`/healthz`, `/health`, `/api/health`, `/up`, `/ping`).

When there is no deploy workflow, `d` runs `kamal deploy` for Kamal or `gcloud run deploy --source .` for Cloud Run.
For Compose, pm2 and GKE, set `deploy.command` to however you ship.

Things it cannot know, it leaves in `vars` with a note: where the app lives on the VM (`compose_dir`, `app_dir`),
the compose service (`service`) and the Kubernetes Deployment (`deployment`). GKE commands keep their own kubeconfig
under `~/.cache/gcdeck`, so they never switch your kubectl's context, and need `gke-gcloud-auth-plugin`.

gcdeck reads the first config it finds: `-config <path>`, `$GCDECK_CONFIG`, `.gcdeck.json` in the current directory
or any parent, then `~/.config/gcdeck/config.json`.

## Keys

| Key | Does |
|---|---|
| `tab` `shift+tab` `1`-`9` | switch environment |
| `d` | deploy this env (type `yes`, or the env's name for a protected env) |
| `p` | promote, e.g. staging to main (type `promote`) |
| `↑` `↓` `enter` | open a deploy run: its jobs and steps, live while it runs |
| `o` | open the chosen run on GitHub |
| `g` | open the project in Cloud Console |
| `a` | `gcloud auth login` (or `gh auth login`) when a login has expired |
| `e` | edit the config |
| `r` | refresh now. It also refreshes every `refresh_seconds`, and every 8s while a deploy runs |
| `?` `q` | more keys, quit |
| your own | each `actions` entry, e.g. `l` logs, `i` details, `c` console, `s` ssh |

**Output views** (logs, details, any action that is not interactive, a job's log):

| Key | Does |
|---|---|
| `↑` `↓` `pgup` `pgdn` | scroll. Scrolling up stops following new output |
| `g` `G` | top, bottom (and follow again) |
| `/` | filter lines; `esc` clears it |
| `w` | wrap long lines on or off |
| `r` | run the command again, once it has finished |
| `esc` | back. This stops the command; a deploy or promote asks first |

**Run screen** (`enter` on a deploy):

| Key | Does |
|---|---|
| `↑` `↓` | choose a job; its steps open beneath it |
| `enter` | the job's log, with a header for each step |
| `R` / `X` | re-run the failed jobs / cancel a run in progress (each asks first) |
| `o` | open the run or job on GitHub |

Lines are tidied as they arrive: Docker timestamps become local times, JSON log lines become
`INFO Request method=GET path=/ status=200 …` with empty fields left out, and errors, warnings, 4xx and 5xx are
coloured.

## Config

```jsonc
{
  "app_dir": "~/code/my-app",       // where actions run. Default: the directory holding .gcdeck.json
  "github_repo": "me/my-app",       // default: the origin remote of app_dir. None hides the GitHub panels
  "ci_workflow": "ci.yml",          // latest run shown for each env's branch
  "refresh_seconds": 30,
  "promote": { "from": "staging", "to": "main", "workflow": "promote.yml" },  // or "command"
  "actions": [                      // shared by every env; an env's own action with the same key wins
    { "key": "l", "label": "logs", "command": "{gcdeck} cloudrun-logs {cloudrun} --region={region} --project={project}" },
    { "key": "t", "label": "tests", "command": "bin/rails test", "pane": true }   // "pane": keep it running beside gcdeck
  ],
  "envs": [
    {
      "name": "production",
      "project": "my-app-prod",
      "region": "us-central1",      // default for resources that take a region
      "zone": "us-central1-a",      // default for VMs
      "branch": "main",
      "protected": true,
      "health_url": "https://example.com/healthz",
      "resources": [
        { "type": "cloudrun", "name": "api" },
        { "type": "vm", "name": "worker", "zone": "us-central1-b" },
        { "type": "sql", "name": "main-db" },
        { "type": "gke", "name": "cluster-1", "location": "us-central1" },
        { "type": "images", "repo": "app/api" }     // or "repo", or a full ...-docker.pkg.dev/... path
      ],
      "deploy": { "workflow": "deploy.yml", "inputs": { "environment": "production" } },
      "actions": [ { "key": "s", "label": "ssh", "command": "gcloud compute ssh {vm} --zone={zone} --project={project}", "interactive": true } ],
      "vars": { "service": "api" }  // extra {placeholders} for this env's commands
    }
  ]
}
```

The file is plain JSON (the comments above are for reading only). Unknown fields are an error, so typos show up
when gcdeck starts.

**Actions.** An action's output streams into gcdeck. `"confirm": true` asks before running, for anything that
changes things.

**Confirmations.** Anything that changes things (deploy, promote, a `confirm` action, re-running or cancelling a
run, stopping a deploy midway) asks you to type a word and press enter: `yes`, the env's name for a `"protected"`
env, or `promote`. A single key never confirms, and pasted text does not count. Set `"interactive": true` for programs you type into (a
console, a shell, anything that asks questions): they get a real terminal, a focused Herdr pane or this terminal.
`"pane": true` puts a non-interactive command in an unfocused Herdr pane instead, to keep it running in view.

**Placeholders.** Action, deploy and promote commands can use `{env}`, `{project}`, `{region}`, `{zone}`,
`{branch}`, the first resource name of each type (`{vm}`, `{sql}`, `{cloudrun}`, `{gke}`), `{gcdeck}` (this program),
and the env's `vars`, which can use placeholders themselves. Anything else in braces, like `${HOME}`, is passed
through unchanged.

**Deploys.** `deploy.workflow` runs `gh workflow run <workflow> --ref <ref> -f <inputs>`. `ref` defaults to the
env's branch. To deploy from your machine instead, set `deploy.command` (for example `"bin/kamal deploy"` or
`"gcloud run deploy {cloudrun} --source . --region={region} --project={project}"`). Its output streams into gcdeck,
and leaving the view while it runs asks first, so a deploy is not cut off by accident.

**Which runs belong to which env.** If each env has its own workflow, all of that workflow's runs are listed. If
envs share a workflow, runs are matched by branch. If a single workflow run deploys whichever env its jobs choose,
set `deploy.job` to the start of that env's job name, and runs are matched by which env's job ran (and not skipped).
Runs still deciding are listed as not started, and runs that failed before any env's job show under every env.

```json
"deploy": { "workflow": "deploy.yml", "inputs": { "environment": "staging" }, "job": "staging" }
```

## Examples

**Kamal on a VM, with Cloud SQL.** Kamal needs its destination and env file per environment, so vars carry them:

```json
{
  "actions": [
    { "key": "l", "label": "logs", "command": "{kamal} logs{dest}" },
    { "key": "i", "label": "details", "command": "{kamal} details{dest}" },
    { "key": "c", "label": "console", "command": "{kamal} console{dest}", "interactive": true }
  ],
  "envs": [
    { "name": "production", "project": "app-prod", "zone": "us-central1-a", "region": "us-central1", "branch": "main",
      "resources": [ { "type": "vm", "name": "web" }, { "type": "sql", "name": "db" } ],
      "vars": { "kamal": "bundle exec dotenv -f .env.production bin/kamal", "dest": "" } },
    { "name": "staging", "project": "app-staging", "zone": "us-central1-a", "region": "us-central1", "branch": "staging",
      "resources": [ { "type": "vm", "name": "web" }, { "type": "sql", "name": "db" } ],
      "vars": { "kamal": "bundle exec dotenv -f .env.staging bin/kamal", "dest": " -d staging" } }
  ]
}
```

**Django on Docker Compose, on a VM reached through IAP:**

```json
{
  "envs": [
    { "name": "production", "project": "shop-prod", "zone": "europe-west1-b", "region": "europe-west1", "protected": true,
      "health_url": "https://shop.example.org/health/",
      "resources": [ { "type": "vm", "name": "box" }, { "type": "sql", "name": "shop-db" } ],
      "vars": { "ssh": "gcloud compute ssh box --zone={zone} --project={project} --tunnel-through-iap",
                "compose_dir": "~/shop", "service": "web" },
      "actions": [
        { "key": "l", "label": "logs", "command": "{ssh} --command 'cd {compose_dir} && docker compose logs -f --tail=200'" },
        { "key": "i", "label": "status", "command": "{ssh} --command 'cd {compose_dir} && docker compose ps'" },
        { "key": "c", "label": "console", "command": "{ssh} -- -t 'cd {compose_dir} && docker compose exec {service} python manage.py shell'", "interactive": true },
        { "key": "M", "label": "migrate", "command": "{ssh} --command 'cd {compose_dir} && docker compose exec -T {service} python manage.py migrate'", "confirm": true }
      ] }
  ]
}
```

**Cloud Run, deployed from your machine:**

```json
{
  "envs": [
    { "name": "prod", "project": "app-prod", "region": "europe-west1", "protected": true,
      "health_url": "https://api.example.com/healthz",
      "resources": [ { "type": "cloudrun", "name": "api" }, { "type": "images", "repo": "containers/api" } ],
      "deploy": { "command": "gcloud run deploy {cloudrun} --source . --region={region} --project={project}" },
      "actions": [ { "key": "l", "label": "logs", "command": "{gcdeck} cloudrun-logs {cloudrun} --region={region} --project={project}" },
                   { "key": "u", "label": "job migrate", "command": "gcloud run jobs execute migrate --region={region} --project={project} --wait", "confirm": true } ] }
  ]
}
```

## Security

A config's commands run on your machine with your gcloud and GitHub logins, so gcdeck is careful about where a config
comes from and what it shows.

- **Repo configs must be trusted first.** The first time gcdeck finds a `.gcdeck.json`, and again whenever the file
  changes, it lists everything the file would run and runs nothing until you type `trust`. Without a terminal to ask
  on, it stops; `gcdeck trust` reviews a config and `gcdeck untrust` takes trust back. Your own
  `~/.config/gcdeck/config.json` is trusted as it is. Trust is recorded by file path and content hash in
  `~/.config/gcdeck/trusted.json` (readable only by you). `gcdeck init` trusts the file it writes, unless the file was
  already there and untrusted.
- **Configs anyone else could have written are refused:** files owned by another user, or writable by group or others.
- **Config values are checked** when gcdeck loads: project ids, regions, zones, resource names, branches, workflows and
  the GitHub repo must look like what they are, so a value cannot pose as a command-line flag or rewrite an API path.
- **Text from outside is cleaned before it reaches your terminal.** Logs, command output, commit messages, job names
  and error messages have escape sequences and control characters removed, including ones a JSON log line encodes,
  so a request with a crafted User-Agent cannot set your clipboard or redraw your screen.
- **Commands gcdeck builds from fetched data run without a shell**, and only `https` links open in the browser.
- **gcdeck stores no credentials**; it uses the `gcloud` and `gh` logins you already have. GKE kubeconfigs it fetches
  live in `~/.cache/gcdeck`, readable only by you.

What it does not protect against: anything that already runs as you. A program that can type into the gcdeck pane
(Herdr lets agents send keys) can type a confirmation word too; the typed word stops slips, not a determined program.

## Troubleshooting

- **"gcloud login expired"**: press `a`, sign in in the pane that opens, then press `r`.
- **A card says an API is not enabled, or permission was denied**: gcdeck can only see what your gcloud account
  can see. Remove that resource from the config, or get `roles/viewer` on the project.
- **No CI or deploy panel**: gcdeck could not find a GitHub repo. Set `github_repo`, and check `gh auth status`.
- **`gcdeck check`** prints every resolved command without running anything.

## Development

```bash
go test ./...
go build -o gcdeck . && ./gcdeck -config path/to/config.json
```
