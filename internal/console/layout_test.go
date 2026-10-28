package console

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden screens")

// screenText is the grid's text, one line per row, trailing blanks cut.
func screenText(g *grid) string {
	var b strings.Builder
	for y := range g.H {
		var row strings.Builder
		for x := range g.W {
			c := g.cells[y*g.W+x]
			switch {
			case c.w == 0:
			case c.text == "":
				row.WriteByte(' ')
			default:
				row.WriteString(c.text)
			}
		}
		b.WriteString(strings.TrimRight(row.String(), " "))
		b.WriteByte('\n')
	}
	return b.String()
}

// golden compares got with testdata/golden/name, or rewrites it with
// -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *update {
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs:\n%s\nwant:\n%s", name, got, want)
	}
}

var sampleChrome = chrome{
	name: "DOGGIE", online: true, status: "en línea", channel: "terminal", clock: "mar 28 oct · 21:40",
	hints: "/help comandos · ↑ historial · Ctrl+D salir", hintsRight: "Tab: panel",
	tasks: "TAREAS", notices: "AVISOS", tooSmall: "Agranda la ventana\npara hablar con DOGGIE",
}

func TestChromeGolden(t *testing.T) {
	for _, size := range [][2]int{{60, 16}, {80, 20}, {120, 24}, {200, 30}, {40, 10}, {39, 12}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			var g grid
			g.resize(size[0], size[1])
			lk := newLooks(newStyleTable(), violetMint)
			drawChrome(&g, layout(size[0], size[1], true, 1), lk, sampleChrome)
			golden(t, fmt.Sprintf("chrome-%dx%d.txt", size[0], size[1]), screenText(&g))
		})
	}
}

// The regions never overlap, stay on screen, and together cover every row.
func FuzzLayout(f *testing.F) {
	f.Add(uint16(120), uint16(30), true, uint8(1))
	f.Add(uint16(40), uint16(10), true, uint8(9))
	f.Fuzz(func(t *testing.T, w, h uint16, panel bool, rows uint8) {
		W, H := int(w%400), int(h%200)
		r := layout(W, H, panel, int(rows))
		if r.tooSmall {
			if W >= fullMinCols && H >= fullMinRows {
				t.Fatalf("%dx%d called too small", W, H)
			}
			return
		}
		parts := []rect{r.header, r.body, r.panel, r.input, r.footer}
		covered := make([]int, W*H)
		for _, p := range parts {
			if p.empty() {
				continue
			}
			if p.x < 0 || p.y < 0 || p.x+p.w > W || p.y+p.h > H {
				t.Fatalf("%+v is off a %dx%d screen", p, W, H)
			}
			for y := p.y; y < p.y+p.h; y++ {
				for x := p.x; x < p.x+p.w; x++ {
					covered[y*W+x]++
				}
			}
		}
		for i, n := range covered {
			if n != 1 {
				t.Fatalf("cell %d,%d covered %d times: %+v", i%W, i/W, n, r)
			}
		}
		if r.body.h < 3 {
			t.Fatalf("the conversation has %d rows on %dx%d", r.body.h, W, H)
		}
		if panel && W >= panelMinCols && r.panel.empty() {
			t.Fatal("no panel on a wide screen")
		}
	})
}

func TestBar(t *testing.T) {
	var g grid
	g.resize(10, 1)
	for _, c := range []struct {
		done float64
		want string
	}{
		{0, "──────────"},
		{0.5, "█████─────"},
		{0.55, "█████▌────"},
		{1, "██████████"},
		{2, "██████████"},
		{-1, "──────────"},
	} {
		bar(&g, 0, 0, 10, c.done, 0, 0)
		if got := strings.TrimSpace(screenText(&g)); got != c.want {
			t.Errorf("%v: %q, want %q", c.done, got, c.want)
		}
	}
}

func TestTextCut(t *testing.T) {
	var g grid
	g.resize(12, 1)
	if x := textIn(&g, 0, 0, 8, "revisando tu agenda", 0); x != 8 || strings.TrimSpace(screenText(&g)) != "revisan…" {
		t.Errorf("%d %q", x, screenText(&g))
	}
	g.resize(12, 1)
	if x := textRight(&g, 0, 12, 0, "21:40", 0); x != 7 || screenText(&g) != "       21:40\n" {
		t.Errorf("%d %q", x, screenText(&g))
	}
	g.resize(6, 1)
	textIn(&g, 0, 0, 6, "日本語です", 0)
	if got := screenText(&g); got != "日本…\n" {
		t.Errorf("wide: %q", got)
	}
}
