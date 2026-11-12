package approval

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Key is one enrolled approval key: a label the user chose and the public
// key, never a private one. Fingerprint is the key's SHA256 fingerprint,
// the name `ssh-keygen -l` prints and the store records on an approval.
type Key struct {
	Label       string
	Type        string
	Fingerprint string
	blob        []byte
	pub         *publicKey
}

// AuthorizedKey is the key as one authorized_keys-style line, `type
// base64`, the way the user supplied it.
func (k Key) AuthorizedKey() string {
	return k.Type + " " + base64.StdEncoding.EncodeToString(k.blob)
}

// ParsePublicKey reads a public key the user supplies: one line of a .pub
// file or an authorized_keys entry, `type base64 [comment]`. It returns
// the key with the comment as its label, empty when there is none. A
// private key is refused with the hint to pass the .pub file instead, and
// an unsupported or malformed key is refused here, at enrollment.
func ParsePublicKey(text string) (Key, string, error) {
	if strings.Contains(text, "PRIVATE KEY") {
		return Key{}, "", errors.New("that is a private key; enroll the public key (the .pub file beside it) — the engine never holds a private key")
	}
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 {
		return Key{}, "", fmt.Errorf("want one public key line, `type base64 [comment]`; got %d lines", len(lines))
	}
	fields := strings.Fields(lines[0])
	if len(fields) < 2 {
		return Key{}, "", errors.New("want a public key line, `type base64 [comment]`")
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return Key{}, "", fmt.Errorf("the public key's base64 does not decode: %v", err)
	}
	key, err := keyOf(blob)
	if err != nil {
		return Key{}, "", err
	}
	if key.Type != fields[0] {
		return Key{}, "", fmt.Errorf("the line says %q but the key is %q", fields[0], key.Type)
	}
	return key, strings.Join(fields[2:], " "), nil
}

func keyOf(blob []byte) (Key, error) {
	pub, err := parsePublicKey(blob)
	if err != nil {
		return Key{}, err
	}
	return Key{Type: pub.typ, Fingerprint: fingerprint(blob), blob: pub.blob, pub: pub}, nil
}

// checkLabel refuses a label the key file could not hold on one line.
func checkLabel(label string) error {
	if label == "" {
		return errors.New("an approval key needs a label")
	}
	if strings.ContainsFunc(label, func(r rune) bool { return r <= ' ' || r == 0x7f }) || strings.HasPrefix(label, "#") {
		return fmt.Errorf("label %q must be one word, no spaces or control characters, not starting with #", label)
	}
	return nil
}

// The key file (<state>/approval-keys, beside the token file) holds one
// enrolled key per line, `label type base64`, with # comments. It holds
// public keys alone, yet it is kept 0600 like the token file, and a file
// any other user could write is refused: whoever writes it chooses who
// may approve.

// LoadKeys reads the enrolled keys. A missing file is no keys — every
// approval then refuses — and a malformed line or a file readable or
// writable by anyone but its owner is an error, never a partial set.
func LoadKeys(path string) ([]Key, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the approval key file %s: %w", path, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("approval key file %s has mode %04o; it must be readable and writable by you alone (chmod 600 %s)", path, perm, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the approval key file %s: %w", path, err)
	}
	var keys []Key
	for n, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("approval key file %s: line %d: want `label type base64`, got %d fields", path, n+1, len(fields))
		}
		blob, err := base64.StdEncoding.DecodeString(fields[2])
		if err != nil {
			return nil, fmt.Errorf("approval key file %s: line %d: %v", path, n+1, err)
		}
		key, err := keyOf(blob)
		if err == nil && key.Type != fields[1] {
			err = fmt.Errorf("the line says %q but the key is %q", fields[1], key.Type)
		}
		if err != nil {
			return nil, fmt.Errorf("approval key file %s: line %d: %v", path, n+1, err)
		}
		key.Label = fields[0]
		keys = append(keys, key)
	}
	return keys, nil
}

// AddKey enrolls key under label. A label or a key already enrolled is
// refused, so one key answers to one name.
func AddKey(path, label string, key Key) error {
	if err := checkLabel(label); err != nil {
		return err
	}
	if key.pub == nil {
		return errors.New("the key was not parsed; use ParsePublicKey")
	}
	keys, err := LoadKeys(path)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if k.Label == label {
			return fmt.Errorf("an approval key labelled %q is already enrolled (%s)", label, k.Fingerprint)
		}
		if k.Fingerprint == key.Fingerprint {
			return fmt.Errorf("that key is already enrolled as %q", k.Label)
		}
	}
	key.Label = label
	return writeKeys(path, append(keys, key))
}

// RemoveKey drops the key enrolled under name, a label or a fingerprint,
// and returns it.
func RemoveKey(path, name string) (Key, error) {
	keys, err := LoadKeys(path)
	if err != nil {
		return Key{}, err
	}
	for i, k := range keys {
		if k.Label == name || k.Fingerprint == name {
			return k, writeKeys(path, append(keys[:i:i], keys[i+1:]...))
		}
	}
	return Key{}, fmt.Errorf("no approval key %q is enrolled", name)
}

// writeKeys replaces the key file through a 0600 temp file and a rename,
// as the token file is written, so a reader never sees half of it.
func writeKeys(path string, keys []Key) error {
	var b strings.Builder
	b.WriteString("# brigada approval keys: `label type base64`, public keys only; see brigada approval --help\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s %s\n", k.Label, k.AuthorizedKey())
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("write the approval key file %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".approval-keys-")
	if err != nil {
		return fmt.Errorf("write the approval key file %s: %w", path, err)
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename below succeeds
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("write the approval key file %s: %w", path, err)
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return fmt.Errorf("write the approval key file %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write the approval key file %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write the approval key file %s: %w", path, err)
	}
	return nil
}
