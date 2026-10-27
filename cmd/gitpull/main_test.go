package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zalgonoise/gitpull/git"
)

const fullConfig = `{
	"org": "acme",
	"dir": "/src/acme",
	"repos": ["api", "web"],
	"identity": "/config/.git-credentials",
	"parallel": 8,
	"shallow": true
}`

func writeFile(t *testing.T, name, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

// newCloneFlags declares the same flags as ExecClone.
func newCloneFlags() *flag.FlagSet {
	fs := flag.NewFlagSet("clone", flag.ContinueOnError)

	fs.String("config", "", "")
	fs.String("org", "", "")
	fs.String("dir", ".", "")
	fs.String("repos", "", "")
	fs.Bool("shallow", false, "")
	fs.Int("parallel", 1, "")
	fs.String("user", "", "")
	fs.String("token", "", "")
	identityFlag(fs)

	return fs
}

// newListFlags declares the same flags as ExecList.
func newListFlags() *flag.FlagSet {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)

	fs.String("config", "", "")
	fs.String("org", "", "")
	fs.String("type", "all", "")
	fs.Bool("skip-archived", false, "")
	fs.String("token", "", "")
	identityFlag(fs)

	return fs
}

func flagValues(fs *flag.FlagSet) map[string]string {
	values := make(map[string]string)

	fs.VisitAll(func(f *flag.Flag) {
		values[f.Name] = f.Value.String()
	})

	return values
}

func TestApplyConfig(t *testing.T) {
	for _, testcase := range []struct {
		name    string
		flags   func() *flag.FlagSet
		args    []string
		config  string
		wants   map[string]string
		wantErr bool
	}{
		{
			name:   "ConfigFillsUnsetFlags",
			flags:  newCloneFlags,
			config: fullConfig,
			wants: map[string]string{
				"org":      "acme",
				"dir":      "/src/acme",
				"repos":    "api,web",
				"shallow":  "true",
				"parallel": "8",
				"i":        "/config/.git-credentials",
				"identity": "/config/.git-credentials",
				"user":     "",
				"token":    "",
			},
		},
		{
			name:   "CommandLineWins",
			flags:  newCloneFlags,
			args:   []string{"-org", "other", "-repos", "cli", "-parallel", "2", "-identity", ""},
			config: fullConfig,
			wants: map[string]string{
				"org":      "other",
				"dir":      "/src/acme",
				"repos":    "cli",
				"shallow":  "true",
				"parallel": "2",
				"i":        "",
				"identity": "",
				"user":     "",
				"token":    "",
			},
		},
		{
			name:   "ShortIdentityFlagWins",
			flags:  newCloneFlags,
			args:   []string{"-i", "/home/.git-credentials"},
			config: fullConfig,
			wants: map[string]string{
				"org":      "acme",
				"dir":      "/src/acme",
				"repos":    "api,web",
				"shallow":  "true",
				"parallel": "8",
				"i":        "/home/.git-credentials",
				"identity": "/home/.git-credentials",
				"user":     "",
				"token":    "",
			},
		},
		{
			name:   "KeysWithoutAFlagAreIgnored",
			flags:  newListFlags,
			config: fullConfig,
			wants: map[string]string{
				"org":           "acme",
				"type":          "all",
				"skip-archived": "false",
				"i":             "/config/.git-credentials",
				"identity":      "/config/.git-credentials",
				"token":         "",
			},
		},
		{
			name:    "UnknownKey",
			flags:   newCloneFlags,
			config:  `{"organisation": "acme"}`,
			wantErr: true,
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			fs := testcase.flags()
			path := writeFile(t, "config.json", testcase.config)

			require.NoError(t, fs.Parse(append([]string{"-config", path}, testcase.args...)))

			err := applyConfig(fs, path)
			if testcase.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)

			values := flagValues(fs)
			delete(values, "config")
			require.Equal(t, testcase.wants, values)
		})
	}
}

func TestApplyConfigWithoutFile(t *testing.T) {
	fs := newCloneFlags()
	require.NoError(t, fs.Parse(nil))

	before := flagValues(fs)
	require.NoError(t, applyConfig(fs, ""))
	require.Equal(t, before, flagValues(fs))

	require.Error(t, applyConfig(fs, filepath.Join(t.TempDir(), "missing.json")))
}

func TestNewAuth(t *testing.T) {
	identity := writeFile(t, ".git-credentials", "https://octocat:from-file@github.com\n")

	for _, testcase := range []struct {
		name     string
		identity string
		user     string
		token    string
		wants    *git.Auth
		wantErr  bool
	}{
		{
			name: "Nothing",
		},
		{
			name:  "Flags",
			user:  "octocat",
			token: "from-flags",
			wants: &git.Auth{Username: "octocat", Token: "from-flags"},
		},
		{
			name:     "IdentityFile",
			identity: identity,
			wants:    &git.Auth{Username: "octocat", Token: "from-file"},
		},
		{
			name:     "FlagsWinOverIdentityFile",
			identity: identity,
			user:     "someone",
			token:    "from-flags",
			wants:    &git.Auth{Username: "someone", Token: "from-flags"},
		},
		{
			name:     "TokenWithoutUser",
			identity: identity,
			token:    "from-flags",
			wantErr:  true,
		},
		{
			name:    "UserWithoutToken",
			user:    "octocat",
			wantErr: true,
		},
		{
			name:     "MissingIdentityFile",
			identity: filepath.Join(t.TempDir(), "missing"),
			wantErr:  true,
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			auth, err := newAuth(testcase.identity, testcase.user, testcase.token)
			if testcase.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, testcase.wants, auth)
		})
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	require.Equal(t, filepath.Join(home, "src", "acme"), expandHome("~/src/acme"))
	require.Equal(t, "/src/acme", expandHome("/src/acme"))
	require.Equal(t, "src/~/acme", expandHome("src/~/acme"))
	require.Equal(t, "~acme", expandHome("~acme"))
	require.Equal(t, ".", expandHome("."))
}

func TestBuildVersion(t *testing.T) {
	// go test builds have no module version, so without a release version it is "devel".
	require.Equal(t, "devel", buildVersion())

	version = "v1.2.0"
	t.Cleanup(func() { version = "" })

	require.Equal(t, "v1.2.0", buildVersion())
}
