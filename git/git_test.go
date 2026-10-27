package git

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
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
