package github

import (
	"context"
	"fmt"

	"github.com/google/go-github/v68/github"
)

func (s *Source) BranchHead(ctx context.Context, repo, branch string) (string, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return "", err
	}
	b, _, err := s.client.Repositories.GetBranch(ctx, owner, name, branch, 0)
	if err != nil {
		return "", fmt.Errorf("resolve branch head %s@%s: %w", repo, branch, err)
	}
	return b.GetCommit().GetSHA(), nil
}

func (s *Source) VerifyCommit(ctx context.Context, repo, sha string) error {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return err
	}
	if _, _, err := s.client.Repositories.GetCommit(ctx, owner, name, sha, nil); err != nil {
		return fmt.Errorf("revision %s not found in %s: %w", sha, repo, err)
	}
	return nil
}

func (s *Source) IsAncestor(ctx context.Context, repo, ancestor, descendant string) (bool, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return false, err
	}
	cmp, _, err := s.client.Repositories.CompareCommits(ctx, owner, name, ancestor, descendant, nil)
	if err != nil {
		return false, fmt.Errorf("compare %s...%s in %s: %w", ancestor, descendant, repo, err)
	}
	switch cmp.GetStatus() {
	case "ahead", "identical":
		return true, nil
	default:
		return false, nil
	}
}

func (s *Source) FileAt(ctx context.Context, repo, path, ref string) ([]byte, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	fc, _, _, err := s.client.Repositories.GetContents(ctx, owner, name, path, &github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		return nil, fmt.Errorf("read %s @ %s in %s: %w", path, ref, repo, err)
	}
	if fc == nil || fc.GetType() != "file" {
		return nil, fmt.Errorf("%s @ %s in %s is not a file", path, ref, repo)
	}
	data, err := fc.GetContent()
	if err != nil {
		return nil, fmt.Errorf("decode %s @ %s in %s: %w", path, ref, repo, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%s @ %s in %s is empty or exceeds the contents API size limit", path, ref, repo)
	}
	return []byte(data), nil
}
