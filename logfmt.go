package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

var (
	// "web-1  | " from docker compose logs, "[pod/api-7d9/app] " from kubectl logs --prefix.
	sourcePrefix = regexp.MustCompile(`^(?:([A-Za-z0-9_.-]+)\s+\| ?|\[(pod/[^\]]+)\] )`)
	leadingStamp = regexp.MustCompile(`^(\d{4}-\d\d-\d\d[T ]\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:?\d\d)?)\s+`)
	badWords     = regexp.MustCompile(`(?i)\b(error|fatal|exception|panic|failed|failure|denied|refused)\b|##\[error\]`)
	warnWords    = regexp.MustCompile(`(?i)\b(warn|warning|deprecated|retry(ing)?)\b|##\[warning\]`)
	tableHeader  = regexp.MustCompile(`^\s*(CONTAINER ID|NAME|ID)\s{2,}[A-Z ]+`)
	hostLine     = regexp.MustCompile(`^\S.*\bHost: \S+$`)
)

// markHeader starts an ingested line that formatLogLine shows as a section header.
const markHeader = "\x01"

// stripANSI cleans a raw output line as it arrives, keeping tabs for ingest functions that split on them.
func stripANSI(s string) string { return cleanKeepTabs(s) }

// formatLogLine styles one line of logs or command output. It cleans again at the end: decoding a JSON log line can
// turn an escaped "\u001b" back into a real escape character.
func formatLogLine(line string) styled {
	s := formatLogLineRaw(line)
	s.prefix, s.text = clean(s.prefix), clean(s.text)
	return s
}

func formatLogLineRaw(line string) styled {
	if strings.HasPrefix(line, markHeader) {
		return styled{text: strings.TrimPrefix(line, markHeader), style: sTitle}
	}
	if t, ok := formatTableLine(line); ok {
		return t
	}
	if strings.HasPrefix(line, "$ ") {
		return styled{text: line, style: sMuted}
	}
	out := styled{text: line, style: lipgloss.NewStyle()}
	if m := sourcePrefix.FindStringSubmatch(out.text); m != nil {
		src := or(m[1], m[2])
		if i := strings.LastIndex(src, "/"); i >= 0 {
			src = src[i+1:] // the container, which is what tells kubectl's lines apart
		}
		out.prefix = src + " "
		out.text = out.text[len(m[0]):]
	}
	if m := leadingStamp.FindStringSubmatch(out.text); m != nil {
		if t, ok := parseStamp(m[1]); ok {
			out.prefix += t.Local().Format("15:04:05") + " "
			out.text = out.text[len(m[0]):]
		}
	}
	if j, ok := formatJSONLog(out.text); ok {
		out.text = j.text
		out.style = j.style
		return out
	}
	text := out.text
	switch {
	case tableHeader.MatchString(text):
		out.style = sBold
	case hostLine.MatchString(text):
		out.style = sTitle
	case strings.HasPrefix(text, "##[group]"):
		out.text, out.style = "▾ "+strings.TrimPrefix(text, "##[group]"), sBold
	case strings.HasPrefix(text, "##[endgroup]"):
		out.text, out.style = "", sMuted
	case badWords.MatchString(text):
		out.style = sBad
	case warnWords.MatchString(text):
		out.style = sWarn
	case strings.Contains(text, " Up ") || strings.HasSuffix(text, "(healthy)"):
		out.style = sGood
	case strings.Contains(text, " Exited "):
		out.style = sBad
	}
	out.text = strings.NewReplacer("##[error]", "", "##[warning]", "", "##[notice]", "").Replace(out.text)
	return out
}

