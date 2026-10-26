package prompts

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from the golden file (run with -update to accept):\n%s", name, got)
	}
}

func TestSystemFull(t *testing.T) {
	got, err := System(Persona{
		AssistantName: " DOGGIE ",
		OwnerName:     "Alex",
		OwnerAbout:    "  Lives by the sea. Likes sci-fi.\n",
		Persona:       "A calm butler with a dry sense of humor.",
		Language:      "es",
		Model:         " Claude Sonnet, by Anthropic ",
	})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "system_full.golden", got)
	checkClean(t, got)
}

func TestSystemMinimal(t *testing.T) {
	got, err := System(Persona{AssistantName: "Rex", Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "system_minimal.golden", got)
	checkClean(t, got)
	for _, absent := range []string{"# About", "# Your persona", "The owner wrote", "Right now it is", "right now you think with"} {
		if strings.Contains(got, absent) {
			t.Errorf("minimal prompt has %q", absent)
		}
	}
	if !strings.Contains(got, "You work for your owner") {
		t.Error("no fallback for a missing owner name")
	}
}

// The owner's texts are data for the template: they are never executed.
func TestSystemDoesNotRunTemplatesInConfig(t *testing.T) {
	got, err := System(Persona{
		AssistantName: "{{.Language}}", OwnerName: "{{ .Persona }}", Persona: `{{template "system"}}`, Language: "es",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"You are {{.Language}}", "You work for {{ .Persona }}", `{{template "system"}}`} {
		if !strings.Contains(got, s) {
			t.Errorf("missing literal %q", s)
		}
	}
}

func TestSystemRejects(t *testing.T) {
	for name, p := range map[string]Persona{
		"no name":     {Language: "es"},
		"blank name":  {AssistantName: " \t", Language: "es"},
		"no language": {AssistantName: "DOGGIE"},
		"blank lang":  {AssistantName: "DOGGIE", Language: "  "},
	} {
		if _, err := System(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLanguageName(t *testing.T) {
	for tag, want := range map[string]string{
		"es":         "Spanish",
		"ES":         "Spanish",
		" es ":       "Spanish",
		"es-PE":      "Spanish (es-PE)",
		"pt-BR":      "Portuguese (pt-BR)",
		"zh-Hant-TW": "Chinese (zh-Hant-TW)",
		"qu":         `the language with the BCP 47 code "qu"`,
		"x\"y":       `the language with the BCP 47 code "x\"y"`,
	} {
		if got := LanguageName(tag); got != want {
			t.Errorf("LanguageName(%q) = %q, want %q", tag, got, want)
		}
	}
}

func checkClean(t *testing.T, got string) {
	t.Helper()
	for _, bad := range []string{"{{", "}}", "<no value>", "\n\n\n"} {
		if strings.Contains(got, bad) {
			t.Errorf("prompt contains %q", bad)
		}
	}
	if !strings.HasSuffix(got, ".\n") || strings.HasSuffix(got, "\n\n") {
		t.Errorf("prompt should end with one newline: %q", got[len(got)-20:])
	}
}
