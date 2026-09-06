package manifest

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	TransportSSH   = "ssh"
	TransportLocal = "local"
)

var ociRepositoryPattern = regexp.MustCompile(
	`^((?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9])(?:\.(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9]))*(?::[0-9]+)?/)?[a-z0-9]+(?:(?:[._]|__|[-]*)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|[-]*)[a-z0-9]+)*)*$`,
)

var bundleIncludePattern = regexp.MustCompile(
	`^[A-Za-z0-9_][A-Za-z0-9_.-]*(/([A-Za-z0-9_.-]+|\*{1,2}))*$`,
)

func checkOCIRepository(name string) error {
	if !ociRepositoryPattern.MatchString(name) {
		return fmt.Errorf("%q is not a valid OCI repository name", name)
	}
	last := name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		last = name[i+1:]
	}
	if strings.ContainsAny(last, ":@") {
		return fmt.Errorf("%q carries a tag or digest; repository fields must be untagged — releases pin the digest separately", name)
	}
	return nil
}

func checkBundleInclude(p string) error {
	if !bundleIncludePattern.MatchString(p) {
		return fmt.Errorf("%q is not a valid repo-root-relative bundle include (no leading '/', no '\\', glob wildcards only as whole '*' or '**' segments)", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("path traversal is not allowed, got %q", p)
		}
	}
	return nil
}
