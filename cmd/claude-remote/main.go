// claude-remote serves persistent terminals (Claude Code) and screenshots
// over a Tailscale-only web UI.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"claude-remote/internal/agents"
	"claude-remote/internal/auth"
	"claude-remote/internal/codexhist"
	"claude-remote/internal/config"
	"claude-remote/internal/history"
	"claude-remote/internal/lock"
	"claude-remote/internal/machine"
	"claude-remote/internal/push"
	"claude-remote/internal/record"
	"claude-remote/internal/screen"
	"claude-remote/internal/server"
	"claude-remote/internal/session"
	"claude-remote/web"
)

const (
	ringSize    = 256 << 10
	maxSessions = 8
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "token":
			os.Exit(tokenCmd(os.Args[2:]))
		case "notify":
			os.Exit(notifyCmd())
		case "notify-codex":
			os.Exit(notifyCodexCmd(os.Args[2:]))
		case "pin":
			os.Exit(pinCmd(os.Args[2:]))
		case "doctor":
			os.Exit(doctorCmd(os.Args[2:]))
		case "install":
			os.Exit(installCmd(os.Args[2:]))
		case "uninstall":
			os.Exit(uninstallCmd())
		case "help", "-h", "--help":
			usage()
			return
		}
	}
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "Ошибка:", friendly(err))
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`claude-remote: удалённый терминал для Claude Code через Tailscale

  claude-remote [флаги]            запустить сервер
  claude-remote doctor [флаги]     проверить Tailscale, claude, проекты, права
  claude-remote install [флаги]    автозапуск при входе в Windows (Task Scheduler)
  claude-remote uninstall          убрать автозапуск
  claude-remote token rotate       выпустить новый токен
  claude-remote pin set|remove     PIN-блокировка приложения на телефоне

Флаги сервера: --project имя=путь (можно несколько), --workdir путь, --cmd,
--codex-cmd, --no-codex, --no-codex-notify,
--listen, --tls-cert/--tls-key, --allow-host, --record, --lock-timeout, --config-dir.
`)
}

// friendly adds a hint in plain words to errors a user can fix.
func friendly(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "no Tailscale interface"):
		return "Tailscale не найден. Установите Tailscale, войдите в аккаунт и убедитесь, что он подключён " +
			"(команда `tailscale ip -4` должна вывести адрес 100.x.y.z). Для проверки без Tailscale: --listen 127.0.0.1:8443"
	case strings.Contains(msg, "Only one usage of each socket address") || strings.Contains(msg, "address already in use"):
		return "порт уже занят: возможно, claude-remote уже запущен (проверьте Task Scheduler или другое окно)"
	case strings.Contains(msg, "wildcard address"):
		return "слушать на всех адресах (0.0.0.0 или ::) небезопасно: сервер открыл бы доступ всей сети. " +
			"Используйте адрес Tailscale (по умолчанию) или --listen 127.0.0.1:8443"
	}
	return msg
}

func tokenCmd(args []string) int {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	dir := fs.String("config-dir", "", "config directory (default %APPDATA%\\claude-remote)")
	if len(args) == 0 || args[0] != "rotate" {
		fmt.Fprintln(os.Stderr, "использование: claude-remote token rotate [--config-dir DIR]")
		return 2
	}
	fs.Parse(args[1:])
	o := &options{cfgDir: *dir}
	d, err := o.configDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка:", err)
		return 1
	}
	cfg, err := config.Rotate(d)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка:", err)
		return 1
	}
	printToken(cfg.Token)
	fmt.Println("Перезапустите сервер, чтобы он принял новый токен (все старые входы с телефона перестанут работать).")
	return 0
}

// printToken is the only place the token is ever shown, right after it is generated.
func printToken(tok string) {
	fmt.Println()
	fmt.Println("  Токен доступа (показывается один раз, сохраните в менеджер паролей):")
	fmt.Println()
	fmt.Println("    " + tok)
	fmt.Println()
}

