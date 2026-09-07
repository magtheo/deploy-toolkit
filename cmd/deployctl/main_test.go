package main

import "testing"

func TestParseProposeArgsDocumentedOrdering(t *testing.T) {
	env, in, err := parseProposeArgs([]string{"production", "--repo", "example/my-app", "--release", ".deploy/releases/my-app-0.1.0.yaml"})
	if err != nil {
		t.Fatalf("documented ordering rejected: %v", err)
	}
	if env != "production" || in.Repo != "example/my-app" || in.ReleasePath != ".deploy/releases/my-app-0.1.0.yaml" {
		t.Errorf("env=%q input=%+v", env, in)
	}
}

func TestParseProposeArgsRejectsSwallowedEnvironment(t *testing.T) {
	if _, _, err := parseProposeArgs([]string{"--repo", "r/x", "production"}); err == nil {
		t.Error("flags-before-positional must not silently swallow the environment")
	}
}

func TestParseCheckArgs(t *testing.T) {
	in, err := parseCheckArgs([]string{"--repo", "example/my-app", "--base", "aaa", "--head", "bbb", "--repo-dir", "/tmp/x"})
	if err != nil {
		t.Fatal(err)
	}
	if in.Repo != "example/my-app" || in.Base != "aaa" || in.Head != "bbb" || in.RepoDir != "/tmp/x" {
		t.Errorf("input = %+v", in)
	}
}
