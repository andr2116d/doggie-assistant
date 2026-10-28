package console

import (
	"strings"
	"unicode/utf8"
)

// printable reports whether r may reach the terminal as is. Control
// characters, C1 included, could move the cursor or start escape sequences,
// and what is printed comes from the network.
func printable(r rune) bool {
	return r >= 0x20 && r != 0x7f && (r < 0x80 || r > 0x9f)
}

// clean makes text safe to print: tabs become spaces, other control
// characters but newlines and invalid UTF-8 are dropped.
func clean(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		i += size
		switch {
		case r == utf8.RuneError && size == 1:
		case r == '\n' || printable(r):
			b.WriteRune(r)
		case r == '\t':
			b.WriteString("    ")
		}
	}
	return b.String()
}

// oneLine cleans text and joins its lines with spaces, for pasting into the
// line being typed.
func oneLine(text string) string {
	text = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ", "\t", " ").Replace(text)
	return strings.Map(func(r rune) rune {
		if !printable(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(text, ""))
}
