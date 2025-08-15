package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/viper"
)

const (
	appName        = "tsm"
	defaultTimeout = 6 * time.Second
	pageStep       = 5 // PgUp/PgDn step
)

// ldflags-set at build time by goreleaser or Makefile
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// ---------------- Options ----------------

type Options struct {
	ConfigPath string
	Print      bool
}

// ---------------- Config ----------------

type Config struct {
	ScanPaths []string `mapstructure:"scan_paths"`
	Bookmarks []string `mapstructure:"bookmarks"`
	Exclude   []string `mapstructure:"exclude_dirs"`
	MaxDepth  int      `mapstructure:"max_depth"`
}

func defaultExclude() []string {
	return []string{
		".git", "node_modules", "vendor", "dist", "build", "target", "out", "bin",
		".cache", ".next", ".nuxt", ".pnpm-store", ".yarn", ".yarn/cache",
		".venv", ".direnv", "deps", "_build",
		".terraform", ".terragrunt-cache",
		".m2", ".gradle", "Pods", "Carthage",
	}
}

func loadConfig(explicit string) (Config, error) {
	v := viper.New()
	if explicit != "" {
		v.SetConfigFile(explicit)
	} else {
		xdg := os.Getenv("XDG_CONFIG_HOME")
		if xdg == "" {
			home, _ := os.UserHomeDir()
			xdg = filepath.Join(home, ".config")
		}
		v.AddConfigPath(filepath.Join(xdg, "tsm"))
		v.SetConfigName("config")
	}
	_ = v.ReadInConfig() // best-effort
	var cfg Config
	_ = v.Unmarshal(&cfg)

	if len(cfg.Exclude) == 0 {
		cfg.Exclude = defaultExclude()
	}
	if cfg.MaxDepth == 0 {
		cfg.MaxDepth = 3
	}
	if len(cfg.ScanPaths) == 0 {
		if home, _ := os.UserHomeDir(); home != "" {
			cfg.ScanPaths = []string{filepath.Join(home, "Code")}
		}
	}
	return cfg, nil
}

func xdgConfigPath() (string, error) {
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		home, _ := os.UserHomeDir()
		if home == "" {
			return "", errors.New("cannot resolve $HOME for XDG")
		}
		xdg = filepath.Join(home, ".config")
	}
	return filepath.Join(xdg, "tsm", "config.yaml"), nil
}

func writeDefaultConfig(w io.Writer) error {
	path, err := xdgConfigPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("config already exists at %s", path)
	}
	var buf bytes.Buffer
	buf.WriteString("# tsm config\n")
	buf.WriteString("scan_paths:\n")
	buf.WriteString("  - \"$HOME/Code\"\n")
	buf.WriteString("bookmarks:\n")
	buf.WriteString("  - \"$HOME\"\n")
	buf.WriteString("exclude_dirs:\n")
	for _, d := range defaultExclude() {
		buf.WriteString("  - \"" + d + "\"\n")
	}
	buf.WriteString("max_depth: 3\n")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "Wrote default config → %s\n", path)
	return nil
}

// ---------------- Items & names ----------------

type ItemKind string

const (
	KindSession  ItemKind = "S"
	KindGitRepo  ItemKind = "G"
	KindBookmark ItemKind = "B"
)

type Item struct {
	Kind ItemKind
	Name string // tmux session name
	Path string // directory for G/B
}

func sanitize(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return "session"
	}
	var b strings.Builder
	prevDash := false
	for _, r := range base {
		ok := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '-'
		if ok {
			b.WriteRune(r)
			prevDash = false
		} else {
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "session"
	}
	return out
}

// "<parent>_<base>" — /home/u/Code/ivuorinen/a -> "ivuorinen_a"
func sessionNameFromPath(dir string) string {
	base := sanitize(filepath.Base(dir))
	parent := sanitize(filepath.Base(filepath.Dir(dir)))
	if parent == "" || parent == "." || parent == string(filepath.Separator) {
		return base
	}
	return parent + "_" + base
}

// ---------------- Tmux shell abstraction ----------------

