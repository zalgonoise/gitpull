package git

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const githubHost = "github.com"

// ReadCredentials returns the GitHub user and token stored in a git credentials file, in the format written by
// `git config credential.helper store` (one `https://user:token@host` URL per line).
//
// A leading `~/` in path is expanded to the user's home directory.
func ReadCredentials(path string) (*Auth, error) {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}

		path = filepath.Join(home, rest)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	defer f.Close()

	auth, err := parseCredentials(f, githubHost)
	if err != nil {
		return nil, fmt.Errorf("reading credentials from %s: %w", path, err)
	}

	return auth, nil
}

// parseCredentials returns the first https entry for host that has both a user and a password. Like git, it skips
// lines it can't parse.
func parseCredentials(r io.Reader, host string) (*Auth, error) {
	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		u, err := url.Parse(line)
		if err != nil || u.Scheme != "https" || u.Host != host || u.User == nil {
			continue
		}

		password, ok := u.User.Password()
		if !ok || u.User.Username() == "" || password == "" {
			continue
		}

		return &Auth{
			Username: u.User.Username(),
			Token:    password,
		}, nil
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return nil, fmt.Errorf("no credentials found for host %q", host)
}
