package git

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListOrgRepos(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)

	// 101 repos: a full first page and a single repo on the second.
	all := make([]Repository, perPage+1)
	for idx := range all {
		all[idx] = Repository{Name: fmt.Sprintf("repo-%03d", idx)}
	}

	for _, testcase := range []struct {
		name    string
		org     string
		token   string
		wants   []Repository
		wantErr bool
	}{
		{
			name:  "PaginatedWithToken",
			org:   "acme",
			token: "secret",
			wants: all,
		},
		{
			name:    "UnknownOrg",
			org:     "nope",
			wantErr: true,
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/orgs/acme/repos" {
					http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)

					return
				}

				require.Equal(t, "Bearer "+testcase.token, r.Header.Get("Authorization"))
				require.Equal(t, "all", r.URL.Query().Get("type"))
				require.Equal(t, strconv.Itoa(perPage), r.URL.Query().Get("per_page"))

				page, err := strconv.Atoi(r.URL.Query().Get("page"))
				require.NoError(t, err)

				start := min((page-1)*perPage, len(all))
				end := min(start+perPage, len(all))

				require.NoError(t, json.NewEncoder(w).Encode(all[start:end]))
			}))
			defer server.Close()

			repos, err := listOrgRepos(t.Context(), server.Client(), server.URL,
				testcase.org, "all", testcase.token, logger)
			if testcase.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, testcase.wants, repos)
		})
	}
}
