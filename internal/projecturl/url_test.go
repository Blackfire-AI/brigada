package projecturl

import "testing"

func TestCanonical(t *testing.T) {
	for _, test := range []struct{ raw, want string }{
		{"git@EXAMPLE.com:Org/Repo.git/", "example.com/Org/Repo"},
		{"ssh://git@example.com:22/Org/Repo.git", "example.com/Org/Repo"},
		{"https://secret:password@EXAMPLE.com:443/Org/Repo.git/", "example.com/Org/Repo"},
		{"https://example.com/Org/Repo", "example.com/Org/Repo"},
		{"ssh://git@example.com:2222/Org/Repo.git", "example.com:2222/Org/Repo"},
		{"example.com:2222/Org/Repo", "example.com:2222/Org/Repo"},
		{"example.com/Org/Repo", "example.com/Org/Repo"},
		{"ssh://git@[::1]:22/Org/Repo.git", "[::1]/Org/Repo"},
	} {
		got, err := Canonical(test.raw)
		if err != nil || got != test.want {
			t.Errorf("Canonical(%q)=%q,%v want %q", test.raw, got, err, test.want)
		}
		if again, err := Canonical(got); err != nil || again != got {
			t.Errorf("canonical URL does not round-trip: %q => %q %v", got, again, err)
		}
	}
}
func TestCanonicalRefusesAmbiguityWithoutLeakingCredentials(t *testing.T) {
	for _, raw := range []string{"/local/repo", "file:///local/repo", "git@alias:Org/Repo", "git@example.com:/absolute/repo", "git@example.com:~/repo", "https://secret:password@example.com:bad/repo", "https://secret:password@example.com/repo?secret=oops", "example.com/Org/../Repo", "example.com:port/Org/Repo", "https://example.com:65536/repo"} {
		got, err := Canonical(raw)
		if err == nil || got != "" {
			t.Errorf("ambiguous input accepted: %q", raw)
		}
	}
}
