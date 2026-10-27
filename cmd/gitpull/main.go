package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/zalgonoise/x/cli/v2"

	"github.com/zalgonoise/gitpull/git"
	"github.com/zalgonoise/gitpull/internal/config"
	"github.com/zalgonoise/gitpull/internal/log"
)

const usage = `gitpull clones and updates many GitHub repositories at once.

Usage:
  gitpull <command> [flags]

Commands:
  clone     clone repositories into a folder, skipping the ones already cloned
  pull      fast-forward cloned repositories from their origin
  list      print the names of a GitHub organization's repositories
  version   print the version
  help      print this help

Run "gitpull <command> -h" for a command's flags. Flags can also come from a JSON
config file (-config); see https://github.com/zalgonoise/gitpull for details.
`

// version is set by release builds (make release: -ldflags "-X main.version=v1.2.0").
var version string

func main() {
	logger := log.New("debug")
	slog.SetDefault(logger)

	help := cli.Executable(ExecHelp)

	runner := cli.NewRunner("gitpull",
		cli.WithExecutors(map[string]cli.Executor{
			"clone":   withSignals(ExecClone),
			"pull":    withSignals(ExecPull),
			"list":    withSignals(ExecList),
			"version": cli.Executable(ExecVersion),
			"help":    help,
			"-h":      help,
			"--help":  help,
			// No command at all.
			"": cli.Executable(ExecUsage),
		}),
	)

	cli.Run(runner, logger)
}

// withSignals cancels the command's context on Ctrl-C or SIGTERM, so the clones and pulls in flight stop (go-git
// removes a half-done clone) and the repositories not started yet are skipped.
func withSignals(fn cli.Executable) cli.Executable {
	return func(ctx context.Context, logger *slog.Logger, args []string) (int, error) {
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()

		return fn(ctx, logger, args)
	}
}

func ExecHelp(context.Context, *slog.Logger, []string) (int, error) {
	fmt.Print(usage)

	return 0, nil
}

func ExecUsage(context.Context, *slog.Logger, []string) (int, error) {
	fmt.Fprint(os.Stderr, usage)

	return 2, nil
}

func ExecVersion(context.Context, *slog.Logger, []string) (int, error) {
	fmt.Println("gitpull", buildVersion())

	return 0, nil
}

// buildVersion is the release build's version, else the module version go install recorded (gitpull@v1.2.0),
// else "devel".
func buildVersion() string {
	if version != "" {
		return version
	}

	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}

	return "devel"
}

func ExecClone(ctx context.Context, logger *slog.Logger, args []string) (int, error) {
	fs := flag.NewFlagSet("clone", flag.ExitOnError)

	cfgPath := fs.String("config", "", "path to a JSON config file; flags set on the command line take precedence over it")
	org := fs.String("org", "", "GitHub organization or user that owns the repositories")
	dir := fs.String("dir", ".", "folder the repositories are cloned into, as <dir>/<name>.git")
	repos := fs.String("repos", "", "comma-separated repository names")
	shallow := fs.Bool("shallow", false, "clone only the latest commit of the default branch, without tags")
	parallel := fs.Int("parallel", 1, "number of repositories to clone at the same time")
	user := fs.String("user", "", "GitHub user, together with -token")
	token := fs.String("token", "", "GitHub token, together with -user; prefer -i, which keeps it out of your shell history")
	identity := identityFlag(fs)

	if err := fs.Parse(args); err != nil {
		logger.ErrorContext(ctx, "failed to parse args", slog.String("error", err.Error()))

		return 1, err
	}

	if err := applyConfig(fs, *cfgPath); err != nil {
		logger.ErrorContext(ctx, "failed to load config", slog.String("error", err.Error()))

		return 1, err
	}

	auth, err := newAuth(*identity, *user, *token)
	if err != nil {
		logger.ErrorContext(ctx, "failed to read credentials", slog.String("error", err.Error()))

		return 1, err
	}

	cloneFn := git.Clone
	if *shallow {
		cloneFn = git.ShallowClone
	}

	if err := git.MultiRepo(ctx, *org, expandHome(*dir), strings.Split(*repos, ","), auth, *parallel, cloneFn, logger); err != nil {
		logger.ErrorContext(ctx, "failed to clone repositories", slog.String("error", err.Error()))

		return 1, err
	}

	return 0, nil
}

