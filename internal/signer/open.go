package signer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Resolver opens signers from references. References are set only by the server administrator
// (spec 0003, "Signer references"); a resolver never accepts paths or hosts outside its config.
type Resolver struct {
	// FileDir is the only directory file: references resolve in; empty disables them.
	FileDir string
	// AllowFile enables file: references (profile dev, or signers.allow_file).
	AllowFile bool
}

// ErrReference is returned for a reference the resolver will not open.
var ErrReference = errors.New("signer: reference not allowed")

// Open opens the signer a reference names:
//
//	file:<name>  a key file <FileDir>/<name>; <name> is a single path element
//	azurekv:...  Azure Key Vault (spec 0004)
func (r Resolver) Open(_ context.Context, ref string) (Signer, error) {
	scheme, rest, ok := strings.Cut(ref, ":")
	if !ok {
		return nil, fmt.Errorf("%w: %q has no scheme", ErrReference, ref)
	}
	switch scheme {
	case "file":
		if !r.AllowFile || r.FileDir == "" {
			return nil, fmt.Errorf("%w: file signers are disabled (profile dev or signers.allow_file with signers.file_dir)", ErrReference)
		}
		if rest == "" || rest == "." || rest == ".." || strings.ContainsAny(rest, `/\`) || rest != filepath.Base(rest) {
			return nil, fmt.Errorf("%w: a file reference is a single file name inside signers.file_dir", ErrReference)
		}
		return OpenFile(filepath.Join(r.FileDir, rest))
	case "azurekv":
		return nil, fmt.Errorf("%w: azurekv signers come with spec 0004", ErrReference)
	}
	return nil, fmt.Errorf("%w: unknown scheme %q", ErrReference, scheme)
}
