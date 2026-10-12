package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

// trustPublisherCommand is `tap trust-publisher`: manage the keys this machine
// accepts as proof that a registry published a package.
//
//	tap trust-publisher add --publisher PREFIX --key-id ID --public-key BASE64
//	tap trust-publisher list
//	tap trust-publisher remove --key-id ID [--publisher PREFIX]
//
// The key and its publisher namespace are the person's decision, like trusting
// a package; the runner has no terminal question here because the key is an
// explicit argument, not something a package can ask for.
func trustPublisherCommand(args []string, stdout, stderr io.Writer) int {
	usage := func() int {
		fmt.Fprintln(stderr, "usage: tap trust-publisher add --publisher PREFIX --key-id ID --public-key BASE64 [--alg ed25519]\n       tap trust-publisher list\n       tap trust-publisher remove --key-id ID [--publisher PREFIX]")
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	roots := newPublisherRoots()
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("tap trust-publisher add", flag.ContinueOnError)
		fs.SetOutput(stderr)
		prefix := fs.String("publisher", "", "publisher namespace the key may sign for, such as com.acme (covers com.acme.tools too)")
		keyID := fs.String("key-id", "", "the key's id as the registry names it, such as tap-attestation:v1")
		pub := fs.String("public-key", "", "the key's Ed25519 public key, base64")
		alg := fs.String("alg", "ed25519", "key algorithm")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if fs.NArg() != 0 {
			return usage()
		}
		root := publisherRoot{PublisherPrefix: *prefix, KeyID: *keyID, Alg: *alg, PublicKeyB64: strings.TrimSpace(*pub)}
		if err := roots.add(root); err != nil {
			fmt.Fprintln(stderr, "tap trust-publisher:", err)
			return 1
		}
		fmt.Fprintf(stdout, "trusted: %s may sign for %s and the namespaces under it\n", root.KeyID, root.PublisherPrefix)
		return 0
	case "list":
		if len(args) != 1 {
			return usage()
		}
		list := roots.list()
		if len(list) == 0 {
			fmt.Fprintln(stdout, "no publishers are trusted")
			return 0
		}
		for _, r := range list {
			fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", r.PublisherPrefix, r.KeyID, r.Alg, r.PublicKeyB64)
		}
		return 0
	case "remove":
		fs := flag.NewFlagSet("tap trust-publisher remove", flag.ContinueOnError)
		fs.SetOutput(stderr)
		prefix := fs.String("publisher", "", "only remove the key for this publisher namespace")
		keyID := fs.String("key-id", "", "the key id to stop trusting")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if fs.NArg() != 0 || *keyID == "" {
			return usage()
		}
		n, err := roots.remove(*prefix, *keyID)
		if err != nil {
			fmt.Fprintln(stderr, "tap trust-publisher:", err)
			return 1
		}
		fmt.Fprintf(stdout, "removed %d trusted key(s)\n", n)
		return 0
	}
	return usage()
}
