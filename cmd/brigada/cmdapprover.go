package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/Blackfire-AI/brigada/internal/approval"
)

// maxPublicKeyBytes bounds what the add command reads: a public key is a
// few hundred bytes, and a file far larger is not one.
const maxPublicKeyBytes = 16 << 10

var approverCommands = map[string]command{
	"add":    {name: "add", description: "Enroll an approval key from a public key file.", run: runApproverAdd},
	"list":   {name: "list", description: "List the enrolled approval keys.", run: runApproverList},
	"remove": {name: "remove", description: "Remove an enrolled approval key.", run: runApproverRemove},
}

const approverUsage = "usage: brigada approver add --label L --public-key FILE | brigada approver list | brigada approver remove --label L"

// runApprover is `brigada approver` (§12.2): the user's approval keys.
// An approval of a plan, a landing or a push is the user's signature with
// one of these keys beyond any token; the engine keeps the public halves
// beside its token file and holds no private key. The commands edit that
// file directly, so they need no running engine.
func runApprover(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return dispatch(ctx, args, stdout, stderr, "approver", approverUsage, "add, list or remove", approverCommands)
}

func runApproverAdd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("approver add", "brigada approver add --label L --public-key FILE",
		"Enroll an approval key: a public key file, such as ~/.ssh/id_ed25519.pub, under a label. The private key is never read.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	var label, file string
	fs.StringVar(&label, "label", "", "the label to enroll the key under (letters, digits, . _ @ -)")
	fs.StringVar(&file, "public-key", "", "the public key file (`path`), or - for standard input")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if len(fs.Args()) != 0 {
		return usagef(stderr, "approver add takes no positional arguments")
	}
	if label == "" || file == "" {
		return usagef(stderr, "approver add needs --label and --public-key")
	}
	data, err := readPublicKey(file)
	if err != nil {
		return fail(stderr, err)
	}
	key, err := approval.ParseKey(label, data)
	if err != nil {
		return fail(stderr, err)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	store := approval.NewStore(approval.StorePath(rt.stateDir))
	if err := store.Add(key); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "enrolled approval key %s\n", key.Label)
	return 0
}

// readPublicKey reads the key text from a file or, for "-", standard
// input, up to a bound. Nothing read is echoed back in an error: a user
// who pointed at a private key by mistake must not see it printed.
func readPublicKey(file string) (string, error) {
	var r io.Reader = os.Stdin
	if file != "-" {
		f, err := os.Open(file)
		if err != nil {
			return "", fmt.Errorf("read the public key: %w", err)
		}
		defer f.Close()
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, maxPublicKeyBytes+1))
	if err != nil {
		return "", fmt.Errorf("read the public key: %w", err)
	}
	if len(data) > maxPublicKeyBytes {
		return "", fmt.Errorf("%s is over %d bytes, so it is not a public key", file, maxPublicKeyBytes)
	}
	return string(data), nil
}

func runApproverList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("approver list", "brigada approver list", "List the enrolled approval keys.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if len(fs.Args()) != 0 {
		return usagef(stderr, "approver list takes no positional arguments")
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	keys, err := approval.NewStore(approval.StorePath(rt.stateDir)).List()
	if err != nil {
		return fail(stderr, err)
	}
	if len(keys) == 0 {
		fmt.Fprintln(stdout, "no approval keys; enroll one: brigada approver add --label <label> --public-key <file>")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LABEL\tTYPE\tKEY")
	for _, k := range keys {
		typ, blob, _ := cutSpace(k.PublicKey)
		fmt.Fprintf(tw, "%s\t%s\t%s\n", k.Label, typ, shortKey(blob))
	}
	tw.Flush()
	return 0
}

func cutSpace(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// shortKey is the tail of a key's base64, enough to tell keys apart.
func shortKey(blob string) string {
	const n = 16
	if len(blob) <= n {
		return blob
	}
	return "..." + blob[len(blob)-n:]
}

func runApproverRemove(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("approver remove", "brigada approver remove --label L", "Remove an enrolled approval key.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	var label string
	fs.StringVar(&label, "label", "", "the label of the key to remove")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if len(fs.Args()) != 0 {
		return usagef(stderr, "approver remove takes no positional arguments")
	}
	if label == "" {
		return usagef(stderr, "approver remove needs --label")
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	if err := approval.NewStore(approval.StorePath(rt.stateDir)).Remove(label); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "removed approval key %s\n", label)
	return 0
}
