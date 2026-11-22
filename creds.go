package main

// Passwords: from the macOS Keychain when one was saved, else asked for in
// the terminal with typing hidden (the floating window rex-glance opens in),
// with an offer to save it. Never written to a file or put on a command line.

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"golang.org/x/term"
)

const keychainService = "rex-glance"

func keychainAccount(user, host string) string { return user + "@" + host }

// keychainGet returns the saved password, or "" when there's none.
func keychainGet(user, host string) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	out, err := exec.Command("security", "find-generic-password",
		"-s", keychainService, "-a", keychainAccount(user, host), "-w").Output()
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(out), "\n")
}

// keychainSet saves the password. It goes to security on standard input, in
// its interactive mode, so it never appears in the list of running processes.
func keychainSet(user, host, password string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("saving passwords needs the macOS Keychain")
	}
	quote := func(s string) string {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
	}
	cmd := exec.Command("security", "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %s -a %s -l %s -w %s\n",
		quote(keychainService), quote(keychainAccount(user, host)),
		quote("rex-glance: "+keychainAccount(user, host)), quote(password)))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("saving to the Keychain: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func keychainDelete(user, host string) {
	if runtime.GOOS == "darwin" {
		exec.Command("security", "delete-generic-password", "-s", keychainService,
			"-a", keychainAccount(user, host)).Run()
	}
}

// password returns the password for user on host: saved, or asked for.
// fromKeychain says which, so a refused saved password can be asked again;
// save says the person wants it kept, which the caller does only after the
// login works, so a mistyped password is never saved.
func password(user, host string, ask bool) (pw string, fromKeychain, save bool, err error) {
	if !ask {
		if pw := keychainGet(user, host); pw != "" {
			return pw, true, false, nil
		}
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", false, false, errors.New("no saved password, and no terminal to ask in")
	}
	defer tty.Close()
	fmt.Fprintf(tty, "Password for %s on %s (typing is hidden): ", user, host)
	b, err := term.ReadPassword(int(tty.Fd()))
	fmt.Fprintln(tty)
	if err != nil {
		return "", false, false, err
	}
	pw = string(b)
	if pw == "" {
		return "", false, false, errors.New("no password given")
	}
	if runtime.GOOS == "darwin" {
		fmt.Fprint(tty, "Save it in the Keychain once it works, so rex-glance doesn't ask again? [y/N] ")
		line, _ := bufio.NewReader(tty).ReadString('\n')
		a := strings.ToLower(strings.TrimSpace(line))
		save = a == "y" || a == "yes"
	}
	return pw, false, save, nil
}
