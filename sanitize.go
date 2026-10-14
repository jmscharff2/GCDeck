package main

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Anything gcdeck shows that it did not write itself (log lines, command output, commit messages, job names, error
// text) passes through clean first. Server logs carry text anyone on the internet can send, and a terminal acts on
// control sequences in what it draws: one hidden in a User-Agent could set the clipboard (OSC 52), retitle the window
// or redraw the screen.

var escapeSequence = regexp.MustCompile(
	// OSC, DCS, SOS, PM and APC strings, ended by BEL or ST (ESC \ or the C1 byte), or left unterminated.
	"\x1b[\\]PX^_][^\x07\x1b\u009c]*(?:\x07|\x1b\\\\|\u009c)?" +
		// CSI, from ESC [ or the C1 CSI.
		"|(?:\x1b\\[|\u009b)[0-?]*[ -/]*[@-~]" +
		// Any other escape: ESC, optional intermediates, a final byte.
		"|\x1b[ -/]*[0-~]?")

// clean removes escape sequences and control characters, turning tabs into spaces. Invalid UTF-8 becomes U+FFFD, so
// a lone C1 byte cannot reach the terminal either, and bidirectional overrides, which can make text read as something
// it is not, are dropped.
func clean(s string) string { return sanitize(s, false) }

// cleanKeepTabs is clean for raw output that is split on tabs before it is shown (gh's job logs).
func cleanKeepTabs(s string) string { return sanitize(s, true) }

func sanitize(s string, keepTabs bool) string {
	if isPlain(s) {
		return s
	}
	s = strings.ToValidUTF8(s, "\ufffd")
	s = escapeSequence.ReplaceAllString(s, "")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t' && keepTabs:
			b.WriteRune(r)
		case r == '\t':
			b.WriteString("    ")
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == 0xfeff:
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isPlain is the fast path: printable ASCII needs no work.
func isPlain(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return clean(err.Error())
}

// cleanCard cleans every string a resource fetch returns; their values come from gcloud and the services themselves.
func cleanCard(c card) card {
	c.Status, c.Warn = clean(c.Status), clean(c.Warn)
	for i := range c.Rows {
		c.Rows[i] = [2]string{clean(c.Rows[i][0]), clean(c.Rows[i][1])}
	}
	for i := range c.Lines {
		name, when, _ := strings.Cut(c.Lines[i], "\t")
		c.Lines[i] = clean(name) + "\t" + clean(when)
	}
	return c
}

func cleanRun(r *Run) {
	r.Status, r.Conclusion, r.Branch = clean(r.Status), clean(r.Conclusion), clean(r.Branch)
	r.SHA, r.Event, r.Title, r.URL = clean(r.SHA), clean(r.Event), clean(r.Title), clean(r.URL)
}

func cleanJobs(jobs []jobDetail) {
	for i := range jobs {
		j := &jobs[i]
		j.Name, j.Status, j.Conclusion, j.URL = clean(j.Name), clean(j.Status), clean(j.Conclusion), clean(j.URL)
		for k := range j.Steps {
			s := &j.Steps[k]
			s.Name, s.Status, s.Conclusion = clean(s.Name), clean(s.Status), clean(s.Conclusion)
		}
	}
}

// truncateBytes keeps at most n bytes without splitting a character.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
