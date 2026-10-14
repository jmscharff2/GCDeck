package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"path"
	"strings"
	"time"
)

// healthUserAgent marks gcdeck's own health checks, so log views can leave them out.
const healthUserAgent = "gcdeck-healthcheck"

// errAuth means a CLI needs an interactive login before anything else will work.
var errAuth = errors.New("login expired")

type level int

const (
	lvNone level = iota
	lvGood
	lvWarn
	lvBad
)

// card is what every resource fetch returns, so the UI renders them all one way.
type card struct {
	Status string
	Level  level
	Rows   [][2]string // label, value
	Lines  []string    // free-form lines under the rows
	Warn   string
}

const cmdTimeout = 45 * time.Second

func run(name string, args ...string) ([]byte, error) {
	return runIn("", name, args...)
}

func runIn(dir, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var notFound *exec.Error
		if errors.As(err, &notFound) {
			return nil, fmt.Errorf("%s is not installed", name)
		}
		return nil, cliError(name, stderr.String(), err)
	}
	return out, nil
}

func runJSON(v any, name string, args ...string) error {
	out, err := run(name, args...)
	if err != nil {
		return err
	}
	return json.Unmarshal(out, v)
}

// cliError turns a CLI's stderr into one readable line, recognizing expired logins.
func cliError(name, stderr string, err error) error {
	if strings.Contains(stderr, "Reauthentication failed") || strings.Contains(stderr, "gcloud auth login") ||
		strings.Contains(stderr, "gh auth login") {
		return fmt.Errorf("%s: %w", name, errAuth)
	}
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "WARNING") {
			return errors.New(clean(strings.TrimPrefix(line, "ERROR: ")))
		}
	}
	return fmt.Errorf("%s: %v", name, err)
}

func resourceTitle(r Resource) string {
	switch r.Type {
	case "vm":
		return "VM  " + r.Name
	case "sql":
		return "Cloud SQL  " + r.Name
	case "cloudrun":
		return "Cloud Run  " + r.Name
	case "gke":
		return "GKE  " + r.Name
	case "images":
		return "Images  " + path.Base(r.Repo)
	}
	return r.Type
}

func fetchResource(e Env, r Resource) (card, error) {
	c, err := fetchResourceRaw(e, r)
	return cleanCard(c), err
}

func fetchResourceRaw(e Env, r Resource) (card, error) {
	switch r.Type {
	case "vm":
		return fetchVM(e, r)
	case "sql":
		return fetchSQL(e, r)
	case "cloudrun":
		return fetchCloudRun(e, r)
	case "gke":
		return fetchGKE(e, r)
	case "images":
		return fetchImages(e, r)
	}
	return card{}, fmt.Errorf("unknown resource type %q", r.Type)
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func stateCard(state string, good ...string) card {
	c := card{Status: strings.ToLower(state), Level: lvWarn}
	switch {
	case contains(good, state):
		c.Level = lvGood
	case contains([]string{"STOPPED", "TERMINATED", "SUSPENDED", "FAILED", "ERROR", "DEGRADED"}, state):
		c.Level = lvBad
	}
	return c
}

func fetchHealth(url string) (card, error) {
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return card{}, err
	}
	req.Header.Set("User-Agent", healthUserAgent)
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		var certErr *tls.CertificateVerificationError
		if errors.As(err, &certErr) {
			return card{Status: "TLS certificate invalid", Level: lvBad}, nil
		}
		return card{Status: "down: " + shortNetErr(err), Level: lvBad}, nil
	}
	defer resp.Body.Close()
	c := card{Status: fmt.Sprintf("up  %d", resp.StatusCode), Level: lvGood}
	if resp.StatusCode >= 300 {
		c.Status, c.Level = fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode)), lvBad
	}
	c.Rows = append(c.Rows, [2]string{"latency", time.Since(start).Round(time.Millisecond).String()})
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		days := int(time.Until(resp.TLS.PeerCertificates[0].NotAfter).Hours() / 24)
		c.Rows = append(c.Rows, [2]string{"TLS cert", fmt.Sprintf("%dd left", days)})
		if days < 14 {
			c.Warn = "TLS certificate expires soon"
		}
	}
	return c, nil
}

func shortNetErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		return s[i+2:]
	}
	return s
}

