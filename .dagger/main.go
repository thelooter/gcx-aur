// A Dagger module that maintains the gcx AUR packages.
//
// It can verify that a package builds in a clean Arch container (makepkg),
// discover the latest upstream release, and produce an updated package
// directory (bumped pkgver, refreshed checksums, regenerated .SRCINFO) that is
// only returned once it has been proven to build.
package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"dagger/aur/internal/dagger"
)

const (
	// Upstream project tracked by the source and binary packages.
	ghOwner = "grafana"
	ghRepo  = "gcx"
)

type Aur struct{}

// base returns an Arch container with the toolchain needed to build packages
// and a non-root `builder` user (makepkg refuses to run as root).
func (m *Aur) base() *dagger.Container {
	return dag.Container().
		From("archlinux:base-devel").
		WithExec([]string{"pacman", "-Syu", "--noconfirm", "--needed",
			"git", "go", "pacman-contrib", "curl"}).
		WithExec([]string{"useradd", "-m", "builder"}).
		WithExec([]string{"sh", "-c",
			"echo 'builder ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/builder"}).
		WithUser("builder").
		WithWorkdir("/home/builder")
}

// pkgDir mounts the package folder into the base container, owned by builder.
func (m *Aur) pkgDir(src *dagger.Directory, pkg string) *dagger.Container {
	return m.base().
		WithMountedDirectory("/src", src.Directory(pkg)).
		WithExec([]string{"sudo", "cp", "-aT", "/src", "/home/builder/pkg"}).
		WithExec([]string{"sudo", "chown", "-R", "builder:builder", "/home/builder/pkg"}).
		WithWorkdir("/home/builder/pkg")
}

// LatestRelease returns the newest upstream release tag with any leading "v"
// stripped (e.g. "0.4.0"). A GitHub token can be supplied to dodge the
// unauthenticated API rate limit.
func (m *Aur) LatestRelease(
	ctx context.Context,
	// GitHub token used to authenticate the API request.
	// +optional
	token *dagger.Secret,
) (string, error) {
	c := dag.Container().From("alpine:3.20").
		WithExec([]string{"apk", "add", "--no-cache", "curl", "jq"})

	cmd := "curl -fsSL"
	if token != nil {
		c = c.WithSecretVariable("GH_TOKEN", token)
		cmd += ` -H "Authorization: Bearer $GH_TOKEN"`
	}
	cmd += fmt.Sprintf(" https://api.github.com/repos/%s/%s/releases/latest | jq -er .tag_name",
		ghOwner, ghRepo)

	out, err := c.WithExec([]string{"sh", "-c", cmd}).Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("querying latest release: %w", err)
	}
	return strings.TrimPrefix(strings.TrimSpace(out), "v"), nil
}

// pkgField extracts a scalar PKGBUILD variable (pkgver, pkgrel, ...),
// tolerating the quoting/spacing/comment styles makepkg accepts
// (e.g. `pkgver=1.3.0`, `pkgver="1.3.0"`, `pkgver='1.3.0' # comment`).
func pkgField(content, name string) (string, error) {
	re := regexp.MustCompile(`(?m)^\s*` + name + `\s*=\s*['"]?([^'"\s#]+)['"]?`)
	match := re.FindStringSubmatch(content)
	if match == nil {
		return "", fmt.Errorf("no %s= line found in PKGBUILD", name)
	}
	return strings.TrimSpace(match[1]), nil
}

// CurrentPkgver reads the pkgver currently declared in a package's PKGBUILD.
func (m *Aur) CurrentPkgver(ctx context.Context, src *dagger.Directory, pkg string) (string, error) {
	content, err := src.File(pkg + "/PKGBUILD").Contents(ctx)
	if err != nil {
		return "", err
	}
	ver, err := pkgField(content, "pkgver")
	if err != nil {
		return "", fmt.Errorf("%s: %w", pkg, err)
	}
	return ver, nil
}

// CommitMessage builds the CI commit message for a comma-separated list of
// changed packages (e.g. "gcx,gcx-bin"), reading each version straight from
// its working-tree PKGBUILD: "ci: update gcx packages (gcx 1.4.0-1)".
// Old versions are deliberately absent — the CI runner owns git state (it
// knows what changed); this function owns parsing and formatting, which is
// where the quoting/comment edge cases live and where unit tests can reach.
func (m *Aur) CommitMessage(ctx context.Context, src *dagger.Directory, pkgs string) (string, error) {
	parts := []string{}
	for _, pkg := range strings.Split(pkgs, ",") {
		pkg = strings.TrimSpace(pkg)
		if pkg == "" {
			continue
		}
		content, err := src.File(pkg + "/PKGBUILD").Contents(ctx)
		if err != nil {
			return "", err
		}
		ver, err := pkgField(content, "pkgver")
		if err != nil {
			return "", fmt.Errorf("%s: %w", pkg, err)
		}
		rel, err := pkgField(content, "pkgrel")
		if err != nil {
			return "", fmt.Errorf("%s: %w", pkg, err)
		}
		parts = append(parts, fmt.Sprintf("%s %s-%s", pkg, ver, rel))
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("no packages given (expected e.g. %q)", "gcx,gcx-bin")
	}
	return fmt.Sprintf("ci: update gcx packages (%s)", strings.Join(parts, ", ")), nil
}

