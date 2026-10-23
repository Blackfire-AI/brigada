package adapter

import "testing"

func TestIsModelRejectedText(t *testing.T) {
	for text, want := range map[string]bool{
		"The 'x' model is not supported when using Codex with a ChatGPT account.": true,
		"The model `x` does not exist or you do not have access to it.":           true,
		"Model not found: x": true,
		"Model Not Exist":    true,
		"There's an issue with the selected model (x).":                    true,
		"Model metadata for x not found. Defaulting to fallback metadata.": false,
		"session not found":  false,
		"rate limit reached": false,
	} {
		if got := IsModelRejectedText(text); got != want {
			t.Errorf("IsModelRejectedText(%q) = %v, want %v", text, got, want)
		}
	}
}
