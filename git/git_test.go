package git

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

// newSourceRepo creates a local repository with the given number of empty commits and a tag on the last one,
// and returns its file:// URL.
func newSourceRepo(t *testing.T, commits int) (string, *git.Repository) {
	t.Helper()

	dir := t.TempDir()

	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err)

	var hash plumbing.Hash
	for idx := range commits {
		hash = addCommit(t, repo, fmt.Sprintf("commit %d", idx))
	}

	_, err = repo.CreateTag("v1.0.0", hash, nil)
	require.NoError(t, err)

	return "file://" + dir, repo
}

func addCommit(t *testing.T, repo *git.Repository, msg string) plumbing.Hash {
	t.Helper()

	workTree, err := repo.Worktree()
	require.NoError(t, err)

	hash, err := workTree.Commit(msg, &git.CommitOptions{
		AllowEmptyCommits: true,
		Author:            &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
	})
	require.NoError(t, err)

	return hash
}

func TestClone(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)

	for _, testcase := range []struct {
		name         string
		fn           GitFunc
		wantsShallow int
		wantsTags    int
	}{
		{
			name:         "Full",
			fn:           Clone,
			wantsShallow: 0,
			wantsTags:    1,
		},
		{
			name:         "Shallow",
			fn:           ShallowClone,
			wantsShallow: 1,
			wantsTags:    0,
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			uri, _ := newSourceRepo(t, 5)
			path := filepath.Join(t.TempDir(), "repo")

			require.NoError(t, testcase.fn(t.Context(), uri, path, nil, logger))

			repo, err := git.PlainOpen(path)
			require.NoError(t, err)

			shallow, err := repo.Storer.Shallow()
			require.NoError(t, err)
			require.Len(t, shallow, testcase.wantsShallow)

			tags, err := repo.Tags()
			require.NoError(t, err)

			var count int
			require.NoError(t, tags.ForEach(func(*plumbing.Reference) error {
				count++

				return nil
			}))
			require.Equal(t, testcase.wantsTags, count)
		})
	}
}

// logMessages returns a logger and a function that lists the messages logged through it so far.
func logMessages(t *testing.T) (*slog.Logger, func() []string) {
	t.Helper()

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, nil))

	return logger, func() []string {
		var messages []string

		decoder := json.NewDecoder(bytes.NewReader(buf.Bytes()))
		for decoder.More() {
			var record struct {
				Msg string `json:"msg"`
			}
			require.NoError(t, decoder.Decode(&record))

			messages = append(messages, record.Msg)
		}

		buf.Reset()

		return messages
	}
}

func TestCloneAlreadyCloned(t *testing.T) {
	uri, _ := newSourceRepo(t, 2)
	path := filepath.Join(t.TempDir(), "repo")
	logger, messages := logMessages(t)

	require.NoError(t, Clone(t.Context(), uri, path, nil, logger))
	require.Equal(t, []string{"cloning repository", "cloned remote"}, messages())

	require.NoError(t, Clone(t.Context(), uri, path, nil, logger))
	require.Equal(t, []string{"repository already cloned"}, messages())
}

func TestCloneIntoFolderThatIsNotARepository(t *testing.T) {
	uri, _ := newSourceRepo(t, 2)
	path := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(path, "notes.txt"), []byte("mine"), 0o600))

	require.Error(t, Clone(t.Context(), uri, path, nil, slog.New(slog.DiscardHandler)))

	content, err := os.ReadFile(filepath.Join(path, "notes.txt"))
	require.NoError(t, err)
	require.Equal(t, "mine", string(content))
}

func TestCloneEmptyRepository(t *testing.T) {
	source := t.TempDir()
	_, err := git.PlainInit(source, false)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "repo")
	logger, messages := logMessages(t)

	require.NoError(t, Clone(t.Context(), "file://"+source, path, nil, logger))
	require.Equal(t, []string{"cloning repository", "cloned empty repository"}, messages())

	require.NoError(t, Pull(t.Context(), "file://"+source, path, nil, logger))
	require.Equal(t, []string{"repository already up-to-date"}, messages())
}

func TestCloneCancelled(t *testing.T) {
	uri, _ := newSourceRepo(t, 2)
	path := filepath.Join(t.TempDir(), "repo")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.ErrorIs(t, Clone(ctx, uri, path, nil, slog.New(slog.DiscardHandler)), context.Canceled)

	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist, "a cancelled clone leaves nothing behind")
}

