package workspace

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

func Normalize(remoteURL string) string {
	s := strings.TrimSpace(remoteURL)

	if strings.HasPrefix(s, "ssh://") {
		s = strings.TrimPrefix(s, "ssh://")
		s = regexp.MustCompile(`^git@`).ReplaceAllString(s, "")
	}

	if strings.HasPrefix(s, "git@") {
		s = strings.TrimPrefix(s, "git@")
		s = strings.Replace(s, ":", "/", 1)
	}

	s = regexp.MustCompile(`^https?://`).ReplaceAllString(s, "")

	s = strings.TrimRight(s, "/")

	s = strings.TrimSuffix(s, ".git")

	return strings.ToLower(s)
}

func Resolve(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("resolving workspace from git remote in %s: %w", dir, err)
	}
	id := Normalize(string(out))
	if id == "" {
		return "", fmt.Errorf("empty workspace ID after normalizing remote URL")
	}
	return id, nil
}
