package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/term"
)

// A .gcdeck.json can come from any repo, and its commands run on your machine with your Google Cloud and GitHub
// logins: version_command as soon as gcdeck opens, actions a key away. So gcdeck runs nothing from a repo's config
// until you have read what it would run and trusted it, and asks again whenever the file changes, the way direnv
// treats .envrc. Your own ~/.config/gcdeck/config.json is trusted as it is.

func trustStorePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "gcdeck", "trusted.json")
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func loadTrustStore() map[string]string {
	store := map[string]string{}
	if data, err := os.ReadFile(trustStorePath()); err == nil {
		_ = json.Unmarshal(data, &store)
	}
	return store
}

func saveTrust(path, hash string) error {
	store := loadTrustStore()
	if hash == "" {
		delete(store, path)
	} else {
		store[path] = hash
	}
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	p := trustStorePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	// Written whole and renamed into place, readable only by you.
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// implicitlyTrusted is your own global config, which no repo can supply.
func implicitlyTrusted(path string) bool {
	return samePath(path, globalConfigPath())
}

func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// trustState says whether a config may run its commands: "trusted", "new" (never trusted) or "changed".
func trustState(cfg Config) string {
	if implicitlyTrusted(cfg.path) {
		return "trusted"
	}
	h, ok := loadTrustStore()[cfg.absPath()]
	switch {
	case !ok:
		return "new"
	case h != cfg.hash:
		return "changed"
	}
	return "trusted"
}

func (c Config) absPath() string {
	p, err := filepath.Abs(c.path)
	if err != nil {
		return c.path
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// checkOwnership refuses a config someone else could have written: one owned by another user, or writable by others.
func checkOwnership(path string, info os.FileInfo) error {
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by other users; fix with: chmod go-w %s", path, shellQuote(path))
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s belongs to another user, so gcdeck will not use it", path)
	}
	return nil
}

// ensureTrusted asks you to trust a config that is new or has changed, showing everything it would run. It reads the
// answer from the terminal; with no terminal it stops and says how to trust the file.
func ensureTrusted(cfg Config, in io.Reader, out io.Writer, interactive bool) error {
	state := trustState(cfg)
	if state == "trusted" {
		return nil
	}
	if !interactive {
		return fmt.Errorf("%s is not trusted yet: run `gcdeck trust -config %s` to review what it runs", cfg.path, shellQuote(cfg.path))
	}
	head := "This config is new to gcdeck."
	if state == "changed" {
		head = "This config has changed since you trusted it."
	}
	fmt.Fprintf(out, "%s gcdeck would run these commands on this machine, with your gcloud and gh logins:\n\n", head)
	printSummary(out, cfg)
	fmt.Fprintf(out, "\nOnly trust it if you know where it came from. Type trust to allow it, anything else to stop: ")
	answer, _ := bufio.NewReader(in).ReadString('\n')
	if strings.TrimSpace(answer) != "trust" {
		return errors.New("not trusted, nothing was run")
	}
	if err := saveTrust(cfg.absPath(), cfg.hash); err != nil {
		return fmt.Errorf("could not save trust: %w", err)
	}
	fmt.Fprintln(out, "Trusted. gcdeck asks again if the file changes.")
	return nil
}

// isTerminal asks the terminal driver, so /dev/null (also a character device) does not count.
func isTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// printSummary lists what a config points at and every command it can run, expanded the way they will run.
func printSummary(out io.Writer, cfg Config) {
	fmt.Fprintln(out, "config  ", cfg.path)
	fmt.Fprintln(out, "app dir ", cfg.AppDir)
	fmt.Fprintln(out, "github  ", or(cfg.GitHubRepo, "(none: CI and deploy panels off)"))
	if p := cfg.Promote; p != nil {
		if p.Command != "" {
			fmt.Fprintln(out, "promote ", p.Command)
		} else {
			fmt.Fprintf(out, "promote  %s → %s with %s\n", p.From, p.To, p.Workflow)
		}
	}
	for _, e := range cfg.Envs {
		fmt.Fprintf(out, "\n%s  (%s", e.Name, e.Project)
		if e.Branch != "" {
			fmt.Fprintf(out, ", branch %s", e.Branch)
		}
		fmt.Fprintln(out, ")")
		if e.HealthURL != "" {
			fmt.Fprintln(out, "  health    ", e.HealthURL)
		}
		for _, r := range e.Resources {
			fmt.Fprintf(out, "  %-10s %s\n", r.Type, or(r.Name, imagePath(e, r)))
		}
		if e.VersionCommand != "" {
			fmt.Fprintln(out, "  version   ", e.expand(e.VersionCommand), " (runs on its own, every refresh)")
		}
		if d := e.Deploy; d != nil {
			if d.Command != "" {
				fmt.Fprintln(out, "  deploy    ", e.expand(d.Command))
			} else {
				fmt.Fprintf(out, "  deploy     %s on %s, runs matched by %s\n", d.Workflow, d.Ref, matchMode(cfg, e))
			}
		}
		for _, a := range cfg.actions(e) {
			note := ""
			if a.Confirm {
				note = "  (asks first)"
			}
			fmt.Fprintf(out, "  key %s      %s: %s%s\n", a.Key, a.Label, e.expand(a.Command), note)
		}
	}
}

func runTrust(args []string, untrust bool) int {
	path := ""
	if len(args) >= 2 && args[0] == "-config" {
		path = args[1]
	} else if len(args) == 1 {
		path = args[0]
	}
	cfg, err := openConfigUntrusted(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gcdeck:", err)
		return 1
	}
	if untrust {
		if err := saveTrust(cfg.absPath(), ""); err != nil {
			fmt.Fprintln(os.Stderr, "gcdeck:", err)
			return 1
		}
		fmt.Println("No longer trusted:", cfg.path)
		return 0
	}
	if trustState(cfg) == "trusted" {
		fmt.Println("Already trusted:", cfg.path)
		return 0
	}
	if err := ensureTrusted(cfg, os.Stdin, os.Stdout, isTerminal(os.Stdin)); err != nil {
		fmt.Fprintln(os.Stderr, "gcdeck:", err)
		return 1
	}
	return 0
}
