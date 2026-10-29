package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"claude-remote/internal/lock"
)

// pinCmd sets or removes the PIN. It only runs on the laptop, by design: if
// the phone could change the PIN, anyone holding an unlocked phone could
// replace it.
func pinCmd(args []string) int {
	fs := flag.NewFlagSet("pin", flag.ContinueOnError)
	dir := fs.String("config-dir", "", "config directory (default %APPDATA%\\claude-remote)")
	if len(args) == 0 || (args[0] != "set" && args[0] != "remove") {
		fmt.Fprintln(os.Stderr, "использование: claude-remote pin set | pin remove  [--config-dir DIR]")
		return 2
	}
	action := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	o := &options{cfgDir: *dir}
	d, err := o.configDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка:", err)
		return 1
	}
	if action == "remove" {
		if err := lock.Remove(d); err != nil {
			fmt.Fprintln(os.Stderr, "Ошибка:", err)
			return 1
		}
		fmt.Println("PIN удалён: приложение на телефоне больше не просит его.")
		return 0
	}

	pin, err := readSecret("Новый PIN (6–12 цифр): ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка:", err)
		return 1
	}
	if err := lock.ValidatePIN(pin); err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка:", err)
		return 1
	}
	again, err := readSecret("Повторите PIN: ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка:", err)
		return 1
	}
	if pin != again {
		fmt.Fprintln(os.Stderr, "Ошибка: PIN не совпали.")
		return 1
	}
	if err := lock.Set(d, pin); err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка:", err)
		return 1
	}
	fmt.Println("PIN сохранён. Перезапускать сервер не нужно: он подхватит его сам.")
	fmt.Println("Если забудете PIN: `claude-remote pin remove`, затем `pin set`.")
	return 0
}

// readSecret reads a line without echoing it.
func readSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("PIN нужно вводить в интерактивном окне PowerShell или cmd")
	}
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
