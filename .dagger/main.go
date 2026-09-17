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

// pkgverRe tolerates the quoting/spacing/comment styles makepkg accepts
// (e.g. `pkgver=1.3.0`, `pkgver="1.3.0"`, `pkgver='1.3.0' # comment`).
var pkgverRe = regexp.MustCompile(`(?m)^\s*pkgver\s*=\s*['"]?([^'"\s#]+)['"]?`)

// CurrentPkgver reads the pkgver currently declared in a package's PKGBUILD.
func (m *Aur) CurrentPkgver(ctx context.Context, src *dagger.Directory, pkg string) (string, error) {
	content, err := src.File(pkg + "/PKGBUILD").Contents(ctx)
	if err != nil {
		return "", err
	}
	match := pkgverRe.FindStringSubmatch(content)
	if match == nil {
		return "", fmt.Errorf("no pkgver= line found in %s/PKGBUILD", pkg)
	}
	return strings.TrimSpace(match[1]), nil
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
		// Pull the official checksums file and slot the per-arch sums straight
		// in. Computing them by download would skip the foreign arch on an
		// amd64 runner, so we trust upstream's signed checksum list instead.
		script := fmt.Sprintf(`set -euo pipefail
sums=$(curl -fsSL https://github.com/%s/%s/releases/download/v%s/gcx_%s_checksums.txt)
amd=$(printf '%%s\n' "$sums" | awk '/linux_amd64/{print $1}')
arm=$(printf '%%s\n' "$sums" | awk '/linux_arm64/{print $1}')
test -n "$amd" && test -n "$arm"
sed -i "s/^sha256sums_x86_64=.*/sha256sums_x86_64=('$amd')/"  PKGBUILD
sed -i "s/^sha256sums_aarch64=.*/sha256sums_aarch64=('$arm')/" PKGBUILD`,
			ghOwner, ghRepo, version, version)
		c = c.WithExec([]string{"bash", "-c", script})

	case "gcx":
		// Single source tarball: hash it directly, no build artifacts left behind.
		script := fmt.Sprintf(`set -euo pipefail
sum=$(curl -fsSL https://github.com/%s/%s/archive/refs/tags/v%s.tar.gz | sha256sum | awk '{print $1}')
test -n "$sum"
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