func TestPull(t *testing.T) {
	for _, testcase := range []struct {
		name string
		fn   GitFunc
	}{
		{
			name: "AfterFullClone",
			fn:   Clone,
		},
		{
			name: "AfterShallowClone",
			fn:   ShallowClone,
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			uri, source := newSourceRepo(t, 5)
			path := filepath.Join(t.TempDir(), "repo")
			logger, messages := logMessages(t)

			require.NoError(t, testcase.fn(t.Context(), uri, path, nil, logger))
			messages()

			// Nothing new upstream: already up-to-date is not an error.
			require.NoError(t, Pull(t.Context(), uri, path, nil, logger))
			require.Equal(t, []string{"repository already up-to-date"}, messages())

			latest := addCommit(t, source, "new upstream commit")
			require.NoError(t, Pull(t.Context(), uri, path, nil, logger))
			require.Equal(t, []string{"pulled from remote"}, messages())
			require.Equal(t, latest, headOf(t, path))

			// Several commits at once are a fast-forward too.
			for idx := range 3 {
				latest = addCommit(t, source, fmt.Sprintf("upstream commit %d", idx))
			}

			require.NoError(t, Pull(t.Context(), uri, path, nil, logger))
			require.Equal(t, []string{"pulled from remote"}, messages())
			require.Equal(t, latest, headOf(t, path))
		})
	}
}

func TestPullNotCloned(t *testing.T) {
	require.ErrorIs(t, Pull(t.Context(), "file:///nowhere", t.TempDir(), nil, slog.New(slog.DiscardHandler)),
		git.ErrRepositoryNotExists)
}

func TestPullOrClone(t *testing.T) {
	for _, testcase := range []struct {
		name         string
		clone        GitFunc
		prepare      func(t *testing.T, uri, path string)
		wants        []string
		wantsShallow int
	}{
		{
			name:  "Missing",
			clone: Clone,
			wants: []string{"repository not cloned yet", "cloning repository", "cloned remote"},
		},
		{
			name:         "MissingShallow",
			clone:        ShallowClone,
			wants:        []string{"repository not cloned yet", "cloning repository", "cloned remote"},
			wantsShallow: 1,
		},
		{
			name:  "EmptyFolder",
			clone: Clone,
			prepare: func(t *testing.T, _, path string) {
				require.NoError(t, os.MkdirAll(path, 0o755))
			},
			wants: []string{"repository not cloned yet", "cloning repository", "cloned remote"},
		},
		{
			name:  "AlreadyCloned",
			clone: Clone,
			prepare: func(t *testing.T, uri, path string) {
				require.NoError(t, Clone(t.Context(), uri, path, nil, slog.New(slog.DiscardHandler)))
			},
			wants: []string{"repository already up-to-date"},
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			uri, source := newSourceRepo(t, 3)
			path := filepath.Join(t.TempDir(), "repo.git")
			logger, messages := logMessages(t)

			if testcase.prepare != nil {
				testcase.prepare(t, uri, path)
			}

			require.NoError(t, PullOrClone(Pull, testcase.clone)(t.Context(), uri, path, nil, logger))
			require.Equal(t, testcase.wants, messages())

			sourceHead, err := source.Head()
			require.NoError(t, err)

			repo, err := git.PlainOpen(path)
			require.NoError(t, err)

			head, err := repo.Head()
			require.NoError(t, err)
			require.Equal(t, sourceHead.Hash(), head.Hash())

			shallow, err := repo.Storer.Shallow()
			require.NoError(t, err)
			require.Len(t, shallow, testcase.wantsShallow)
		})
	}
}

func TestPullOrCloneLeavesOtherFoldersAlone(t *testing.T) {
	uri, _ := newSourceRepo(t, 2)
	path := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(path, "notes.txt"), []byte("mine"), 0o600))

	err := PullOrClone(Pull, Clone)(t.Context(), uri, path, nil, slog.New(slog.DiscardHandler))
	require.ErrorIs(t, err, git.ErrTargetDirNotEmpty)

	content, err := os.ReadFile(filepath.Join(path, "notes.txt"))
	require.NoError(t, err)
	require.Equal(t, "mine", string(content))
}

// caseInsensitive reports whether the filesystem holding dir ignores case in file names, like macOS and Windows do
// by default.
func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "probe"), nil, 0o600))
	_, err := os.Stat(filepath.Join(dir, "PROBE"))

	return err == nil
}

func countTags(t *testing.T, repo *git.Repository) int {
	t.Helper()

	tags, err := repo.Tags()
	require.NoError(t, err)

	var count int
	require.NoError(t, tags.ForEach(func(*plumbing.Reference) error {
		count++

		return nil
	}))

	return count
}

