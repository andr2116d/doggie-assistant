package console

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

// Any input decodes to sane keys, the same whatever it is split into, and
// the decoder never holds more than a sequence or a paste.
func FuzzDecoder(f *testing.F) {
	for _, s := range []string{"hola\r", "\x1b[200~a\nb\x1b[201~", "\x1b[1;5C\x1bOA", "\xff\xc3\xa9", "\x1b[" + strings.Repeat("9", 40)} {
		f.Add([]byte(s), uint8(3))
	}
	f.Fuzz(func(t *testing.T, in []byte, split uint8) {
		var whole decoder
		keys := whole.feed(in)
		for _, k := range keys {
			switch k.kind {
			case keyRune:
				if !printable(k.r) || !utf8.ValidRune(k.r) {
					t.Fatalf("rune key %U", k.r)
				}
			case keyPaste:
				if k.text == "" || !utf8.ValidString(k.text) || strings.ContainsFunc(k.text, func(r rune) bool { return !printable(r) }) {
					t.Fatalf("paste %q", k.text)
				}
			}
		}
		if len(whole.buf) > maxPaste+maxSequence || len(whole.paste) > maxPaste {
			t.Fatalf("decoder holds %d+%d bytes", len(whole.buf), len(whole.paste))
		}
		var parts decoder
		n := int(split)%max(len(in), 1) + 1
		var got []key
		for i := 0; i < len(in); i += n {
			got = append(got, parts.feed(in[i:min(i+n, len(in))])...)
		}
		if len(got) != len(keys) {
			t.Fatalf("split by %d: %d keys, whole: %d", n, len(got), len(keys))
		}
		for i := range keys {
			if got[i] != keys[i] {
				t.Fatalf("split by %d: key %d = %v, whole %v", n, i, got[i], keys[i])
			}
		}
	})
}

// Whatever is typed and printed, the screen matches the model, nothing but
// the console's own sequences reach the terminal, and no row is too wide.
func FuzzScreen(f *testing.F) {
	f.Add([]byte("abc\x1b[D\x7fx\r"), []byte("doggie> hola\nmundo"), uint8(20))
	f.Add([]byte("\x1b[200~日本\x1b[201~\x01\x0b"), []byte("é\x9b2J\ttab"), uint8(8))
	f.Fuzz(func(t *testing.T, typed, printed []byte, cols uint8) {
		g := newRig(t, minCols+int(cols)%60, "you> ")
		var d decoder
		// Interleave: a chunk of keys, a chunk of output.
		for len(typed) > 0 || len(printed) > 0 {
			n := min(len(typed), 7)
			for _, k := range d.feed(typed[:n]) {
				g.key(k)
			}
			typed = typed[n:]
			m := min(len(printed), 11)
			g.print(string(printed[:m]))
			printed = printed[m:]
		}
		var b bytes.Buffer
		g.s.finish(&b)
		g.v.Write(b.Bytes())
	})
}
