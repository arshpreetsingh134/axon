# Publishing axon to Homebrew (free)

This uses a public GitHub repo for axon, a public **tap** repo, GoReleaser, and
GitHub Actions. All of it is free for public repositories.

```
tag v0.1.0 ──► GitHub Actions (.github/workflows/release.yml)
                 └─ GoReleaser (.goreleaser.yaml)
                      ├─ builds darwin/linux × amd64/arm64
                      ├─ uploads archives + checksums to a GitHub Release
                      └─ commits Formula/axon.rb to <owner>/homebrew-tap
users: brew install <owner>/tap/axon
```

## One-time setup

1. **Push axon to GitHub** as a public repo, e.g. `github.com/<owner>/axon`.

   ```bash
   cd ~/Repositories/axon
   git add -A && git commit -m "axon 0.1.0"
   gh repo create <owner>/axon --public --source . --push
   ```

2. **Create the tap repo.** It must be named `homebrew-tap` so that
   `brew install <owner>/tap/axon` resolves.

   ```bash
   gh repo create <owner>/homebrew-tap --public --add-readme
   ```

3. **Create a token that can push to the tap.**
   1. Go to GitHub → Settings → Developer settings → Fine-grained tokens.
   2. Scope it to the repository `homebrew-tap`.
   3. Give it permission **Contents: Read and write**.

   The workflow's built-in `GITHUB_TOKEN` can only push to the axon repo itself, which
   is why the tap needs its own token.

4. **Add the repo secrets** (axon repo → Settings → Secrets and variables → Actions):

   | Secret | Value |
   |---|---|
   | `HOMEBREW_TAP_GITHUB_TOKEN` | the token from step 3 |
   | `HOMEBREW_COMMIT_EMAIL` | the email shown as the formula's commit author |

   The tap owner comes from `github.repository_owner` automatically.

5. **Optional: try a local dry run first.** This builds every archive into `./dist`
   without publishing anything.

   ```bash
   brew install goreleaser
   HOMEBREW_TAP_OWNER=<owner> HOMEBREW_COMMIT_EMAIL=you@example.com make snapshot
   ```

## Every release

```bash
git tag v0.1.0
git push origin v0.1.0
```

Watch the `release` workflow in the Actions tab. When it finishes, the tap has
`Formula/axon.rb`. Users then install and upgrade with:

```bash
brew install <owner>/tap/axon
brew upgrade axon
axon --version     # axon 0.1.0, then commit, build date and Go version
```

The formula installs the binary and the bash, zsh and fish completions.

## Notes

- **Version stamping:** the version comes from the tag, via ldflags into
  `axon/internal/commands.Version`.
- **No Apple code signing needed.** Homebrew formulas install from the GitHub Release
  archive, so the binary isn't quarantined the way a downloaded `.app` is.
- **Private repos:** to keep axon private, the tap and the release assets must still be
  readable by users. For a private distribution, have users set `HOMEBREW_GITHUB_API_TOKEN`
  instead.
- **Inclusion in homebrew-core** (`brew install axon` with no tap) needs these, then a PR
  to `Homebrew/homebrew-core`:
  - an open-source licence;
  - a stable, notable project;
  - a name that doesn't clash with an existing formula.
- **Licence:** axon is Apache-2.0 (`LICENSE`, `NOTICE`), and the formula declares
  `license "Apache-2.0"`. That meets the licence requirement for homebrew-core.
