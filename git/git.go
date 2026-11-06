package git

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"path"
	"strings"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/plumbing/transport/http"
)

// Auth holds the credentials sent to GitHub over HTTPS, as HTTP basic auth.
type Auth struct {
	Username string
	Token    string
}

// Clone clones the repository at uri into path: every branch and tag, then the default branch and its submodules. A
// tag or submodule that fails is logged and left out, keeping the repository.
//
// A path that already holds a git repository is left as it is.
func Clone(ctx context.Context, uri, path string, auth *Auth, logger *slog.Logger) error {
	return clone(ctx, uri, path, auth, false, logger)
}

// ShallowClone clones only the latest commit of the default branch, without tags, and its submodules the same way.
func ShallowClone(ctx context.Context, uri, path string, auth *Auth, logger *slog.Logger) error {
	return clone(ctx, uri, path, auth, true, logger)
}

func clone(ctx context.Context, uri, path string, auth *Auth, shallow bool, logger *slog.Logger) error {
	if repo, err := git.PlainOpen(path); err == nil {
		_ = repo.Close()

		logger.InfoContext(ctx, "repository already cloned",
			slog.String("uri", uri),
			slog.String("path", path))

		return nil
	}

	logger.InfoContext(ctx, "cloning repository",
		slog.String("uri", uri),
		slog.String("path", path),
		slog.Bool("shallow", shallow))

	// Tags and submodules are fetched after the clone (fetchTags, updateSubmodules): go-git discards the whole clone
	// when one of them fails, while git keeps the repository.
	cloneOpts := &git.CloneOptions{
		URL:            uri,
		AllowEmptyRepo: true,
		Tags:           plumbing.NoTags,
		ClientOptions:  clientOptions(auth),
	}

	if shallow {
		cloneOpts.Depth = 1
		cloneOpts.SingleBranch = true
	}

	// On error, including a cancelled context, go-git removes what it cloned so far.
	repo, err := git.PlainCloneContext(ctx, path, cloneOpts)
	if err != nil {
		return err
	}

	defer func() {
		if err := repo.Close(); err != nil {
			logger.ErrorContext(ctx, "error closing repo in path",
				slog.String("path", path), slog.String("err", err.Error()))
		}
	}()

	ref, err := repo.Head()
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			logger.InfoContext(ctx, "cloned empty repository", slog.String("uri", uri))

			return nil
		}

		return err
	}

	if !shallow {
		if err := fetchTags(ctx, repo, uri, auth, logger); err != nil {
			return err
		}
	}

	if err := updateSubmodules(ctx, repo, uri, auth, shallow, int(git.DefaultSubmoduleRecursionDepth), logger); err != nil {
		return err
	}

	logger.InfoContext(ctx, "cloned remote",
		slog.String("uri", uri),
		slog.String("branch", ref.Name().Short()),
		slog.String("commit", ref.Hash().String()))

	return nil
}

// fetchTags fetches every tag of a fresh clone. A tag that can't be stored is logged and left out, keeping the
// others: on a case-insensitive filesystem (macOS, Windows), two tags whose names differ only by case, such as
// v1.0.0 and V1.0.0, are the same file, which git avoids by packing refs and go-git doesn't.
func fetchTags(ctx context.Context, repo *git.Repository, uri string, auth *Auth, logger *slog.Logger) error {
	// AllTags rather than a refs/tags/* refspec: with a refspec, go-git skips a tag it can't store without saying so.
	err := repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName:    git.DefaultRemoteName,
		Tags:          plumbing.AllTags,
		ClientOptions: clientOptions(auth),
	})

	switch {
	case err == nil, errors.Is(err, git.NoErrAlreadyUpToDate):
		return nil
	case ctx.Err() != nil:
		return err
	case errors.Is(err, git.ErrForceNeeded):
		logger.WarnContext(ctx, "some tags were not stored",
			slog.String("uri", uri),
			slog.String("error", "tags whose names differ only by case can't be stored side by side on this filesystem"))
	default:
		logger.WarnContext(ctx, "tags not fetched",
			slog.String("uri", uri),
			slog.String("error", err.Error()))
	}

	return nil
}

