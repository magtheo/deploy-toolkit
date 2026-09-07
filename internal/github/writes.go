package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/google/go-github/v68/github"

	"github.com/magtheo/deploy-toolkit/internal/promotion"
	"github.com/magtheo/deploy-toolkit/internal/release"
)

func (s *Source) HeadTree(ctx context.Context, repo, commitSHA string) (string, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return "", err
	}
	c, _, err := s.client.Git.GetCommit(ctx, owner, name, commitSHA)
	if err != nil {
		return "", fmt.Errorf("read commit %s in %s: %w", commitSHA, repo, err)
	}
	return c.GetTree().GetSHA(), nil
}

func (s *Source) CreateBlob(ctx context.Context, repo string, content []byte) (string, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return "", err
	}
	b, _, err := s.client.Git.CreateBlob(ctx, owner, name, &github.Blob{
		Content:  github.Ptr(base64.StdEncoding.EncodeToString(content)),
		Encoding: github.Ptr("base64"),
	})
	if err != nil {
		return "", fmt.Errorf("create blob in %s: %w", repo, err)
	}
	return b.GetSHA(), nil
}

func (s *Source) CreateTree(ctx context.Context, repo, baseTree string, entries []promotion.TreeEntry) (string, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return "", err
	}
	treeEntries := make([]*github.TreeEntry, 0, len(entries))
	blobSHAs := make([]string, 0, len(entries))
	for _, e := range entries {
		sha, err := s.CreateBlob(ctx, repo, e.Content)
		if err != nil {
			return "", err
		}
		blobSHAs = append(blobSHAs, sha)
	}
	for i, e := range entries {
		treeEntries = append(treeEntries, &github.TreeEntry{
			Path: github.Ptr(e.Path),
			Mode: github.Ptr("100644"),
			Type: github.Ptr("blob"),
			SHA:  github.Ptr(blobSHAs[i]),
		})
	}
	t, _, err := s.client.Git.CreateTree(ctx, owner, name, baseTree, treeEntries)
	if err != nil {
		return "", fmt.Errorf("create tree in %s: %w", repo, err)
	}
	return t.GetSHA(), nil
}

func (s *Source) CreateCommit(ctx context.Context, repo, message, treeSHA string, parents []string) (string, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return "", err
	}
	commit := &github.Commit{
		Message: github.Ptr(message),
		Tree:    &github.Tree{SHA: github.Ptr(treeSHA)},
	}
	for _, p := range parents {
		commit.Parents = append(commit.Parents, &github.Commit{SHA: github.Ptr(p)})
	}
	c, _, err := s.client.Git.CreateCommit(ctx, owner, name, commit, nil)
	if err != nil {
		return "", fmt.Errorf("create commit in %s: %w", repo, err)
	}
	return c.GetSHA(), nil
}

func (s *Source) BranchExists(ctx context.Context, repo, branch string) (bool, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return false, err
	}
	_, _, err = s.client.Git.GetRef(ctx, owner, name, "heads/"+branch)
	if err == nil {
		return true, nil
	}
	if _, ok := err.(*github.ErrorResponse); ok {
		return false, nil
	}
	return false, fmt.Errorf("resolve branch %s in %s: %w", branch, repo, err)
}

func (s *Source) CreateBranch(ctx context.Context, repo, branch, sha string) error {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return err
	}
	_, _, err = s.client.Git.CreateRef(ctx, owner, name, &github.Reference{
		Ref:    github.Ptr("refs/heads/" + branch),
		Object: &github.GitObject{SHA: github.Ptr(sha), Type: github.Ptr("commit")},
	})
	if err != nil {
		return fmt.Errorf("create branch %s in %s: %w", branch, repo, err)
	}
	return nil
}

func (s *Source) OpenPRForBranch(ctx context.Context, repo, branch string) (*promotion.PullRequest, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	prs, _, err := s.client.PullRequests.List(ctx, owner, name, &github.PullRequestListOptions{
		State: "open",
		Base:  release.TrustedBranch,
		Head:  owner + ":" + branch,
	})
	if err != nil {
		return nil, fmt.Errorf("list open PRs for %s in %s: %w", branch, repo, err)
	}
	if len(prs) == 0 {
		return nil, nil
	}
	return toPR(prs[0]), nil
}

