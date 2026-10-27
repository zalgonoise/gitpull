# gitpull

Clone and update many GitHub repositories at once: every repository of an organization, or the set you keep in a
config file. One command brings a whole organization's code onto your machine, another keeps it up to date, several
repositories at a time. Written in Go on [go-git](https://github.com/go-git/go-git), so it needs no `git` binary.

```sh
gitpull list  -org acme -i ~/.git-credentials          # what the organization has
gitpull clone -config acme.json -shallow -parallel 8    # clone it, skipping what is already cloned
gitpull pull  -config acme.json -parallel 8             # catch up later
```

## What you can use it for

- **Get an organization onto a new machine.** `gitpull list` reads every repository of an organization from the GitHub
  API, and `gitpull clone` clones them in parallel, shallow if you only need the latest code.
- **Keep many clones up to date.** `gitpull pull` fast-forwards every repository from its `origin`. Repositories that
  are already up to date are not errors.
- **Keep the repositories you work with in one file.** A JSON config holds the organization, the folder, the
  repositories, the credentials file and the number of workers. Flags override it for a single run. Add repositories
  to it and run `clone` again: the ones already cloned are skipped.
- **Script around it.** `list` prints one name per line on stdout, and the logs are JSON on stderr, so both pipe
  straight into `jq`, `paste` or your own scripts.

## How it works

- **Repositories are named, not linked.** Each name becomes `https://github.com/<org>/<name>.git` and is cloned into
  `<dir>/<name>.git`. Only the last part of a name counts, and the `.git` suffix is optional, so `api`, `api.git`,
  `acme/api` and `https://github.com/acme/api.git` are the same repository. Blank names and repeats are ignored.
- **`clone`** fetches every branch and tag, then checks out the default branch and its submodules (up to 10 levels
  deep). With `-shallow` it fetches only the latest commit of the default branch, without tags, and clones submodules
  the same way. A folder that already holds a git repository is skipped, and empty repositories clone fine.
- **`pull`** opens `<dir>/<name>.git`, fetches from `origin` and fast-forwards the checked-out branch. It never
  overwrites your work: a branch that has diverged from `origin`, or a folder with uncommitted changes, is reported as
  an error and left as it is.
- **`-parallel N`** starts N workers that share the list, each repository going to exactly one of them. A failure does
  not stop the others: it is logged as it happens, and the run ends with a summary such as `2 of 120 repositories
  failed` and exit status 1.
- **Ctrl-C stops cleanly.** The clones and pulls in progress are cancelled, a half-done clone is removed, and the
  repositories not started yet are skipped, so the next run starts from a consistent state.
- **`list`** calls the GitHub REST API (`GET /orgs/{org}/repos`), 100 repositories per page, until it reaches the last
  page.
- **Credentials** go to `clone` and `pull` as HTTP basic auth (user and token) and to `list` as a bearer token. They
  are never logged.

## Install

```sh
go install github.com/zalgonoise/gitpull/cmd/gitpull@latest
```

This needs Go 1.27.1 or newer. From a clone of this repository, `make build` writes the binary to `tmp/bin/gitpull`,
and `make release` builds it for macOS and Linux (see [Development](#development)). `gitpull version` prints the
version you have.

## Getting started

1. **Create a GitHub token** that can read the organization's repositories: a classic token with the `repo` scope, or
   a fine-grained token with read access to *Contents* and *Metadata*. If the organization uses SSO, authorize the
   token for it in your GitHub token settings.

2. **Store it in a credentials file**, in the format `git config credential.helper store` uses:

   ```sh
   echo 'https://<your-user>:<your-token>@github.com' >> ~/.git-credentials
   chmod 600 ~/.git-credentials
   ```

3. **See what the organization has:**

   ```sh
   gitpull list -org acme -i ~/.git-credentials -skip-archived
   ```

4. **Save it as a config file**, so you don't have to repeat it:

   ```sh
   gitpull list -org acme -i ~/.git-credentials -skip-archived \
     | jq -R . | jq -s '{org: "acme", dir: "~/src/acme", identity: "~/.git-credentials", parallel: 8, repos: .}' \
     > acme.json
   ```

5. **Clone everything, then pull whenever you want to catch up:**

   ```sh
   gitpull clone -config acme.json -shallow
   gitpull pull -config acme.json
   ```

## Commands

```
gitpull <command> [flags]
```

| Command | What it does |
|---|---|
| `clone` | Clone repositories into a folder, skipping the ones already cloned |
| `pull` | Fast-forward cloned repositories from their `origin` |
| `list` | Print the names of a GitHub organization's repositories |
| `version` | Print the version |
| `help` | Print the commands |

`gitpull <command> -h` lists a command's flags. Every flag with a config key can also come from the
[config file](#config-file).

### `clone`

Clones each repository into `<dir>/<name>.git`. A folder that already holds a git repository is skipped; a folder that
holds anything else is reported as an error and left untouched.

| Flag | Default | Config key | What it does |
|---|---|---|---|
| `-org` | | `org` | GitHub organization or user that owns the repositories (required) |
| `-repos` | | `repos` | Comma-separated repository names (required) |
| `-dir` | `.` | `dir` | Folder the repositories are cloned into |
| `-shallow` | off | `shallow` | Only the latest commit of the default branch, without tags |
| `-parallel` | `1` | `parallel` | How many repositories to clone at the same time |
| `-i`, `-identity` | | `identity` | A `.git-credentials` file to read the user and token from |
| `-user`, `-token` | | | User and token, always together; they take precedence over `-i` |
| `-config` | | | A JSON [config file](#config-file) |

### `pull`

Fast-forwards each repository in `<dir>/<name>.git` from `origin`. It takes the same flags as `clone`, except
`-shallow`: a shallow clone stays shallow and only fetches the new commits.

### `list`

Prints the name of every repository of an organization, one per line, on stdout.

| Flag | Default | Config key | What it does |
|---|---|---|---|
| `-org` | | `org` | GitHub organization to list (required) |
| `-type` | `all` | | `all`, `public`, `private`, `forks`, `sources` or `member` |
| `-skip-archived` | off | | Leave out archived repositories |
| `-token` | | | Token for the GitHub API; takes precedence over `-i` |
| `-i`, `-identity` | | `identity` | A `.git-credentials` file; only its token is used |
| `-config` | | | A JSON [config file](#config-file) |

Without a token, `list` only sees public repositories, and GitHub allows 60 unauthenticated requests per hour.

### Exit status

| Status | Meaning |
|---|---|
| `0` | Every repository succeeded (already cloned and already up to date count as success) |
| `1` | At least one repository failed, or the run couldn't start: an unknown command, no organization or repositories, an unreadable config or credentials file, a failed API call |
| `2` | No command, or invalid flags |

## Config file

Every command takes `-config <file>`, a JSON file whose keys are named after the flags they stand in for:

```json
{
  "org": "acme",
  "dir": "~/src/acme",
  "identity": "~/.git-credentials",
  "parallel": 8,
  "shallow": true,
  "repos": [
    "api",
    "web",
    "infra"
  ]
}
```

| Key | Type | Flag |
|---|---|---|
| `org` | string | `-org` |
| `dir` | string | `-dir` |
| `repos` | list of strings | `-repos` |
| `identity` | string | `-i`, `-identity` |
| `parallel` | number | `-parallel` |
| `shallow` | boolean | `-shallow` |

- **Flags on the command line win** over the file, and the file wins over the defaults. For example,
  `gitpull pull -config acme.json -repos api` pulls only `api`, with everything else from the file.
- **One file serves every command**: a command ignores the keys it has no flag for, like `list` with `dir`.
- **Unknown keys are an error**, so a misspelled key fails loudly instead of being ignored.
- **`~/` is expanded** in `dir` and `identity`, since no shell expands it inside a JSON file.
- To skip a key for one run, set its flag to an empty value, such as `-identity ""` to run without credentials.

## Credentials

`clone` and `pull` use the first of these that is set:

1. `-user` and `-token`. They go together: one without the other is an error.
2. The credentials file given with `-i` / `-identity` (or the `identity` key).
3. Nothing: only public repositories can be cloned.

`list` uses `-token` if it is given, and otherwise the token from the credentials file.

The credentials file is the one `git config credential.helper store` writes: one URL per line, such as
`https://user:token@github.com`. gitpull uses the first `https` line for `github.com` that has both a user and a
token, ignores lines for other hosts and lines it can't parse, and decodes percent-encoded characters (`%40` for
`@`).

Prefer `-i` over `-token`: a token typed on the command line ends up in your shell history and is visible to other
processes while gitpull runs.

## Logs

gitpull logs to stderr, one JSON object per line, so the output of `list` on stdout stays clean:

```json
{"time":"2026-10-27T01:59:49.628+01:00","level":"INFO","source":{"function":"github.com/zalgonoise/gitpull/git.clone","file":".../git/git.go","line":87},"msg":"cloned remote","uri":"https://github.com/acme/api.git","branch":"main","commit":"7b22e41dc674a3c24f644f86050317360a923fe1"}
```

Each repository logs one of these messages, with its `uri`:

| Level | Message | When |
|---|---|---|
| INFO | `cloning repository` | A clone starts (with `path` and `shallow`) |
| INFO | `cloned remote` | A clone is done (with `branch` and `commit`) |
| INFO | `cloned empty repository` | The repository has no commits yet |
| INFO | `repository already cloned` | `clone` found a repository in the folder and skipped it |
| INFO | `pulled from remote` | `pull` brought new commits (with `branch` and `commit`) |
| INFO | `repository already up-to-date` | `pull` found nothing new |
| ERROR | `repository failed` | The repository failed (with `error`); the others go on |

A run with failures ends with `ERROR` lines holding the summary, such as `1 of 2 repositories failed`. When the
context carries an OpenTelemetry span, each line also gets its `trace_id` and `span_id`.

For a readable summary, filter the logs through `jq`:

```sh
gitpull pull -config acme.json 2>&1 | jq -r '[.time[11:19], .level, .msg, .uri // "", .error // ""] | join("  ")'
```

## Recipes

**Clone every repository of an organization without a config file:**

```sh
gitpull clone -org acme -i ~/.git-credentials -shallow -parallel 8 \
  -repos "$(gitpull list -org acme -i ~/.git-credentials -skip-archived | paste -sd, -)"
```

**Refresh the repository list of a config file**, keeping its other keys, then clone the new ones:

```sh
jq --argjson repos "$(gitpull list -config acme.json -skip-archived | jq -R . | jq -s .)" '.repos = $repos' \
  acme.json > acme.json.new && mv acme.json.new acme.json
gitpull clone -config acme.json
```

**Pull only a few repositories from a config file:**

```sh
gitpull pull -config acme.json -repos api,web
```

**See only what failed:**

```sh
gitpull pull -config acme.json 2>&1 | jq -c 'select(.msg == "repository failed") | {uri, error}'
```

## Limitations

- **GitHub only**, over HTTPS: repository URLs and the API are `github.com`'s, and SSH keys are not used.
- **`list` works for organizations**, not personal accounts, because it calls `/orgs/{org}/repos`. `clone` and `pull`
  work for both.
- **`pull` only fast-forwards.** Branches that diverge from `origin`, and folders with uncommitted changes, are
  reported and left for you to sort out with `git`.
- **`pull` follows each clone's own `origin`**: a folder cloned from somewhere else is pulled from there.

## Layout

```
cmd/gitpull/       the CLI: one function per command, its flags, and how the config file fills them in
git/               clone, pull, the worker pool, the credentials file and the GitHub API listing
internal/config/   reading the JSON config file
internal/log/      the JSON logger (log/slog), with OpenTelemetry trace and span IDs
```

## Development

```sh
make test      # gofmt, go vet and the tests
make cover     # statement coverage of the whole suite; fails under COVER_MIN (60%)
make audit     # fails if any file contains a word from DENYLIST (local/denylist.txt by default)
make build     # the binary, in tmp/bin/gitpull
make release   # binaries for macOS and Linux (amd64, arm64) in bin/, with SHA256SUMS; VERSION=v1.2.0 names them
```

The tests need nothing beyond Go: clones and pulls run against local repositories created by the tests, and `list`
against a fake GitHub API. `.github/workflows/test.yml` runs `make test` and `make cover` on every push and pull
request.

`make audit` is for keeping private names out of the repository. List the words that must never be published, one
per line, in a file outside it (`local/` is gitignored), and run it before every push.

## License

MIT; see [LICENSE](LICENSE).
