package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoad(t *testing.T) {
	for _, testcase := range []struct {
		name    string
		input   string
		wants   map[string]string
		wantErr bool
	}{
		{
			name: "AllFields",
			input: `{
				"org": "acme",
				"dir": "/src/acme",
				"repos": ["api", "web"],
				"identity": "~/.git-credentials",
				"parallel": 8,
				"shallow": true
			}`,
			wants: map[string]string{
				"org":      "acme",
				"dir":      "/src/acme",
				"repos":    "api,web",
				"identity": "~/.git-credentials",
				"parallel": "8",
				"shallow":  "true",
			},
		},
		{
			name:  "UnsetFieldsAreLeftOut",
			input: `{"org": "acme", "parallel": 0, "shallow": false, "repos": []}`,
			wants: map[string]string{"org": "acme"},
		},
		{
			name:    "UnknownField",
			input:   `{"organisation": "acme"}`,
			wantErr: true,
		},
		{
			name:    "InvalidJSON",
			input:   `{"org": `,
			wantErr: true,
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			require.NoError(t, os.WriteFile(path, []byte(testcase.input), 0o600))

			cfg, err := Load(path)
			if testcase.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, testcase.wants, cfg.Flags())
		})
	}
}
