package console

import (
	"slices"
	"testing"
)

func TestMarkdown(t *testing.T) {
	B, I, C, H := mdBold, mdItalic, mdCode, mdHeading|mdBold
	for _, c := range []struct {
		in    string
		text  string
		attrs []mdAttr
		hang  int
	}{
		{"plain", "plain", []mdAttr{0, 0, 0, 0, 0}, 0},
		{"a **b** c", "a b c", []mdAttr{0, 0, B, 0, 0}, 0},
		{"*it* `x*y`", "it x*y", []mdAttr{I, I, 0, C, C, C}, 0},
		{"2 * 3", "2 * 3", []mdAttr{0, 0, 0, 0, 0}, 0},
		{`\*no\*`, "*no*", []mdAttr{0, 0, 0, 0}, 0},
		{"## Hola", "Hola", []mdAttr{H, H, H, H}, 0},
		{"- uno", "• uno", nil, 2},
		{"  12. dos", "  12. dos", nil, 6},
		{"**abierta", "abierta", []mdAttr{B, B, B, B, B, B, B}, 0},
		{"#sin espacio", "#sin espacio", nil, 0},
	} {
		p := markdown(c.in, false)
		if p.text != c.text || p.hang != c.hang || c.attrs != nil && !slices.Equal(p.attrs, c.attrs) {
			t.Errorf("%q: %q %v hang %d", c.in, p.text, p.attrs, p.hang)
		}
	}
	if p := markdown("```go", false); !p.fence {
		t.Error("no fence")
	}
	if p := markdown("**sin** marcas", true); p.text != "**sin** marcas" || p.attrs[0] != mdCode {
		t.Errorf("in code: %+v", p)
	}
}

// A reply with markdown shows without its marks, a list wrapping under its
// text, and a block of code as it is.
func TestMarkdownReplyGolden(t *testing.T) {
	c := newConversation("DOGGIE")
	c.finish("r", "# Receta rápida\nNecesitas **tres** cosas, *nada más*:\n- arroz, lavado dos veces para que no quede pegajoso\n- `2 tazas` de agua\n```\nhervir 10 min\n```")
	text, g := drawConversation(c, 70, 22)
	golden(t, "markdown-70x22.txt", text)
	narrow, _ := drawConversation(c, 44, 24)
	golden(t, "markdown-44x24.txt", narrow)
	// The bold word is drawn in a look of its own.
	found := false
	for i, cell := range g.cells {
		if cell.text == "t" && g.cells[i+1].text == "r" && g.cells[i+2].text == "e" && g.cells[i+3].text == "s" && cell.s != g.cells[i-2].s {
			found = true
		}
	}
	if !found {
		t.Error("the bold word isn't drawn apart")
	}
}
