// gcdeck is a terminal dashboard for the Google Cloud environments an app deploys to: health checks, VMs, Cloud SQL,
// Cloud Run, GKE and images, plus GitHub Actions CI and deploys. Inside Herdr, actions open in sibling panes.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

var version = "dev"

const usage = `gcdeck: a terminal dashboard for your Google Cloud environments

Usage:
  gcdeck [-config path]     open the dashboard
  gcdeck init [flags]       write .gcdeck.json from a project and the repo here (gcdeck init -h)
  gcdeck check [-config p]  validate the config and print everything it would run
  gcdeck trust [-config p]  review a repo's config and allow it to run its commands
  gcdeck untrust [-config p]
  gcdeck cloudrun-logs <service> -region=<r> -project=<p>
                            stream a Cloud Run service's logs (used by the logs key)
  gcdeck version

The config is the first of: -config, $GCDECK_CONFIG, .gcdeck.json here or in a parent directory,
~/.config/gcdeck/config.json. See README.md for every field.
`

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "init":
			if err := runInit(os.Args[2:]); err != nil {
				if !errors.Is(err, flag.ErrHelp) {
					fmt.Fprintln(os.Stderr, "gcdeck init:", err)
				}
				os.Exit(1)
			}
			return
		case "check":
			os.Exit(runCheck(os.Args[2:]))
		case "cloudrun-logs":
			os.Exit(runCloudRunLogs(os.Args[2:]))
		case "trust":
			os.Exit(runTrust(os.Args[2:], false))
		case "untrust":
			os.Exit(runTrust(os.Args[2:], true))
		case "version", "--version":
			fmt.Println("gcdeck", version)
			return
		}
	}
	fs := flag.NewFlagSet("gcdeck", flag.ExitOnError)
	configFlag := fs.String("config", "", "config file")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	_ = fs.Parse(os.Args[1:])

	cfg, err := openConfig(*configFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gcdeck:", err)
		os.Exit(1)
	}
	if _, err := tea.NewProgram(newModel(cfg), tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "gcdeck:", err)
		os.Exit(1)
	}
}

// openConfigUntrusted finds and loads the config without the trust check, for commands that run nothing from it.
func openConfigUntrusted(explicit string) (Config, error) {
	path, err := findConfig(explicit)
	if err != nil {
		return Config{}, err
	}
	return loadConfig(path)
}

// openConfig loads the config and, for one gcdeck has not seen or that has changed, asks you to trust it first.
func openConfig(explicit string) (Config, error) {
	cfg, err := openConfigUntrusted(explicit)
	if err != nil {
		return Config{}, err
	}
	if err := ensureTrusted(cfg, os.Stdin, os.Stdout, isTerminal(os.Stdin)); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func runCheck(args []string) int {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	configFlag := fs.String("config", "", "config file")
	_ = fs.Parse(args)
	cfg, err := openConfigUntrusted(*configFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gcdeck:", err)
		return 1
	}
	printSummary(os.Stdout, cfg)
	if state := trustState(cfg); state != "trusted" {
		fmt.Printf("\nNot trusted yet (%s): gcdeck will ask before running any of this.\n", state)
	}
	return 0
}
