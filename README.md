# gcx AUR packages

Arch Linux packaging for [`gcx`](https://github.com/grafana/gcx) — Grafana's CLI
for managing Grafana Cloud resources (the successor to the deprecated
`grafanactl`). This repository is a monorepo: each subfolder is one AUR package,
and a Dagger-powered GitHub Actions workflow keeps them in sync with upstream
automatically.

## Packages

| Folder     | AUR name  | What it is                                                              |
|------------|-----------|------------------------------------------------------------------------|
| `gcx/`     | `gcx`     | Builds the latest tagged release **from source** with the Go toolchain |
| `gcx-bin/` | `gcx-bin` | Installs the official prebuilt release binary (no compilation)         |
| `gcx-git/` | `gcx-git` | Builds the **`main` branch** from git (VCS package)                    |

All three install the binary to `/usr/bin/gcx`. The `gcx-bin` and `gcx-git`
variants declare `provides` / `conflicts=('gcx')` (so only one may be
installed at a time — `gcx-bin` pins it as `provides=("gcx=${pkgver}")`); the `gcx` package itself needs neither, since it *is*
`gcx`. All three ship the Apache-2.0 license and upstream docs.

### Shell completions

`gcx` is Cobra-based, so it exposes a `completion` subcommand. Every package
generates completions **from the exact binary being packaged** and installs them
to the standard vendor locations, so they are picked up automatically with no
user action:

- bash → `/usr/share/bash-completion/completions/gcx`
- zsh  → `/usr/share/zsh/site-functions/_gcx`
- fish → `/usr/share/fish/vendor_completions.d/gcx.fish`

There is no man-page generation: upstream does not wire up `cobra/doc`, so no
`gcx man`/`doc` command exists to generate them from. Completions are the only
shell integration upstream offers, and all three packages install them.

## Auto-update CI

`.github/workflows/update.yml` runs **daily (04:17 UTC) and on demand**
(`workflow_dispatch`). The heavy lifting lives in a Go [Dagger](https://dagger.io)
module under `.dagger/`, so every build runs in a clean, reproducible Arch
container — identical locally and in CI.

Each run has three jobs:

1. **`update` (matrix: `gcx`, `gcx-bin`)** — each leg checks upstream for the
   latest release tag and, if newer than the packaged `pkgver`, bumps `pkgver`,
   resets `pkgrel`, refreshes the checksums (source tarball hash for `gcx`;
   the official per-arch `checksums.txt` for `gcx-bin`), regenerates
   `.SRCINFO`, and **verifies the bump builds** with `makepkg` in the
   container. A bump is only accepted if it builds — a broken release can
   never be published. `fail-fast: false`, so one broken package never blocks
   the other; each leg exports its result as an artifact.
2. **`git-check`** — build-checks `gcx-git` against `main` so upstream
   breakage surfaces early.
3. **`publish`** (runs even if 1–2 failed) — overlays whichever artifacts
   succeeded, **commits** them with a versioned message (built by
   `dagger call commit-message`), and **pushes each changed package to its
   AUR repository**. A final gate then fails the run if anything broke, so
   failures are reported but never block healthy packages.

Orchestration lives in the Dagger module; the workflow file itself is
deliberately thin (checkout, toolchain, secrets, artifacts, push).

### Dagger functions

Run any of these locally (requires the [Dagger CLI](https://docs.dagger.io/install);
run `dagger develop` once after cloning to generate the SDK bindings):

```bash
dagger functions                                          # list everything
dagger call latest-release                                # newest upstream tag
dagger call current-pkgver --src=. --pkg=gcx              # packaged version
dagger call build         --src=. --pkg=gcx-bin           # build in a clean container
dagger call bump          --src=. --pkg=gcx --version=X   # bump + refresh sums + .SRCINFO
dagger call commit-message --src=. --pkgs=gcx,gcx-bin     # CI commit message for changed pkgs
dagger call update        --src=. --pkg=gcx \
  export --path=./gcx                                     # full verified update-in-place
```

`update` returns the package folder unchanged when it is already current, and
errors (publishing nothing) if the bumped package fails to build.

### Required configuration

**Secret:**

- `AUR_SSH_KEY` — the **private** SSH key whose public half is registered on the
  [AUR account](https://aur.archlinux.org/account) that maintains these packages.

**Variable (opt-in publishing):**

- `AUR_PUBLISH` — AUR pushes are **off until you set this to `true`**. While it is
  unset, CI still checks upstream, builds, and commits version bumps to this
  repo, but never touches the AUR. So pushing to GitHub (or the first scheduled
  run) cannot publish anything until you explicitly opt in. Set it under
  *Settings → Secrets and variables → Actions → Variables*.

`GITHUB_TOKEN` is provided automatically and is used only to raise the GitHub API
rate limit for the release check. Note the workflow has **no `push` trigger** — it
runs only on schedule and manual dispatch.

Pin the Dagger version in two places that must match: `engineVersion` in
`dagger.json` and `DAGGER_VERSION` in the workflow. `dagger develop` reconciles
the module to whichever CLI is installed.

## Manual use

Build and install any package directly:

```bash
cd gcx        # or gcx-bin / gcx-git
makepkg -si
```

Push to the AUR by hand (the CI does this for you, but for the first ever push or
ad-hoc fixes):

```bash
scripts/aur-sync.sh gcx        # sync one package
scripts/aur-sync.sh            # sync all three
```

`aur-sync.sh` clones each `ssh://aur@aur.archlinux.org/<pkg>.git`, copies the
tracked files in, and pushes only when something changed. The first push for a
brand-new package name creates the AUR repository.

## Layout

```
.
├── gcx/            # source package        (PKGBUILD, .SRCINFO)
├── gcx-bin/        # binary package        (PKGBUILD, .SRCINFO)
├── gcx-git/        # VCS package           (PKGBUILD, .SRCINFO)
├── .dagger/        # Dagger Go module (the CI logic)
├── dagger.json     # Dagger module config
├── scripts/
│   └── aur-sync.sh # push package folders to their AUR repos
└── .github/workflows/update.yml
```
