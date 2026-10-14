package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return true
		}
	}
	return strings.ContainsRune(s, 0x9b)
}

func TestCleanRemovesTerminalControl(t *testing.T) {
	for name, in := range map[string]string{
		"OSC 52 clipboard, BEL": "a\x1b]52;c;Y3VybCBldmlsIHwgc2g=\x07b",
		"OSC title, ST":         "a\x1b]0;owned\x1b\\b",
		"OSC 8 hyperlink":       "\x1b]8;;https://evil.example\x1b\\click\x1b]8;;\x1b\\",
		"unterminated OSC":      "a\x1b]0;never ends",
		"CSI clear screen":      "a\x1b[2J\x1b[Hb",
		"C1 CSI as a rune":      "a\u009b2Jb",
		"C1 CSI as a raw byte":  "a\x9b2Jb",
		"DCS":                   "a\x1bP+q544e\x1b\\b",
		"carriage return":       "safe text\rEVIL",
		"backspaces":            "rm -rf /\x08\x08\x08\x08\x08\x08\x08\x08ls",
		"bidi override":         "invoice‮gpj.exe",
	} {
		got := clean(in)
		if hasControl(got) || strings.ContainsAny(got, "‮") {
			t.Errorf("%s: %q still holds control characters", name, got)
		}
	}
	if got := clean("tab\there"); got != "tab    here" {
		t.Errorf("tabs: %q", got)
	}
	if got := cleanKeepTabs("job\tstep\tline"); got != "job\tstep\tline" {
		t.Errorf("cleanKeepTabs: %q", got)
	}
	if got := clean("plain ascii stays as it is"); got != "plain ascii stays as it is" {
		t.Errorf("plain: %q", got)
	}
}

// The attack from the review: a request whose User-Agent carries an escape sequence, logged JSON-escaped by the
// proxy, decoded back into a real escape character when gcdeck tidies the line.
func TestJSONLogCannotSmuggleEscapes(t *testing.T) {
	line := `2026-10-13T21:45:21Z {"level":"INFO","msg":"Request","user_agent":"\u001b]52;c;Y3VybA==\u0007x","path":"/\u009b2J"}`
	got := formatLogLine(stripANSI(line))
	if hasControl(got.text) || hasControl(got.prefix) {
		t.Errorf("escape reached the screen: %q", got.text)
	}
}

func TestReadLineCapsLength(t *testing.T) {
	long := strings.Repeat("x", 3*maxLineBytes) + "\nnext\n"
	r := bufio.NewReaderSize(strings.NewReader(long), 4096)
	first, err := readLine(r)
	if err != nil || len(first) > maxLineBytes+64 || !strings.HasSuffix(first, "(line cut at 64 KB)") {
		t.Fatalf("first line: %d bytes, err %v", len(first), err)
	}
	if second, _ := readLine(r); second != "next" {
		t.Errorf("the line after a cut one: %q", second)
	}
	empty := bufio.NewReader(strings.NewReader("\nafter\n"))
	if l, err := readLine(empty); l != "" || err != nil {
		t.Errorf("an empty line should come through: %q %v", l, err)
	}
}

func keyRunes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func TestConfirmationNeedsTypedWord(t *testing.T) {
	c := newConfirmation("Deploy?", &Env{Name: "staging"}, nil)
	if done, ok := c.key(keyRunes("y")); done || ok {
		t.Fatal("a single y must not confirm")
	}
	c.typed = ""
	if done, ok := c.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("yes"), Paste: true}); done || ok || c.typed != "" {
		t.Fatal("pasted text must not count")
	}
	if done, ok := c.key(tea.KeyMsg{Type: tea.KeyEnter}); done || ok {
		t.Fatal("enter alone must not confirm")
	}
	for _, r := range "yes" {
		c.key(keyRunes(string(r)))
	}
	if done, ok := c.key(tea.KeyMsg{Type: tea.KeyEnter}); !done || !ok {
		t.Fatal("typing yes and enter should confirm")
	}
	p := newConfirmation("Deploy?", &Env{Name: "production", Protected: true}, nil)
	if p.require != "production" {
		t.Errorf("a protected env should ask for its name, asks for %q", p.require)
	}
	if done, ok := p.key(tea.KeyMsg{Type: tea.KeyEsc}); !done || ok {
		t.Error("esc should cancel")
	}
}