// updateSubmodules clones the submodules of repo, and theirs, up to depth levels. A submodule that fails is logged and
// left out, keeping the repository: it may need SSH keys, or credentials for another host, that gitpull doesn't have.
//
// GitHub SSH URLs (git@github.com:org/repo.git) and relative URLs (../repo.git) are cloned over HTTPS instead, with
// the repository's credentials. Those credentials are only ever sent to github.com.
func updateSubmodules(ctx context.Context, repo *git.Repository, uri string, auth *Auth, shallow bool, depth int, logger *slog.Logger) error {
	if depth <= 0 {
		return nil
	}

	workTree, err := repo.Worktree()
	if err != nil {
		return err
	}

	submodules, err := workTree.Submodules()
	if err != nil {
		return err
	}

	for _, submodule := range submodules {
		cfg := submodule.Config()
		cfg.URL = submoduleURL(uri, cfg.URL)

		opts := &git.SubmoduleUpdateOptions{
			Init:          true,
			ClientOptions: clientOptions(submoduleAuth(cfg.URL, auth)),
		}

		if shallow {
			opts.Depth = 1
		}

		if err := submodule.UpdateContext(ctx, opts); err != nil {
			if ctx.Err() != nil {
				return err
			}

			logger.WarnContext(ctx, "submodule not cloned",
				slog.String("uri", uri),
				slog.String("submodule", cfg.Path),
				slog.String("url", cfg.URL),
				slog.String("error", err.Error()))

			continue
		}

		subRepo, err := submodule.Repository()
		if err != nil {
			return err
		}

		err = updateSubmodules(ctx, subRepo, cfg.URL, auth, shallow, depth-1, logger)
		_ = subRepo.Close()

		if err != nil {
			return err
		}
	}

	return nil
}

const githubURL = "https://github.com/"

// submoduleAuth returns the credentials to send for a submodule's URL: the repository's for GitHub, none for any other
// host, which must never see them.
func submoduleAuth(url string, auth *Auth) *Auth {
	if strings.HasPrefix(url, githubURL) {
		return auth
	}

	return nil
}

// submoduleURL returns the HTTPS URL for a submodule on GitHub, given as SSH or relative to its parent repository's
// URL, as git resolves it. Other URLs are returned as they are.
func submoduleURL(parent, submodule string) string {
	for _, prefix := range []string{"git@github.com:", "ssh://git@github.com/"} {
		if rest, ok := strings.CutPrefix(submodule, prefix); ok {
			return githubURL + rest
		}
	}

	if strings.HasPrefix(submodule, "./") || strings.HasPrefix(submodule, "../") {
		if u, err := url.Parse(parent); err == nil && u.Scheme != "" {
			u.Path = path.Join(u.Path, submodule)

			return u.String()
		}
	}

	return submodule
}

// Pull fast-forwards the checked-out branch of the repository in path from its origin remote. A repository that is
// already up to date is not an error.
func Pull(ctx context.Context, uri, path string, auth *Auth, logger *slog.Logger) error {
	repo, err := git.PlainOpen(path)
	if err != nil {
		return err
	}

	defer func() {
		if err := repo.Close(); err != nil {
			logger.ErrorContext(ctx, "error closing repo in path",
				slog.String("path", path), slog.String("err", err.Error()))
		}
	}()

	workTree, err := repo.Worktree()
	if err != nil {
		return err
	}

	before, err := headHash(repo)
	if err != nil {
		return err
	}

	if err := workTree.PullContext(ctx, &git.PullOptions{
		RemoteName:    "origin",
		ClientOptions: clientOptions(auth),
	}); err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) && !errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return err
	}

	after, err := headHash(repo)
	if err != nil {
		return err
	}

	// go-git doesn't return NoErrAlreadyUpToDate for shallow clones, so compare the commits instead.
	if after == before {
		logger.InfoContext(ctx, "repository already up-to-date", slog.String("uri", uri))

		return nil
	}

	ref, err := repo.Head()
	if err != nil {
		return err
	}

	logger.InfoContext(ctx, "pulled from remote",
		slog.String("uri", uri),
		slog.String("branch", ref.Name().Short()),
		slog.String("commit", ref.Hash().String()))

	return nil
}

// PullOrClone returns a GitFunc that pulls a repository like Pull, and clones it with clone when its path doesn't hold
// a git repository yet, e.g. a repository added to the list since the last clone. A path that holds something else is
// still an error, from clone, and is left untouched.
func PullOrClone(clone GitFunc) GitFunc {
	return func(ctx context.Context, uri, path string, auth *Auth, logger *slog.Logger) error {
		err := Pull(ctx, uri, path, auth, logger)
		if !errors.Is(err, git.ErrRepositoryNotExists) {
			return err
		}

		logger.InfoContext(ctx, "repository not cloned yet",
			slog.String("uri", uri),
			slog.String("path", path))

		return clone(ctx, uri, path, auth, logger)
	}
}

// headHash returns the commit HEAD points to, or the zero hash for a repository without commits.
func headHash(repo *git.Repository) (plumbing.Hash, error) {
	ref, err := repo.Head()
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return plumbing.ZeroHash, nil
		}

		return plumbing.ZeroHash, err
	}

	return ref.Hash(), nil
}

func clientOptions(auth *Auth) []client.Option {
	if auth == nil {
		return nil
	}

	return []client.Option{
		client.WithHTTPAuth(&http.BasicAuth{
			Username: auth.Username,
			Password: auth.Token,
		}),
	}
}
