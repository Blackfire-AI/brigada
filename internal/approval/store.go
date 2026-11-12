package approval

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// StoreFile is the name of the enrollment file inside the state directory,
// beside the token file (internal/lifecycle/tokens.go).
const StoreFile = "approval_keys"

// StorePath is the enrollment file's path under a state directory.
func StorePath(stateDir string) string { return filepath.Join(stateDir, StoreFile) }

var (
	ErrKeyExists  = errors.New("an approval key with this label is already enrolled")
	ErrNoSuchKey  = errors.New("no approval key with this label is enrolled")
	labelPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)
	keyTypePrefix = []string{"ssh-", "ecdsa-sha2-", "sk-ssh-", "sk-ecdsa-sha2-"}
)

// Key is one enrolled approval key: a label the user picks and the public
// half of the key as `type base64`, with no options and no comment. The
// private half never reaches this package.
type Key struct {
	Label     string
	PublicKey string
}

// ParseKey checks a label and a public key as ssh-keygen writes it
// (`type base64 [comment]`) and returns the canonical Key. It refuses
// anything that is not a public key, a private key included, and a leading
// options field, so a key line can never smuggle options into the
// allowed_signers file.
func ParseKey(label, public string) (Key, error) {
	if !labelPattern.MatchString(label) {
		return Key{}, fmt.Errorf("approval key label %q: want 1 to 64 of letters, digits and . _ @ -, starting with a letter or digit", label)
	}
	fields := strings.Fields(public)
	if len(fields) < 2 {
		return Key{}, errors.New("approval key: want a public key as `type base64`")
	}
	typ, blob := fields[0], fields[1]
	known := false
	for _, p := range keyTypePrefix {
		if strings.HasPrefix(typ, p) {
			known = true
		}
	}
	raw, err := base64.StdEncoding.DecodeString(blob)
	if !known || err != nil {
		return Key{}, errors.New("approval key: not an OpenSSH public key (`type base64`)")
	}
	// The blob opens with its own type as a length-prefixed string; it must
	// agree with the text field.
	if len(raw) < 4 {
		return Key{}, errors.New("approval key: public key blob is too short")
	}
	n := binary.BigEndian.Uint32(raw)
	if uint64(n) > uint64(len(raw)-4) || string(raw[4:4+n]) != typ {
		return Key{}, errors.New("approval key: the key type does not match its blob")
	}
	return Key{Label: label, PublicKey: typ + " " + blob}, nil
}

// Store is the enrollment file: one `label type base64` line per key. It is
// written mode 0600 through a temp file and a rename, as the token file is.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore returns the store at path; the file need not exist yet.
func NewStore(path string) *Store { return &Store{path: path} }

// Path is the enrollment file's path.
func (s *Store) Path() string { return s.path }

// List returns the enrolled keys sorted by label. A missing file is no keys.
func (s *Store) List() ([]Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read()
}

// Add enrolls a key. A label already enrolled is ErrKeyExists: replacing a
// key is Remove then Add, two deliberate steps.
func (s *Store) Add(k Key) error {
	k, err := ParseKey(k.Label, k.PublicKey)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	keys, err := s.read()
	if err != nil {
		return err
	}
	for _, e := range keys {
		if e.Label == k.Label {
			return fmt.Errorf("%w: %s", ErrKeyExists, k.Label)
		}
	}
	return s.write(append(keys, k))
}

// Remove withdraws the key with label.
func (s *Store) Remove(label string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys, err := s.read()
	if err != nil {
		return err
	}
	kept := keys[:0]
	for _, e := range keys {
		if e.Label != label {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(keys) {
		return fmt.Errorf("%w: %s", ErrNoSuchKey, label)
	}
	return s.write(kept)
}

func (s *Store) read() ([]Key, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the approval key file %s: %w", s.path, err)
	}
	var keys []Key
	for n, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		label, public, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("approval key file %s: line %d: want `label type base64`", s.path, n+1)
		}
		k, err := ParseKey(label, public)
		if err != nil {
			return nil, fmt.Errorf("approval key file %s: line %d: %w", s.path, n+1, err)
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Label < keys[j].Label })
	return keys, nil
}

func (s *Store) write(keys []Key) error {
	sort.Slice(keys, func(i, j int) bool { return keys[i].Label < keys[j].Label })
	var body bytes.Buffer
	body.WriteString("# approval keys: label, then the public key; public halves only\n")
	for _, k := range keys {
		fmt.Fprintf(&body, "%s %s\n", k.Label, k.PublicKey)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".approval-keys-")
	if err != nil {
		return fmt.Errorf("write the approval key file %s: %w", s.path, err)
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename below succeeds
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("write the approval key file %s: %w", s.path, err)
	}
	if _, err := tmp.Write(body.Bytes()); err != nil {
		tmp.Close()
		return fmt.Errorf("write the approval key file %s: %w", s.path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write the approval key file %s: %w", s.path, err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("write the approval key file %s: %w", s.path, err)
	}
	return nil
}
