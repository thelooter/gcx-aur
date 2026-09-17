#!/usr/bin/env bash
# Sync each package folder in this monorepo to its own AUR repository.
#
# For every package it clones the matching ssh://aur@aur.archlinux.org/<pkg>.git,
# copies the tracked package files in, and pushes only when something actually
# changed. Safe to run every CI invocation — unchanged packages are a no-op.
#
# Usage: scripts/aur-sync.sh [-n|--dry-run] [pkg ...]   (defaults to all three gcx packages)
set -euo pipefail

usage() {
	cat <<'EOF'
Usage: scripts/aur-sync.sh [-n|--dry-run] [-h|--help] [pkg ...]

Sync one or more package folders to their AUR repositories.
Defaults to all three gcx packages. With --dry-run, show what would
change without committing or pushing anything.
EOF
}

dry_run=0
packages=()
for arg in "$@"; do
	case "${arg}" in
	-n | --dry-run)
		dry_run=1
		;;
	-h | --help)
		usage
		exit 0
		;;
	-*)
		echo "unknown option: ${arg}" >&2
		usage >&2
		exit 2
		;;
	*)
		packages+=("${arg}")
		;;
	esac
done
if [[ ${#packages[@]} -eq 0 ]]; then
	packages=(gcx gcx-bin gcx-git)
fi

repo_root="$(git rev-parse --show-toplevel)"
failed=0

# Track temp dirs so SIGINT/ERR never leaves clones behind.
tmpdirs=()
cleanup() {
	for d in "${tmpdirs[@]:-}"; do
		[[ -n "${d}" && -d "${d}" ]] && rm -rf "${d}"
	done
}
trap cleanup EXIT

# Read a variable from a PKGBUILD by sourcing it in a clean subshell.
# More robust than grepping: tolerates quotes, spacing and comments.
pkgvar() {
	bash -c 'source "$1"; printf "%s" "${!2}"' bash "$1" "$2"
}

for pkg in "${packages[@]}"; do
	echo "::group::AUR sync ${pkg}"
	src="${repo_root}/${pkg}"

	if [[ ! -f "${src}/PKGBUILD" ]]; then
		echo "skip: ${src} has no PKGBUILD"
		echo "::endgroup::"
		continue
	fi

	work="$(mktemp -d)"
	tmpdirs+=("${work}")
	if ! git clone --quiet "ssh://aur@aur.archlinux.org/${pkg}.git" "${work}"; then
		echo "warning: could not clone AUR repo for ${pkg}"
		echo "  (create it with an initial push, and make sure the SSH key is authorized)"
		rm -rf "${work}"
		failed=1
		echo "::endgroup::"
		continue
	fi

	# Copy every git-tracked file under the package dir, preserving relative
	# paths. New file types (patches, .install, keys, ...) are picked up
	# automatically; build artifacts are untracked and never copied.
	while IFS= read -r f; do
		dest="${work}/${f#${pkg}/}"
		mkdir -p "$(dirname "${dest}")"
		cp "${repo_root}/${f}" "${dest}"
	done < <(git -C "${repo_root}" ls-files -- "${pkg}")

	if [[ ! -f "${work}/PKGBUILD" || ! -f "${work}/.SRCINFO" ]]; then
		echo "::error title=${pkg}::package dir tracks no PKGBUILD/.SRCINFO"
		failed=1
		rm -rf "${work}"
		echo "::endgroup::"
		continue
	fi

	(
		cd "${work}"
		git add -A
		if git diff --cached --quiet; then
			echo "${pkg}: AUR already up to date"
			exit 0
		fi
		ver="$(pkgvar PKGBUILD pkgver)"
		rel="$(pkgvar PKGBUILD pkgrel)"
		if [[ "${dry_run}" -eq 1 ]]; then
			echo "${pkg}: dry-run — would commit upgpkg: ${pkg} ${ver}-${rel}"
			git diff --cached --stat
			exit 0
		fi
		git commit --quiet -m "upgpkg: ${pkg} ${ver}-${rel}"
		# Push to the AUR's canonical `master` branch regardless of the local
		# branch name — a freshly cloned *empty* repo checks out the client's
		# default branch (often `main`), so `git push origin master` would fail
		# with "src refspec master does not match any". HEAD:master sidesteps it.
		#
		# Guard the success message on the push: this subshell runs with `set -e`
		# effectively disabled (it sits on the left of `|| failed=1`), so a bare
		# `git push` failure would otherwise fall through to the success echo and
		# report a publish that never happened.
		if git push origin HEAD:master; then
			echo "${pkg}: pushed ${ver}-${rel} to the AUR"
		else
			echo "::error title=${pkg}::AUR push failed"
			exit 1
		fi
	) || failed=1

	rm -rf "${work}"
	echo "::endgroup::"
done

exit "${failed}"
