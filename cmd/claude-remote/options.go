package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"claude-remote/internal/config"
	"claude-remote/internal/server"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// options are the launch flags. Everything that decides what runs where
// comes from here, never from a client.
type options struct {
	listen    string
	dangerous bool
	tlsCert   string
	tlsKey    string
	workdir   string
	projects  multiFlag
	cmd       string
	allowHost string
	cfgDir    string
	record    bool

	lockTimeout   time.Duration
	codexCmd      string
	noCodex       bool
	noCodexNotify bool

	noPower   bool
	keepAwake bool
	noAlerts  bool
}

func newFlagSet(name string) (*flag.FlagSet, *options) {
	o := &options{}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.StringVar(&o.listen, "listen", "", "address to listen on (default: Tailscale IP, port 8443)")
	fs.BoolVar(&o.dangerous, "i-know-this-is-dangerous", false, "allow listening on 0.0.0.0 / ::")
	fs.StringVar(&o.tlsCert, "tls-cert", "", "TLS certificate file (e.g. from `tailscale cert`)")
	fs.StringVar(&o.tlsKey, "tls-key", "", "TLS key file")
	fs.StringVar(&o.workdir, "workdir", "", "a project folder (shortcut for --project NAME=PATH)")
	fs.Var(&o.projects, "project", "allowed project as NAME=PATH; repeat for several (clients pick by name)")
	fs.StringVar(&o.cmd, "cmd", "claude", "command to run in each terminal")
	fs.StringVar(&o.allowHost, "allow-host", "", "comma-separated extra Host names to accept, e.g. the MagicDNS name")
	fs.StringVar(&o.cfgDir, "config-dir", "", "config directory (default %APPDATA%\\claude-remote)")
	fs.BoolVar(&o.record, "record", false, "record full terminal output to files (contains secrets; off by default)")
	fs.DurationVar(&o.lockTimeout, "lock-timeout", 5*time.Minute, "lock the app after this much inactivity when a PIN is set (0 = only lock when the page is reopened)")
	fs.StringVar(&o.codexCmd, "codex-cmd", "codex", "command that starts Codex CLI")
	fs.BoolVar(&o.noCodex, "no-codex", false, "do not offer Codex CLI")
	fs.BoolVar(&o.noCodexNotify, "no-codex-notify", false, "do not set Codex's notify hook for terminals started here (keeps the notify program from ~/.codex/config.toml, but no \"finished\" push)")
	fs.BoolVar(&o.noPower, "no-power", false, "do not offer sleep, hibernate, shutdown and restart from the phone (monitoring, screen lock and keep-awake stay)")
	fs.BoolVar(&o.keepAwake, "keep-awake", false, "keep the laptop from going to sleep by itself while the server runs (can also be switched in the UI)")
	fs.BoolVar(&o.noAlerts, "no-alerts", false, "do not send push alerts about battery, temperature and disk space")
	return fs, o
}

func (o *options) configDir() (string, error) {
	if o.cfgDir != "" {
		return o.cfgDir, nil
	}
	return config.DefaultDir()
}

var projectName = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N}._ -]{0,39}$`)

// resolveProjects turns --project/--workdir into a validated list. With
// neither flag the current directory is the single project.
func (o *options) resolveProjects() ([]server.Project, error) {
	specs := append([]string(nil), o.projects...)
	if o.workdir != "" {
		specs = append(specs, o.workdir)
	}
	if len(specs) == 0 {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		specs = []string{wd}
	}
	var out []server.Project
	seen := map[string]bool{}
	for _, spec := range specs {
		name, path := "", spec
		// "C:\dir" has a colon but no "=", so "=" is a safe separator.
		if i := strings.Index(spec, "="); i > 0 {
			name, path = strings.TrimSpace(spec[:i]), spec[i+1:]
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		fi, err := os.Stat(abs)
		if err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("папка проекта не найдена: %q. Проверьте путь в --project / --workdir", path)
		}
		if name == "" {
			name = filepath.Base(abs)
		}
		if !projectName.MatchString(name) {
			return nil, fmt.Errorf("недопустимое имя проекта %q: буквы, цифры, пробел, точка, дефис, до 40 символов", name)
		}
		if seen[strings.ToLower(name)] {
			return nil, fmt.Errorf("имя проекта %q повторяется; задайте разные имена: --project имя=путь", name)
		}
		seen[strings.ToLower(name)] = true
		out = append(out, server.Project{Name: name, Path: abs})
	}
	return out, nil
}

// firstToken returns the program part of a command line, honouring quotes
// around paths with spaces (`"C:\Program Files\x\claude.exe" -v`).
func firstToken(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if strings.HasPrefix(cmd, `"`) {
		if i := strings.Index(cmd[1:], `"`); i >= 0 {
			return cmd[1 : 1+i]
		}
		return strings.TrimPrefix(cmd, `"`)
	}
	if i := strings.IndexAny(cmd, " \t"); i >= 0 {
		return cmd[:i]
	}
	return cmd
}
