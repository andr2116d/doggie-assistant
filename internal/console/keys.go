package console

import (
	"bytes"
	"unicode/utf8"
)

// keyKind is what a key does.
type keyKind int

const (
	keyRune keyKind = iota
	keyPaste
	keyEnter
	keyBackspace
	keyDelete
	keyLeft
	keyRight
	keyWordLeft
	keyWordRight
	keyUp
	keyDown
	keyHome
	keyEnd
	keyKillStart
	keyKillEnd
	keyKillWord
	keyInterrupt
	keyEOF
	keyRedraw
)

// key is a keystroke, or text pasted at once.
type key struct {
	kind keyKind
	r    rune   // keyRune
	text string // keyPaste, already one clean line
}

const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
	// maxSequence bounds an escape sequence; longer ones are dropped, so
	// garbage can't grow the buffer.
	maxSequence = 32
	// maxPaste bounds a paste kept in memory; the rest is dropped.
	maxPaste = 64 << 10
)

// decoder turns what the terminal sends into keys. Input may be split
// anywhere, even inside a rune or an escape sequence: an incomplete one waits
// for the next feed.
type decoder struct {
	buf     []byte
	pasting bool
	paste   []byte
	afterCR bool // a "\n" right after "\r" is the same Enter
}

func (d *decoder) feed(p []byte) []key {
	d.buf = append(d.buf, p...)
	var keys []key
	for len(d.buf) > 0 {
		if d.pasting {
			if !d.readPaste(&keys) {
				break
			}
			continue
		}
		k, n, ok := d.next()
		if !ok {
			break
		}
		d.buf = d.buf[n:]
		if k != nil {
			keys = append(keys, *k)
		}
	}
	if len(d.buf) == 0 {
		d.buf = nil // let a large paste's memory go
	}
	return keys
}

// readPaste consumes pasted bytes up to the end marker. It reports false
// when it needs more input.
func (d *decoder) readPaste(keys *[]key) bool {
	if i := bytes.Index(d.buf, []byte(pasteEnd)); i >= 0 {
		d.keepPaste(d.buf[:i])
		d.buf = d.buf[i+len(pasteEnd):]
		d.pasting = false
		if text := oneLine(string(d.paste)); text != "" {
			*keys = append(*keys, key{kind: keyPaste, text: text})
		}
		d.paste = nil
		return true
	}
	// Keep a tail that may be the start of the end marker.
	keep := min(len(d.buf), len(pasteEnd)-1)
	d.keepPaste(d.buf[:len(d.buf)-keep])
	d.buf = d.buf[len(d.buf)-keep:]
	return false
}

func (d *decoder) keepPaste(p []byte) {
	if room := maxPaste - len(d.paste); room > 0 {
		d.paste = append(d.paste, p[:min(len(p), room)]...)
	}
}

// next decodes the key at the start of the buffer and how many bytes it
// takes. A nil key means bytes to drop. ok is false when the buffer holds
// only the start of a key.
func (d *decoder) next() (k *key, n int, ok bool) {
	b := d.buf[0]
	afterCR := d.afterCR
	d.afterCR = false
	switch b {
	case '\x1b':
		return d.escape()
	case '\r':
		d.afterCR = true
		return &key{kind: keyEnter}, 1, true
	case '\n':
		if afterCR {
			return nil, 1, true
		}
		return &key{kind: keyEnter}, 1, true
	case 0x7f, 0x08:
		return &key{kind: keyBackspace}, 1, true
	case '\t':
		return &key{kind: keyRune, r: ' '}, 1, true
	}
	if b < 0x20 {
		if kind, known := controls[b]; known {
			return &key{kind: kind}, 1, true
		}
		return nil, 1, true
	}
	if !utf8.FullRune(d.buf) {
		return nil, 0, false
	}
	r, size := utf8.DecodeRune(d.buf)
	if r == utf8.RuneError && size == 1 {
		return nil, 1, true
	}
	return &key{kind: keyRune, r: r}, size, true
}

var controls = map[byte]keyKind{
	0x01: keyHome,      // Ctrl+A
	0x02: keyLeft,      // Ctrl+B
	0x03: keyInterrupt, // Ctrl+C
	0x04: keyEOF,       // Ctrl+D
	0x05: keyEnd,       // Ctrl+E
	0x06: keyRight,     // Ctrl+F
	0x0b: keyKillEnd,   // Ctrl+K
	0x0c: keyRedraw,    // Ctrl+L
	0x0e: keyDown,      // Ctrl+N
	0x10: keyUp,        // Ctrl+P
	0x15: keyKillStart, // Ctrl+U
	0x17: keyKillWord,  // Ctrl+W
}

// escape decodes a sequence that starts with ESC.
func (d *decoder) escape() (*key, int, bool) {
	if len(d.buf) < 2 {
		return nil, 0, false
	}
	switch d.buf[1] {
	case '[':
		return d.csi()
	case 'O':
		if len(d.buf) < 3 {
			return nil, 0, false
		}
		if kind, known := ss3[d.buf[2]]; known {
			return &key{kind: kind}, 3, true
		}
		return nil, 3, true
	case 'b':
		return &key{kind: keyWordLeft}, 2, true // Alt+B
	case 'f':
		return &key{kind: keyWordRight}, 2, true // Alt+F
	}
	// Alt with another key, or a lone ESC: drop the ESC and keep the key.
	return nil, 1, true
}

var ss3 = map[byte]keyKind{'A': keyUp, 'B': keyDown, 'C': keyRight, 'D': keyLeft, 'H': keyHome, 'F': keyEnd}

// csi decodes ESC [ parameters final.
func (d *decoder) csi() (*key, int, bool) {
	i := 2
	for i < len(d.buf) && d.buf[i] >= 0x20 && d.buf[i] <= 0x3f {
		i++
	}
	if i >= len(d.buf) {
		if len(d.buf) >= maxSequence {
			return nil, len(d.buf), true
		}
		return nil, 0, false
	}
	final := d.buf[i]
	if final < 0x40 || final > 0x7e {
		return nil, i, true // not a sequence: drop what was read
	}
	params := string(d.buf[2:i])
	n := i + 1
	if final == '~' {
		switch params {
		case "200":
			d.pasting = true
			return nil, n, true
		case "1", "7":
			return &key{kind: keyHome}, n, true
		case "4", "8":
			return &key{kind: keyEnd}, n, true
		case "3":
			return &key{kind: keyDelete}, n, true
		}
		return nil, n, true
	}
	// Modifiers come as "1;5" (Ctrl), "1;3" (Alt): Ctrl or Alt with an
	// arrow moves by words.
	word := params == "1;5" || params == "1;3" || params == "5"
	switch final {
	case 'A':
		return &key{kind: keyUp}, n, true
	case 'B':
		return &key{kind: keyDown}, n, true
	case 'C':
		if word {
			return &key{kind: keyWordRight}, n, true
		}
		return &key{kind: keyRight}, n, true
	case 'D':
		if word {
			return &key{kind: keyWordLeft}, n, true
		}
		return &key{kind: keyLeft}, n, true
	case 'H':
		return &key{kind: keyHome}, n, true
	case 'F':
		return &key{kind: keyEnd}, n, true
	}
	return nil, n, true
}
