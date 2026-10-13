package prompt

import (
	"strings"
	"testing"
)

func TestWorkerTemplateVersion(t *testing.T) {
	if WorkerTemplateVersion <= 0 {
		t.Errorf("WorkerTemplateVersion = %d, want a positive version", WorkerTemplateVersion)
	}
}

func TestComposeWorkerPrompt(t *testing.T) {
	spec := "Add the login form.\nInclude tests."
	got := ComposeWorkerPrompt(spec, "")

	// The template is the fixed part (§5.3)...
	if !strings.HasPrefix(got, "You are a brigada worker") {
		t.Errorf("prompt does not start with the template")
	}
	// ...the spec rides verbatim between data delimiters (§1.4)...
	if !strings.Contains(got, "<task-spec>\n"+spec+"\n</task-spec>") {
		t.Errorf("the spec is not verbatim between delimiters:\n%s", got)
	}
	// ...and no feedback section belongs to a first-round job.
	if strings.Contains(got, "feedback") {
		t.Errorf("a fresh prompt carries a feedback section:\n%s", got)
	}
	// The template tells the worker to end with the result block (§5.4).
	if !strings.Contains(got, "```json") {
		t.Errorf("the template does not name the fenced json result block")
	}

	withFeedback := ComposeWorkerPrompt(spec, "The check failed:\ngo test: undefined: Foo")
	if !strings.Contains(got, "<task-spec>") || !strings.Contains(withFeedback, "<feedback>\nThe check failed:\ngo test: undefined: Foo\n</feedback>") {
		t.Errorf("feedback is not verbatim between delimiters:\n%s", withFeedback)
	}
	if !strings.Contains(withFeedback, "<task-spec>\n"+spec+"\n</task-spec>") {
		t.Errorf("a follow-up prompt lost the spec:\n%s", withFeedback)
	}
}
