#!/bin/sh
set -eu

script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
script_path="$script_dir/$(basename -- "$0")"

run_self_test() {
	self_test_root="$(mktemp -d "${TMPDIR:-/tmp}/release-safety-self-test.XXXXXX")"
	trap 'rm -rf "$self_test_root"' EXIT HUP INT TERM
	self_test_repo="$self_test_root/repository"
	mkdir "$self_test_repo"
	git -C "$self_test_repo" init -q
	git -C "$self_test_repo" config core.autocrlf false
	git -C "$self_test_repo" config user.name "CI Fixture"
	git -C "$self_test_repo" config user.email "ci@users.noreply.github.com"
	printf 'neutral release notes\n' > "$self_test_repo/release.txt"
	git -C "$self_test_repo" add release.txt
	git -C "$self_test_repo" commit -q -m "test: add neutral fixture"

	if ! (cd "$self_test_repo" && PRIVATE_PATTERNS= CHANGE_TEXT= "$script_path") >/dev/null 2>&1; then
		printf '%s\n' 'release-safety self-test failed: neutral content was rejected' >&2
		exit 1
	fi

	probe_one="$(printf '%s.%s.%s.%s' 192 168 23 19)"
	probe_two="$(printf '/%s/%s/%s' home sample-user project)"
	probe_three="$(printf '%s=%s' api_key runtime_fixture_value)"
	probe_four="$(printf '%s@%s.%s' person sample invalid)"
	probe_five="$(printf '\143\157\144\145\170')"
	probe_six="$(printf '\146\151\147\155\141')"

	for probe in "$probe_one" "$probe_two" "$probe_three" "$probe_four" "$probe_five" "$probe_six"; do
		printf '%s\n' "$probe" > "$self_test_repo/release.txt"
		git -C "$self_test_repo" add release.txt
		if (cd "$self_test_repo" && PRIVATE_PATTERNS= CHANGE_TEXT= "$script_path") >/dev/null 2>&1; then
			printf '%s\n' 'release-safety self-test failed: unsafe content was accepted' >&2
			exit 1
		fi
		git -C "$self_test_repo" reset -q --hard HEAD
	done

	printf '%s\n' 'release-safety self-test passed'
}

if [ "${1:-}" = "--self-test" ]; then
	if [ "$#" -ne 1 ]; then
		printf '%s\n' 'usage: check-release-safety.sh --self-test' >&2
		exit 2
	fi
	run_self_test
	exit 0
fi

if [ "$#" -gt 1 ]; then
	printf '%s\n' 'usage: check-release-safety.sh [base-ref]' >&2
	exit 2
fi

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	printf '%s\n' 'release-safety scan requires a Git working tree' >&2
	exit 2
fi

base_ref="${1:-}"
case "$base_ref" in
	-*)
		printf '%s\n' 'release-safety scan rejected an invalid base reference' >&2
		exit 2
		;;
esac

umask 077
scan_root="$(mktemp -d "${TMPDIR:-/tmp}/release-safety-scan.XXXXXX")"
trap 'rm -rf "$scan_root"' EXIT HUP INT TERM

general_patterns="$scan_root/general-patterns"
credential_patterns="$scan_root/credential-patterns"
marker_patterns="$scan_root/marker-patterns"
private_patterns="$scan_root/private-patterns"
email_matches="$scan_root/email-matches"
credential_matches="$scan_root/credential-matches"

octet='(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])'
{
	printf '(^|[^0-9])10\\.%s\\.%s\\.%s([^0-9]|$)\n' "$octet" "$octet" "$octet"
	printf '(^|[^0-9])172\\.(1[6-9]|2[0-9]|3[01])\\.%s\\.%s([^0-9]|$)\n' "$octet" "$octet"
	printf '(^|[^0-9])192\\.168\\.%s\\.%s([^0-9]|$)\n' "$octet" "$octet"
	printf '%s%s\n' '(^|[^[:alnum:]_])/' 'home/[^/[:space:]]+(/|$)'
	printf '%s%s\n' '(^|[^[:alnum:]_])/' 'Users/[^/[:space:]]+(/|$)'
	printf '%s%s\n' '(^|[^[:alnum:]_])/' 'root(/|$)'
	printf '%s\n' '(^|[^[:alnum:]_])[A-Za-z]:[\\/]+Users[\\/]+[^\\/[:space:]:]+([\\/]|$)'
	printf '%s%s\n' '-----BEGIN ([A-Z0-9 ]+ )?PRIVATE' ' KEY-----'
} > "$general_patterns"

credential_name='(password|passwd|token|api[_-]?key|apikey|client[_-]?secret|access[_-]?key|secret)'
{
	printf '^[[:space:]]*(export[[:space:]]+)?[[:alnum:]_.-]*%s[[:alnum:]_.-]*=[[:space:]]*[^$<{[:space:]][^[:space:]]*[[:space:]]*$\n' "$credential_name"
} > "$credential_patterns"

