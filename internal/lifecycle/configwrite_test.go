package lifecycle_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
)

func TestSetConfigValues(t *testing.T) {
	for _, tc := range []struct {
		name, old, want string
		missing         bool
	}{
		{"replace and append", "# my settings\nnotify = none\n\ncaps.global = 9\n",
			"# my settings\nnotify = none\n\ncaps.global = 2\n# set by brigada init\ncaps.account.claude = 2\ncaps.plan = 3\n", false},
		{"create", "", "# set by brigada init\ncaps.account.claude = 2\ncaps.global = 2\ncaps.plan = 3\n", true},
		{"one comment", "# set by brigada init\ncaps.global = 2\n\n",
			"# set by brigada init\ncaps.global = 2\n\ncaps.account.claude = 2\ncaps.plan = 3\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "nested", "config")
			if !tc.missing {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tc.old), 0640); err != nil {
					t.Fatal(err)
				}
			}
			values := map[string]string{
				"caps.global": "2", "caps.plan": "3", "caps.account.claude": "2",
			}
			changed, err := lifecycle.SetConfigValues(path, values)
			if err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != tc.want {
				t.Fatalf("config=%q want=%q err=%v", data, tc.want, err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			mode := os.FileMode(0640)
			if tc.missing {
				mode = 0600
			}
			if info.Mode().Perm() != mode {
				t.Errorf("permissions=%o want=%o", info.Mode().Perm(), mode)
			}
			if _, err := lifecycle.LoadConfigFile(path); err != nil {
				t.Fatalf("writer produced invalid config: %v", err)
			}
			changed, err = lifecycle.SetConfigValues(path, values)
			if err != nil || changed {
				t.Fatalf("repeat: changed=%v err=%v", changed, err)
			}
		})
	}
	t.Run("preserve exact unchanged line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config")
		const old = "# keep\n\n caps.global  =  2"
		if err := os.WriteFile(path, []byte(old), 0600); err != nil {
			t.Fatal(err)
		}
		changed, err := lifecycle.SetConfigValues(path, map[string]string{"caps.global": "2"})
		data, readErr := os.ReadFile(path)
		if changed || err != nil || readErr != nil || string(data) != old {
			t.Fatalf("changed=%v err=%v read=%v config=%q", changed, err, readErr, data)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target, link := filepath.Join(dir, "target"), filepath.Join(dir, "config")
		if err := os.WriteFile(target, []byte("notify = none\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		changed, err := lifecycle.SetConfigValues(link, map[string]string{"caps.global": "2"})
		if changed || err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
		data, _ := os.ReadFile(target)
		if string(data) != "notify = none\n" {
			t.Fatalf("symlink target changed: %q", data)
		}
	})
	for _, existing := range []bool{false, true} {
		name := "create through symlinked directory"
		if existing {
			name = "replace through symlinked directory"
		}
		t.Run(name, func(t *testing.T) {
			target := t.TempDir()
			link := filepath.Join(t.TempDir(), "brigada")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(link, "config")
			if existing {
				if err := os.WriteFile(path, []byte("# keep\nnotify = none\n\ncaps.global = 9\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := lifecycle.CheckConfigWritePath(path); err != nil {
				t.Fatalf("parent symlink refused before write: %v", err)
			}
			changed, err := lifecycle.SetConfigValues(path, map[string]string{"caps.global": "2"})
			if err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			want := "# set by brigada init\ncaps.global = 2\n"
			if existing {
				want = "# keep\nnotify = none\n\ncaps.global = 2\n"
			}
			data, err := os.ReadFile(filepath.Join(target, "config"))
			if err != nil || string(data) != want {
				t.Fatalf("config=%q want=%q err=%v", data, want, err)
			}
			info, err := os.Lstat(link)
			if err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("parent symlink changed: info=%v err=%v", info, err)
			}
		})
	}
}
