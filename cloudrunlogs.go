package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

// runCloudRunLogs streams a Cloud Run service's logs by polling Cloud Logging. gcloud's own `run services logs tail`
// needs the beta component, which prompts to install itself, and a prompt cannot be answered inside gcdeck's views.
func runCloudRunLogs(args []string) int {
	fs := flag.NewFlagSet("cloudrun-logs", flag.ExitOnError)
	region := fs.String("region", "", "the service's region")
	project := fs.String("project", "", "project id")
	every := fs.Duration("every", 3*time.Second, "how often to poll")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: gcdeck cloudrun-logs <service> -region=<region> -project=<project>")
	}
	// Flags may come after the service name.
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		args = append(args[1:], args[0])
	}
	_ = fs.Parse(args)
	if fs.NArg() != 1 || *project == "" {
		fs.Usage()
		return 2
	}
	service := fs.Arg(0)
	filter := fmt.Sprintf(`resource.type="cloud_run_revision" AND resource.labels.service_name="%s"`, service)
	if *region != "" {
		filter += fmt.Sprintf(` AND resource.labels.location="%s"`, *region)
	}

	// The last 100 entries first, newest last, then whatever is newer than the last one shown.
	entries, err := readLogs(*project, filter, "--freshness=1d", "--order=desc", "--limit=100")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	last, seen := printEntries(entries, "", nil)
	for {
		time.Sleep(*every)
		since := last
		if since == "" {
			since = time.Now().Add(-*every).UTC().Format(time.RFC3339Nano)
		}
		entries, err := readLogs(*project, filter+fmt.Sprintf(` AND timestamp>="%s"`, since), "--order=asc", "--limit=500")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			continue
		}
		last, seen = printEntries(entries, last, seen)
	}
}

type logEntry struct {
	InsertID    string          `json:"insertId"`
	Timestamp   string          `json:"timestamp"`
	Severity    string          `json:"severity"`
	TextPayload string          `json:"textPayload"`
	JSONPayload json.RawMessage `json:"jsonPayload"`
	HTTPRequest *struct {
		Method  string `json:"requestMethod"`
		URL     string `json:"requestUrl"`
		Status  int    `json:"status"`
		Latency string `json:"latency"`
	} `json:"httpRequest"`
}

func readLogs(project, filter string, extra ...string) ([]logEntry, error) {
	args := append([]string{"logging", "read", filter, "--project=" + project, "--format=json"}, extra...)
	var entries []logEntry
	err := runJSON(&entries, "gcloud", args...)
	return entries, err
}

// printEntries writes each entry as "<timestamp> <json>", which gcdeck's log view tidies, skipping any already shown
// (polls overlap by the last timestamp so nothing logged in the same instant is missed).
func printEntries(entries []logEntry, last string, seen map[string]bool) (string, map[string]bool) {
	next := map[string]bool{}
	for _, e := range entries {
		if seen[e.InsertID] {
			next[e.InsertID] = true
			continue
		}
		line := map[string]any{"level": e.Severity}
		switch {
		case e.HTTPRequest != nil:
			line["msg"] = "Request"
			line["method"], line["path"], line["status"], line["dur"] = e.HTTPRequest.Method, e.HTTPRequest.URL,
				e.HTTPRequest.Status, e.HTTPRequest.Latency
		case e.TextPayload != "":
			line["msg"] = e.TextPayload
		case len(e.JSONPayload) > 0:
			var payload map[string]any
			if json.Unmarshal(e.JSONPayload, &payload) == nil {
				for k, v := range payload {
					line[k] = v
				}
			}
		}
		out, _ := json.Marshal(line)
		fmt.Printf("%s %s\n", e.Timestamp, out)
		if e.Timestamp >= last {
			if e.Timestamp > last {
				next = map[string]bool{}
			}
			last = e.Timestamp
			next[e.InsertID] = true
		}
	}
	if len(entries) == 0 {
		return last, seen
	}
	return last, next
}
