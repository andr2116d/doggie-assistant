package console

import (
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
				if k.text == "" || !utf8.ValidString(k.text) || strings.ContainsFunc(k.text, func(r rune) bool { return !printable(r) && r != '\n' }) {
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