type Shell interface {
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
	Run(ctx context.Context, name string, args ...string) error
}

type execShell struct{}

func (execShell) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = os.Environ()
	return cmd.Output()
}
func (execShell) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = os.Environ()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

var shell Shell = execShell{}

func listTmuxSessions(ctx context.Context) []string {
	out, err := shell.Output(ctx, "tmux", "list-sessions", "-F", "#S")
	if err != nil {
		return nil
	}
	var res []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			res = append(res, line)
		}
	}
	slices.Sort(res)
	return res
}

func hasSession(ctx context.Context, name string) bool {
	return shell.Run(ctx, "tmux", "has-session", "-t", name) == nil
}

func switchToSession(ctx context.Context, name string, inTmux bool) error {
	if inTmux {
		return shell.Run(ctx, "tmux", "switch-client", "-t", name)
	}
	return shell.Run(ctx, "tmux", "attach", "-t", name)
}

func createOrSwitchForDir(ctx context.Context, sess, dir string, inTmux bool) error {
	if hasSession(ctx, sess) {
		return switchToSession(ctx, sess, inTmux)
	}
	if err := shell.Run(ctx, "tmux", "new-session", "-ds", sess, "-c", dir); err != nil {
		return err
	}
	return switchToSession(ctx, sess, inTmux)
}

func isInTmux() bool { return os.Getenv("TMUX") != "" }

// ---------------- Discovery (concurrent) ----------------

func expandPath(p string) (string, bool) {
	p = os.ExpandEnv(p)
	if strings.HasPrefix(p, "~") {
		home, _ := os.UserHomeDir()
		if home == "" {
			return "", false
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	abs, err := filepath.Abs(p)
	return abs, err == nil
}

func depthFrom(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return 0
	}
	return len(strings.Split(rel, string(os.PathSeparator)))
}

func scanGitReposConcurrent(cfg Config) []string {
	type none struct{}
	excluded := map[string]none{}
	for _, n := range cfg.Exclude {
		excluded[n] = none{}
	}

	outCh := make(chan string, 256)
	var wg sync.WaitGroup

	for _, raw := range cfg.ScanPaths {
		root, ok := expandPath(raw)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(root string) {
			defer wg.Done()
			filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if d.IsDir() {
					if cfg.MaxDepth > 0 && depthFrom(root, path) > cfg.MaxDepth {
						return fs.SkipDir
					}
					name := d.Name()
					if name != ".git" {
						if _, skip := excluded[name]; skip {
							return fs.SkipDir
						}
					}
					if name == ".git" {
						outCh <- filepath.Dir(path)
						return fs.SkipDir
					}
				}
				return nil
			})
		}(root)
	}

	go func() {
		wg.Wait()
		close(outCh)
	}()

	seen := map[string]struct{}{}
	var repos []string
	for dir := range outCh {
		if _, ok := seen[dir]; !ok {
			seen[dir] = struct{}{}
			repos = append(repos, dir)
		}
	}
	slices.Sort(repos)
	return repos
}

// ---------------- Fuzzy UI ----------------

func fuzzyScore(needle, hay string) int {
	if needle == "" {
		return 1
	}
	ni, score, streak := 0, 0, 0
	for i := 0; i < len(hay) && ni < len(needle); i++ {
		if toLower(hay[i]) == toLower(needle[ni]) {
			score += 2 + streak
			ni++
			streak++
		} else {
			streak = 0
		}
	}
	if ni < len(needle) {
		return -1
	}
	if strings.HasPrefix(strings.ToLower(hay), strings.ToLower(needle)) {
		score += 5
	}
	return score
}

func toLower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + 32
	}
	return b
}

type viewItem struct {
	Item
	score int
}

