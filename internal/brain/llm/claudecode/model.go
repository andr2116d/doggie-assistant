package claudecode

import "strings"

// families are the model families DOGGIE names; any other model is just
// Claude.
var families = map[string]bool{"fable": true, "haiku": true, "opus": true, "sonnet": true}

// ModelName is how DOGGIE names a model of this provider when asked about
// it: "sonnet" is "Claude Sonnet, by Anthropic" and "claude-opus-5-5" is
// "Claude Opus 5.5, by Anthropic".
func ModelName(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	m, _, _ = strings.Cut(m, "[") // e.g. "sonnet[1m]"
	parts := strings.Split(strings.TrimPrefix(m, "claude-"), "-")
	family := parts[0]
	if !families[family] {
		return "Claude, by Anthropic"
	}
	name := "Claude " + strings.ToUpper(family[:1]) + family[1:]
	var version []string
	for _, p := range parts[1:] {
		// A longer number is a release date, not part of the version.
		if p == "" || len(p) > 2 || strings.Trim(p, "0123456789") != "" {
			break
		}
		version = append(version, p)
	}
	if len(version) > 0 {
		name += " " + strings.Join(version, ".")
	}
	return name + ", by Anthropic"
}
