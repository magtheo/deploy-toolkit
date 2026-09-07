package oci

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

type Remote struct {
	Keychain authn.Keychain
}

func NewRemote(kc authn.Keychain) *Remote {
	if kc == nil {
		kc = authn.DefaultKeychain
	}
	return &Remote{Keychain: kc}
}

func (r *Remote) Resolve(ctx context.Context, repository, tag string) (string, error) {
	ref, err := name.ParseReference(fmt.Sprintf("%s:%s", repository, tag))
	if err != nil {
		return "", fmt.Errorf("parse %s:%s: %w", repository, tag, err)
	}
	desc, err := remote.Head(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(r.Keychain))
	if err != nil {
		return "", fmt.Errorf("resolve %s:%s: %w", repository, tag, err)
	}
	return desc.Digest.String(), nil
}
