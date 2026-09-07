package main

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/amber-store/core/fstree"
	"github.com/amber-store/core/key"
	"github.com/amber-store/core/packstore"
	"github.com/amber-store/core/reference"
	"github.com/amber-store/core/refstore"
)

// resolveSpec parses a content spec: either KEY[/PATH] (lowercase-hex key,
// slash-separated subpath) or ref:NAME[@PATH] (reference name, '@'-separated
// subpath — '@' is banned in names, so the first '@' is unambiguous).
// Reference names resolve through the store's references DB.
func resolveSpec(refs *refstore.Store, s string) (key.Key, string, error) {
	rest, isRef := strings.CutPrefix(s, "ref:")
	if !isRef {
		return parseKeyPath(s)
	}
	name, path, _ := strings.Cut(rest, "@")
	if err := reference.ValidateName(name); err != nil {
		return key.Key{}, "", fmt.Errorf("invalid reference spec %q: %w", s, err)
	}
	raw, err := refs.Get(name)
	if err != nil {
		return key.Key{}, "", err
	}
	rec, err := reference.Decode(raw)
	if err != nil {
		return key.Key{}, "", fmt.Errorf("reference %q: %w", name, err)
	}
	k, err := key.Parse(rec.Key)
	if err != nil {
		return key.Key{}, "", fmt.Errorf("reference %q: stored key: %w", name, err)
	}
	return k, path, nil
}

// parseKeyPath splits a KEY[/PATH] argument at the first slash and decodes the
// key part. The returned path is empty when no slash follows the key.
func parseKeyPath(s string) (key.Key, string, error) {
	keyPart, path, _ := strings.Cut(s, "/")
	k, err := parseHexKey(keyPart)
	if err != nil {
		return key.Key{}, "", err
	}
	return k, path, nil
}

// parseHexKey decodes a lowercase-hex key argument into a validated key.
func parseHexKey(s string) (key.Key, error) {
	raw, err := hex.DecodeString(s)
	if err != nil {
		return key.Key{}, fmt.Errorf("invalid key %q: %w", s, err)
	}
	k, err := key.Parse(raw)
	if err != nil {
		return key.Key{}, fmt.Errorf("invalid key %q: %w", s, err)
	}
	return k, nil
}

// descend resolves a slash-separated subpath from root and returns the target
// entry's content key. Every traversed segment must be an entry carrying a
// content key (a regular file or a directory).
func descend(objects *packstore.Store, root key.Key, path string) (key.Key, error) {
	k := root
	for seg := range strings.SplitSeq(path, "/") {
		if seg == "" {
			continue
		}
		e, err := fstree.LookupEntry(k, []byte(seg), objects.Get)
		if err != nil {
			return key.Key{}, fmt.Errorf("resolving %q: %w", path, err)
		}
		ck, err := key.Parse(e.ContentKey)
		if err != nil {
			return key.Key{}, fmt.Errorf("resolving %q: %q is not a file or directory", path, seg)
		}
		k = ck
	}
	return k, nil
}