func fetchVM(e Env, r Resource) (card, error) {
	var raw struct {
		Status             string `json:"status"`
		MachineType        string `json:"machineType"`
		LastStartTimestamp string `json:"lastStartTimestamp"`
		DeletionProtection bool   `json:"deletionProtection"`
		NetworkInterfaces  []struct {
			NetworkIP     string `json:"networkIP"`
			AccessConfigs []struct {
				NatIP string `json:"natIP"`
			} `json:"accessConfigs"`
		} `json:"networkInterfaces"`
	}
	err := runJSON(&raw, "gcloud", "compute", "instances", "describe", r.Name,
		"--zone="+or(r.Zone, e.Zone), "--project="+e.Project, "--format=json")
	if err != nil {
		return card{}, err
	}
	c := stateCard(raw.Status, "RUNNING")
	c.Rows = append(c.Rows, [2]string{"type", path.Base(raw.MachineType)})
	if len(raw.NetworkInterfaces) > 0 {
		ni := raw.NetworkInterfaces[0]
		ip := ni.NetworkIP + " (internal)"
		if len(ni.AccessConfigs) > 0 && ni.AccessConfigs[0].NatIP != "" {
			ip = ni.AccessConfigs[0].NatIP
		}
		c.Rows = append(c.Rows, [2]string{"ip", ip})
	}
	if t, err := time.Parse(time.RFC3339, raw.LastStartTimestamp); err == nil && raw.Status == "RUNNING" {
		c.Rows = append(c.Rows, [2]string{"up", age(t)})
	}
	if !raw.DeletionProtection {
		c.Warn = "deletion protection off"
	}
	return c, nil
}

func fetchSQL(e Env, r Resource) (card, error) {
	var raw struct {
		State           string `json:"state"`
		DatabaseVersion string `json:"databaseVersion"`
		Settings        struct {
			Tier                string `json:"tier"`
			BackupConfiguration struct {
				Enabled bool `json:"enabled"`
			} `json:"backupConfiguration"`
		} `json:"settings"`
		IPAddresses []struct {
			IP   string `json:"ipAddress"`
			Type string `json:"type"`
		} `json:"ipAddresses"`
	}
	err := runJSON(&raw, "gcloud", "sql", "instances", "describe", r.Name, "--project="+e.Project, "--format=json")
	if err != nil {
		return card{}, err
	}
	c := stateCard(raw.State, "RUNNABLE")
	c.Rows = append(c.Rows, [2]string{"tier", raw.Settings.Tier}, [2]string{"version", strings.ToLower(raw.DatabaseVersion)})
	for _, ip := range raw.IPAddresses {
		c.Rows = append(c.Rows, [2]string{strings.ToLower(ip.Type) + " ip", ip.IP})
	}
	if !raw.Settings.BackupConfiguration.Enabled {
		c.Warn = "backups off"
	}
	return c, nil
}

func fetchCloudRun(e Env, r Resource) (card, error) {
	var raw struct {
		Status struct {
			URL                     string `json:"url"`
			LatestReadyRevisionName string `json:"latestReadyRevisionName"`
			LatestCreatedRevision   string `json:"latestCreatedRevisionName"`
			Conditions              []struct {
				Type               string `json:"type"`
				Status             string `json:"status"`
				Message            string `json:"message"`
				LastTransitionTime string `json:"lastTransitionTime"`
			} `json:"conditions"`
			Traffic []struct {
				RevisionName string `json:"revisionName"`
				Percent      int    `json:"percent"`
			} `json:"traffic"`
		} `json:"status"`
	}
	err := runJSON(&raw, "gcloud", "run", "services", "describe", r.Name, "--region="+or(r.Region, e.Region),
		"--project="+e.Project, "--format=json")
	if err != nil {
		return card{}, err
	}
	c := card{Status: "unknown", Level: lvWarn}
	for _, cond := range raw.Status.Conditions {
		if cond.Type != "Ready" {
			continue
		}
		switch cond.Status {
		case "True":
			c.Status, c.Level = "ready", lvGood
		case "False":
			c.Status, c.Level = "not ready", lvBad
			c.Warn = cond.Message
		default:
			c.Status = "deploying"
		}
		if t, err := time.Parse(time.RFC3339, cond.LastTransitionTime); err == nil {
			c.Rows = append(c.Rows, [2]string{"since", age(t) + " ago"})
		}
	}
	c.Rows = append(c.Rows, [2]string{"revision", raw.Status.LatestReadyRevisionName})
	if raw.Status.LatestCreatedRevision != raw.Status.LatestReadyRevisionName && c.Warn == "" {
		c.Warn = raw.Status.LatestCreatedRevision + " not ready"
	}
	if len(raw.Status.Traffic) > 1 {
		var split []string
		for _, t := range raw.Status.Traffic {
			if t.Percent > 0 {
				split = append(split, fmt.Sprintf("%d%% %s", t.Percent, shortRevision(t.RevisionName)))
			}
		}
		c.Rows = append(c.Rows, [2]string{"traffic", strings.Join(split, ", ")})
	}
	c.Rows = append(c.Rows, [2]string{"url", strings.TrimPrefix(raw.Status.URL, "https://")})
	return c, nil
}