func ExecPull(ctx context.Context, logger *slog.Logger, args []string) (int, error) {
	fs := flag.NewFlagSet("pull", flag.ExitOnError)

	cfgPath := fs.String("config", "", "path to a JSON config file; flags set on the command line take precedence over it")
	org := fs.String("org", "", "GitHub organization or user that owns the repositories")
	dir := fs.String("dir", ".", "folder the repositories were cloned into, as <dir>/<name>.git")
	repos := fs.String("repos", "", "comma-separated repository names")
	parallel := fs.Int("parallel", 1, "number of repositories to pull at the same time")
	user := fs.String("user", "", "GitHub user, together with -token")
	token := fs.String("token", "", "GitHub token, together with -user; prefer -i, which keeps it out of your shell history")
	identity := identityFlag(fs)

	if err := fs.Parse(args); err != nil {
		logger.ErrorContext(ctx, "failed to parse args", slog.String("error", err.Error()))

		return 1, err
	}

	if err := applyConfig(fs, *cfgPath); err != nil {
		logger.ErrorContext(ctx, "failed to load config", slog.String("error", err.Error()))

		return 1, err
	}

	auth, err := newAuth(*identity, *user, *token)
	if err != nil {
		logger.ErrorContext(ctx, "failed to read credentials", slog.String("error", err.Error()))

		return 1, err
	}

	if err := git.MultiRepo(ctx, *org, expandHome(*dir), strings.Split(*repos, ","), auth, *parallel, git.Pull, logger); err != nil {
		logger.ErrorContext(ctx, "failed to pull repositories", slog.String("error", err.Error()))

		return 1, err
	}

	return 0, nil
}

func ExecList(ctx context.Context, logger *slog.Logger, args []string) (int, error) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)

	cfgPath := fs.String("config", "", "path to a JSON config file; flags set on the command line take precedence over it")
	org := fs.String("org", "", "GitHub organization to list repos from")
	repoType := fs.String("type", "all", "type of repos to list: all, public, private, forks, sources, member")
	skipArchived := fs.Bool("skip-archived", false, "exclude archived repositories")
	token := fs.String("token", "", "GitHub token, needed to see private repositories; prefer -i")
	identity := identityFlag(fs)

	if err := fs.Parse(args); err != nil {
		logger.ErrorContext(ctx, "failed to parse args", slog.String("error", err.Error()))

		return 1, err
	}

	if err := applyConfig(fs, *cfgPath); err != nil {
		logger.ErrorContext(ctx, "failed to load config", slog.String("error", err.Error()))

		return 1, err
	}

	if *token == "" && *identity != "" {
		auth, err := git.ReadCredentials(*identity)
		if err != nil {
			logger.ErrorContext(ctx, "failed to read credentials", slog.String("error", err.Error()))

			return 1, err
		}

		*token = auth.Token
	}

	repos, err := git.ListOrgRepos(ctx, *org, *repoType, *token, logger)
	if err != nil {
		logger.ErrorContext(ctx, "failed to list repositories", slog.String("error", err.Error()))

		return 1, err
	}

	for _, repo := range repos {
		if *skipArchived && repo.Archived {
			continue
		}

		fmt.Println(repo.Name)
	}

	return 0, nil
}

// identityFlag registers -i and -identity as two names for the same flag.
func identityFlag(fs *flag.FlagSet) *string {
	path := new(string)
	usage := "path to a .git-credentials file to take the user and token from, e.g. ~/.git-credentials"

	fs.StringVar(path, "i", "", usage)
	fs.StringVar(path, "identity", "", usage)

	return path
}

// newAuth uses -user and -token when they are set, and the identity file otherwise. It returns nil when neither is set.
func newAuth(identity, user, token string) (*git.Auth, error) {
	if (user == "") != (token == "") {
		return nil, errors.New("-user and -token must be given together")
	}

	if user != "" {
		return &git.Auth{
			Username: user,
			Token:    token,
		}, nil
	}

	if identity == "" {
		return nil, nil
	}

	return git.ReadCredentials(identity)
}

// applyConfig reads the config file at path, if any, and uses its values for the flags that weren't set on the command
// line. Keys for flags this command doesn't have are ignored, so one file can serve every command.
func applyConfig(fs *flag.FlagSet, path string) error {
	if path == "" {
		return nil
	}

	cfg, err := config.Load(path)
	if err != nil {
		return err
	}

	set := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		set[f.Name] = true
	})

	// -i and -identity are the same flag.
	if set["i"] {
		set["identity"] = true
	}

	for name, value := range cfg.Flags() {
		if set[name] || fs.Lookup(name) == nil {
			continue
		}

		if err := fs.Set(name, value); err != nil {
			return fmt.Errorf("config key %q: %w", name, err)
		}
	}

	return nil
}

// expandHome replaces a leading ~/ with the user's home directory, for paths the shell didn't expand: those in the
// config file, or in -dir=~/src.
func expandHome(path string) string {
	rest, ok := strings.CutPrefix(path, "~/")
	if !ok {
		return path
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}

	return filepath.Join(home, rest)
}