func run(args []string) error {
	fs, o := newFlagSet("claude-remote")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if (o.tlsCert == "") != (o.tlsKey == "") {
		return fmt.Errorf("--tls-cert и --tls-key нужно задавать вместе")
	}
	addr, err := server.ResolveListen(o.listen, o.dangerous)
	if err != nil {
		return err
	}
	projects, err := o.resolveProjects()
	if err != nil {
		return err
	}
	dir, err := o.configDir()
	if err != nil {
		return err
	}
	cfg, created, err := config.Load(dir)
	if err != nil {
		return err
	}
	if created {
		printToken(cfg.Token)
	}
	if first := firstToken(o.cmd); first != "" {
		if _, err := exec.LookPath(first); err != nil {
			log.Warn("команда не найдена в PATH: терминалы не запустятся, пока её не установят", "cmd", first)
		}
	}

	pushSvc, err := push.Load(dir, log)
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}
	hooksPath, err := writeHooks(dir)
	if err != nil {
		return fmt.Errorf("hooks: %w", err)
	}
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return err
	}
	notifySecret := hex.EncodeToString(secretBytes)

	// Loopback-only listener for the hook helper (see server.InternalHandler).
	iln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	notifyURL := "http://" + iln.Addr().String() + "/notify"

	claudeCmd := isClaude(o.cmd)
	exePath, _ := os.Executable()
	mgr := session.NewManager(func(m session.Meta) session.StartFunc {
		spec := session.Spec{
			Cmd:     o.cmd,
			Workdir: m.Workdir,
			Env:     childEnv(notifyURL, notifySecret, m.ID),
		}
		if m.Tool == "codex" {
			spec.Cmd = o.codexCmd
			spec.ExtraArgs = codexArgs(exePath, m.ID, m.Resume, !o.noCodexNotify)
			return session.NewStarter(spec)
		}
		if claudeCmd && m.Attach != "" {
			// A background session started elsewhere: attach to it. The name
			// was validated by the server (agents.SafeName).
			spec.ExtraArgs = []string{"attach", m.Attach}
		} else if claudeCmd {
			spec.ExtraArgs = []string{"--settings", hooksPath}
			// Re-checked here as well: the id is placed on a command line.
			if m.Resume != "" && history.ValidID(m.Resume) {
				spec.ExtraArgs = append(spec.ExtraArgs, "--resume", m.Resume)
			}
		}
		return session.NewStarter(spec)
	}, ringSize, maxSessions)

	if o.record {
		recDir := filepath.Join(dir, "recordings")
		mgr.SetRecorderFactory(func(m session.Meta) io.Writer {
			w, err := record.Open(recDir, m.Project, m.ID, record.DefaultMaxBytes)
			if err != nil {
				log.Error("не удалось открыть запись сессии", "err", err)
				return nil
			}
			return w
		})
		log.Warn("включена запись терминала: в файлах лежит ВСЁ, что выводилось на экран, включая секреты", "dir", recDir)
	}

	host := machine.NewCached(machine.New(), 3*time.Second)
	if o.keepAwake {
		if err := host.SetKeepAwake(true); err != nil {
			log.Warn("не удалось включить режим «не засыпать»", "err", err)
		}
	}
	histRoot, _ := history.DefaultRoot()
	codexRoot, _ := codexhist.DefaultRoot()
	opts := server.Options{
		Auth:        auth.New(cfg.Token),
		Sessions:    mgr,
		Projects:    projects,
		HistoryRoot: histRoot,
		CodexRoot:   codexRoot,
		ToolAvailable: func(tool string) bool {
			switch tool {
			case "claude":
				return true // the default tool is always offered; a missing program is reported when it starts
			case "codex":
				if o.noCodex {
					return false
				}
				_, err := exec.LookPath(firstToken(o.codexCmd))
				return err == nil
			}
			return false
		},
		Web:          web.FS,
		Log:          log,
		Push:         pushSvc,
		NotifySecret: notifySecret,
		Screenshot:   screen.Capture,
		Displays:     screen.Displays,
		Lock:         lock.New(dir, o.lockTimeout),
		Machine:      host,
		NoPower:      o.noPower,
		Secure:       o.tlsCert != "",
	}
	for _, h := range strings.Split(o.allowHost, ",") {
		if h = strings.TrimSpace(h); h != "" {
			opts.AllowedHosts = append(opts.AllowedHosts, h)
		}
	}
	if claudeCmd {
		opts.Agents = agents.New(firstToken(o.cmd))
	}
	srv := server.New(opts)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		iln.Close()
		return err
	}
	scheme := "http"
	if o.tlsCert != "" {
		scheme = "https"
	}
	names := make([]string, len(projects))
	for i, p := range projects {
		names[i] = p.Name
	}
	if opts.Lock.Enabled() {
		log.Info("PIN-блокировка включена", "idle", o.lockTimeout)
	}
	log.Info("слушаю", "url", scheme+"://"+ln.Addr().String(), "projects", strings.Join(names, ","), "cmd", o.cmd)
	if scheme == "http" {
		log.Info("работает по HTTP: уведомления, буфер обмена и установка как приложение требуют HTTPS (см. README, `tailscale cert`)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if o.noPower {
		log.Info("управление питанием с телефона выключено (--no-power)")
	} else {
		log.Info("с телефона доступны сон, гибернация, перезагрузка и выключение (с подтверждением; --no-power отключает)")
	}
	if !o.noAlerts {
		// Heads-up pushes: low battery, mains lost, overheating, little disk space.
		al := machine.NewAlerter(host.Status, func(a machine.Alert) {
			pushSvc.Notify(push.Notification{Title: a.Title, Body: a.Body, Tag: a.Kind})
		})
		go al.Run(ctx, time.Minute)
	}
	errc := make(chan error, 2)
	go func() { errc <- srv.Serve(ln, o.tlsCert, o.tlsKey) }()
	go func() { errc <- srv.ServeInternal(iln) }()

	select {
	case err = <-errc:
	case <-ctx.Done():
		log.Info("останавливаюсь")
	}
	// Order: stop accepting and close WebSockets, then kill every process tree.
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	mgr.CloseAll()
	return err
}

// isClaude reports whether the configured command is Claude Code, the only
// one that understands --settings and --resume.
func isClaude(cmd string) bool {
	first := firstToken(cmd)
	if first == "" {
		return false
	}
	base := strings.ToLower(filepath.Base(first))
	base = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(base, ".exe"), ".cmd"), ".bat")
	return base == "claude"
}

// childEnv is the server's environment plus the hook variables. Existing
// CLAUDE_REMOTE_* entries (e.g. when the server was started from inside one of
// its own terminals) are dropped first: duplicates would make it unspecified
// which value the child sees.
func childEnv(url, secret, session string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name := strings.ToUpper(kv[:max(0, strings.Index(kv, "="))])
		if name == envNotifyURL || name == envNotifySecret || name == envSession {
			continue
		}
		env = append(env, kv)
	}
	return append(env, envNotifyURL+"="+url, envNotifySecret+"="+secret, envSession+"="+session)
}
