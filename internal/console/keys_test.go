package console

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func r(c rune) key       { return key{kind: keyRune, r: c} }
func k(kind keyKind) key { return key{kind: kind} }
func paste(s string) key { return key{kind: keyPaste, text: s} }

func TestDecoder(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []key
	}{
		{"ascii", "hi", []key{r('h'), r('i')}},
		{"utf8", "ñé😀", []key{r('ñ'), r('é'), r('😀')}},
		{"enter cr", "\r", []key{k(keyEnter)}},
		{"enter lf", "\n", []key{k(keyEnter)}},
		{"crlf is one enter", "a\r\nb", []key{r('a'), k(keyEnter), r('b')}},
		{"two enters", "\n\n", []key{k(keyEnter), k(keyEnter)}},
		{"cr cr", "\r\r", []key{k(keyEnter), k(keyEnter)}},
		{"backspace", "\x7f\x08", []key{k(keyBackspace), k(keyBackspace)}},
		{"tab is a space", "\t", []key{r(' ')}},
		{"controls", "\x01\x05\x03\x04\x15\x0b\x17\x0c\x02\x06\x10\x0e", []key{
			k(keyHome), k(keyEnd), k(keyInterrupt), k(keyEOF), k(keyKillStart), k(keyKillEnd),
			k(keyKillWord), k(keyRedraw), k(keyLeft), k(keyRight), k(keyUp), k(keyDown)}},
		{"unknown control dropped", "\x07\x1fa", []key{r('a')}},
		{"arrows", "\x1b[A\x1b[B\x1b[C\x1b[D", []key{k(keyUp), k(keyDown), k(keyRight), k(keyLeft)}},
		{"ss3 arrows", "\x1bOA\x1bOD\x1bOH\x1bOF", []key{k(keyUp), k(keyLeft), k(keyHome), k(keyEnd)}},
		{"home end", "\x1b[H\x1b[F\x1b[1~\x1b[4~\x1b[7~\x1b[8~", []key{k(keyHome), k(keyEnd), k(keyHome), k(keyEnd), k(keyHome), k(keyEnd)}},
		{"delete", "\x1b[3~", []key{k(keyDelete)}},
		{"ctrl arrows", "\x1b[1;5C\x1b[1;5D\x1b[1;3D", []key{k(keyWordRight), k(keyWordLeft), k(keyWordLeft)}},
		{"alt b f", "\x1bb\x1bf", []key{k(keyWordLeft), k(keyWordRight)}},
		{"alt other keeps the key", "\x1bx", []key{r('x')}},
		{"unknown csi dropped", "\x1b[5~\x1b[99Za", []key{r('a')}},
		{"csi with bad byte", "\x1b[1\x01a", []key{k(keyHome), r('a')}},
		{"invalid utf8 dropped", "a\xffb\xc3", []key{r('a'), r('b')}},
		{"paste", "\x1b[200~hola\r\nmundo\tya\x1b[201~", []key{paste("hola mundo ya")}},
		{"paste drops controls", "\x1b[200~a\x1b[31mb\x07c\x1b[201~", []key{paste("a[31mbc")}},
		{"empty paste", "\x1b[200~\x1b[201~x", []key{r('x')}},
		{"paste then keys", "\x1b[200~ab\x1b[201~\r", []key{paste("ab"), k(keyEnter)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var d decoder
			if got := d.feed([]byte(tc.in)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("feed(%q) = %v, want %v", tc.in, got, tc.want)
			}
			// The same keys whatever the input is split into.
			for split := 1; split < len(tc.in); split++ {
				var d decoder
				got := append(d.feed([]byte(tc.in[:split])), d.feed([]byte(tc.in[split:]))...)
				if !reflect.DeepEqual(got, tc.want) {
					t.Errorf("split at %d: %v, want %v", split, got, tc.want)
				}
			}
			var d1 decoder
			var got []key
			for i := range len(tc.in) {
				got = append(got, d1.feed([]byte{tc.in[i]})...)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("byte by byte: %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDecoderWaitsForIncompleteInput(t *testing.T) {
	for _, in := range []string{"\x1b", "\x1b[", "\x1b[1;5", "\x1bO", "\xc3", "\xf0\x9f\x98", "\x1b[200~abc", "\x1b[200~abc\x1b[20"} {
		var d decoder
		if got := d.feed([]byte(in)); len(got) != 0 {
			t.Errorf("feed(%q) = %v, want nothing yet", in, got)
		}
	}
}

func TestDecoderBoundsGarbage(t *testing.T) {
	var d decoder
	// An escape sequence that never ends is dropped once too long.
	d.feed([]byte("\x1b[" + strings.Repeat("1", 1000)))
	if len(d.buf) > maxSequence {
		t.Errorf("kept %d bytes of an endless sequence", len(d.buf))
	}
	// Its final byte ends it, and what follows is typed as usual.
	if got := d.feed([]byte("1~a")); !reflect.DeepEqual(got, []key{r('a')}) {
		t.Errorf("after garbage: %v", got)
	}
}

func TestDecoderBoundsPaste(t *testing.T) {
	var d decoder
	d.feed([]byte(pasteStart))
	chunk := []byte(strings.Repeat("x", 4096))
	for range 100 {
		d.feed(chunk)
	}
	if len(d.paste) > maxPaste || len(d.buf) >= len(pasteEnd) {
		t.Errorf("paste kept %d+%d bytes", len(d.paste), len(d.buf))
	}
	got := d.feed([]byte(pasteEnd + "a"))
	if len(got) != 2 || got[0].kind != keyPaste || len(got[0].text) != maxPaste || got[1] != r('a') {
		t.Errorf("end of a huge paste: %d keys", len(got))
	}
}

func TestFullScreenKeys(t *testing.T) {
	d := decoder{full: true}
	got := d.feed([]byte("\x1b[5~\x1b[6~\x1b[1;2A\x1b[1;2B\x1b[1;5H\x1b[1;5F\x1b[I\x1b[O\t\x1b\ra\nb\r\n"))
	want := []keyKind{keyPageUp, keyPageDown, keyScrollUp, keyScrollDown, keyTop, keyBottom, keyFocusIn, keyFocusOut, keyTab, keyNewline, keyRune, keyNewline, keyRune, keyEnter}
	var kinds []keyKind
	for _, k := range got {
		kinds = append(kinds, k.kind)
	}
	if !slices.Equal(kinds, want) {
		t.Errorf("%v, want %v", kinds, want)
	}
	p := decoder{full: true}
	keys := p.feed([]byte("\x1b[200~uno\r\ndos\tx\x1b[31m\x1b[201~"))
	if len(keys) != 1 || keys[0].text != "uno\ndos x[31m" {
		t.Errorf("paste %+v", keys)
	}
	// The same bytes in the inline console keep their old meaning.
	var inline decoder
	if k := inline.feed([]byte("\t\n\x1b[1;5H")); len(k) != 3 || k[0].r != ' ' || k[1].kind != keyEnter || k[2].kind != keyHome {
		t.Errorf("inline %+v", k)
	}
}

func TestMouseKeys(t *testing.T) {
	d := decoder{full: true}
	keys := d.feed([]byte("\x1b[<0;5;3M\x1b[<32;9;4M\x1b[<0;9;4m\x1b[<64;1;1M\x1b[<65;1;1M\x1b[<4;2;2M\x1b[<0;99999999;1M\x1b[<0;0;1M"))
	var got []mouseEvent
	for _, k := range keys {
		if k.kind != keyMouse {
			t.Fatalf("not a mouse key: %+v", k)
		}
		got = append(got, k.mouse)
	}
	want := []mouseEvent{
		{x: 4, y: 2},
		{x: 8, y: 3, drag: true},
		{x: 8, y: 3, release: true},
		{button: wheelUp},
		{button: wheelDown},
		{x: 1, y: 1}, // with Shift
	}
	if !slices.Equal(got, want) {
		t.Errorf("%+v\nwant %+v", got, want)
	}
	var inline decoder
	if k := inline.feed([]byte("\x1b[<0;5;3Ma")); len(k) != 1 || k[0].r != 'a' {
		t.Errorf("the inline console took a mouse report: %+v", k)
	}
}
