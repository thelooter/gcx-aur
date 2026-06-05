#!/usr/bin/env bash
# Sync each package folder in this monorepo to its own AUR repository.
#
# For every package it clones the matching ssh://aur@aur.archlinux.org/<pkg>.git,
# copies the tracked package files in, and pushes only when something actually
# changed. Safe to run every CI invocation — unchanged packages are a no-op.
#
# Usage: scripts/aur-sync.sh [pkg ...]   (defaults to all three gcx packages)
set -euo pipefail

packages=("$@")
if [[ ${#packages[@]} -eq 0 ]]; then
	packages=(gcx gcx-bin gcx-git)
fi

repo_root="$(git rev-parse --show-toplevel)"
failed=0

for pkg in "${packages[@]}"; do
	echo "::group::AUR sync ${pkg}"
	src="${repo_root}/${pkg}"

	if [[ ! -f "${src}/PKGBUILD" ]]; then
		echo "skip: ${src} has no PKGBUILD"
		echo "::endgroup::"
		continue
	fi

	work="$(mktemp -d)"
	if ! git clone --quiet "ssh://aur@aur.archlinux.org/${pkg}.git" "${work}"; then
		echo "warning: could not clone AUR repo for ${pkg}"
		echo "  (create it with an initial push, and make sure the SSH key is authorized)"
		rm -rf "${work}"
		failed=1
		echo "::endgroup::"
		continue
	fi

	# Copy the tracked package files. PKGBUILD and .SRCINFO are mandatory; the
	# rest are optional and only copied if present.
	cp "${src}/PKGBUILD" "${src}/.SRCINFO" "${work}/"
	[[ -f "${src}/.gitignore" ]] && cp "${src}/.gitignore" "${work}/"
	shopt -s nullglob
	for extra in "${src}"/*.install "${src}"/*.sysusers "${src}"/*.tmpfiles; do
		cp "${extra}" "${work}/"
	done
	shopt -u nullglob

	(
		cd "${work}"
		git add -A
		if git diff --cached --quiet; then
			echo "${pkg}: AUR already up to date"
			exit 0
		fi
		ver="$(awk -F= '/^pkgver=/{print $2; exit}' PKGBUILD)"
		rel="$(awk -F= '/^pkgrel=/{print $2; exit}' PKGBUILD)"
		git commit --quiet -m "upgpkg: ${pkg} ${ver}-${rel}"
		git push origin master
		echo "${pkg}: pushed ${ver}-${rel} to the AUR"
	) || failed=1

	rm -rf "${work}"
	echo "::endgroup::"
done

exit "${failed}"
