# Publishing and releasing

How to publish KairoCode as open source for individuals (M6) and how to cut a version.

## 1. Check before publishing

- M6's acceptance criteria (`docs/milestones.md`) are done, except the release item. In particular, the "gate before publishing" (checking by hand in Zed and the TUI).
- The README matches the results of the hand check.
- CI (`check`, macOS and Linux) passes on `main`.
- kairo's SDK can be fetched without `replace` (proxy.golang.org has the version of `github.com/r-hashi01/kairo` we use).
- The author address is GitHub's noreply on every commit (rewritten on 2026-10-27).

```sh
git log --format='%ae%n%ce' | sort | uniq -c
# Only 146686591+r-hashi01@users.noreply.github.com, and noreply@github.com as the
# committer of commits merged on GitHub
```

## 2. Publish the repository

In the current (private) repository, the commits from before the rewrite (with the old address) can be reached from the force-push record of PR #12. So we do not make it public as is.

The module path is `github.com/r-hashi01/kairocode`, so the public repository gets this name too (`go install` fetches by this name).

1. Rename the current repository and keep it private (for example `kairocode-dev`). GitHub's Settings → Repository name. Repoint the local `origin` too.

   ```sh
   git remote set-url origin https://github.com/r-hashi01/kairocode-dev.git
   ```

2. Create an empty public repository with the same name, `r-hashi01/kairocode` (do not add a README or LICENSE).
3. Push only `main` and the tags (do not bring PRs, issues or old branches).

   ```sh
   git remote add public https://github.com/r-hashi01/kairocode.git
   git push public main
   git push public --tags
   ```

4. Check that CI passes on `main` in the public repository. In Settings, protect `main` (forbid force pushes).

## 3. Cut a version

A version is a `vMAJOR.MINOR.PATCH` tag (the first is `v0.1.0`).

1. Bring `main` up to date, tag it and push the tag.

   ```sh
   git switch main && git pull
   git tag -a v0.1.0 -m "v0.1.0"
   git push public v0.1.0
   ```

2. `.github/workflows/release.yml` runs.
   - First `check.yml` (macOS and Linux, all checks). If it fails, no release is created.
   - If it passes, `scripts/release-build.sh` builds four tar.gz files for macOS and Linux on amd64 and arm64, `checksums.txt` and the Homebrew formula (`kairocode.rb`), and attaches them to a **draft** release.
3. Open the draft in GitHub's Releases, check the attached files and the generated list of changes, and publish it.

You can build the same thing locally (when you want to check the contents).

```sh
scripts/release-build.sh v0.1.0 dist
```

## 4. Homebrew tap

1. First time only: create the public repository `r-hashi01/homebrew-tap`.
2. Put the `kairocode.rb` attached to the release at the tap's `Formula/kairocode.rb` and push it (replace it for each version).
3. Check it.

   ```sh
   brew install r-hashi01/tap/kairocode
   kairocode version   # v0.1.0
   brew test kairocode
   ```

With Homebrew on Linux, `bubblewrap` is installed along with it (the formula's `depends_on`).

## 5. How to install (write in the README after publishing)

| Method | Command |
|---|---|
| Homebrew (macOS, Linux) | `brew install r-hashi01/tap/kairocode` |
| Go | `go install github.com/r-hashi01/kairocode/cmd/kairocode@latest` (the `kairo` link is not created) |
| Binary | Download the tar.gz from Releases and put `kairocode` and `kairo` on your `PATH` |
| Source | `make build` |

## 6. Things to watch

- **macOS binaries are not signed or notarized.** They only have the ad-hoc signature Go adds to arm64 binaries. What you install with Homebrew or `curl` runs as is. What you download in a browser may be stopped by Gatekeeper (`xattr -d com.apple.quarantine kairocode`). Notarization needs an Apple developer registration, so we will decide on it separately if it becomes necessary.
- **We do not ship for Windows** (there is no sandbox).
- The only permission the release workflow uses is `contents: write` to create the release (`GITHUB_TOKEN`). Pushing to the tap is done by hand, so no other token is needed. To automate it, we would put a fine-grained token that can write to the tap in a secret (to be decided then).