func shortRevision(name string) string {
	if i := strings.LastIndex(name, "-"); i >= 0 && i+1 < len(name) {
		return name[i+1:]
	}
	return name
}

func fetchGKE(e Env, r Resource) (card, error) {
	var raw struct {
		Status               string `json:"status"`
		CurrentMasterVersion string `json:"currentMasterVersion"`
		CurrentNodeCount     int    `json:"currentNodeCount"`
		Autopilot            struct {
			Enabled bool `json:"enabled"`
		} `json:"autopilot"`
		NodePools []struct {
			Status string `json:"status"`
		} `json:"nodePools"`
	}
	err := runJSON(&raw, "gcloud", "container", "clusters", "describe", r.Name,
		"--location="+or(r.Location, or(e.Region, e.Zone)), "--project="+e.Project, "--format=json")
	if err != nil {
		return card{}, err
	}
	c := stateCard(raw.Status, "RUNNING")
	mode := "standard"
	if raw.Autopilot.Enabled {
		mode = "autopilot"
	}
	c.Rows = append(c.Rows, [2]string{"version", raw.CurrentMasterVersion}, [2]string{"mode", mode},
		[2]string{"nodes", fmt.Sprint(raw.CurrentNodeCount)}, [2]string{"node pools", fmt.Sprint(len(raw.NodePools))})
	for _, np := range raw.NodePools {
		if np.Status != "RUNNING" {
			c.Warn = "a node pool is " + strings.ToLower(np.Status)
		}
	}
	return c, nil
}

// imagePath turns a short repo ("repo" or "repo/image") into the full Artifact Registry path.
func imagePath(e Env, r Resource) string {
	if strings.Contains(r.Repo, ".pkg.dev/") {
		return r.Repo
	}
	return fmt.Sprintf("%s-docker.pkg.dev/%s/%s", or(r.Region, e.Region), e.Project, r.Repo)
}

func fetchImages(e Env, r Resource) (card, error) {
	var raw []struct {
		Package    string          `json:"package"`
		Tags       json.RawMessage `json:"tags"`
		UpdateTime string          `json:"updateTime"`
	}
	err := runJSON(&raw, "gcloud", "artifacts", "docker", "images", "list", imagePath(e, r),
		"--include-tags", "--sort-by=~UPDATE_TIME", "--limit=12", "--format=json")
	if err != nil {
		return card{}, err
	}
	// Untagged digests are build cache and superseded pushes, not images anyone deploys.
	tagged := raw[:0]
	for _, img := range raw {
		if firstTag(img.Tags) != "untagged" {
			tagged = append(tagged, img)
		}
	}
	raw = tagged
	if len(raw) > 4 {
		raw = raw[:4]
	}
	c := card{}
	if len(raw) == 0 {
		c.Lines = append(c.Lines, "no images")
		return c, nil
	}
	// Name the image only when the repo holds more than one.
	multi := false
	for _, img := range raw {
		multi = multi || img.Package != raw[0].Package
	}
	for _, img := range raw {
		tag := firstTag(img.Tags)
		if len(tag) > 12 {
			tag = tag[:12]
		}
		if multi {
			tag = path.Base(img.Package) + ":" + tag
		}
		updated, _ := time.Parse(time.RFC3339, img.UpdateTime)
		c.Lines = append(c.Lines, fmt.Sprintf("%s\t%s ago", tag, age(updated)))
	}
	return c, nil
}

// firstTag picks a tag other than latest. gcloud has printed tags both as a list and as a comma-separated string.
func firstTag(raw json.RawMessage) string {
	var list []string
	var s string
	if json.Unmarshal(raw, &list) != nil && json.Unmarshal(raw, &s) == nil && s != "" {
		list = strings.Split(s, ",")
	}
	for _, t := range list {
		if t != "latest" {
			return t
		}
	}
	if len(list) > 0 {
		return list[0]
	}
	return "untagged"
}