func TestCloneTagsDifferingOnlyByCase(t *testing.T) {
	// v1.0.0 is packed, as git leaves it after a clone or gc, so V1.0.0 can be created next to it even on a
	// case-insensitive filesystem.
	uri, source := newSourceRepo(t, 1)
	require.NoError(t, source.Storer.(interface{ PackRefs() error }).PackRefs())

	_, err := source.CreateTag("V1.0.0", addCommit(t, source, "second"), nil)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "repo")
	logger, messages := logMessages(t)

	require.NoError(t, Clone(t.Context(), uri, path, nil, logger))

	repo, err := git.PlainOpen(path)
	require.NoError(t, err)

	if caseInsensitive(t, t.TempDir()) {
		require.Equal(t, []string{"cloning repository", "some tags were not stored", "cloned remote"}, messages())
		require.Equal(t, 1, countTags(t, repo))

		return
	}

	require.Equal(t, []string{"cloning repository", "cloned remote"}, messages())
	require.Equal(t, 2, countTags(t, repo))
}

// newSuperproject creates a repository whose only commit holds the given submodules, path to URL, each pinned to
// commit, and returns its file:// URL.
func newSuperproject(t *testing.T, commit plumbing.Hash, submodules map[string]string) string {
	t.Helper()

	dir := t.TempDir()

	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err)

	store := func(o interface {
		Encode(plumbing.EncodedObject) error
	}) plumbing.Hash {
		obj := repo.Storer.NewEncodedObject()
		require.NoError(t, o.Encode(obj))

		hash, err := repo.Storer.SetEncodedObject(obj)
		require.NoError(t, err)

		return hash
	}

	var gitmodules strings.Builder
	entries := make([]object.TreeEntry, 0, len(submodules)+1)

	for _, name := range slices.Sorted(maps.Keys(submodules)) {
		fmt.Fprintf(&gitmodules, "[submodule %q]\n\tpath = %s\n\turl = %s\n", name, name, submodules[name])
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Submodule, Hash: commit})
	}

	blob := repo.Storer.NewEncodedObject()
	blob.SetType(plumbing.BlobObject)

	writer, err := blob.Writer()
	require.NoError(t, err)

	_, err = writer.Write([]byte(gitmodules.String()))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	blobHash, err := repo.Storer.SetEncodedObject(blob)
	require.NoError(t, err)

	// Tree entries are sorted by name, and ".gitmodules" sorts before any letter.
	entries = append([]object.TreeEntry{{Name: ".gitmodules", Mode: filemode.Regular, Hash: blobHash}}, entries...)

	signature := object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}
	commitHash := store(&object.Commit{
		Author:    signature,
		Committer: signature,
		Message:   "add submodules",
		TreeHash:  store(&object.Tree{Entries: entries}),
	})

	head, err := repo.Storer.Reference(plumbing.HEAD)
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(head.Target(), commitHash)))

	return "file://" + dir
}

func TestCloneSubmodules(t *testing.T) {
	subURI, sub := newSourceRepo(t, 2)

	subHead, err := sub.Head()
	require.NoError(t, err)

	uri := newSuperproject(t, subHead.Hash(), map[string]string{
		"good":   subURI,
		"broken": "file://" + filepath.Join(t.TempDir(), "missing"),
	})

	path := filepath.Join(t.TempDir(), "repo")
	logger, messages := logMessages(t)

	// The broken submodule is left out, and the repository and its other submodule are kept.
	require.NoError(t, Clone(t.Context(), uri, path, nil, logger))
	require.Equal(t, []string{"cloning repository", "submodule not cloned", "cloned remote"}, messages())

	repo, err := git.PlainOpen(path)
	require.NoError(t, err)

	workTree, err := repo.Worktree()
	require.NoError(t, err)

	good, err := workTree.Submodule("good")
	require.NoError(t, err)

	status, err := good.Status()
	require.NoError(t, err)
	require.Equal(t, subHead.Hash(), status.Current)
}

func TestSubmoduleURL(t *testing.T) {
	const parent = "https://github.com/acme/app.git"

	for _, testcase := range []struct {
		name      string
		parent    string
		submodule string
		wants     string
	}{
		{name: "GitHubSSH", parent: parent, submodule: "git@github.com:acme/lib.git", wants: "https://github.com/acme/lib.git"},
		{name: "GitHubSSHURL", parent: parent, submodule: "ssh://git@github.com/acme/lib.git", wants: "https://github.com/acme/lib.git"},
		{name: "GitHubHTTPS", parent: parent, submodule: "https://github.com/acme/lib.git", wants: "https://github.com/acme/lib.git"},
		{name: "SiblingRepository", parent: parent, submodule: "../lib.git", wants: "https://github.com/acme/lib.git"},
		{name: "OtherOrganization", parent: parent, submodule: "../../other/lib.git", wants: "https://github.com/other/lib.git"},
		{name: "OtherHostSSH", parent: parent, submodule: "git@bitbucket.org:team/styles.git", wants: "git@bitbucket.org:team/styles.git"},
		{name: "OtherHostHTTPS", parent: parent, submodule: "https://gitlab.com/team/lib.git", wants: "https://gitlab.com/team/lib.git"},
		{name: "RelativeToLocalPath", parent: "/src/app", submodule: "../lib", wants: "../lib"},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			require.Equal(t, testcase.wants, submoduleURL(testcase.parent, testcase.submodule))
		})
	}
}

