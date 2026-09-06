package github

import (
	"context"
	"fmt"

	"github.com/google/go-github/v68/github"
	"github.com/magtheo/deploy-toolkit/internal/release"
)

func (s *Source) CheckRuns(ctx context.Context, repo, ref string) ([]release.CheckRun, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	var out []release.CheckRun
	filter := "all"
	opts := &github.ListCheckRunsOptions{
		Filter: &filter,
		ListOptions: github.ListOptions{
			PerPage: 100,
		},
	}
	for {
		res, resp, err := s.client.Checks.ListCheckRunsForRef(ctx, owner, name, ref, opts)
		if err != nil {
			return nil, fmt.Errorf("list check runs for %s @ %s: %w", repo, ref, err)
		}
		for _, cr := range res.CheckRuns {
			out = append(out, release.CheckRun{
				Name:       cr.GetName(),
				Status:     cr.GetStatus(),
				Conclusion: cr.GetConclusion(),
				AppID:      cr.GetApp().GetID(),
				SuiteID:    cr.GetCheckSuite().GetID(),
			})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}
