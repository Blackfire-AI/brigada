package planfile

import (
	"context"
	"strings"
	"testing"
)

// TestPushErrorsRedactTheRemoteURL: a failed push names the remote git
// could not reach, and the userinfo of an http(s) remote — a lone token
// or a user:password pair — is never in the error.
func TestPushErrorsRedactTheRemoteURL(t *testing.T) {
	token := "tk-" + strings.Repeat("z", 8)
	for _, userinfo := range []string{token, "ci:" + token} {
		target := Target{URL: "http://" + userinfo + "@127.0.0.1:1/org/repo.git", Branch: "brigada/plans"}
		err := Push(context.Background(), target, goldenPlan())
		if err == nil {
			t.Fatal("a push to an unreachable remote succeeded")
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("push error %q carries the remote's credential", err)
		}
	}
}
