package console

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// printed prints texts on a fresh screen cols wide and returns the rows.
func printed(t *testing.T, cols int, texts ...Text) []string {
	t.Helper()
	v := newVT(t, cols)
	s := &screen{cols: cols}
	s.style(nil, NoColor)
	var b bytes.Buffer
	for _, x := range texts {
		s.printText(&b, x)
	}
	if len(s.partial) > 0 {
		s.commit(&b)
	}
	v.Write(b.Bytes())
	return v.lines()
}

func TestWordWrap(t *testing.T) {
	for _, c := range []struct {
		name  string
		cols  int
		texts []Text
		want  []string
	}{
		{"between words", 20, []Text{{Text: "Listo, te aviso en tres minutos cuando el té esté.", Wrap: true}},
			[]string{"Listo, te aviso en", "tres minutos cuando", "el té esté."}},
		{"hanging indent", 24, []Text{{Text: "DOGGIE", Role: Assistant}, {Text: "  "}, {Text: "Listo, te aviso en tres minutos.\nY otra línea.", Wrap: true, Indent: 8}},
			[]string{"DOGGIE  Listo, te aviso", "        en tres minutos.", "        Y otra línea."}},
		{"a word longer than a row", 10, []Text{{Text: "ab supercalifragilistico cd", Wrap: true, Indent: 2}},
			[]string{"  ab", "  supercal", "  ifragili", "  stico cd"}},
		{"a space at the end of a row", 5, []Text{{Text: "abcde fgh", Wrap: true}},
			[]string{"abcde", "fgh"}},
		{"spaces kept inside a row", 20, []Text{{Text: "a  b", Wrap: true}},
			[]string{"a  b"}},
		{"wide runes", 9, []Text{{Text: "日本 語です 東京", Wrap: true, Indent: 1}},
			[]string{" 日本", " 語です", " 東京"}},
		{"indent over half the width", 10, []Text{{Text: "uno dos tres cuatro", Wrap: true, Indent: 50}},
			[]string{"     uno", "     dos", "     tres", "     cuatr", "     o"}},
		{"without wrap, the last column", 10, []Text{{Text: "abc defghij klm"}},
			[]string{"abc defghi", "j klm"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := printed(t, c.cols, c.texts...); !slices.Equal(got, c.want) {
				t.Errorf("\n%q\nwant\n%q", got, c.want)
			}
		})
	}
}

// Words wrap the same whatever pieces the text arrives in, as a reply
// streamed in bursts does.
func TestWordWrapInPieces(t *testing.T) {
	text := "Claro: la reunión es el jueves a las nueve, y ya anoté que lleves los papeles del seguro."
	whole := printed(t, 23, Text{Text: text, Wrap: true, Indent: 4})
	for n := 1; n < 12; n++ {
		var pieces []Text
		for s := text; s != ""; {
			i := 0
			for k := 0; k < n && i < len(s); k++ {
				_, size := utf8.DecodeRuneInString(s[i:])
				i += size
			}
			pieces = append(pieces, Text{Text: s[:i], Wrap: true, Indent: 4})
			s = s[i:]
		}
		if got := printed(t, 23, pieces...); !slices.Equal(got, whole) {
			t.Fatalf("in pieces of %d:\n%q\nwhole:\n%q", n, got, whole)
		}
	}
}

func TestGap(t *testing.T) {
	got := printed(t, 30,
		Text{Text: "first", Gap: true}, // at the start: no gap
		Text{Text: " still first\n"},
		Text{Text: "second", Gap: true},
		Text{Text: "third", Gap: true}, // ends "second" first
		Text{Text: "\n\n"},
		Text{Text: "fourth", Gap: true}, // an empty row is there already
	)
	want := []string{"first still first", "", "second", "", "third", "", "fourth"}
	if !slices.Equal(got, want) {
		t.Errorf("\n%q\nwant\n%q", got, want)
	}
}