func parseStamp(s string) (time.Time, bool) {
	s = strings.Replace(s, " ", "T", 1)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999-0700", "2006-01-02T15:04:05.999999999"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// Request fields shown first, in this order, when a JSON log line has them.
var leadingKeys = []string{"method", "host", "path", "query", "status", "dur", "duration", "duration_ms", "remote_addr", "user_agent"}

// formatJSONLog turns {"time":…,"level":"INFO","msg":"Request","status":200,…} into
// "INFO Request method=GET path=/ status=200 …", coloured by level and by HTTP status.
func formatJSONLog(s string) (styled, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return styled{}, false
	}
	var fields map[string]any
	if json.Unmarshal([]byte(s), &fields) != nil {
		return styled{}, false
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := fields[k]; ok {
				delete(fields, k)
				return fmt.Sprint(v)
			}
		}
		return ""
	}
	pick("time", "timestamp", "ts", "@timestamp")
	level := strings.ToUpper(pick("level", "severity", "lvl"))
	msg := pick("msg", "message")

	// Flatten one level of nesting ({"request":{"method":…}} becomes request.method), so it all fits on a line.
	flat := map[string]string{}
	for k, v := range fields {
		if inner, ok := v.(map[string]any); ok {
			for ik, iv := range inner {
				flat[ik] = scalar(iv)
			}
			continue
		}
		flat[k] = scalar(v)
	}
	for k, v := range flat {
		if v == "" || v == "null" || v == `""` {
			delete(flat, k) // empty fields are noise on a line read at a glance
		}
	}
	status := flat["status"]
	var parts []string
	if level != "" {
		parts = append(parts, pad(level, 5))
	}
	if msg != "" {
		parts = append(parts, msg)
	}
	for _, k := range leadingKeys {
		if v, ok := flat[k]; ok {
			parts = append(parts, k+"="+v)
			delete(flat, k)
		}
	}
	rest := make([]string, 0, len(flat))
	for k := range flat {
		rest = append(rest, k)
	}
	sort.Strings(rest)
	for _, k := range rest {
		parts = append(parts, k+"="+flat[k])
	}

	style := lipgloss.NewStyle()
	switch {
	case level == "ERROR" || level == "FATAL" || level == "CRITICAL" || strings.HasPrefix(status, "5"):
		style = sBad
	case level == "WARN" || level == "WARNING" || strings.HasPrefix(status, "4"):
		style = sWarn
	case level == "DEBUG":
		style = sMuted
	}
	return styled{text: strings.Join(parts, " "), style: style}, true
}

func scalar(v any) string {
	switch t := v.(type) {
	case string:
		if strings.ContainsAny(t, " \t") {
			return fmt.Sprintf("%q", t)
		}
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprint(int64(t))
		}
		return fmt.Sprintf("%.2f", t)
	case nil:
		return "null"
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// jobLogIngest turns `gh run view --job --log` output into display lines, starting a header whenever a line's time
// falls into the next step. gh prints "UNKNOWN STEP" for every line, so the steps' own times are the only guide.
func jobLogIngest(steps []step) func(string) []string {
	current := -1
	return func(raw string) []string {
		// <job>\t<step>\t<timestamp> <text>
		parts := strings.SplitN(raw, "\t", 3)
		if len(parts) != 3 {
			return []string{raw}
		}
		line := parts[2]
		var out []string
		if m := leadingStamp.FindStringSubmatch(line); m != nil {
			if t, ok := parseStamp(m[1]); ok {
				if i := stepAt(steps, t); i > current {
					current = i
					s := steps[i]
					out = append(out, "", markHeader+fmt.Sprintf("%s %s", stepIcon(s), s.Name)+durationNote(s.StartedAt, s.CompletedAt))
				}
			}
		}
		return append(out, line)
	}
}

// stepAt finds the step running at t. Steps are timed to the second, so a line can fall just outside every window;
// it then belongs to the last step that started by t.
func stepAt(steps []step, t time.Time) int {
	best := -1
	for i, s := range steps {
		if s.StartedAt.IsZero() {
			continue
		}
		if !s.StartedAt.After(t.Truncate(time.Second)) {
			best = i
		}
	}
	return best
}

func durationNote(start, end time.Time) string {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return ""
	}
	return "  (" + end.Sub(start).Round(time.Second).String() + ")"
}
