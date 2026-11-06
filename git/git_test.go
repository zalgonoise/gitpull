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

			repo, err := git.PlainOpen(path)
			require.NoError(t, err)

			head, err := repo.Head()
			require.NoError(t, err)
			require.Equal(t, latest, head.Hash())
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

			require.NoError(t, PullOrClone(testcase.clone)(t.Context(), uri, path, nil, logger))
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

	err := PullOrClone(Clone)(t.Context(), uri, path, nil, slog.New(slog.DiscardHandler))
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