// Build runs `makepkg` for one package in a clean Arch container, installing
// dependencies as needed. It returns the build log and errors if the build (or
// the package's check()) fails — this is the gate the auto-updater relies on.
func (m *Aur) Build(ctx context.Context, src *dagger.Directory, pkg string) (string, error) {
	return m.pkgDir(src, pkg).
		WithExec([]string{"makepkg", "--noconfirm", "--syncdeps", "--cleanbuild", "--force"}).
		Stdout(ctx)
}

// Bump produces an updated copy of a package folder for a new version: it sets
// pkgver, resets pkgrel to 1, refreshes the checksums from upstream, and
// regenerates .SRCINFO. It does NOT build — use Update for the verified path.
func (m *Aur) Bump(
	ctx context.Context,
	src *dagger.Directory,
	pkg string,
	version string,
) (*dagger.Directory, error) {
	c := m.pkgDir(src, pkg).
		WithExec([]string{"sed", "-i",
			"-e", fmt.Sprintf("s/^[[:space:]]*pkgver[[:space:]]*=.*/pkgver=%s/", version),
			"-e", "s/^[[:space:]]*pkgrel[[:space:]]*=.*/pkgrel=1/",
			"PKGBUILD"})

	switch pkg {
	case "gcx-bin":
		// Pull the official checksums file and slot the per-arch tarball sums
		// straight in. Computing them by download would skip the foreign arch
		// on a single-arch runner, so we trust upstream's checksum list
		// instead. Trust assumption: the list is fetched over TLS from the
		// same GitHub release as the tarballs, so a compromised release
		// would defeat this anyway — there is no separate signature check.
		// Match the full tarball filename (not just the arch substring) so a
		// renamed/new asset (e.g. .deb, .zip) fails loudly instead of slotting
		// the wrong hash. Matched lines are echoed for the build log.
		script := fmt.Sprintf(`set -euo pipefail
url=https://github.com/%s/%s/releases/download/v%s/gcx_%s_checksums.txt
echo "Fetching $url"
sums=$(curl -fsSL "$url")
printf '%%s\n' "$sums"
amd=$(printf '%%s\n' "$sums" | awk '$2 ~ /gcx_.*_linux_amd64\.tar\.gz$/ {print $1; exit}')
arm=$(printf '%%s\n' "$sums" | awk '$2 ~ /gcx_.*_linux_arm64\.tar\.gz$/ {print $1; exit}')
if [ -z "$amd" ] || [ -z "$arm" ]; then
  echo "ERROR: expected linux_amd64 and linux_arm64 tarball lines in $url" >&2
  printf '%%s\n' "$sums" >&2
  exit 1
fi
echo "linux_amd64: $amd"
echo "linux_arm64: $arm"
sed -i "s/^sha256sums_x86_64=.*/sha256sums_x86_64=('$amd')/"  PKGBUILD
sed -i "s/^sha256sums_aarch64=.*/sha256sums_aarch64=('$arm')/" PKGBUILD`,
			ghOwner, ghRepo, version, version)
		c = c.WithExec([]string{"bash", "-c", script})

	case "gcx":
		// Single source tarball: hash it directly, no build artifacts left behind.
		script := fmt.Sprintf(`set -euo pipefail
url=https://github.com/%s/%s/archive/refs/tags/v%s.tar.gz
echo "Hashing $url"
sum=$(curl -fsSL "$url" | tee /tmp/src.tar.gz | sha256sum | awk '{print $1}')
test -n "$sum"
echo "sha256: $sum ($(wc -c < /tmp/src.tar.gz) bytes)"
rm -f /tmp/src.tar.gz
sed -i "s/^sha256sums=.*/sha256sums=('$sum')/" PKGBUILD`,
			ghOwner, ghRepo, version)
		c = c.WithExec([]string{"bash", "-c", script})

	default:
		return nil, fmt.Errorf("Bump does not support package %q (VCS packages are not version-bumped)", pkg)
	}

	c = c.WithExec([]string{"bash", "-c", "makepkg --printsrcinfo > .SRCINFO"})
	return c.Directory("/home/builder/pkg"), nil
}

// Update is the full auto-update path for a release-tracking package (gcx or
// gcx-bin). It compares the current pkgver against the latest upstream release;
// if they match it returns the package unchanged. Otherwise it bumps, then
// builds the bumped package to verify it, and only returns the updated folder
// when that build succeeds. The returned directory should be exported over the
// package folder.
func (m *Aur) Update(
	ctx context.Context,
	src *dagger.Directory,
	pkg string,
	// GitHub token used to authenticate the release lookup.
	// +optional
	token *dagger.Secret,
) (*dagger.Directory, error) {
	latest, err := m.LatestRelease(ctx, token)
	if err != nil {
		return nil, err
	}
	current, err := m.CurrentPkgver(ctx, src, pkg)
	if err != nil {
		return nil, err
	}
	if latest == current {
		// Up to date: hand back the folder unchanged so an export is a no-op.
		return src.Directory(pkg), nil
	}

	bumped, err := m.Bump(ctx, src, pkg, latest)
	if err != nil {
		return nil, fmt.Errorf("bumping %s to %s: %w", pkg, latest, err)
	}

	// Verify the bumped package actually builds before we hand it back.
	verifySrc := src.WithDirectory(pkg, bumped)
	if _, err := m.Build(ctx, verifySrc, pkg); err != nil {
		return nil, fmt.Errorf("%s %s failed verification build: %w", pkg, latest, err)
	}
	return bumped, nil
}
