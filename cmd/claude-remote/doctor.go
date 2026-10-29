package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"

	"claude-remote/internal/config"
	"claude-remote/internal/lock"
	"claude-remote/internal/server"
)

type check struct {
	ok   bool
	warn bool // not fatal, but worth reading
	name string
	note string
}

func (c check) print() {
	mark := "[ OK ]"
	switch {
	case !c.ok && !c.warn:
		mark = "[FAIL]"
	case !c.ok:
		mark = "[WARN]"
	}
	fmt.Printf("%s %s", mark, c.name)
	if c.note != "" {
		fmt.Printf("\n       %s", c.note)
	}
	fmt.Println()
}

// doctorCmd diagnoses the usual setup problems and says how to fix each.
func doctorCmd(args []string) int {
	set, o := newFlagSet("doctor")
	if err := set.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	fmt.Println("claude-remote doctor")
	fmt.Println()
	failed := 0
	for _, c := range runChecks(o) {
		c.print()
		if !c.ok && !c.warn {
			failed++
		}
	}
	fmt.Println()
	if failed > 0 {
		fmt.Printf("Найдено проблем: %d. Исправьте их и запустите проверку снова.\n", failed)
		return 1
	}
	fmt.Println("Всё готово к запуску.")
	return 0
}

func runChecks(o *options) []check {
	var out []check

	addr, err := server.ResolveListen(o.listen, o.dangerous)
	if err != nil {
		out = append(out, check{name: "Tailscale", note: friendly(err)})
	} else {
		out = append(out, check{ok: true, name: "Адрес для прослушивания: " + addr})
	}

	cmdName := firstToken(o.cmd)
	if cmdName == "" {
		cmdName = "claude"
	}
	if p, err := exec.LookPath(cmdName); err != nil {
		out = append(out, check{name: "Команда " + cmdName, note: "не найдена в PATH. Установите Claude Code (npm i -g @anthropic-ai/claude-code) или задайте --cmd"})
	} else {
		out = append(out, check{ok: true, name: "Команда " + cmdName + ": " + p})
	}

	if o.noCodex {
		out = append(out, check{warn: true, name: "Codex отключён (--no-codex)"})
	} else if p, err := exec.LookPath(firstToken(o.codexCmd)); err != nil {
		out = append(out, check{warn: true, name: "Codex не найден: " + firstToken(o.codexCmd),
			note: "в выборе инструмента будет только Claude Code. Установить: npm i -g @openai/codex, либо задайте --codex-cmd"})
	} else {
		out = append(out, check{ok: true, name: "Codex: " + p})
	}

	if projects, err := o.resolveProjects(); err != nil {
		out = append(out, check{name: "Проекты", note: err.Error()})
	} else {
		for _, p := range projects {
			out = append(out, check{ok: true, name: fmt.Sprintf("Проект %q: %s", p.Name, p.Path)})
		}
	}

	if dir, err := o.configDir(); err != nil {
		out = append(out, check{name: "Папка конфига", note: err.Error()})
	} else if _, err := config.ReadPrivateFile(dir, "config.json"); errors.Is(err, fs.ErrNotExist) {
		out = append(out, check{ok: true, name: "Токен", note: "ещё не создан: появится при первом запуске сервера"})
	} else if err != nil {
		out = append(out, check{name: "Токен", note: "config.json недоступен или открыт другим пользователям: " + err.Error()})
	} else {
		out = append(out, check{ok: true, name: "Токен и права на config.json"})
	}

	switch {
	case (o.tlsCert == "") != (o.tlsKey == ""):
		out = append(out, check{name: "HTTPS", note: "--tls-cert и --tls-key нужно задавать вместе"})
	case o.tlsCert == "":
		out = append(out, check{warn: true, name: "HTTPS выключен",
			note: "без HTTPS не работают уведомления на телефон, буфер обмена и установка как приложение. Настройка: `tailscale cert имя.tailnet.ts.net`, затем --tls-cert/--tls-key/--allow-host"})
	default:
		missing := false
		for _, f := range []string{o.tlsCert, o.tlsKey} {
			if _, err := os.Stat(f); err != nil {
				out = append(out, check{name: "HTTPS: файл " + f, note: "не найден"})
				missing = true
			}
		}
		if !missing {
			out = append(out, check{ok: true, name: "HTTPS: сертификат и ключ найдены"})
		}
	}

	if dir, err := o.configDir(); err == nil {
		if lock.New(dir, 0).Enabled() {
			out = append(out, check{ok: true, name: "PIN-блокировка включена"})
		} else {
			out = append(out, check{warn: true, name: "PIN-блокировка выключена",
				note: "защитит приложение на телефоне, если он окажется в чужих руках: claude-remote pin set"})
		}
	}

	if !o.noPower {
		if dir, err := o.configDir(); err == nil && !lock.New(dir, 0).Enabled() {
			out = append(out, check{warn: true, name: "Сон и выключение с телефона доступны без PIN",
				note: "любой, кто откроет приложение с токеном, сможет выключить ноутбук. Рекомендуется: claude-remote pin set (или --no-power)"})
		}
	}

	out = append(out, taskCheck())
	return out
}
