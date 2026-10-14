package main

import (
	"testing"
	"time"
)

func TestParseVersion(t *testing.T) {
	kamal := "  INFO [e9a9c900] Running /usr/bin/env sh -c 'docker ps' on 203.0.113.10\n" +
		"  INFO [e9a9c900] Finished in 3.484 seconds with exit status 0 (successful).\nApp Host: 203.0.113.10\n" +
		"c3628aa125c2edf92efdd51e183e9463433a6c61\n"
	for out, want := range map[string]string{
		kamal: "c3628aa125c2edf92efdd51e183e9463433a6c61",
		"europe-west1-docker.pkg.dev/p/app/api:3f2a9c1d7e8b\n": "3f2a9c1d7e8b",
		"9f8e7d6\n": "9f8e7d6",
	} {
		if got := parseVersion(out); got != want {
			t.Errorf("parseVersion(%q) = %q, want %q", out, got, want)
		}
	}
}

func TestSameCommit(t *testing.T) {
	full := "c3628aa125c2edf92efdd51e183e9463433a6c61"
	if !sameCommit(full, "c3628aa125c2") || !sameCommit("C3628AA", full) {
		t.Error("prefixes of the same commit should match")
	}
	if sameCommit(full, "c3628") || sameCommit(full, "") || sameCommit(full, "d3628aa") {
		t.Error("too short, empty or different should not match")
	}
}

func TestPipelineActive(t *testing.T) {
	cases := []struct {
		ci, deploy stageState
		want       bool
	}{
		{stageRunning, stageWaiting, true},
		{stageDone, stageWaiting, true}, // CI passed, deploy about to start
		{stageDone, stageRunning, true},
		{stageDone, stageDone, false},
		{stageFailed, stageSkipped, false},
	}
	for _, c := range cases {
		p := pipeline{CI: stage{State: c.ci}, Deploy: stage{State: c.deploy}}
		if p.active() != c.want {
			t.Errorf("CI %v, deploy %v: active = %v", c.ci, c.deploy, !c.want)
		}
	}
}

func TestAnnounceOnlyNews(t *testing.T) {
	cfg := Config{Envs: []Env{{Name: "staging", Branch: "staging", VersionCommand: "x"}}}
	m := newModel(cfg)
	set := func(key string, v any) any {
		p := m.panels[0][key]
		if p == nil {
			p = &panel{}
			m.panels[0][key] = p
		}
		before := p.val
		p.val, p.at = v, time.Now()
		return before
	}
	head := commitInfo{SHA: "c3628aa125c2edf92efdd51e183e9463433a6c61", Message: "Merge"}
	// Loaded for the first time with CI already failed: old news, nothing announced.
	before := set(pPipeline, pipeline{Head: head, CI: stage{State: stageFailed}})
	if m.announce(0, pPipeline, before) != nil {
		t.Error("a failure already there at start should not be announced")
	}
	// A new commit whose deploy then fails: announced once.
	head2 := commitInfo{SHA: "d000000aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Message: "Fix"}
	before = set(pPipeline, pipeline{Head: head2, CI: stage{State: stageDone}, Deploy: stage{State: stageRunning}})
	m.announce(0, pPipeline, before)
	if m.flash == "" {
		t.Error("a new commit should be mentioned")
	}
	before = set(pPipeline, pipeline{Head: head2, CI: stage{State: stageDone}, Deploy: stage{State: stageFailed}})
	if m.announce(0, pPipeline, before) == nil {
		t.Error("the deploy failing should be announced")
	}
	if m.announce(0, pPipeline, before) != nil {
		t.Error("and only once")
	}
	// The server switching to the newest commit is announced once.
	set(pVersion, "c3628aa125c2")
	before = set(pVersion, "d000000aaaaa")
	if m.announce(0, pVersion, before) == nil {
		t.Error("going live should be announced")
	}
}