func TestSubmoduleAuth(t *testing.T) {
	auth := &Auth{Username: "octocat", Token: "secret"}

	require.Equal(t, auth, submoduleAuth("https://github.com/acme/lib.git", auth))
	require.Nil(t, submoduleAuth("https://gitlab.com/acme/lib.git", auth))
	require.Nil(t, submoduleAuth("https://github.com.evil.example/acme/lib.git", auth))
	require.Nil(t, submoduleAuth("git@bitbucket.org:team/styles.git", auth))
	require.Nil(t, submoduleAuth("file:///src/lib", auth))
}

// commitFile writes a file in repo's folder, or removes it when content is empty, and commits that.
func commitFile(t *testing.T, repo *git.Repository, name, content string) plumbing.Hash {
	t.Helper()

	workTree, err := repo.Worktree()
	require.NoError(t, err)

	if content == "" {
		_, err = workTree.Remove(name)
		require.NoError(t, err)
	} else {
		require.NoError(t, os.WriteFile(filepath.Join(workTree.Filesystem().Root(), name), []byte(content), 0o644))

		_, err = workTree.Add(name)
		require.NoError(t, err)
	}

	hash, err := workTree.Commit("change "+name, &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
	})
	require.NoError(t, err)

	return hash
}

// headOf returns the commit the repository in path is at.
func headOf(t *testing.T, path string) plumbing.Hash {
	t.Helper()

	repo, err := git.PlainOpen(path)
	require.NoError(t, err)

	defer repo.Close()

	hash, err := headHash(repo)
	require.NoError(t, err)

	return hash
}

// changesIn lists the tracked files of the repository in path that differ from its commit.
func changesIn(t *testing.T, path string) []string {
	t.Helper()

	repo, err := git.PlainOpen(path)
	require.NoError(t, err)

	defer repo.Close()

	workTree, err := repo.Worktree()
	require.NoError(t, err)

	changed, err := localChanges(repo, workTree)
	require.NoError(t, err)

	return changed
}

// logRecords returns a logger and a function that lists the records logged through it so far, as decoded JSON.
func logRecords(t *testing.T) (*slog.Logger, func() []map[string]any) {
	t.Helper()

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	return logger, func() []map[string]any {
		var records []map[string]any

		decoder := json.NewDecoder(bytes.NewReader(buf.Bytes()))
		for decoder.More() {
			var record map[string]any
			require.NoError(t, decoder.Decode(&record))

			records = append(records, record)
		}

		buf.Reset()

		return records
	}
}

// A pull that local changes stop is an error, every time, and leaves the repository as it was. go-git moves the
// branch before it finds the changes: left there, the second pull would find it up to date, with the files behind.
func TestPullStoppedByLocalChanges(t *testing.T) {
	uri, source := newSourceRepo(t, 1)
	commitFile(t, source, "a.txt", "one\n")

	path := filepath.Join(t.TempDir(), "repo")
	logger := slog.New(slog.DiscardHandler)

	require.NoError(t, Clone(t.Context(), uri, path, nil, logger))

	cloned := headOf(t, path)

	require.NoError(t, os.WriteFile(filepath.Join(path, "a.txt"), []byte("mine\n"), 0o644))
	commitFile(t, source, "b.txt", "two\n")

	for range 2 {
		err := Pull(t.Context(), uri, path, nil, logger)
		require.ErrorIs(t, err, git.ErrUnstagedChanges)
		require.True(t, BlockedByLocalChanges(err))
		require.Equal(t, cloned, headOf(t, path))
		require.Equal(t, []string{"a.txt"}, changesIn(t, path))
	}

	content, err := os.ReadFile(filepath.Join(path, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "mine\n", string(content))
	require.NoFileExists(t, filepath.Join(path, "b.txt"))
}

// A pull never drops a commit the clone has of its own: with nothing new upstream it is up to date, and with new
// commits there the branch has diverged, an error that leaves it as it was. go-git doesn't tell in a shallow clone.
func TestPullKeepsOwnCommits(t *testing.T) {
	for _, testcase := range []struct {
		name     string
		clone    GitFunc
		upstream bool
		wantErr  error
	}{
		{name: "Full", clone: Clone},
		{name: "FullDiverged", clone: Clone, upstream: true, wantErr: git.ErrNonFastForwardUpdate},
		{name: "Shallow", clone: ShallowClone},
		{name: "ShallowDiverged", clone: ShallowClone, upstream: true, wantErr: git.ErrNonFastForwardUpdate},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			uri, source := newSourceRepo(t, 1)
			commitFile(t, source, "a.txt", "one\n")

			path := filepath.Join(t.TempDir(), "repo")
			logger, messages := logMessages(t)

			require.NoError(t, testcase.clone(t.Context(), uri, path, nil, logger))
			messages()

			repo, err := git.PlainOpen(path)
			require.NoError(t, err)

			mine := commitFile(t, repo, "mine.txt", "mine\n")
			require.NoError(t, repo.Close())

			if testcase.upstream {
				commitFile(t, source, "b.txt", "two\n")
			}

			err = Pull(t.Context(), uri, path, nil, logger)
			if testcase.wantErr != nil {
				require.ErrorIs(t, err, testcase.wantErr)
				require.True(t, BlockedByLocalChanges(err))
			} else {
				require.NoError(t, err)
				require.Equal(t, []string{"repository already up-to-date"}, messages())
			}

			require.Equal(t, mine, headOf(t, path))
			require.Empty(t, changesIn(t, path))
			require.FileExists(t, filepath.Join(path, "mine.txt"))
			require.NoFileExists(t, filepath.Join(path, "b.txt"))
		})
	}
}

