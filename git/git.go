package git

import (
	"context"
	"errors"
	"iter"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/filemode"
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

	submodules := submoduleOptions{shallow: shallow, missing: true}
	if err := updateSubmodules(ctx, repo, uri, auth, submodules, submoduleDepth, logger); err != nil {
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

// submoduleOptions say how updateSubmodules treats a repository's submodules.
type submoduleOptions struct {
	// shallow fetches only the commit a submodule is pinned to.
	shallow bool
	// missing also clones the submodules that aren't cloned yet. A pull only does when the repository changed, so a
	// submodule that can't be cloned isn't tried again, and warned about, on every run.
	missing bool
	// force discards the changes inside a submodule that stop it from moving to its pinned commit.
	force bool
}

// updateSubmodules brings the submodules of repo, and theirs, up to depth levels, to the commits their repository
// pins them to, cloning them first when opts.missing. One already there is left alone, without a fetch. A submodule
// that fails is logged and left as it is, keeping the repository: it may need SSH keys, or credentials for another
// host, that gitpull doesn't have.
//
// GitHub SSH URLs (git@github.com:org/repo.git) and relative URLs (../repo.git) are cloned over HTTPS instead, with
// the repository's credentials. Those credentials are only ever sent to github.com.
func updateSubmodules(ctx context.Context, repo *git.Repository, uri string, auth *Auth, opts submoduleOptions, depth int, logger *slog.Logger) error {
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

		status, err := submodule.Status()

		switch {
		case err == nil && status.Current.IsZero() && !opts.missing:
			continue
		case err == nil && status.IsClean():
		default:
			var current plumbing.Hash
			if err == nil {
				current = status.Current
			}

			forced, err := updateSubmodule(ctx, submodule, current, auth, opts)
			if err != nil {
				if ctx.Err() != nil {
					return err
				}

				msg := "submodule not cloned"
				if !current.IsZero() {
					msg = "submodule not updated"
				}

				logger.WarnContext(ctx, msg,
					slog.String("uri", uri),
					slog.String("submodule", cfg.Path),
					slog.String("url", cfg.URL),
					slog.String("error", err.Error()))

				continue
			}

			if forced {
				logger.WarnContext(ctx, "local changes discarded",
					slog.String("uri", uri),
					slog.String("submodule", cfg.Path))
			}
		}

		subRepo, err := submodule.Repository()
		if err != nil {
			return err
		}

		err = updateSubmodules(ctx, subRepo, cfg.URL, auth, opts, depth-1, logger)
		_ = subRepo.Close()

		if err != nil {
			return err
		}
	}

	return nil
}

// updateSubmodule moves a submodule from the commit current to the one its repository pins it to, cloning it first
// when it isn't yet. Changes inside the submodule that stop the move leave it as it was, or, with opts.force, are
// discarded, which forced reports.
func updateSubmodule(ctx context.Context, submodule *git.Submodule, current plumbing.Hash, auth *Auth, opts submoduleOptions) (forced bool, err error) {
	update := &git.SubmoduleUpdateOptions{
		Init:          true,
		ClientOptions: clientOptions(submoduleAuth(submodule.Config().URL, auth)),
	}

	if opts.shallow {
		update.Depth = 1
	}

	updateErr := submodule.UpdateContext(ctx, update)
	if !errors.Is(updateErr, git.ErrUnstagedChanges) || current.IsZero() {
		return false, updateErr
	}

	// The pinned commit is fetched, and go-git has pointed the submodule at it before finding the changes in the way
	// of its files. Put it back: left there, the submodule would pass for updated, with its files behind.
	subRepo, err := submodule.Repository()
	if err != nil {
		return false, errors.Join(updateErr, err)
	}

	defer subRepo.Close()

	if err := setHead(subRepo, current); err != nil {
		return false, errors.Join(updateErr, err)
	}

	if !opts.force {
		return false, updateErr
	}

	status, err := submodule.Status()
	if err != nil {
		return false, err
	}

	subTree, err := subRepo.Worktree()
	if err != nil {
		return false, err
	}

	return true, subTree.Reset(&git.ResetOptions{Mode: git.HardReset, Commit: status.Expected})
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

// submoduleDepth is how many levels of submodules within submodules are followed.
const submoduleDepth = int(git.DefaultSubmoduleRecursionDepth)

// Pull fast-forwards the checked-out branch of the repository in path from its origin remote, and brings its
// submodules to the commits it pins them to. A repository that is already up to date is not an error.
//
// Changes to tracked files, and commits the remote doesn't have, stop the pull when they are in its way: that is an
// error (see BlockedByLocalChanges), and the repository is left as it was. ResetPull discards them instead.
func Pull(ctx context.Context, uri, path string, auth *Auth, logger *slog.Logger) error {
	return pull(ctx, uri, path, auth, false, logger)
}

// ResetPull pulls like Pull, and makes the repository match its remote whatever was done to it: changes to tracked
// files, and commits the remote doesn't have, are discarded with a warning, also when there is nothing new to pull,
// and so are the changes inside a submodule that keep it from its pinned commit. Untracked files are kept, as git
// reset --hard keeps them.
func ResetPull(ctx context.Context, uri, path string, auth *Auth, logger *slog.Logger) error {
	return pull(ctx, uri, path, auth, true, logger)
}

// BlockedByLocalChanges reports whether err, from Pull or from a MultiRepo that ran it, holds a pull that local
// changes or local commits stopped: what ResetPull discards.
func BlockedByLocalChanges(err error) bool {
	return errors.Is(err, git.ErrUnstagedChanges) || errors.Is(err, git.ErrNonFastForwardUpdate)
}

func pull(ctx context.Context, uri, path string, auth *Auth, reset bool, logger *slog.Logger) error {
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

	shallow, err := repo.Storer.Shallow()
	if err != nil {
		return err
	}

	// A submodule that an earlier pull left behind its pinned commit counts as a local change, and would stop this
	// pull.
	submodules := submoduleOptions{shallow: len(shallow) > 0, force: reset}
	if err := updateSubmodules(ctx, repo, uri, auth, submodules, submoduleDepth, logger); err != nil {
		return err
	}

	pullErr := workTree.PullContext(ctx, &git.PullOptions{
		RemoteName:    git.DefaultRemoteName,
		ClientOptions: clientOptions(auth),
	})

	// tip is the remote's commit, when the pull didn't leave the branch at it; ahead says the branch may hold
	// commits of its own that tip doesn't have.
	var (
		tip   plumbing.Hash
		ahead bool
	)

	// In a shallow clone go-git can't tell a fast-forward, and moves the branch to the remote's commit whatever
	// commits of its own it has. Tell for it, and put the branch back where it was.
	if pullErr == nil && len(shallow) > 0 && !before.IsZero() {
		if tip, err = headHash(repo); err != nil {
			return err
		}

		switch {
		case isAncestor(repo, before, tip):
			tip = plumbing.ZeroHash
		case isAncestor(repo, tip, before):
			pullErr = git.NoErrAlreadyUpToDate
		default:
			pullErr = git.ErrNonFastForwardUpdate
		}

		if pullErr != nil {
			if err := workTree.Reset(&git.ResetOptions{Mode: git.HardReset, Commit: before}); err != nil {
				return err
			}
		}
	}

	switch {
	case pullErr == nil, errors.Is(pullErr, transport.ErrEmptyRemoteRepository):
	case errors.Is(pullErr, git.NoErrAlreadyUpToDate):
		// Up to date, or ahead of the remote by commits of its own.
		if reset && !before.IsZero() && !atRemoteBranch(repo, before) {
			ahead = true

			if tip.IsZero() {
				if tip, err = remoteHead(ctx, repo, auth); err != nil {
					return err
				}
			}
		}
	case errors.Is(pullErr, git.ErrUnstagedChanges):
		// go-git moves the branch to the pulled commit before it looks for local changes. Left there, the next pull
		// would find the branch up to date, and the files would stay behind it without a word.
		if tip, err = headHash(repo); err != nil {
			return err
		}

		if err := setHead(repo, before); err != nil {
			return errors.Join(pullErr, err)
		}

		if !reset {
			// Not everything go-git finds changed is a change (see unreliable): when nothing else is, nothing of
			// anyone's is in the way, and the pull goes through.
			changed, err := localChanges(repo, workTree)
			if err != nil {
				return err
			}

			if len(changed) > 0 {
				return pullErr
			}

			if err := workTree.Reset(&git.ResetOptions{Mode: git.HardReset, Commit: tip}); err != nil {
				return err
			}
		}
	case errors.Is(pullErr, git.ErrNonFastForwardUpdate) && reset:
		ahead = true

		if tip.IsZero() {
			if tip, err = remoteHead(ctx, repo, auth); err != nil {
				return err
			}
		}
	default:
		return pullErr
	}

	if reset {
		if err := resetTo(ctx, repo, workTree, uri, tip, ahead, logger); err != nil {
			return err
		}
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

	// The pulled commits may pin submodules elsewhere, or add some.
	submodules.missing = true
	if err := updateSubmodules(ctx, repo, uri, auth, submodules, submoduleDepth, logger); err != nil {
		return err
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

// resetTo makes the repository match the commit tip, or the one it is at when tip is zero: the checked-out branch
// at that commit, and every tracked file as the commit has it. It warns about what that discards: the files changed,
// and, when ahead, the commit the branch was at, whose own commits tip doesn't have. It also says when files still
// differ afterwards, which a reset can't help: on a filesystem that ignores case, two files whose names differ only
// by case are one.
func resetTo(ctx context.Context, repo *git.Repository, workTree *git.Worktree, uri string, tip plumbing.Hash, ahead bool, logger *slog.Logger) error {
	head, err := headHash(repo)
	if err != nil || head.IsZero() {
		return err
	}

	if tip.IsZero() {
		tip = head
	}

	changed, err := localChanges(repo, workTree)
	if err != nil {
		return err
	}

	if len(changed) == 0 && head == tip {
		return nil
	}

	if err := workTree.Reset(&git.ResetOptions{Mode: git.HardReset, Commit: tip}); err != nil {
		return err
	}

	var left []string

	if len(changed) > 0 {
		if left, err = localChanges(repo, workTree); err != nil {
			return err
		}
	}

	discarded := 0

	for _, file := range changed {
		if !slices.Contains(left, file) {
			discarded++
		}
	}

	lost := ahead && head != tip

	if discarded > 0 || lost {
		attrs := []any{slog.String("uri", uri), slog.Int("files", discarded)}
		if lost {
			// The commit is still in the repository: git branch <name> <commit> brings it back.
			attrs = append(attrs, slog.String("commit", head.String()))
		}

		logger.WarnContext(ctx, "local changes discarded", attrs...)
	}

	if len(left) > 0 {
		logger.DebugContext(ctx, "files still differ after reset",
			slog.String("uri", uri),
			slog.Int("files", len(left)))
	}

	return nil
}

// localChanges lists the tracked files that differ from the checked-out commit, staged or not, leaving out those
// the folder can't hold as the repository has them (see unreliable).
func localChanges(repo *git.Repository, workTree *git.Worktree) ([]string, error) {
	status, err := workTree.Status()
	if err != nil {
		return nil, err
	}

	changed := func(code git.StatusCode) bool {
		return code != git.Unmodified && code != git.Untracked
	}

	var files []string

	for file, s := range status {
		if changed(s.Worktree) || changed(s.Staging) {
			files = append(files, file)
		}
	}

	if len(files) == 0 {
		return nil, nil
	}

	skip, err := unreliable(repo, workTree)
	if err != nil {
		return nil, err
	}

	return slices.DeleteFunc(files, skip), nil
}

// unreliable returns a function that tells the tracked paths whose state in the folder says nothing about local
// changes, because the folder can't hold them as the repository has them. go-git reports them as changed however
// often they are checked out:
//
//   - a path that differs only by case from another, in its name or in a folder's on the way, on a filesystem that
//     ignores case (macOS, Windows): the two are one file or folder there;
//   - a submodule entry that .gitmodules doesn't describe, which is checked out as an empty folder.
func unreliable(repo *git.Repository, workTree *git.Worktree) (func(path string) bool, error) {
	idx, err := repo.Storer.Index()
	if err != nil {
		return nil, err
	}

	submodules, err := workTree.Submodules()
	if err != nil {
		return nil, err
	}

	described := make(map[string]bool, len(submodules))
	for _, submodule := range submodules {
		described[submodule.Config().Path] = true
	}

	undescribed := make(map[string]bool)
	spelling := make(map[string]string) // a path or folder in lower case, to how the repository spells it
	clashing := make(map[string]bool)
	caseless := ignoresCase(workTree.Filesystem().Root())

	for _, entry := range idx.Entries {
		if entry.Mode == filemode.Submodule && !described[entry.Name] {
			undescribed[entry.Name] = true
		}

		if !caseless {
			continue
		}

		for prefix := range prefixes(entry.Name) {
			lower := strings.ToLower(prefix)

			switch spelled, ok := spelling[lower]; {
			case !ok:
				spelling[lower] = prefix
			case spelled != prefix:
				clashing[lower] = true
			}
		}
	}

	return func(name string) bool {
		if undescribed[name] {
			return true
		}

		for prefix := range prefixes(name) {
			if clashing[strings.ToLower(prefix)] {
				return true
			}
		}

		return false
	}, nil
}

// prefixes yields each folder on the way to name, then name itself: "a", "a/b" and "a/b/c.txt" for "a/b/c.txt".
func prefixes(name string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for idx, char := range name {
			if char == '/' && !yield(name[:idx]) {
				return
			}
		}

		yield(name)
	}
}

// ignoresCase reports whether the filesystem holding the repository at root ignores case in names.
func ignoresCase(root string) bool {
	lower, err := os.Stat(filepath.Join(root, git.GitDirName))
	if err != nil {
		return false
	}

	upper, err := os.Stat(filepath.Join(root, strings.ToUpper(git.GitDirName)))

	return err == nil && os.SameFile(lower, upper)
}

// atRemoteBranch reports whether the checked-out branch is at hash on the remote too, as of the last fetch.
func atRemoteBranch(repo *git.Repository, hash plumbing.Hash) bool {
	head, err := repo.Storer.Reference(plumbing.HEAD)
	if err != nil || head.Type() != plumbing.SymbolicReference {
		return false
	}

	remote, err := repo.Reference(plumbing.NewRemoteReferenceName(git.DefaultRemoteName, head.Target().Short()), true)

	return err == nil && remote.Hash() == hash
}

// remoteHead asks the origin remote for the commit its default branch is at.
func remoteHead(ctx context.Context, repo *git.Repository, auth *Auth) (plumbing.Hash, error) {
	remote, err := repo.Remote(git.DefaultRemoteName)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	refs, err := remote.ListContext(ctx, &git.ListOptions{ClientOptions: clientOptions(auth)})
	if err != nil {
		return plumbing.ZeroHash, err
	}

	byName := make(map[plumbing.ReferenceName]*plumbing.Reference, len(refs))
	for _, ref := range refs {
		byName[ref.Name()] = ref
	}

	ref := byName[plumbing.HEAD]
	for range len(refs) {
		if ref == nil || ref.Type() != plumbing.SymbolicReference {
			break
		}

		ref = byName[ref.Target()]
	}

	if ref == nil || ref.Type() != plumbing.HashReference {
		return plumbing.ZeroHash, errors.New("the remote doesn't say which commit its default branch is at")
	}

	return ref.Hash(), nil
}

// isAncestor reports whether the commit ancestor is in the history of the commit descendant, or is that commit, as
// far as the repository holds that history: a shallow clone's ends where it was cut.
func isAncestor(repo *git.Repository, ancestor, descendant plumbing.Hash) bool {
	seen := map[plumbing.Hash]bool{descendant: true}

	for queue := []plumbing.Hash{descendant}; len(queue) > 0; queue = queue[1:] {
		if queue[0] == ancestor {
			return true
		}

		commit, err := repo.CommitObject(queue[0])
		if err != nil {
			continue // past where the history was cut
		}

		for _, parent := range commit.ParentHashes {
			if !seen[parent] {
				seen[parent] = true
				queue = append(queue, parent)
			}
		}
	}

	return false
}

// setHead points the checked-out branch, or a detached HEAD, at hash.
func setHead(repo *git.Repository, hash plumbing.Hash) error {
	head, err := repo.Storer.Reference(plumbing.HEAD)
	if err != nil {
		return err
	}

	name := plumbing.HEAD
	if head.Type() == plumbing.SymbolicReference {
		name = head.Target()
	}

	return repo.Storer.SetReference(plumbing.NewHashReference(name, hash))
}

// PullOrClone returns a GitFunc that pulls a repository with pull (Pull or ResetPull), and clones it with clone when
// its path doesn't hold a git repository yet, e.g. a repository added to the list since the last clone. A path that
// holds something else is still an error, from clone, and is left untouched.
func PullOrClone(pull, clone GitFunc) GitFunc {
	return func(ctx context.Context, uri, path string, auth *Auth, logger *slog.Logger) error {
		err := pull(ctx, uri, path, auth, logger)
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
