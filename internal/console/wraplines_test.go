package console

import (
	"slices"
	"strings"
	"testing"
)

func TestWrapLines(t *testing.T) {
	for _, c := range []struct {
		text  string
		width int
		want  []string
	}{
		{"Listo, te aviso en tres minutos cuando el té esté.", 20, []string{"Listo, te aviso en", "tres minutos cuando", "el té esté."}},
		{"uno\n\ndos", 10, []string{"uno", "", "dos"}},
		{"", 10, []string{""}},
		{"supercalifragilistico", 8, []string{"supercal", "ifragili", "stico"}},
		{"ab supercalifragilistico cd", 8, []string{"ab", "supercal", "ifragili", "stico cd"}},
		{"  - una lista con sangría", 12, []string{"  - una", "lista con", "sangría"}},
		{"日本 語です 東京", 7, []string{"日本", "語です", "東京"}},
		{"👍🏽👍🏽👍🏽 ok", 4, []string{"👍🏽👍🏽", "👍🏽", "ok"}},
		{"a  b", 10, []string{"a  b"}},
		{"abcde fgh", 5, []string{"abcde", "fgh"}},
		{"tab\there \x1b[31mred", 20, []string{"tab here [31mred"}},
		{"e\u0301e\u0301", 1, []string{"e\u0301e\u0301"}}, // width at least 2
	} {
		if got := wrapLines(c.text, c.width); !slices.Equal(got, c.want) {
			t.Errorf("%q at %d:\n%q\nwant\n%q", c.text, c.width, got, c.want)
		}
	}
}

// Any text wraps into lines that fit, keeping every visible cluster, and
// a line never starts with the space it broke at.
func FuzzWrapLines(f *testing.F) {
	f.Add("Listo, te aviso en tres minutos.\nY otra.", uint8(20))
	f.Add("日本 語です  東京\tx 👍🏽👍🏽 e\u0301", uint8(3))
	f.Fuzz(func(t *testing.T, text string, width uint8) {
		w := int(width) % 80
		lines := wrapLines(text, w)
		for i, l := range lines {
			if lw := textWidth(l); lw > max(w, 2) {
				t.Fatalf("line %d %q is %d wide, limit %d", i, l, lw, w)
			}
			if strings.ContainsAny(l, "\n\t\x1b") {
				t.Fatalf("line %d %q has a control character", i, l)
			}
		}
		visible := func(s string) string {
			var b strings.Builder
			for _, c := range clusters(s) {
				if c != " " {
					b.WriteString(c)
				}
			}
			return b.String()
		}
		got := visible(strings.Join(lines, ""))
		want := ""
		for p := range strings.SplitSeq(text, "\n") {
			want += visible(p)
		}
		if got != want {
			t.Fatalf("shows %q, want %q", got, want)
		}
	})
}