func TestResetPull(t *testing.T) {
	stage := func(t *testing.T, path, name string) {
		t.Helper()

		repo, err := git.PlainOpen(path)
		require.NoError(t, err)

		defer repo.Close()

		workTree, err := repo.Worktree()
		require.NoError(t, err)

		_, err = workTree.Add(name)
		require.NoError(t, err)
	}

	edit := func(t *testing.T, path string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(path, "a.txt"), []byte("mine\n"), 0o644))
	}

	commit := func(t *testing.T, path string) {
		t.Helper()

		repo, err := git.PlainOpen(path)
		require.NoError(t, err)

		defer repo.Close()

		commitFile(t, repo, "mine.txt", "mine\n")
	}

	for _, testcase := range []struct {
		name     string
		clone    GitFunc
		local    func(t *testing.T, path string)
		upstream func(t *testing.T, source *git.Repository)
		wants    []string
		// wantsCommit is whether the warning names the commit that was dropped.
		wantsCommit bool
	}{
		{
			name:  "NothingToDiscard",
			clone: Clone,
			wants: []string{"repository already up-to-date"},
		},
		{
			name:  "ChangedFile",
			clone: Clone,
			local: edit,
			wants: []string{"local changes discarded", "repository already up-to-date"},
		},
		{
			name:  "StagedChange",
			clone: Clone,
			local: func(t *testing.T, path string) {
				edit(t, path)
				stage(t, path, "a.txt")
			},
			wants: []string{"local changes discarded", "repository already up-to-date"},
		},
		{
			name:  "DeletedFile",
			clone: Clone,
			local: func(t *testing.T, path string) {
				require.NoError(t, os.Remove(filepath.Join(path, "a.txt")))
			},
			wants: []string{"local changes discarded", "repository already up-to-date"},
		},
		{
			name:  "ChangedFileStopsThePull",
			clone: Clone,
			local: edit,
			upstream: func(t *testing.T, source *git.Repository) {
				commitFile(t, source, "b.txt", "two\n")
			},
			wants: []string{"local changes discarded", "pulled from remote"},
		},
		{
			name:  "ChangedFileStopsThePullOfAShallowClone",
			clone: ShallowClone,
			local: edit,
			upstream: func(t *testing.T, source *git.Repository) {
				commitFile(t, source, "b.txt", "two\n")
			},
			wants: []string{"local changes discarded", "pulled from remote"},
		},
		{
			// The pull that was stopped removes what the remote removed, too.
			name:  "FileRemovedUpstream",
			clone: Clone,
			local: edit,
			upstream: func(t *testing.T, source *git.Repository) {
				commitFile(t, source, "old.txt", "")
			},
			wants: []string{"local changes discarded", "pulled from remote"},
		},
		{
			name:        "LocalCommit",
			clone:       Clone,
			local:       commit,
			wants:       []string{"local changes discarded", "pulled from remote"},
			wantsCommit: true,
		},
		{
			name:        "LocalCommitInAShallowClone",
			clone:       ShallowClone,
			local:       commit,
			wants:       []string{"local changes discarded", "pulled from remote"},
			wantsCommit: true,
		},
		{
			name:  "LocalCommitAndRemoteCommit",
			clone: Clone,
			local: commit,
			upstream: func(t *testing.T, source *git.Repository) {
				commitFile(t, source, "b.txt", "two\n")
			},
			wants:       []string{"local changes discarded", "pulled from remote"},
			wantsCommit: true,
		},
		{
			name:  "UntrackedFileIsKept",
			clone: Clone,
			local: func(t *testing.T, path string) {
				require.NoError(t, os.WriteFile(filepath.Join(path, "notes.txt"), []byte("mine\n"), 0o644))
			},
			upstream: func(t *testing.T, source *git.Repository) {
				commitFile(t, source, "b.txt", "two\n")
			},
			wants: []string{"pulled from remote"},
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			uri, source := newSourceRepo(t, 1)
			commitFile(t, source, "a.txt", "one\n")
			commitFile(t, source, "old.txt", "old\n")

			path := filepath.Join(t.TempDir(), "repo")
			logger, records := logRecords(t)

			require.NoError(t, testcase.clone(t.Context(), uri, path, nil, logger))
			records()

			if testcase.local != nil {
				testcase.local(t, path)
			}

			discarded := headOf(t, path)

			if testcase.upstream != nil {
				testcase.upstream(t, source)
			}

			require.NoError(t, ResetPull(t.Context(), uri, path, nil, logger))

			var messages []string

			for _, record := range records() {
				messages = append(messages, record["msg"].(string))

				if record["msg"] != "local changes discarded" {
					continue
				}

				require.Equal(t, uri, record["uri"])

				if testcase.wantsCommit {
					require.Equal(t, discarded.String(), record["commit"])
				} else {
					require.NotContains(t, record, "commit")
				}
			}

			require.Equal(t, testcase.wants, messages)

			// The clone is at the remote's commit, with the remote's files and none of its own changes.
			sourceHead, err := source.Head()
			require.NoError(t, err)
			require.Equal(t, sourceHead.Hash(), headOf(t, path))
			require.Empty(t, changesIn(t, path))

			sourceTree, err := source.Worktree()
			require.NoError(t, err)

			for _, name := range []string{"a.txt", "b.txt", "old.txt", "mine.txt"} {
				want, err := os.ReadFile(filepath.Join(sourceTree.Filesystem().Root(), name))
				if err != nil {
					require.NoFileExists(t, filepath.Join(path, name))

					continue
				}

				got, err := os.ReadFile(filepath.Join(path, name))
				require.NoError(t, err)
				require.Equal(t, string(want), string(got), name)
			}

			if testcase.name == "UntrackedFileIsKept" {
				require.FileExists(t, filepath.Join(path, "notes.txt"))
			}

			// Nothing is left to discard.
			require.NoError(t, ResetPull(t.Context(), uri, path, nil, logger))
			require.Len(t, records(), 1)
		})
	}
}

