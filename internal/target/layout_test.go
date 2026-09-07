package target

import (
	"testing"
)

func TestLayoutPathDerivation(t *testing.T) {
	l, err := NewLayout("/srv/deploy")
	if err != nil {
		t.Fatal(err)
	}
	got, err := l.ReleaseDir("my-app", "1.2.3")
	if err != nil || got != "/srv/deploy/my-app/releases/1.2.3" {
		t.Errorf("ReleaseDir = %q, %v", got, err)
	}
	got, err = l.StatePath("my-app", "staging")
	if err != nil || got != "/srv/deploy/my-app/state/staging.json" {
		t.Errorf("StatePath = %q, %v", got, err)
	}
	got, err = l.HistoryPath("my-app", "staging")
	if err != nil || got != "/srv/deploy/my-app/history/staging.jsonl" {
		t.Errorf("HistoryPath = %q, %v", got, err)
	}
}

func TestLayoutValidation(t *testing.T) {
	if _, err := NewLayout("relative/root"); err == nil {
		t.Error("relative deployRoot accepted")
	}
	if _, err := NewLayout(""); err == nil {
		t.Error("empty deployRoot accepted")
	}
	l, err := NewLayout("/srv/deploy")
	if err != nil {
		t.Fatal(err)
	}
	bad := []struct{ f func() (string, error) }{
		{func() (string, error) { return l.ReleaseDir("My-App", "1.2.3") }},      // uppercase
		{func() (string, error) { return l.ReleaseDir("my/app", "1.2.3") }},      // slash
		{func() (string, error) { return l.ReleaseDir("..", "1.2.3") }},          // traversal
		{func() (string, error) { return l.ReleaseDir("", "1.2.3") }},            // empty
		{func() (string, error) { return l.ReleaseDir("my-app", "1.2") }},        // not semver
		{func() (string, error) { return l.ReleaseDir("my-app", "../../etc") }},  // traversal via version
		{func() (string, error) { return l.StatePath("my-app", "st/aging") }},    // slash in env
		{func() (string, error) { return l.HistoryPath("my-app", "..") }},        // traversal via env
		{func() (string, error) { return l.StatePath("-leading-dash", "prod") }}, // slug must start alnum
	}
	for i, c := range bad {
		if _, err := c.f(); err == nil {
			t.Errorf("case %d: invalid identity accepted", i)
		}
	}
}

func TestLayoutSemverAcceptance(t *testing.T) {
	l, _ := NewLayout("/srv")
	good := []string{"0.0.1", "1.2.3", "10.20.30", "1.2.3-alpha.1", "1.2.3+build.5", "1.0.0-rc.1+build.2"}
	for _, v := range good {
		if _, err := l.ReleaseDir("app", v); err != nil {
			t.Errorf("valid semver %q rejected: %v", v, err)
		}
	}
}
