package main

// A package installed from a registry can carry a signed publish attestation
// beside it: ATTESTATION.json holds the exact signed bytes and ATTESTATION.sig
// names the key and holds the Ed25519 signature. The runner checks the pair at
// admission against the publishers this person trusts, and needs no network and
// no service to do it.
//
// Three outcomes, and a fourth for a package that carries none:
//
//	verified    a trusted key for that publisher made the signature, and the
//	            attestation describes this package
//	invalid     the pair claims a signature and fails it (malformed, a different
//	            package, a failed validation, a signature that does not verify
//	            under a trusted key): the package is refused
//	unverified  the pair is well formed and consistent but no trusted key
//	            covers it, so nothing can be said; the package is treated as one
//	            with no attestation
//	absent      no attestation files: a local primitive, unsigned by design
//
// What the signature binds is the publisher, name, version and artifact digest
// of the package as it was published. The runner compares those to the
// package's manifest and to the digest the installer recorded; it cannot
// recompute the published tarball's digest from an unpacked folder.

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

const (
	attestationFile    = "ATTESTATION.json"
	attestationSigFile = "ATTESTATION.sig"
	maxAttestationSize = 64 << 10
)

type attestationState int

const (
	attestationAbsent attestationState = iota
	attestationUnverified
	attestationVerified
	attestationInvalid
)

// attestationResult is what checking a package's attestation found.
type attestationResult struct {
	State  attestationState
	KeyID  string
	Reason string
}

// label is the short text tap_search and tap_load show; empty when the package
// carries no attestation.
func (r attestationResult) label() string {
	switch r.State {
	case attestationVerified:
		return "verified by " + r.KeyID
	case attestationUnverified:
		return "attestation present, not verified: " + r.Reason
	case attestationInvalid:
		return "attestation invalid: " + r.Reason
	}
	return ""
}

// publisherRoot is a key the person trusts to sign for a publisher namespace.
type publisherRoot struct {
	PublisherPrefix string `json:"publisher_prefix"`
	KeyID           string `json:"key_id"`
	Alg             string `json:"alg"`
	PublicKeyB64    string `json:"public_key_b64"`
}

// publisherRoots keeps trusted_publishers in the runner's configuration
// directory, beside the packages and bindings the person has chosen.
type publisherRoots struct {
	path string
	mu   sync.Mutex
}

func defaultPublisherRootsPath() string {
	dir, err := userConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "tap", "trusted_publishers.json")
}

func newPublisherRoots() *publisherRoots { return &publisherRoots{path: defaultPublisherRootsPath()} }

type publisherRootsFile struct {
	TrustedPublishers []publisherRoot `json:"trusted_publishers"`
}

func (p *publisherRoots) load() []publisherRoot {
	if p.path == "" {
		return nil
	}
	b, err := os.ReadFile(p.path)
	if err != nil {
		return nil
	}
	var f publisherRootsFile
	if json.Unmarshal(b, &f) != nil {
		return nil
	}
	return f.TrustedPublishers
}

func (p *publisherRoots) list() []publisherRoot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.load()
}

func (p *publisherRoots) save(roots []publisherRoot) error {
	if p.path == "" {
		return errors.New("this machine has no user config directory to keep the answer in")
	}
	sort.Slice(roots, func(i, j int) bool {
		if roots[i].PublisherPrefix != roots[j].PublisherPrefix {
			return roots[i].PublisherPrefix < roots[j].PublisherPrefix
		}
		return roots[i].KeyID < roots[j].KeyID
	})
	b, _ := json.MarshalIndent(publisherRootsFile{TrustedPublishers: roots}, "", "  ")
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p.path, append(b, '\n'), 0o600)
}

// validate reports what is wrong with a root, or "".
func (r publisherRoot) validate() string {
	switch {
	case r.PublisherPrefix == "" || strings.ContainsAny(r.PublisherPrefix, "*/@ \t\r\n"):
		return "the publisher prefix must be a reverse-DNS name such as com.acme, with no wildcard"
	case r.KeyID == "":
		return "the key id is empty"
	case r.Alg != "ed25519":
		return fmt.Sprintf("algorithm %q is not supported; only ed25519", r.Alg)
	}
	if key, err := base64.StdEncoding.DecodeString(r.PublicKeyB64); err != nil || len(key) != ed25519.PublicKeySize {
		return "the public key is not a base64 Ed25519 public key (32 bytes)"
	}
	return ""
}

// add keeps root, replacing one with the same prefix and key id.
func (p *publisherRoots) add(root publisherRoot) error {
	if why := root.validate(); why != "" {
		return errors.New(why)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	roots := p.load()
	for i, r := range roots {
		if r.PublisherPrefix == root.PublisherPrefix && r.KeyID == root.KeyID {
			roots[i] = root
			return p.save(roots)
		}
	}
	return p.save(append(roots, root))
}

// remove drops every root for keyID (under prefix, when prefix is not empty)
// and reports how many it dropped.
func (p *publisherRoots) remove(prefix, keyID string) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var kept []publisherRoot
	removed := 0
	for _, r := range p.load() {
		if r.KeyID == keyID && (prefix == "" || r.PublisherPrefix == prefix) {
			removed++
			continue
		}
		kept = append(kept, r)
	}
	if removed == 0 {
		return 0, nil
	}
	return removed, p.save(kept)
}

