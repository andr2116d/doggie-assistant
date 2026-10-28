package console

import (
	"encoding/base64"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// maxCopy bounds what is copied at once, in bytes: terminals cap what
// they take through OSC 52.
const maxCopy = 64 << 10

// copyText puts text on the clipboard: through the terminal (OSC 52),
// which works where it supports it, also over SSH; and, on this machine,
// through the system's clipboard command, if there is one, for the
// terminals that don't. A variable so tests can see what is copied.
var copyText = func(t Terminal, text string) {
	if len(text) > maxCopy {
		text = strings.ToValidUTF8(text[:maxCopy], "")
	}
	t.Write([]byte("\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07"))
	if os.Getenv("SSH_CONNECTION") != "" {
		return // the system's clipboard here isn't the user's
	}
	if name, args := clipboardCommand(); name != "" {
		go func() {
			cmd := exec.Command(name, args...)
			cmd.Stdin = strings.NewReader(text)
			cmd.Run()
		}()
	}
}

// clipboardCommand is the command that takes text to the system's
// clipboard on its standard input, "" if none is found.
func clipboardCommand() (string, []string) {
	var tries [][]string
	switch runtime.GOOS {
	case "darwin":
		tries = [][]string{{"pbcopy"}}
	case "windows":
		tries = [][]string{{"clip.exe"}}
	default:
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			tries = append(tries, []string{"wl-copy"})
		}
		tries = append(tries, []string{"xclip", "-selection", "clipboard"}, []string{"xsel", "--clipboard", "--input"})
	}
	for _, t := range tries {
		if _, err := exec.LookPath(t[0]); err == nil {
			return t[0], t[1:]
		}
	}
	return "", nil
}