// pinSubmodules adds a commit to the superproject at uri that pins each of its submodules to commit.
func pinSubmodules(t *testing.T, uri string, commit plumbing.Hash) {
	t.Helper()

	repo, err := git.PlainOpen(strings.TrimPrefix(uri, "file://"))
	require.NoError(t, err)

	defer repo.Close()

	head, err := repo.Head()
	require.NoError(t, err)

	parent, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)

	tree, err := parent.Tree()
	require.NoError(t, err)

	entries := slices.Clone(tree.Entries)
	for idx := range entries {
		if entries[idx].Mode == filemode.Submodule {
			entries[idx].Hash = commit
		}
	}

	store := func(o interface {
		Encode(plumbing.EncodedObject) error
	}) plumbing.Hash {
		obj := repo.Storer.NewEncodedObject()
		require.NoError(t, o.Encode(obj))

		hash, err := repo.Storer.SetEncodedObject(obj)
		require.NoError(t, err)

		return hash
	}

	signature := object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}
	pinned := store(&object.Commit{
		Author:       signature,
		Committer:    signature,
		Message:      "pin submodules",
		TreeHash:     store(&object.Tree{Entries: entries}),
		ParentHashes: []plumbing.Hash{head.Hash()},
	})

	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(head.Name(), pinned)))
}

// submoduleAt returns the commit the submodule name of the repository in path is checked out at.
func submoduleAt(t *testing.T, path, name string) plumbing.Hash {
	t.Helper()

	repo, err := git.PlainOpen(path)
	require.NoError(t, err)

	defer repo.Close()

	workTree, err := repo.Worktree()
	require.NoError(t, err)

	submodule, err := workTree.Submodule(name)
	require.NoError(t, err)

	status, err := submodule.Status()
	require.NoError(t, err)

	return status.Current
}

// A pull moves the submodules to the commits the pulled repository pins them to. Left behind, a submodule counts as
// a local change, and stops the next pull.
func TestPullUpdatesSubmodules(t *testing.T) {
	subURI, sub := newSourceRepo(t, 2)

	subHead, err := sub.Head()
	require.NoError(t, err)

	uri := newSuperproject(t, subHead.Hash(), map[string]string{"lib": subURI})
	path := filepath.Join(t.TempDir(), "repo")
	logger, messages := logMessages(t)

	require.NoError(t, Clone(t.Context(), uri, path, nil, logger))
	messages()

	for range 2 {
		pinned := addCommit(t, sub, "more work")
		pinSubmodules(t, uri, pinned)

		require.NoError(t, Pull(t.Context(), uri, path, nil, logger))
		require.Equal(t, []string{"pulled from remote"}, messages())
		require.Equal(t, pinned, submoduleAt(t, path, "lib"))
		require.Empty(t, changesIn(t, path))
	}

	// Nothing new: the submodule is left alone.
	require.NoError(t, Pull(t.Context(), uri, path, nil, logger))
	require.Equal(t, []string{"repository already up-to-date"}, messages())
}