func (s *Source) OpenPromotionPRs(ctx context.Context, repo, env string) ([]promotion.PullRequest, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	prefix := fmt.Sprintf(promotion.BranchPrefixFmt, env)
	var out []promotion.PullRequest
	opts := &github.PullRequestListOptions{State: "open", Base: release.TrustedBranch, ListOptions: github.ListOptions{PerPage: 100}}
	for {
		prs, resp, err := s.client.PullRequests.List(ctx, owner, name, opts)
		if err != nil {
			return nil, fmt.Errorf("list open PRs in %s: %w", repo, err)
		}
		for _, pr := range prs {
			if strings.HasPrefix(pr.GetHead().GetRef(), prefix) {
				out = append(out, *toPR(pr))
			}
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

func (s *Source) CreatePR(ctx context.Context, repo, base, head, title, body string) (*promotion.PullRequest, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	pr, _, err := s.client.PullRequests.Create(ctx, owner, name, &github.NewPullRequest{
		Title: github.Ptr(title),
		Head:  github.Ptr(head),
		Base:  github.Ptr(base),
		Body:  github.Ptr(body),
	})
	if err != nil {
		return nil, fmt.Errorf("create PR in %s: %w", repo, err)
	}
	return toPR(pr), nil
}

func (s *Source) BlobAt(ctx context.Context, repo, blobSHA string) ([]byte, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	b, _, err := s.client.Git.GetBlob(ctx, owner, name, blobSHA)
	if err != nil {
		return nil, fmt.Errorf("read blob %s in %s: %w", blobSHA, repo, err)
	}
	data, err := base64.StdEncoding.DecodeString(b.GetContent())
	if err != nil {
		return nil, fmt.Errorf("decode blob %s: %w", blobSHA, err)
	}
	return data, nil
}

func toPR(pr *github.PullRequest) *promotion.PullRequest {
	return &promotion.PullRequest{
		Number:  pr.GetNumber(),
		URL:     pr.GetHTMLURL(),
		HeadRef: pr.GetHead().GetRef(),
		BaseRef: pr.GetBase().GetRef(),
		HeadSHA: pr.GetHead().GetSHA(),
	}
}

func (s *Source) CommitParents(ctx context.Context, repo, sha string) ([]string, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	c, _, err := s.client.Git.GetCommit(ctx, owner, name, sha)
	if err != nil {
		return nil, fmt.Errorf("read commit %s in %s: %w", sha, repo, err)
	}
	parents := make([]string, 0, len(c.Parents))
	for _, p := range c.Parents {
		parents = append(parents, p.GetSHA())
	}
	return parents, nil
}

func (s *Source) CommitTreePaths(ctx context.Context, repo, sha string) (map[string]string, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	c, _, err := s.client.Git.GetCommit(ctx, owner, name, sha)
	if err != nil {
		return nil, fmt.Errorf("read commit %s in %s: %w", sha, repo, err)
	}
	out := make(map[string]string)
	if err := s.walkTree(ctx, owner, name, c.GetTree().GetSHA(), "", out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Source) walkTree(ctx context.Context, owner, name, treeSHA, prefix string, out map[string]string) error {
	t, _, err := s.client.Git.GetTree(ctx, owner, name, treeSHA, true)
	if err != nil {
		return fmt.Errorf("read tree %s: %w", treeSHA, err)
	}
	if !t.GetTruncated() {
		for _, e := range t.Entries {
			p := e.GetPath()
			if prefix != "" {
				p = prefix + "/" + p
			}
			if e.GetType() == "blob" {
				out[p] = e.GetSHA()
			}
		}
		return nil
	}
	t, _, err = s.client.Git.GetTree(ctx, owner, name, treeSHA, false)
	if err != nil {
		return fmt.Errorf("read tree %s: %w", treeSHA, err)
	}
	for _, e := range t.Entries {
		p := e.GetPath()
		if prefix != "" {
			p = prefix + "/" + p
		}
		switch e.GetType() {
		case "blob":
			out[p] = e.GetSHA()
		case "tree":
			if err := s.walkTree(ctx, owner, name, e.GetSHA(), p, out); err != nil {
				return err
			}
		}
	}
	return nil
}
