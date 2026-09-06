package oci

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

type Remote struct {
	Auth authn.Authenticator
}

func NewRemote(auth authn.Authenticator) *Remote {
	if auth == nil {
		auth = authn.Anonymous
	}
	return &Remote{Auth: auth}
}

func (r *Remote) Resolve(ctx context.Context, repository, tag string) (string, error) {
	ref, err := name.ParseReference(fmt.Sprintf("%s:%s", repository, tag))
	if err != nil {
		return "", fmt.Errorf("parse %s:%s: %w", repository, tag, err)
	}
	desc, err := remote.Head(ref, remote.WithContext(ctx), remote.WithAuth(r.Auth))
	if err != nil {
		return "", fmt.Errorf("resolve %s:%s: %w", repository, tag, err)
	}
	return desc.Digest.String(), nil
}