func TestOpenURLOnlyHTTPS(t *testing.T) {
	for _, link := range []string{"file:///Applications/Calculator.app", "javascript:alert(1)", "/etc/passwd", "http://example.com", "https://"} {
		msg := openURL(link)().(actionMsg)
		if msg.err == nil {
			t.Errorf("%q should be refused", link)
		}
	}
}

// writeRepoConfig puts a .gcdeck.json in a fresh repo dir, with HOME pointed at a temp dir so the trust store is
// the test's own.
func writeRepoConfig(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	p := filepath.Join(dir, localConfigName)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

const evilConfig = `{"github_repo":"acme/app","envs":[{"name":"x","project":"my-project","version_command":"touch /tmp/gcdeck-pwned"}]}`

func TestRepoConfigNeedsTrust(t *testing.T) {
	p := writeRepoConfig(t, evilConfig, 0o644)
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if trustState(cfg) != "new" {
		t.Fatalf("a repo config should start untrusted, is %s", trustState(cfg))
	}
	var out strings.Builder
	if err := ensureTrusted(cfg, strings.NewReader(""), &out, false); err == nil {
		t.Fatal("with no terminal to ask on, an untrusted config must be refused")
	}
	if err := ensureTrusted(cfg, strings.NewReader("y\n"), &out, true); err == nil {
		t.Fatal("anything but the word trust must refuse")
	}
	if !strings.Contains(out.String(), "touch /tmp/gcdeck-pwned") {
		t.Error("the prompt must show the commands the config would run")
	}
	if err := ensureTrusted(cfg, strings.NewReader("trust\n"), &out, true); err != nil {
		t.Fatal(err)
	}
	if trustState(cfg) != "trusted" {
		t.Fatal("trusting should stick")
	}
	// Any change to the file means asking again.
	if err := os.WriteFile(p, []byte(strings.Replace(evilConfig, "pwned", "pwned2", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if trustState(changed) != "changed" {
		t.Errorf("a changed config should need trust again, is %s", trustState(changed))
	}
	info, _ := os.Stat(trustStorePath())
	if info.Mode().Perm() != 0o600 {
		t.Errorf("trust store is %v, want 0600", info.Mode().Perm())
	}
}

func TestGlobalConfigIsTrusted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	g := globalConfigPath()
	_ = os.MkdirAll(filepath.Dir(g), 0o700)
	if err := os.WriteFile(g, []byte(evilConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(g)
	if err != nil {
		t.Fatal(err)
	}
	if trustState(cfg) != "trusted" {
		t.Error("your own global config should not need trust")
	}
}

func TestConfigWritableByOthersRefused(t *testing.T) {
	for _, mode := range []os.FileMode{0o664, 0o646} {
		p := writeRepoConfig(t, evilConfig, mode)
		if _, err := loadConfig(p); err == nil || !strings.Contains(err.Error(), "writable by other users") {
			t.Errorf("mode %v: err = %v", mode, err)
		}
	}
}

func TestInitDoesNotVouchForExistingCommands(t *testing.T) {
	p := writeRepoConfig(t, evilConfig, 0o644)
	if initMayVouch(p) {
		t.Error("init must not trust a file whose earlier contents were never trusted")
	}
	if !initMayVouch(filepath.Join(filepath.Dir(p), "new.json")) {
		t.Error("init should trust a file it creates")
	}
	cfg, _ := loadConfig(p)
	_ = saveTrust(cfg.absPath(), cfg.hash)
	if !initMayVouch(p) {
		t.Error("init may keep trusting a file you already trusted")
	}
}
