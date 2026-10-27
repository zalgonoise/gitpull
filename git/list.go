package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const githubAPI = "https://api.github.com"
const perPage = 100

// apiClient bounds each GitHub API call, so a stalled connection fails instead of hanging the command.
var apiClient = &http.Client{Timeout: 30 * time.Second}

// Repository is the part of GitHub's repository object that gitpull reads.
type Repository struct {
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	CloneURL string `json:"clone_url"`
	Private  bool   `json:"private"`
	Fork     bool   `json:"fork"`
	Archived bool   `json:"archived"`
}

// ListOrgRepos returns every repository in the GitHub organization org, following pagination.
//
// repoType is passed through to the GitHub API (all, public, private, forks, sources, member). The token is
// optional, but without it only public repositories are visible.
func ListOrgRepos(ctx context.Context, org, repoType, token string, logger *slog.Logger) ([]Repository, error) {
	if org == "" {
		return nil, errors.New("no organization given")
	}

	return listOrgRepos(ctx, apiClient, githubAPI, org, repoType, token, logger)
}

func listOrgRepos(ctx context.Context, client *http.Client, baseURL, org, repoType, token string, logger *slog.Logger) ([]Repository, error) {
	repos := make([]Repository, 0, perPage)

	for page := 1; ; page++ {
		batch, err := fetchReposPage(ctx, client, baseURL, org, repoType, token, page)
		if err != nil {
			return nil, err
		}

		logger.DebugContext(ctx, "fetched repositories page",
			slog.String("org", org),
			slog.Int("page", page),
			slog.Int("count", len(batch)))

		repos = append(repos, batch...)

		if len(batch) < perPage {
			return repos, nil
		}
	}
}

func fetchReposPage(ctx context.Context, client *http.Client, baseURL, org, repoType, token string, page int) ([]Repository, error) {
	query := url.Values{}
	query.Set("type", repoType)
	query.Set("per_page", strconv.Itoa(perPage))
	query.Set("page", strconv.Itoa(page))

	uri := fmt.Sprintf("%s/orgs/%s/repos?%s", baseURL, url.PathEscape(org), query.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1024))

		return nil, fmt.Errorf("listing repositories for org %q: unexpected status %s: %s",
			org, res.Status, strings.TrimSpace(string(body)))
	}

	var batch []Repository
	if err := json.NewDecoder(res.Body).Decode(&batch); err != nil {
		return nil, fmt.Errorf("decoding repositories page %d: %w", page, err)
	}

	return batch, nil
}