// A submodule that a pull left behind its pinned commit, as gitpull's pulls used to, is moved there before the next
// pull, which it would stop.
func TestPullMovesSubmoduleLeftBehind(t *testing.T) {
	subURI, sub := newSourceRepo(t, 2)

	subHead, err := sub.Head()
	require.NoError(t, err)

	uri := newSuperproject(t, subHead.Hash(), map[string]string{"lib": subURI})
	path := filepath.Join(t.TempDir(), "repo")
	logger := slog.New(slog.DiscardHandler)

	require.NoError(t, Clone(t.Context(), uri, path, nil, logger))

	// A pull of the repository alone.
	pinSubmodules(t, uri, addCommit(t, sub, "more work"))

	repo, err := git.PlainOpen(path)
	require.NoError(t, err)

	workTree, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, workTree.PullContext(t.Context(), &git.PullOptions{RemoteName: git.DefaultRemoteName}))
	require.NoError(t, repo.Close())
	require.Equal(t, subHead.Hash(), submoduleAt(t, path, "lib"))
	require.Equal(t, []string{"lib"}, changesIn(t, path))

	pinned := addCommit(t, sub, "even more work")
	pinSubmodules(t, uri, pinned)

	require.NoError(t, Pull(t.Context(), uri, path, nil, logger))
	require.Equal(t, pinned, submoduleAt(t, path, "lib"))
	require.Empty(t, changesIn(t, path))
}

// Changes inside a submodule keep it from its pinned commit: a pull says so and goes on, and a reset discards them.
func TestResetPullSubmoduleWithChanges(t *testing.T) {
	subURI, sub := newSourceRepo(t, 1)
	subCommit := commitFile(t, sub, "f.txt", "one\n")

	uri := newSuperproject(t, subCommit, map[string]string{"lib": subURI})
	path := filepath.Join(t.TempDir(), "repo")
	logger, records := logRecords(t)

	require.NoError(t, Clone(t.Context(), uri, path, nil, logger))
	records()

	require.NoError(t, os.WriteFile(filepath.Join(path, "lib", "f.txt"), []byte("mine\n"), 0o644))

	pinned := commitFile(t, sub, "g.txt", "two\n")
	pinSubmodules(t, uri, pinned)

	messages := func() []string {
		var out []string
		for _, record := range records() {
			out = append(out, record["msg"].(string))
		}

		return out
	}

	require.NoError(t, Pull(t.Context(), uri, path, nil, logger))
	require.Equal(t, []string{"submodule not updated", "pulled from remote"}, messages())
	require.Equal(t, subCommit, submoduleAt(t, path, "lib"))

	require.NoError(t, ResetPull(t.Context(), uri, path, nil, logger))

	logged := records()
	require.Len(t, logged, 2)
	require.Equal(t, "local changes discarded", logged[0]["msg"])
	require.Equal(t, "lib", logged[0]["submodule"])
	require.Equal(t, "repository already up-to-date", logged[1]["msg"])
	require.Equal(t, pinned, submoduleAt(t, path, "lib"))
	require.Empty(t, changesIn(t, path))

	content, err := os.ReadFile(filepath.Join(path, "lib", "f.txt"))
	require.NoError(t, err)
	require.Equal(t, "one\n", string(content))
	require.FileExists(t, filepath.Join(path, "lib", "g.txt"))
}

