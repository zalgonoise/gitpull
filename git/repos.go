package git

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

const repoFmt = "https://github.com/%s/%s.git"

// TopLevelDir returns the directory a repository is cloned into: the last element of its URL, e.g. repo.git.
func TopLevelDir(uri string) string {
	if uri == "" {
		return ""
	}

	return path.Base(uri)
}

// formatRepos turns repository names into the org's GitHub URLs. Only the last element of each name is kept, so
// "api", "api.git", "acme/api" and "https://github.com/acme/api.git" are the same repository. Blank names and
// repeated repositories are left out, keeping the original order.
func formatRepos(org string, repos []string) []string {
	formatted := make([]string, 0, len(repos))
	seen := make(map[string]bool, len(repos))

	for _, repo := range repos {
		repo = strings.Trim(strings.TrimSpace(repo), "/")
		repo = strings.TrimSuffix(path.Base(repo), ".git")

		if repo == "" || repo == "." || repo == ".." || seen[repo] {
			continue
		}

		seen[repo] = true
		formatted = append(formatted, fmt.Sprintf(repoFmt, org, repo))
	}

	return formatted
}

// GitFunc is a git operation on one repository: its URL, the directory it lives in and the credentials to use.
type GitFunc func(ctx context.Context, uri, path string, auth *Auth, logger *slog.Logger) error

// failures is the error MultiRepo returns when repositories fail or are skipped: a short summary, with each
// repository's error, and the reason for skipping, still reachable through errors.Is and errors.As.
type failures struct {
	errs    []error
	total   int
	skipped int
	cause   error
}

func (f *failures) Error() string {
	msg := fmt.Sprintf("%d of %d repositories failed", len(f.errs), f.total)
	if f.skipped > 0 {
		msg += fmt.Sprintf(", %d skipped: %v", f.skipped, f.cause)
	}

	return msg
}

func (f *failures) Unwrap() []error {
	if f.cause == nil {
		return f.errs
	}

	return append(slices.Clone(f.errs), f.cause)
}

// MultiRepo runs fn on each repository of org, spreading them across the given number of workers. A failure doesn't
// stop the other repositories: each one is logged as it happens, and all of them are returned at the end.
//
// Once ctx is done, the repositories not started yet are skipped.
func MultiRepo(ctx context.Context, org, baseDir string, repos []string, auth *Auth, workers int, fn GitFunc, logger *slog.Logger) error {
	if org == "" {
		return errors.New("no organization given")
	}

	repos = formatRepos(org, repos)
	if len(repos) == 0 {
		return errors.New("no repositories given")
	}

	workers = max(1, min(workers, len(repos)))

	jobs := make(chan string)

	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		started int
		errs    = make([]error, 0, len(repos))
	)

	for range workers {
		wg.Go(func() {
			for repo := range jobs {
				// A repository handed out just as ctx is done is skipped like the ones never handed out.
				if ctx.Err() != nil {
					continue
				}

				mu.Lock()
				started++
				mu.Unlock()

				if err := fn(ctx, repo, filepath.Join(baseDir, TopLevelDir(repo)), auth, logger); err != nil {
					logger.ErrorContext(ctx, "repository failed",
						slog.String("uri", repo),
						slog.String("error", err.Error()))

					mu.Lock()
					errs = append(errs, fmt.Errorf("%s: %w", repo, err))
					mu.Unlock()
				}
			}
		})
	}

dispatch:
	for _, repo := range repos {
		select {
		case jobs <- repo:
		case <-ctx.Done():
			break dispatch
		}
	}

	close(jobs)
	wg.Wait()

	result := &failures{errs: errs, total: len(repos), skipped: len(repos) - started}
	if result.skipped > 0 {
		result.cause = context.Cause(ctx)
	}

	if len(result.errs) > 0 || result.skipped > 0 {
		return result
	}

	return nil
}
