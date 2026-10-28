// claude-remote serves a persistent terminal (Claude Code) and screenshots
// over a Tailscale-only web UI.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"claude-remote/internal/auth"
	"claude-remote/internal/config"
	"claude-remote/internal/screen"
	"claude-remote/internal/server"
	"claude-remote/internal/session"
	"claude-remote/web"
)

const ringSize = 256 << 10

func main() {
	if len(os.Args) > 1 && os.Args[1] == "token" {
		os.Exit(tokenCmd(os.Args[2:]))
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func tokenCmd(args []string) int {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	dir := fs.String("config-dir", "", "config directory (default %APPDATA%\\claude-remote)")
	if len(args) == 0 || args[0] != "rotate" {
		fmt.Fprintln(os.Stderr, "usage: claude-remote token rotate [--config-dir DIR]")
		return 2
	}
	fs.Parse(args[1:])
	d, err := configDir(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	cfg, err := config.Rotate(d)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	printToken(cfg.Token)
	fmt.Println("Restart the server (if running) so it picks up the new token.")
	return 0
}

func configDir(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	return config.DefaultDir()
}

// printToken is the only place the token is ever shown, right after it is generated.
func printToken(tok string) {
	fmt.Println()
	fmt.Println("  Access token (shown only once, store it in a password manager):")
	fmt.Println()
	fmt.Println("    " + tok)
	fmt.Println()
}

func run() error {
	var (
		listen    = flag.String("listen", "", "address to listen on (default: Tailscale IP, port 8443)")
		dangerous = flag.Bool("i-know-this-is-dangerous", false, "allow listening on 0.0.0.0 / ::")
		tlsCert   = flag.String("tls-cert", "", "TLS certificate file (e.g. from `tailscale cert`)")
		tlsKey    = flag.String("tls-key", "", "TLS key file")
		workdir   = flag.String("workdir", "", "working directory of the command (default: current directory)")
		cmd       = flag.String("cmd", "claude", "command to run in the terminal")
		allowHost = flag.String("allow-host", "", "comma-separated extra Host names to accept, e.g. the MagicDNS name")
		cfgDir    = flag.String("config-dir", "", "config directory (default %APPDATA%\\claude-remote)")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if (*tlsCert == "") != (*tlsKey == "") {
		return fmt.Errorf("--tls-cert and --tls-key must be given together")
	}
	addr, err := server.ResolveListen(*listen, *dangerous)
	if err != nil {
		return err
	}
	wd := *workdir
	if wd == "" {
		if wd, err = os.Getwd(); err != nil {
			return err
		}
	}
	if fi, err := os.Stat(wd); err != nil || !fi.IsDir() {
		return fmt.Errorf("--workdir %q is not a directory", wd)
	}

	dir, err := configDir(*cfgDir)
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

	sess := session.New(session.NewStarter(*cmd, wd), ringSize, 80, 24)
	opts := server.Options{
		Auth:       auth.New(cfg.Token),
		Session:    sess,
		Web:        web.FS,
		Log:        log,
		Screenshot: screen.Capture,
		Secure:     *tlsCert != "",
	}
	for _, h := range strings.Split(*allowHost, ",") {
		if h = strings.TrimSpace(h); h != "" {
			opts.AllowedHosts = append(opts.AllowedHosts, h)
		}
	}
	srv := server.New(opts)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	scheme := "http"
	if *tlsCert != "" {
		scheme = "https"
	}
	log.Info("listening", "url", scheme+"://"+ln.Addr().String(), "workdir", wd, "cmd", *cmd)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln, *tlsCert, *tlsKey) }()

	select {
	case err = <-errc:
	case <-ctx.Done():
		log.Info("shutting down")
	}
	// Order: stop accepting and close WebSockets, then kill the process tree.
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	sess.Close()
	return err
}
