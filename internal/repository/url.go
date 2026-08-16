package repository

import (
	"errors"
	"net/url"
	"strings"
)

type gitHost struct {
	httpsHostname string
	sshHostname   string
	sshUser       string
	sshPort       string
}

type gitOriginConversion func(string) (string, bool)

var gitOriginConversions = func() []gitOriginConversion {
	host := gitHost{
		httpsHostname: "github.com",
		sshHostname:   "ssh.github.com",
		sshUser:       "git",
		sshPort:       "443",
	}
	return []gitOriginConversion{
		shorthandToGitOrigin(host),
		httpsURLToGitOrigin(host),
		sshURLAsGitOrigin,
		scpReferenceAsGitOrigin,
	}
}()

func GitOrigin(reference string) (string, error) {
	reference = strings.TrimSpace(reference)
	for _, convert := range gitOriginConversions {
		if origin, ok := convert(reference); ok {
			return origin, nil
		}
	}
	return "", errors.New("repo must be owner/name, a supported HTTPS URL, or an SSH Git reference")
}

func shorthandToGitOrigin(host gitHost) gitOriginConversion {
	return func(reference string) (string, bool) {
		if strings.Contains(reference, "://") || strings.Contains(reference, ":") {
			return "", false
		}
		owner, name, ok := splitRepositoryPath(reference)
		return host.sshURL(owner, name), ok
	}
}

func httpsURLToGitOrigin(host gitHost) gitOriginConversion {
	return func(reference string) (string, bool) {
		u, err := url.Parse(reference)
		if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, host.httpsHostname) {
			return "", false
		}
		owner, name, ok := splitRepositoryPath(strings.TrimPrefix(u.Path, "/"))
		return host.sshURL(owner, name), ok
	}
}

func sshURLAsGitOrigin(reference string) (string, bool) {
	u, err := url.Parse(reference)
	return reference, err == nil && u.Scheme == "ssh" && u.Host != ""
}

func scpReferenceAsGitOrigin(reference string) (string, bool) {
	if strings.Contains(reference, "://") || strings.HasPrefix(reference, "/") {
		return "", false
	}
	host, path, ok := strings.Cut(reference, ":")
	return reference, ok && host != "" && path != "" && !strings.Contains(host, "/")
}

func (host gitHost) sshURL(owner, name string) string {
	return "ssh://" + host.sshUser + "@" + host.sshHostname + ":" + host.sshPort + "/" + owner + "/" + name + ".git"
}

func splitRepositoryPath(path string) (string, string, bool) {
	owner, name, ok := strings.Cut(path, "/")
	name = strings.TrimSuffix(name, ".git")
	return owner, name, ok && owner != "" && name != "" && !strings.Contains(name, "/")
}