// commitTree adds a commit to repo holding exactly files, path to content, and gitlinks, path to commit. It is built
// from objects rather than from a folder, so it can hold what the filesystem here may not: names that differ only by
// case.
func commitTree(t *testing.T, repo *git.Repository, files map[string]string, gitlinks map[string]plumbing.Hash) plumbing.Hash {
	t.Helper()

	store := func(o interface {
		Encode(plumbing.EncodedObject) error
	}) plumbing.Hash {
		obj := repo.Storer.NewEncodedObject()
		require.NoError(t, o.Encode(obj))

		hash, err := repo.Storer.SetEncodedObject(obj)
		require.NoError(t, err)

		return hash
	}

	type folder struct {
		entries []object.TreeEntry
		folders map[string]*folder
	}

	root := &folder{folders: map[string]*folder{}}

	add := func(name string, entry object.TreeEntry) {
		at := root

		parts := strings.Split(name, "/")
		for _, part := range parts[:len(parts)-1] {
			if at.folders[part] == nil {
				at.folders[part] = &folder{folders: map[string]*folder{}}
			}

			at = at.folders[part]
		}

		entry.Name = parts[len(parts)-1]
		at.entries = append(at.entries, entry)
	}

	for name, content := range files {
		blob := repo.Storer.NewEncodedObject()
		blob.SetType(plumbing.BlobObject)

		writer, err := blob.Writer()
		require.NoError(t, err)

		_, err = writer.Write([]byte(content))
		require.NoError(t, err)
		require.NoError(t, writer.Close())

		hash, err := repo.Storer.SetEncodedObject(blob)
		require.NoError(t, err)

		add(name, object.TreeEntry{Mode: filemode.Regular, Hash: hash})
	}

	for name, commit := range gitlinks {
		add(name, object.TreeEntry{Mode: filemode.Submodule, Hash: commit})
	}

	var write func(*folder) plumbing.Hash

	write = func(f *folder) plumbing.Hash {
		for name, sub := range f.folders {
			f.entries = append(f.entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: write(sub)})
		}

		sort.Sort(object.TreeEntrySorter(f.entries))

		return store(&object.Tree{Entries: f.entries})
	}

	commit := &object.Commit{
		Author:    object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
		Committer: object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
		Message:   "tree",
		TreeHash:  write(root),
	}

	head, err := repo.Storer.Reference(plumbing.HEAD)
	require.NoError(t, err)

	if parent, err := repo.Storer.Reference(head.Target()); err == nil {
		commit.ParentHashes = []plumbing.Hash{parent.Hash()}
	}

	hash := store(commit)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(head.Target(), hash)))

	return hash
}

// Two kinds of path are never as the repository has them, whatever is checked out: paths that differ only by case,
// where the filesystem ignores it, and a submodule entry that .gitmodules doesn't describe. They are not local
// changes: a pull goes past them, a reset doesn't keep discarding them, and a real change still counts.
func TestPullPastWhatTheFolderCannotHold(t *testing.T) {
	for _, testcase := range []struct {
		name     string
		files    map[string]string
		gitlinks map[string]plumbing.Hash
	}{
		{
			name: "NamesDifferingOnlyByCase",
			files: map[string]string{
				"README.md":  "upper\n",
				"readme.md":  "lower\n",
				"Docs/a.txt": "a\n",
				"docs/b.txt": "b\n",
			},
		},
		{
			name:     "SubmoduleEntryWithoutGitmodules",
			gitlinks: map[string]plumbing.Hash{"vendor/lib": plumbing.NewHash("9b6890af691d820b3290a886c2a8425c9e445da4")},
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			uri, source := newSourceRepo(t, 1)

			files := maps.Clone(testcase.files)
			if files == nil {
				files = map[string]string{}
			}

			files["keep.txt"] = "one\n"
			commitTree(t, source, files, testcase.gitlinks)

			path := filepath.Join(t.TempDir(), "repo")
			logger, messages := logMessages(t)

			require.NoError(t, Clone(t.Context(), uri, path, nil, logger))
			require.Empty(t, changesIn(t, path))
			messages()

			// A pull goes through.
			files["keep.txt"] = "two\n"
			latest := commitTree(t, source, files, testcase.gitlinks)

			require.NoError(t, Pull(t.Context(), uri, path, nil, logger))
			require.Equal(t, []string{"pulled from remote"}, messages())
			require.Equal(t, latest, headOf(t, path))

			content, err := os.ReadFile(filepath.Join(path, "keep.txt"))
			require.NoError(t, err)
			require.Equal(t, "two\n", string(content))

			// A reset finds nothing to discard, run after run.
			for range 2 {
				require.NoError(t, ResetPull(t.Context(), uri, path, nil, logger))
				require.Equal(t, []string{"repository already up-to-date"}, messages())
			}

			// A change of one's own still stops a pull, and is what a reset discards.
			require.NoError(t, os.WriteFile(filepath.Join(path, "keep.txt"), []byte("mine\n"), 0o644))

			files["keep.txt"] = "three\n"
			latest = commitTree(t, source, files, testcase.gitlinks)

			require.ErrorIs(t, Pull(t.Context(), uri, path, nil, logger), git.ErrUnstagedChanges)
			require.Equal(t, []string{"keep.txt"}, changesIn(t, path))
			messages()

			require.NoError(t, ResetPull(t.Context(), uri, path, nil, logger))
			require.Equal(t, []string{"local changes discarded", "pulled from remote"}, messages())
			require.Equal(t, latest, headOf(t, path))

			content, err = os.ReadFile(filepath.Join(path, "keep.txt"))
			require.NoError(t, err)
			require.Equal(t, "three\n", string(content))
		})
	}
}