// prefixCovers is true when publisher is the prefix or lies under it on a
// dot boundary: com.acme covers com.acme and com.acme.tools, not com.acmex.
func prefixCovers(prefix, publisher string) bool {
	return publisher == prefix || strings.HasPrefix(publisher, prefix+".")
}

func readSmallRegularFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("is not a regular file")
	}
	if info.Size() > maxAttestationSize {
		return nil, fmt.Errorf("is larger than %d bytes", maxAttestationSize)
	}
	return os.ReadFile(path)
}

type attestationSig struct {
	KeyID     string `json:"key_id"`
	Alg       string `json:"alg"`
	Signature string `json:"signature"`
}

type attestationPayload struct {
	Validated      bool   `json:"validated"`
	ArtifactDigest string `json:"artifact_digest"`
	Publisher      string `json:"publisher"`
	Name           string `json:"name"`
	Version        string `json:"version"`
}

func invalidAttestation(format string, a ...any) attestationResult {
	return attestationResult{State: attestationInvalid, Reason: fmt.Sprintf(format, a...)}
}

// checkAttestation examines the attestation beside the package in dir, whose
// manifest is m, against the trusted roots.
func checkAttestation(dir string, m *mf.Manifest, roots []publisherRoot) attestationResult {
	payloadPath, sigPath := filepath.Join(dir, attestationFile), filepath.Join(dir, attestationSigFile)
	_, payloadErr := os.Lstat(payloadPath)
	_, sigErr := os.Lstat(sigPath)
	if os.IsNotExist(payloadErr) && os.IsNotExist(sigErr) {
		return attestationResult{State: attestationAbsent}
	}
	payload, err := readSmallRegularFile(payloadPath)
	if err != nil {
		return invalidAttestation("%s: %v", attestationFile, err)
	}
	sigRaw, err := readSmallRegularFile(sigPath)
	if err != nil {
		return invalidAttestation("%s: %v", attestationSigFile, err)
	}
	var sf attestationSig
	if err := json.Unmarshal(sigRaw, &sf); err != nil || sf.KeyID == "" {
		return invalidAttestation("%s is not a signature file", attestationSigFile)
	}
	if sf.Alg != "ed25519" {
		return invalidAttestation("algorithm %q is not supported", sf.Alg)
	}
	sig, err := base64.StdEncoding.DecodeString(sf.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return invalidAttestation("the signature is not a base64 Ed25519 signature")
	}
	var att attestationPayload
	if err := json.Unmarshal(payload, &att); err != nil {
		return invalidAttestation("%s is not valid JSON", attestationFile)
	}

	// Consistency with the package comes before any key is consulted: an
	// attestation that describes another package is wrong whoever signed it.
	md := m.Metadata
	for _, c := range []struct{ field, attested, have string }{
		{"publisher", att.Publisher, md.Publisher},
		{"name", att.Name, md.Name},
		{"version", att.Version, md.Version},
	} {
		if c.attested != c.have {
			return invalidAttestation("it attests %s %q but this package has %q", c.field, c.attested, c.have)
		}
	}
	if _, installed := readRegistryMarker(dir); installed != "" && installed != att.ArtifactDigest {
		return invalidAttestation("it attests artifact digest %s but this package was installed as %s", att.ArtifactDigest, installed)
	}
	if !att.Validated {
		return invalidAttestation("the publish it attests did not pass validation")
	}

	var covering []publisherRoot
	for _, r := range roots {
		if r.KeyID == sf.KeyID && r.Alg == "ed25519" && prefixCovers(r.PublisherPrefix, att.Publisher) {
			covering = append(covering, r)
		}
	}
	if len(covering) == 0 {
		return attestationResult{State: attestationUnverified, KeyID: sf.KeyID,
			Reason: fmt.Sprintf("no trusted key %s for publisher %s (tap trust-publisher add)", sf.KeyID, att.Publisher)}
	}
	for _, r := range covering {
		if key, err := base64.StdEncoding.DecodeString(r.PublicKeyB64); err == nil && len(key) == ed25519.PublicKeySize &&
			ed25519.Verify(key, payload, sig) {
			return attestationResult{State: attestationVerified, KeyID: sf.KeyID}
		}
	}
	return attestationResult{State: attestationInvalid, KeyID: sf.KeyID,
		Reason: fmt.Sprintf("the signature does not verify under the trusted key %s", sf.KeyID)}
}

// attestationRefusal is why a package must not run on account of its
// attestation, or "".
func attestationRefusal(dir string, m *mf.Manifest) string {
	r := checkAttestation(dir, m, newPublisherRoots().list())
	if r.State != attestationInvalid {
		return ""
	}
	return fmt.Sprintf("%s is refused: %s. Its %s claims the registry signed it and that does not hold; pull it again from the registry",
		m.Metadata.Name, r.Reason, attestationFile)
}