marker_one="$(printf '\143\157\144\145\170')"
marker_two="$(printf '\146\151\147\155\141')"
printf '%s\n%s\n' "$marker_one" "$marker_two" > "$marker_patterns"

: > "$private_patterns"
if [ -n "${PRIVATE_PATTERNS:-}" ]; then
	printf '%s\n' "$PRIVATE_PATTERNS" | tr -d '\r' | sed '/^$/d' > "$private_patterns"
fi

scan_failed=0

reject_source() {
	printf 'release-safety scan failed: %s contains %s\n' "$1" "$2" >&2
	scan_failed=1
}

scan_source() {
	source_file=$1
	source_name=$2

	if grep -Eiq -f "$general_patterns" "$source_file"; then
		reject_source "$source_name" 'private network, local path, or key metadata'
	fi

	if grep -Fiq -f "$marker_patterns" "$source_file"; then
		reject_source "$source_name" 'prohibited attribution'
	fi

	if [ -s "$private_patterns" ] && grep -Fq -f "$private_patterns" "$source_file"; then
		reject_source "$source_name" 'a configured private pattern'
	fi

	: > "$credential_matches"
	grep -Ei -f "$credential_patterns" "$source_file" > "$credential_matches" || true
	if [ -s "$credential_matches" ] && grep -Eiv '=[[:space:]]*"?(replace([-_]with)?|change([-_]?me)?|example|placeholder|dummy)([-_.[:alnum:]]*)"?[[:space:]]*$' "$credential_matches" >/dev/null; then
		reject_source "$source_name" 'a credential assignment'
	fi

	: > "$email_matches"
	grep -Eio '[[:alnum:]._%+-]+@[[:alnum:].-]+\.[[:alpha:]]{2,}' "$source_file" > "$email_matches" || true
	if [ -s "$email_matches" ] && grep -Eiv '^(noreply@github\.com|[[:alnum:]._%+-]+@users\.noreply\.github\.com)$' "$email_matches" >/dev/null; then
		reject_source "$source_name" 'an ordinary email address'
	fi
}

tracked_paths="$scan_root/tracked-paths"
tracked_content="$scan_root/tracked-content"
git ls-files -z > "$tracked_paths"
: > "$tracked_content"
if [ -s "$tracked_paths" ]; then
	xargs -0 grep -I -h -e '' -- < "$tracked_paths" > "$tracked_content" 2>/dev/null || true
fi
scan_source "$tracked_content" 'tracked working-tree content'

staged_content="$scan_root/staged-content"
staged_status=0
git grep --cached -I -h -e '' -- . > "$staged_content" || staged_status=$?
case "$staged_status" in
	0|1) ;;
	*)
		printf '%s\n' 'release-safety scan could not inspect staged content' >&2
		exit 2
		;;
esac
scan_source "$staged_content" 'staged content'

if [ -n "$base_ref" ]; then
	commit_content="$scan_root/commit-content"
	case "$base_ref" in
		*[!0]*)
			if ! git rev-parse --verify "$base_ref^{commit}" >/dev/null 2>&1; then
				printf '%s\n' 'release-safety scan could not resolve the base reference' >&2
				exit 2
			fi
			git log --format='%s%n%b' "$base_ref..HEAD" -- > "$commit_content"
			;;
		*)
			git log --format='%s%n%b' HEAD -- > "$commit_content"
			;;
	esac
	scan_source "$commit_content" 'commit messages'
fi

change_content="$scan_root/change-content"
printf '%s' "${CHANGE_TEXT:-}" > "$change_content"
scan_source "$change_content" 'proposed change text'

image_paths="$scan_root/image-paths"
git ls-files -z -- '*.avif' '*.AVIF' '*.gif' '*.GIF' '*.jpg' '*.JPG' '*.jpeg' '*.JPEG' '*.png' '*.PNG' '*.tif' '*.TIF' '*.tiff' '*.TIFF' '*.webp' '*.WEBP' > "$image_paths"
if [ -s "$image_paths" ]; then
	if ! command -v exiftool >/dev/null 2>&1; then
		printf '%s\n' 'release-safety scan requires exiftool for tracked images' >&2
		exit 2
	fi
	image_metadata="$scan_root/image-metadata"
	if ! xargs -0 exiftool -json -- < "$image_paths" > "$image_metadata" 2>/dev/null; then
		printf '%s\n' 'release-safety scan could not inspect tracked image metadata' >&2
		exit 2
	fi
	scan_source "$image_metadata" 'tracked image metadata'
fi

if [ "$scan_failed" -ne 0 ]; then
	exit 1
fi

printf '%s\n' 'release-safety scan passed'
