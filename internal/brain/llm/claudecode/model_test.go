package claudecode

import "testing"

func TestModelName(t *testing.T) {
	for model, want := range map[string]string{
		"sonnet":                    "Claude Sonnet, by Anthropic",
		" Opus ":                    "Claude Opus, by Anthropic",
		"sonnet[1m]":                "Claude Sonnet, by Anthropic",
		"claude-sonnet-5-5":         "Claude Sonnet 5.5, by Anthropic",
		"claude-haiku-5-5-20261001": "Claude Haiku 5.5, by Anthropic",
		"claude-opus-5":             "Claude Opus 5, by Anthropic",
		"claude-3-5-sonnet":         "Claude, by Anthropic",
		"":                          "Claude, by Anthropic",
		"claude-":                   "Claude, by Anthropic",
		"sónnet":                    "Claude, by Anthropic",
		"opusplan":                  "Claude, by Anthropic",
		"default":                   "Claude, by Anthropic",
		"fable":                     "Claude Fable, by Anthropic",
	} {
		if got := ModelName(model); got != want {
			t.Errorf("ModelName(%q) = %q, want %q", model, got, want)
		}
	}
}
