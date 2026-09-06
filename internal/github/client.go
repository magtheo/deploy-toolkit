package github

import (
	"fmt"
	"strings"

	"github.com/google/go-github/v68/github"
)

type Source struct {
	client *github.Client
}

func New(token string) *Source {
	return &Source{client: github.NewClient(nil).WithAuthToken(token)}
}

func splitRepo(repo string) (string, string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("repository must be owner/name, got %q", repo)
	}
	return owner, name, nil
}