// Whatever is printed, wrapped or not, nothing is lost but the spaces at a
// break, no row is too wide, and rows begun by a text start with its
// indentation.
func FuzzWrap(f *testing.F) {
	f.Add("Listo, te aviso en tres minutos.\nY otra.", uint8(20), uint8(4), true)
	f.Add("日本 語です  東京\tx", uint8(9), uint8(1), true)
	f.Add("ab supercalifragilistico cd", uint8(10), uint8(30), false)
	f.Fuzz(func(t *testing.T, text string, cols, indent uint8, wrap bool) {
		w := minCols + int(cols)%60
		rows := printed(t, w, Text{Text: text, Wrap: wrap, Indent: int(indent)})
		in := int(indent) % 256
		in = min(in, w/2)
		for i, row := range rows {
			if rw := runewidthOf(row); rw > w {
				t.Fatalf("row %d is %d columns on %d", i, rw, w)
			}
			if in > 0 && row != "" && !strings.HasPrefix(row, strings.Repeat(" ", in)) {
				t.Fatalf("row %d %q isn't indented by %d", i, row, in)
			}
		}
		visible := func(s string) string {
			return strings.Map(func(r rune) rune {
				if r == ' ' || r == '\n' || width(r) == 0 {
					return -1
				}
				return r
			}, s)
		}
		if got, want := visible(strings.Join(rows, "")), visible(clean(text)); got != want {
			t.Fatalf("shows %q, want %q", got, want)
		}
	})
}

// Whatever mix of styled, wrapped, indented and gapped text, echoes,
// activity and status, the live region is always found again, no row is too
// wide, nothing but the console's own sequences reach the terminal, and in
// the end every printed rune is on screen.
func FuzzLive(f *testing.F) {
	f.Add([]byte("DOGGIE  Listo, te aviso en tres minutos.\nY otra."), []byte{1, 7, 200, 33, 9}, uint8(20))
	f.Add([]byte("日本 語です 東京 ab supercalifragilistico"), []byte{3, 250, 4, 128, 64, 5}, uint8(5))
	f.Fuzz(func(t *testing.T, text, ops []byte, cols uint8) {
		w := minCols + int(cols)%60
		v := newVT(t, w)
		s := &screen{cols: w, prompt: []rune("› "), promptRole: Accent}
		s.style(nil, ANSI16)
		e := newEditor(0, nil)
		var b bytes.Buffer
		update := func(f func()) {
			s.clear(&b)
			f()
			s.draw(&b, e)
			v.Write(b.Bytes())
			b.Reset()
			if rows := v.lines(); len(rows) > 0 && v.row != len(v.rows)-1 {
				t.Fatalf("cursor on row %d of %d", v.row, len(v.rows))
			}
			for i, row := range v.lines() {
				if rw := runewidthOf(row); rw > w {
					t.Fatalf("row %d is %d columns on %d", i, rw, w)
				}
			}
		}
		var want []string // each printed piece, cleaned on its own as the screen does
		for i := 0; len(text) > 0 && i < 200; i++ {
			op := byte(0)
			if len(ops) > 0 {
				op, ops = ops[0], ops[1:]
			}
			// Whole runes: a rune split between texts is dropped as invalid.
			n := 0
			for k := 0; k < 1+int(op)%9 && n < len(text); k++ {
				_, size := utf8.DecodeRune(text[n:])
				n += size
			}
			piece := string(text[:n])
			text = text[n:]
			tx := Text{Text: piece, Role: Role(op % byte(numRoles)), Indent: int(op>>3) % 12, Wrap: op&1 == 1, Gap: op&2 == 2}
			switch op % 5 {
			case 0:
				want = append(want, clean(piece))
				update(func() { s.printAbove(&b, tx) })
			case 1:
				update(func() { s.setActivity(piece); s.phase = int(op) })
			case 2:
				update(func() { s.setStatus(piece) })
			default:
				want = append(want, clean(piece))
				update(func() { s.printText(&b, tx) })
			}
		}
		update(func() { s.setActivity(""); s.setStatus("") })
		s.finish(&b)
		v.Write(b.Bytes())
		count := func(s string) map[rune]int {
			m := map[rune]int{}
			for _, r := range s {
				if r != ' ' && r != '\n' && width(r) > 0 {
					m[r]++
				}
			}
			return m
		}
		got, exp := count(strings.Join(v.lines(), "\n")), count(strings.Join(want, ""))
		// The prompt's runes are on screen too, once per input row left.
		got['›']--
		for r, n := range exp {
			if got[r] < n {
				t.Fatalf("%q shows %d times, printed %d", r, got[r], n)
			}
		}
	})
}
