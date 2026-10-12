package main

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The flagged inputs below are assembled at runtime from fragments, so
// this file itself contains no whole literal for the guard to find and the
// allowlist stays empty. Addresses that are always clean (loopback and the
// documentation blocks and domains) are written out literally.

func TestScanLine(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string // rule:matched text, in order
	}{
		// Private and special IPv4 literals.
		{name: "private use 10/8",
			text: "db at " + "10." + "0.0.7",
			want: []string{ruleIP + ": " + "10." + "0.0.7"}},
		{name: "private use 172.16/12 low edge",
			text: "ssh " + "172." + "16.0.1",
			want: []string{ruleIP + ": " + "172." + "16.0.1"}},
		{name: "private use 172.16/12 high edge",
			text: "ssh " + "172." + "31.255.255",
			want: []string{ruleIP + ": " + "172." + "31.255.255"}},
		{name: "private use 192.168/16",
			text: "gw " + "192." + "168.1.1",
			want: []string{ruleIP + ": " + "192." + "168.1.1"}},
		{name: "link-local",
			text: "metadata at " + "169." + "254.169.254",
			want: []string{ruleIP + ": " + "169." + "254.169.254"}},
		{name: "cgnat low edge",
			text: "isp " + "100." + "64.0.1",
			want: []string{ruleIP + ": " + "100." + "64.0.1"}},
		{name: "cgnat high edge",
			text: "isp " + "100." + "127.255.255",
			want: []string{ruleIP + ": " + "100." + "127.255.255"}},

		// Addresses followed or preceded by punctuation are still
		// addresses; only a dot that continues the numeric run glues.
		{name: "sentence-final address",
			text: "see " + "10." + "0.0.7.",
			want: []string{ruleIP + ": " + "10." + "0.0.7"}},
		{name: "address in parentheses",
			text: "(" + "10." + "0.0.7)",
			want: []string{ruleIP + ": " + "10." + "0.0.7"}},
		{name: "address before comma",
			text: "10." + "0.0.7,",
			want: []string{ruleIP + ": " + "10." + "0.0.7"}},
		{name: "address after sentence dot",
			text: "at ." + "10." + "0.0.7",
			want: []string{ruleIP + ": " + "10." + "0.0.7"}},

		// Private-network hostnames.
		{name: "link-local multicast name suffix",
			text: "hosts: " + "printer" + ".local",
			want: []string{ruleHost + ": " + "printer" + ".local"}},
		{name: "lan suffix is case-insensitive",
			text: "nas" + ".LAN",
			want: []string{ruleHost + ": " + "nas" + ".LAN"}},
		{name: "internal suffix",
			text: "api at " + "db" + ".internal",
			want: []string{ruleHost + ": " + "db" + ".internal"}},
		{name: "home arpa suffix",
			text: "route via " + "gw" + ".home.arpa",
			want: []string{ruleHost + ": " + "gw" + ".home.arpa"}},
		{name: "multi-label private name",
			text: "mail" + ".corp" + ".lan",
			want: []string{ruleHost + ": " + "mail" + ".corp" + ".lan"}},

		// Credential shapes.
		{name: "github token",
			text: "token ghp_" + strings.Repeat("a", 36),
			want: []string{ruleCred + ": ghp_" + strings.Repeat("a", 36)}},
		{name: "github oauth token",
			text: "gho_" + strings.Repeat("b", 36),
			want: []string{ruleCred + ": gho_" + strings.Repeat("b", 36)}},
		{name: "github server token",
			text: "ghs_" + strings.Repeat("c", 36),
			want: []string{ruleCred + ": ghs_" + strings.Repeat("c", 36)}},
		{name: "github fine-grained token",
			text: "github_pat_" + strings.Repeat("d", 60),
			want: []string{ruleCred + ": github_pat_" + strings.Repeat("d", 60)}},
		{name: "api key",
			text: "sk-ant-" + strings.Repeat("e", 40),
			want: []string{ruleCred + ": sk-ant-" + strings.Repeat("e", 40)}},
		{name: "aws access key id",
			text: "AKIA" + strings.Repeat("F", 16),
			want: []string{ruleCred + ": AKIA" + strings.Repeat("F", 16)}},
		{name: "slack token",
			text: "xoxb-" + strings.Repeat("g", 24),
			want: []string{ruleCred + ": xoxb-" + strings.Repeat("g", 24)}},
		{name: "private key block",
			text: "-----BEGIN RSA PRIV" + "ATE KEY-----",
			want: []string{ruleCred + ": -----BEGIN RSA PRIV" + "ATE KEY-----"}},

		// Two leaks on one line come out ordered by position.
		{name: "two leaks on one line",
			text: "from " + "10." + "0.0.7" + " with ghp_" + strings.Repeat("a", 36),
			want: []string{
				ruleIP + ": " + "10." + "0.0.7",
				ruleCred + ": ghp_" + strings.Repeat("a", 36),
			}},

		// Clean text: public addresses and names.
		{name: "public address", text: "dns 8.8.8.8"},
		{name: "loopback", text: "listen on 127.0.0.1:8080"},
		{name: "documentation block one", text: "see 192.0.2.3"},
		{name: "documentation block two", text: "see 198.51.100.7"},
		{name: "documentation block three", text: "see 203.0.113.9"},
		{name: "public domain", text: "docs at www.example.org/guard"},
		{name: "reserved tld test", text: "host site.test"},
		{name: "reserved tld invalid", text: "peer box.invalid"},
		{name: "reserved tld example", text: "peer node.example"},

		// Clean text: just outside the flagged ranges.
		{name: "below 172.16/12", text: "pub " + "172." + "15.255.255"},
		{name: "above 172.16/12", text: "pub " + "172." + "32.0.1"},
		{name: "below cgnat", text: "pub " + "100." + "63.255.255"},
		{name: "above cgnat", text: "pub " + "100." + "128.0.1"},
		{name: "below link-local", text: "pub " + "169." + "253.255.255"},
		{name: "above link-local", text: "pub " + "169." + "255.0.1"},

		// Clean text: address-shaped but not addresses.
		{name: "octet over 255", text: "v" + "10." + "0.0.300"},
		{name: "dotted run", text: "9." + "10." + "0.0.1" + ".8"},
		{name: "three octet version", text: "go1.27.1"},

		// Clean text: credential prefixes without a body.
		{name: "short token body", text: "ghp_" + "short"},
		{name: "short key id", text: "AKIA" + "SHORT"},
		{name: "other slack prefix", text: "xoxz-" + strings.Repeat("i", 24)},
		{name: "certificate block", text: "-----BEGIN CERTIFICATE-----"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, lk := range scanLine(tt.text) {
				got = append(got, lk.rule+": "+tt.text[lk.start:lk.end])
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("scanLine(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

func TestScanFile(t *testing.T) {
	ip := "10." + "0.0.7"
	exs, err := parseAllowlist("docs/net.md:benchmark\n")
	if err != nil {
		t.Fatalf("parseAllowlist: %v", err)
	}
	data := []byte("the benchmark host is " + ip + "\nplain mention: " + ip + "\n")
	got := scanFile("docs/net.md", data, exs)
	want := []finding{{path: "docs/net.md", line: 2, rule: ruleIP, excerpt: "plain mention: " + ip}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scanFile() = %+v, want %+v", got, want)
	}
}

func TestScanFileRedactsCredentials(t *testing.T) {
	token := "ghp_" + strings.Repeat("a", 36)
	data := []byte("config token " + token + " here\n")
	got := scanFile("conf.txt", data, nil)
	want := []finding{{path: "conf.txt", line: 1, rule: ruleCred, excerpt: "config token " + "ghp_" + "… here"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scanFile() = %+v, want %+v", got, want)
	}
	if strings.Contains(got[0].excerpt, strings.Repeat("a", 36)) {
		t.Errorf("excerpt %q republishes the token body", got[0].excerpt)
	}
}

func TestScanFileKeepsPEMHeader(t *testing.T) {
	data := []byte("key " + "-----BEGIN RSA PRIV" + "ATE KEY-----" + " lost\n")
	got := scanFile("key.txt", data, nil)
	want := []finding{{path: "key.txt", line: 1, rule: ruleCred, excerpt: "key " + "-----BEGIN RSA PRIV" + "ATE KEY-----" + " lost"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scanFile() = %+v, want %+v", got, want)
	}
}

func TestScanFileSkipsBinary(t *testing.T) {
	data := []byte("nul ahead " + "10." + "0.0.7" + "\x00trailer")
	if got := scanFile("bin", data, nil); got != nil {
		t.Errorf("scanFile(binary) = %+v, want no findings", got)
	}
}

func TestParseAllowlist(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []string // parsed path:pattern pairs
		wantErr bool
	}{
		{name: "empty"},
		{name: "comments and blanks", in: "# a comment\n\n   \n"},
		{name: "entry", in: "docs/net.md:benchmark", want: []string{"docs/net.md:benchmark"}},
		{name: "pattern may contain colons", in: "a.md:host:\\d+", want: []string{"a.md:host:\\d+"}},
		{name: "empty pattern allows whole file", in: "a.md:", want: []string{"a.md:"}},
		{name: "missing separator", in: "docs/net.md", wantErr: true},
		{name: "bad regexp", in: "docs/net.md:[", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exs, err := parseAllowlist(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseAllowlist(%q) = %v, want error", tt.in, exs)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAllowlist(%q): %v", tt.in, err)
			}
			var got []string
			for _, ex := range exs {
				got = append(got, ex.path+":"+ex.re.String())
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseAllowlist(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFindingString(t *testing.T) {
	f := finding{path: "docs/net.md", line: 3, rule: ruleIP, excerpt: "excerpt"}
	if got, want := f.String(), "docs/net.md:3: private-ip: excerpt"; got != want {
		t.Errorf("finding.String() = %q, want %q", got, want)
	}
}

// TestRepositoryIsClean runs the guard over the repository itself: the
// guard must pass on the tree that ships it, with an empty allowlist.
func TestRepositoryIsClean(t *testing.T) {
	if _, err := repoRoot(); err != nil {
		t.Skipf("outside a git checkout: %v", err)
	}
	var stdout bytes.Buffer
	if code := run(&stdout, os.Stderr); code != 0 {
		t.Fatalf("guard exit code %d, output:\n%s", code, stdout.String())
	}
	if stdout.String() != "" {
		t.Fatalf("guard reported findings on a clean tree:\n%s", stdout.String())
	}
}
