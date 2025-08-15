package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeShell struct {
	out map[string][]byte
	err map[string]error
}

func k(name string, args ...string) string { return name + " " + strings.Join(args, " ") }
func (f *fakeShell) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f.out[k(name, args...)], f.err[k(name, args...)]
}
func (f *fakeShell) Run(ctx context.Context, name string, args ...string) error {
	return f.err[k(name, args...)]
}

func TestSessionNameFromPath(t *testing.T) {
	cases := map[string]string{
		"/a/b":            "a_b",
		"/x/y/z":          "y_z",
		"/weird/äö!/n":    "weird_n",
		"/single":         "single",
		"/a/.hidden":      "a_.hidden",
	}
	for in, want := range cases {
		got := sessionNameFromPath(in)
		if got != want {
			t.Fatalf("sessionNameFromPath(%q)=%q want %q", in, got, want)
		}
	}
}

func TestFuzzyScoreAndFilter(t *testing.T) {
	items := []Item{
		{Kind: KindGitRepo, Name: "ivuorinen_a", Path: "/Code/ivuorinen/a"},
		{Kind: KindGitRepo, Name: "test_a", Path: "/Code/test/a"},
		{Kind: KindSession, Name: "util"},
	}
	got := filterAndRank(items, "a", 10)
	if len(got) < 2 {
		t.Fatalf("expected 2+ matches, got %d", len(got))
	}
}

func TestScanGitReposConcurrent(t *testing.T) {
	tmp := t.TempDir()
	mk := func(p string) { _ = os.MkdirAll(p, 0o755) }
	mk(filepath.Join(tmp, "r1", ".git"))
	mk(filepath.Join(tmp, "x", "r2", ".git"))
	mk(filepath.Join(tmp, "node_modules", "bad", ".git"))
	cfg := Config{
		ScanPaths: []string{tmp},
		Exclude:   defaultExclude(),
		MaxDepth:  3,
	}
	repos := scanGitReposConcurrent(cfg)
	if len(repos) != 2 {
		t.Fatalf("expected 2 repos, got %d: %v", len(repos), repos)
	}
}

func TestCreateOrSwitchForDir(t *testing.T) {
	// swap global shell with fake
	old := shell
	f := &fakeShell{
		out: map[string][]byte{},
		err: map[string]error{
			k("tmux", "has-session", "-t", "ivuorinen_a"):                   errors.New("no"),
			k("tmux", "new-session", "-ds", "ivuorinen_a", "-c", "/Code/ivuorinen/a"): nil,
			k("tmux", "switch-client", "-t", "ivuorinen_a"):                  nil,
			k("tmux", "attach", "-t", "ivuorinen_a"):                         nil,
		},
	}
	shell = f
	defer func() { shell = old }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := createOrSwitchForDir(ctx, "ivuorinen_a", "/Code/ivuorinen/a", true); err != nil {
		t.Fatalf("inside tmux path switch failed: %v", err)
	}
	if err := createOrSwitchForDir(ctx, "ivuorinen_a", "/Code/ivuorinen/a", false); err != nil {
		t.Fatalf("outside tmux path switch failed: %v", err)
	}
}
