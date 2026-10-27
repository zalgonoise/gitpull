package git

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFormatRepos(t *testing.T) {
	for _, testcase := range []struct {
		name  string
		org   string
		input []string
		wants []string
	}{
		{
			name:  "Names",
			org:   "acme",
			input: []string{"api", "web-app", "sdk_go", ".github"},
			wants: []string{
				"https://github.com/acme/api.git",
				"https://github.com/acme/web-app.git",
				"https://github.com/acme/sdk_go.git",
				"https://github.com/acme/.github.git",
			},
		},
		{
			name:  "OnlyTheLastElementIsKept",
			org:   "acme",
			input: []string{"acme/api", "github.com/someone/web-app", "https://github.com/acme/sdk.git", "infra/"},
			wants: []string{
				"https://github.com/acme/api.git",
				"https://github.com/acme/web-app.git",
				"https://github.com/acme/sdk.git",
				"https://github.com/acme/infra.git",
			},
		},
		{
			name:  "GitSuffixIsOptional",
			org:   "acme",
			input: []string{"api.git", "web"},
			wants: []string{
				"https://github.com/acme/api.git",
				"https://github.com/acme/web.git",
			},
		},
		{
			name:  "BlankNamesAreSkipped",
			org:   "acme",
			input: []string{"", " api", "web ", "  ", ".", "..", ".git"},
			wants: []string{
				"https://github.com/acme/api.git",
				"https://github.com/acme/web.git",
			},
		},
		{
			name:  "RepeatedRepositoriesAreSkipped",
			org:   "acme",
			input: []string{"api", "web", "api", "acme/web", "api.git"},
			wants: []string{
				"https://github.com/acme/api.git",
				"https://github.com/acme/web.git",
			},
		},
		{
			name:  "Nothing",
			org:   "acme",
			input: nil,
			wants: []string{},
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			input := slices.Clone(testcase.input)

			require.Equal(t, testcase.wants, formatRepos(testcase.org, input))
			require.Equal(t, testcase.input, input, "input must not be modified")
		})
	}
}

func TestTopLevelDir(t *testing.T) {
	require.Equal(t, "api.git", TopLevelDir("https://github.com/acme/api.git"))
	require.Equal(t, "", TopLevelDir(""))
}

func TestMultiRepo(t *testing.T) {
	errFailed := errors.New("failed")

	for _, testcase := range []struct {
		name        string
		workers     int
		failing     []string
		wantsActive int
	}{
		{
			name:        "Sequential",
			workers:     1,
			wantsActive: 1,
		},
		{
			name:        "ZeroMeansSequential",
			workers:     0,
			wantsActive: 1,
		},
		{
			name:        "FourWorkers",
			workers:     4,
			wantsActive: 4,
		},
		{
			name:        "MoreWorkersThanRepos",
			workers:     50,
			wantsActive: 20,
		},
		{
			name:        "ErrorsNameTheRepo",
			workers:     4,
			failing:     []string{"https://github.com/acme/repo-03.git", "https://github.com/acme/repo-17.git"},
			wantsActive: 4,
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				repos := make([]string, 20)
				for idx := range repos {
					repos[idx] = fmt.Sprintf("repo-%02d", idx)
				}

				var (
					mu        sync.Mutex
					seen      = make(map[string]int)
					active    int
					maxActive int
				)

				fn := func(ctx context.Context, uri, path string, auth *Auth, logger *slog.Logger) error {
					mu.Lock()
					seen[uri]++
					active++
					maxActive = max(maxActive, active)
					mu.Unlock()

					// Inside the synctest bubble this only returns once every worker is blocked, so all of
					// them have picked up a repo by then.
					time.Sleep(time.Second)

					mu.Lock()
					active--
					mu.Unlock()

					for _, failing := range testcase.failing {
						if uri == failing {
							return errFailed
						}
					}

					return nil
				}

				err := MultiRepo(t.Context(), "acme", "base", repos, nil, testcase.workers, fn, slog.New(slog.DiscardHandler))

				require.Len(t, seen, len(repos))
				for uri, count := range seen {
					require.Equal(t, 1, count, uri)
				}

				require.Equal(t, testcase.wantsActive, maxActive)

				if len(testcase.failing) == 0 {
					require.NoError(t, err)

					return
				}

				require.ErrorIs(t, err, errFailed)
				require.EqualError(t, err, fmt.Sprintf("%d of %d repositories failed", len(testcase.failing), len(repos)))

				var messages []string
				for _, repoErr := range err.(interface{ Unwrap() []error }).Unwrap() {
					messages = append(messages, repoErr.Error())
				}

				for _, failing := range testcase.failing {
					require.Contains(t, messages, failing+": "+errFailed.Error())
				}
			})
		})
	}
}

func TestMultiRepoRequiresOrgAndRepos(t *testing.T) {
	noop := func(context.Context, string, string, *Auth, *slog.Logger) error {
		return nil
	}

	logger := slog.New(slog.DiscardHandler)

	require.ErrorContains(t, MultiRepo(t.Context(), "", "base", []string{"api"}, nil, 1, noop, logger), "no organization")
	require.ErrorContains(t, MultiRepo(t.Context(), "acme", "base", nil, nil, 1, noop, logger), "no repositories")
	require.ErrorContains(t, MultiRepo(t.Context(), "acme", "base", []string{"", " "}, nil, 1, noop, logger), "no repositories")
}

func TestMultiRepoStopsWhenContextIsDone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())

		repos := make([]string, 10)
		for idx := range repos {
			repos[idx] = fmt.Sprintf("repo-%02d", idx)
		}

		var (
			mu   sync.Mutex
			seen []string
		)

		fn := func(ctx context.Context, uri, path string, auth *Auth, logger *slog.Logger) error {
			mu.Lock()
			seen = append(seen, uri)
			mu.Unlock()

			// The first two repositories are in flight when the run is cancelled.
			cancel()
			<-ctx.Done()

			return ctx.Err()
		}

		err := MultiRepo(ctx, "acme", "base", repos, nil, 2, fn, slog.New(slog.DiscardHandler))

		require.ErrorIs(t, err, context.Canceled)
		require.EqualError(t, err, fmt.Sprintf("%d of 10 repositories failed, %d skipped: context canceled", len(seen), 10-len(seen)))
		require.LessOrEqual(t, len(seen), 2)
	})
}