func filterAndRank(items []Item, q string, limit int) []viewItem {
	var out []viewItem
	for _, it := range items {
		key := it.Name
		if it.Path != "" {
			key += " " + it.Path
		}
		if s := fuzzyScore(q, key); s >= 0 {
			out = append(out, viewItem{Item: it, score: s})
		}
	}
	slices.SortFunc(out, func(a, b viewItem) int {
		if a.score != b.score {
			return b.score - a.score
		}
		return strings.Compare(a.Name, b.Name)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func interactiveSelect(items []Item) (Item, error) {
	if runtime.GOOS == "windows" {
		fmt.Println("Query: ")
		var q string
		_, _ = fmt.Scanln(&q)
		cands := filterAndRank(items, q, 20)
		for i, v := range cands {
			fmt.Printf("%2d) %-3s %-24s %s\n", i+1, v.Kind, v.Name, v.Path)
		}
		fmt.Print("Pick number: ")
		var n int
		_, _ = fmt.Scanln(&n)
		if n <= 0 || n > len(cands) {
			return Item{}, errors.New("invalid selection")
		}
		return cands[n-1].Item, nil
	}

	_, restore, err := enableRawMode()
	if err != nil {
		return promptOnce(items)
	}
	defer restore()

	query := ""
	idx := 0
	showPreview := false

	render := func() {
		clearScreen()
		fmt.Printf("tsm — %s (commit %s) — filter (↑/↓, Ctrl-N/P, Enter, Backspace, Ctrl-U, Tab, Home/End, PgUp/PgDn, Ctrl-C)\n", version, commit)
		fmt.Printf("> %s\n\n", query)
		cands := filterAndRank(items, query, 30)
		if idx >= len(cands) {
			idx = len(cands) - 1
		}
		if idx < 0 {
			idx = 0
		}
		for i, v := range cands {
			prefix := "  "
			if i == idx {
				prefix = "➤ "
			}
			fmt.Printf("%s%-3s %-24s %s\n", prefix, v.Kind, v.Name, v.Path)
		}
		if showPreview && len(cands) > 0 {
			sel := cands[idx].Item
			fmt.Println("\n--- preview ---")
			switch sel.Kind {
			case KindSession:
				fmt.Printf("Action : switch to session \"%s\"\n", sel.Name)
			default:
				fmt.Printf("Action : new-session -ds %q -c %q; switch/attach\n", sel.Name, sel.Path)
			}
			if sel.Path != "" {
				fmt.Printf("Path   : %s\n", sel.Path)
			}
		}
	}

	readKey := bufio.NewReader(os.Stdin)
	render()
	for {
		r, _, err := readKey.ReadRune()
		if err != nil {
			return Item{}, err
		}
		switch r {
		case 3: // Ctrl-C
			return Item{}, errors.New("cancelled")
		case 13: // Enter
			cands := filterAndRank(items, query, 30)
			if len(cands) == 0 {
				continue
			}
			return cands[idx].Item, nil
		case 21: // Ctrl-U
			query, idx = "", 0
		case 9: // Tab
			showPreview = !showPreview
		case 127, 8: // Backspace
			if len(query) > 0 {
				query = query[:len(query)-1]
			}
		case 27:
			b1, _ := readKey.ReadByte()
			if b1 != '[' {
				break
			}
			b2, _ := readKey.ReadByte()
			switch b2 {
			case 'A':
				idx--
			case 'B':
				idx++
			case 'H', '1':
				if b2 == '1' {
					_, _ = readKey.ReadByte()
				}
				idx = 0
			case 'F', '4':
				if b2 == '4' {
					_, _ = readKey.ReadByte()
				}
				cands := filterAndRank(items, query, 30)
				if len(cands) > 0 {
					idx = len(cands) - 1
				}
			case '5':
				_, _ = readKey.ReadByte()
				idx -= pageStep
			case '6':
				_, _ = readKey.ReadByte()
				idx += pageStep
			default:
				if b2 >= '0' && b2 <= '9' {
					_, _ = readKey.ReadByte()
				}
			}
		case 14: // Ctrl-N
			idx++
		case 16: // Ctrl-P
			idx--
		default:
			if r >= 32 && r <= 126 {
				query += string(r)
			}
		}
		render()
	}
}

func promptOnce(items []Item) (Item, error) {
	fmt.Print("Query: ")
	var q string
	_, _ = fmt.Scanln(&q)
	cands := filterAndRank(items, q, 30)
	for i, v := range cands {
		fmt.Printf("%2d) %-3s %-24s %s\n", i+1, v.Kind, v.Name, v.Path)
	}
	fmt.Print("Pick number: ")
	var n int
	_, _ = fmt.Scanln(&n)
	if n <= 0 || n > len(cands) {
		return Item{}, errors.New("invalid selection")
	}
	return cands[n-1].Item, nil
}

// Raw mode via stty (no extra deps)
func enableRawMode() (bool, func(), error) {
	if runtime.GOOS == "windows" {
		return false, func() {}, nil
	}
	cmd := exec.Command("stty", "-g")
	cmd.Stdin = os.Stdin
	old, err := cmd.Output()
	if err != nil {
		return false, func() {}, err
	}
	set := exec.Command("stty", "-icanon", "min", "1", "-echo")
	set.Stdin = os.Stdin
	if err := set.Run(); err != nil {
		return false, func() {}, err
	}
	restore := func() {
		cmd := exec.Command("stty", string(bytes.TrimSpace(old)))
		cmd.Stdin = os.Stdin
		_ = cmd.Run()
	}
	return true, restore, nil
}

func clearScreen() { fmt.Print("\x1b[2J\x1b[H") }

// ---------------- Orchestrator ----------------

func buildItems(ctx context.Context, cfg Config) []Item {
	var items []Item
	for _, s := range listTmuxSessions(ctx) {
		items = append(items, Item{Kind: KindSession, Name: s})
	}
	for _, r := range scanGitReposConcurrent(cfg) {
		items = append(items, Item{Kind: KindGitRepo, Name: sessionNameFromPath(r), Path: r})
	}
	for _, b := range cfg.Bookmarks {
		if p, ok := expandPath(b); ok {
			items = append(items, Item{Kind: KindBookmark, Name: sessionNameFromPath(p), Path: p})
		}
	}
	seen := map[string]struct{}{}
	var uniq []Item
	for _, it := range items {
		key := ""
		if it.Kind == KindSession {
			key = "S|" + it.Name
		} else {
			key = string(it.Kind) + "|" + it.Name + "|" + it.Path
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		uniq = append(uniq, it)
	}
	return uniq
}

func Run(opts Options) error {
	if opts.Print && opts.ConfigPath == "__init__" {
		return writeDefaultConfig(os.Stdout)
	}

	cfg, err := loadConfig(opts.ConfigPath)
	if err != nil {
		return fmt.Errorf("%s: config error: %w", appName, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	items := buildItems(ctx, cfg)
	if opts.Print {
		for _, it := range items {
			fmt.Printf("%s\t%s\t%s\n", it.Kind, it.Name, it.Path)
		}
		return nil
	}
	if len(items) == 0 {
		return errors.New("no candidates")
	}

	selected, err := interactiveSelect(items)
	if err != nil {
		return err
	}
	inTmux := isInTmux()
	switch selected.Kind {
	case KindSession:
		return switchToSession(ctx, selected.Name, inTmux)
	case KindGitRepo, KindBookmark:
		return createOrSwitchForDir(ctx, selected.Name, selected.Path, inTmux)
	default:
		return nil
	}
}

// ---------------- main() ----------------

func main() {
	var (
		flagCfg     string
		flagPrint   bool
		flagInitCfg bool
		flagVersion bool
	)
	flag.StringVar(&flagCfg, "config", "", "Explicit config file path")
	flag.BoolVar(&flagPrint, "print", false, "Print candidates and exit")
	flag.BoolVar(&flagInitCfg, "init-config", false, "Write default config to XDG path and exit")
	flag.BoolVar(&flagVersion, "version", false, "Print version and exit")
	flag.Parse()

	if flagVersion {
		fmt.Printf("tsm %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	if flagInitCfg {
		if err := writeDefaultConfig(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if err := Run(Options{
		ConfigPath: flagCfg,
		Print:      flagPrint,
	}); err != nil && err.Error() != "cancelled" {
		fmt.Fprintln(os.Stderr, err)
	}
}
