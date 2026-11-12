package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/Blackfire-AI/brigada/internal/approval"
)

var approvalCommands = map[string]command{
	"add":    {name: "add", description: "Enroll an approval key from its public key file.", run: runApprovalAdd},
	"list":   {name: "list", description: "List the enrolled approval keys.", run: runApprovalList},
	"remove": {name: "remove", description: "Remove an enrolled approval key by label or fingerprint.", run: runApprovalRemove},
}

// runApproval is `brigada approval` (§12.2): the keys whose signatures
// are the user's approval authority. Enrollment is local — the state
// directory's approval-keys file beside the token file — and takes the
// public half alone; no engine is contacted.
func runApproval(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return dispatch(ctx, args, stdout, stderr, "approval", "usage: brigada approval add|list|remove ...", "add, list or remove", approvalCommands)
}

// runApprovalAdd enrolls one public key. The label defaults to the key's
// comment; a private key, an unsupported type or a label or key already
// enrolled is refused.
func runApprovalAdd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("approval add",
		"brigada approval add [--label NAME] <public-key-file>",
		"Enroll an approval key from its public key file (a .pub file or one authorized_keys line). Only the public key is stored. "+
			"Keep the private key out of reach of processes running as you: a security key (ed25519-sk) or a key on another device.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	label := fs.String("label", "", "the key's label (default: the key's comment)")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return usagef(stderr, "usage: brigada approval add [--label NAME] <public-key-file>")
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	data, err := os.ReadFile(rest[0])
	if err != nil {
		return fail(stderr, fmt.Errorf("read the public key: %w", err))
	}
	key, comment, err := approval.ParsePublicKey(string(data))
	if err != nil {
		return fail(stderr, fmt.Errorf("%s: %w", rest[0], err))
	}
	name := *label
	if name == "" {
		name = comment
	}
	if name == "" {
		return usagef(stderr, "the key has no comment to label it by; pass --label NAME")
	}
	if err := approval.AddKey(rt.paths.ApprovalKeys, name, key); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "approval key %s enrolled: %s %s\n", name, key.Type, key.Fingerprint)
	return 0
}

// runApprovalList prints one line per enrolled key: label, type and
// fingerprint.
func runApprovalList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("approval list",
		"brigada approval list",
		"List the enrolled approval keys: label, key type and SHA256 fingerprint.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "approval list takes no positional arguments, got %q", extra)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	keys, err := approval.LoadKeys(rt.paths.ApprovalKeys)
	if err != nil {
		return fail(stderr, err)
	}
	if len(keys) == 0 {
		fmt.Fprintln(stdout, "no approval keys enrolled; every approval is refused until one is (brigada approval add)")
		return 0
	}
	for _, k := range keys {
		fmt.Fprintf(stdout, "%s\t%s\t%s\n", k.Label, k.Type, k.Fingerprint)
	}
	return 0
}

// runApprovalRemove drops one enrolled key, named by its label or its
// fingerprint.
func runApprovalRemove(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("approval remove",
		"brigada approval remove <label|fingerprint>",
		"Remove an enrolled approval key, named by its label or its SHA256 fingerprint.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return usagef(stderr, "usage: brigada approval remove <label|fingerprint>")
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	key, err := approval.RemoveKey(rt.paths.ApprovalKeys, rest[0])
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "approval key %s removed: %s\n", key.Label, key.Fingerprint)
	return 0
}
