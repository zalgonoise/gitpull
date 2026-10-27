package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseCredentials(t *testing.T) {
	for _, testcase := range []struct {
		name    string
		input   string
		wants   *Auth
		wantErr bool
	}{
		{
			name:  "Single",
			input: "https://octocat:ghp_secret@github.com\n",
			wants: &Auth{Username: "octocat", Token: "ghp_secret"},
		},
		{
			name: "PicksGitHubAmongOtherHosts",
			input: "https://someone:other@gitlab.com\n" +
				"https://octocat:ghp_secret@github.com\n" +
				"https://octocat:second@github.com\n",
			wants: &Auth{Username: "octocat", Token: "ghp_secret"},
		},
		{
			name:  "DecodesEscapedCharacters",
			input: "https://first.last%40example.com:p%40ss%3Aword@github.com",
			wants: &Auth{Username: "first.last@example.com", Token: "p@ss:word"},
		},
		{
			name:  "WithRepoPath",
			input: "https://octocat:ghp_secret@github.com/acme/api.git",
			wants: &Auth{Username: "octocat", Token: "ghp_secret"},
		},
		{
			name: "SkipsInvalidAndIncompleteLines",
			input: "\n" +
				"not a url\n" +
				"http://octocat:insecure@github.com\n" +
				"https://ghp_tokenonly@github.com\n" +
				"https://octocat:ghp_secret@github.com\n",
			wants: &Auth{Username: "octocat", Token: "ghp_secret"},
		},
		{
			name:    "NoGitHubEntry",
			input:   "https://someone:other@gitlab.com\n",
			wantErr: true,
		},
		{
			name:    "Empty",
			input:   "",
			wantErr: true,
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			auth, err := parseCredentials(strings.NewReader(testcase.input), githubHost)
			if testcase.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, testcase.wants, auth)
		})
	}
}

func TestReadCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".git-credentials")
	require.NoError(t, os.WriteFile(path, []byte("https://octocat:ghp_secret@github.com\n"), 0o600))

	auth, err := ReadCredentials(path)
	require.NoError(t, err)
	require.Equal(t, &Auth{Username: "octocat", Token: "ghp_secret"}, auth)

	_, err = ReadCredentials(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
}
