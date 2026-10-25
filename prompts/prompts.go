// Package prompts holds DOGGIE's base prompts. They are written in English;
// the language DOGGIE speaks, its persona and what it knows about its owner
// come from the owner's config.
package prompts

import (
	_ "embed"
	"fmt"
	"strings"
	"text/template"
)

//go:embed system.md
var systemText string

var system = template.Must(template.New("system").Option("missingkey=error").Parse(systemText))

// Persona is what the system prompt is filled in with.
type Persona struct {
	AssistantName string
	OwnerName     string
	// OwnerAbout is free text the owner wrote about themselves.
	OwnerAbout string
	// Persona is free text the owner wrote about how the assistant behaves.
	Persona string
	// Language is a BCP 47 tag, e.g. "es" or "pt-BR".
	Language string
}

// System returns the system prompt for p.
func System(p Persona) (string, error) {
	if strings.TrimSpace(p.AssistantName) == "" {
		return "", fmt.Errorf("prompts: the assistant needs a name")
	}
	if strings.TrimSpace(p.Language) == "" {
		return "", fmt.Errorf("prompts: no language")
	}
	data := Persona{
		AssistantName: strings.TrimSpace(p.AssistantName),
		OwnerName:     strings.TrimSpace(p.OwnerName),
		OwnerAbout:    strings.TrimSpace(p.OwnerAbout),
		Persona:       strings.TrimSpace(p.Persona),
		Language:      LanguageName(p.Language),
	}

	var b strings.Builder
	if err := system.Execute(&b, data); err != nil {
		return "", fmt.Errorf("prompts: %w", err)
	}
	return strings.TrimSpace(b.String()) + "\n", nil
}

// languages names common languages in English, which the model follows more
// reliably than a bare code.
var languages = map[string]string{
	"ar": "Arabic", "ca": "Catalan", "de": "German", "en": "English", "es": "Spanish",
	"fr": "French", "hi": "Hindi", "it": "Italian", "ja": "Japanese", "ko": "Korean",
	"nl": "Dutch", "pl": "Polish", "pt": "Portuguese", "ru": "Russian", "sv": "Swedish",
	"tr": "Turkish", "uk": "Ukrainian", "zh": "Chinese",
}

// LanguageName describes a BCP 47 tag in English: "es" is "Spanish", "pt-BR"
// is "Portuguese (pt-BR)", and an unknown tag is given as is.
func LanguageName(tag string) string {
	tag = strings.TrimSpace(tag)
	base, region, _ := strings.Cut(tag, "-")
	name, ok := languages[strings.ToLower(base)]
	switch {
	case !ok:
		return fmt.Sprintf("the language with the BCP 47 code %q", tag)
	case region != "":
		return fmt.Sprintf("%s (%s)", name, tag)
	}
	return name
}
