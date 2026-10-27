package git

import (
	"context"
	"errors"
	"log/slog"

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

// Clone clones the repository at uri into path: every branch and tag, then the default branch and its submodules.
//
// A path that already holds a git repository is left as it is.
func Clone(ctx context.Context, uri, path string, auth *Auth, logger *slog.Logger) error {
	return clone(ctx, uri, path, auth, false, logger)
}

// ShallowClone clones only the latest commit of the default branch, without tags.
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

	cloneOpts := &git.CloneOptions{
		URL:               uri,
		RecurseSubmodules: git.DefaultSubmoduleRecursionDepth,
		AllowEmptyRepo:    true,
		ClientOptions:     clientOptions(auth),
	}

	if shallow {
		cloneOpts.Depth = 1
		cloneOpts.SingleBranch = true
		cloneOpts.ShallowSubmodules = true
		cloneOpts.Tags = plumbing.NoTags
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

	logger.InfoContext(ctx, "cloned remote",
		slog.String("uri", uri),
		slog.String("branch", ref.Name().Short()),
		slog.String("commit", ref.Hash().String()))

	return nil
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
