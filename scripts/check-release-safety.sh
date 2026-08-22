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
single_quote="$(printf '\047')"
{
	printf '^[[:space:]]*(export[[:space:]]+)?[[:alnum:]_.-]*%s[[:alnum:]_.-]*=[[:space:]]*[^$<{[:space:]][^[:space:]]*[[:space:]]*$\n' "$credential_name"
	printf '(^|[,{[:space:]])("|%s)?[[:alnum:]_.-]*%s[[:alnum:]_.-]*("|%s)?[[:space:]]*(:=|=|:)[[:space:]]*("|%s)[^"%s]+("|%s)' "$single_quote" "$credential_name" "$single_quote" "$single_quote" "$single_quote" "$single_quote"
	printf '\n'
	printf '^[[:space:]]*("|%s)?[[:alnum:]_.-]*%s[[:alnum:]_.-]*("|%s)?[[:space:]]*:[[:space:]]*[^=$<{!&*[:space:]#][^#]*([[:space:]]+#.*)?[[:space:]]*$\n' "$single_quote" "$credential_name" "$single_quote"
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

	if grep -aEiq -f "$general_patterns" "$source_file"; then
		reject_source "$source_name" 'private network, local path, or key metadata'
	fi

	if grep -aFiq -f "$marker_patterns" "$source_file"; then
		reject_source "$source_name" 'prohibited attribution'
	fi

	if [ -s "$private_patterns" ] && grep -aFq -f "$private_patterns" "$source_file"; then
		reject_source "$source_name" 'a configured private pattern'
	fi

	: > "$credential_matches"
	grep -aEi -f "$credential_patterns" "$source_file" > "$credential_matches" || true
	safe_credential_pattern="(:=|=|:)[[:space:]]*(\"|$single_quote)?(replace([-_]with)?|change([-_]?me)?|example|placeholder|dummy)([-_.[:alnum:]])*(\"|$single_quote)?([[:space:]]+#.*)?[[:space:]]*$"
	if [ -s "$credential_matches" ] && grep -Eiv "$safe_credential_pattern" "$credential_matches" >/dev/null; then
		reject_source "$source_name" 'a credential assignment'
	fi

	: > "$email_matches"
	grep -aEio '[[:alnum:]._%+-]+@[[:alnum:].-]+\.[[:alpha:]]{2,}' "$source_file" > "$email_matches" || true
	if [ -s "$email_matches" ] && grep -Eiv '^(noreply@github\.com|[[:alnum:]._%+-]+@users\.noreply\.github\.com)$' "$email_matches" >/dev/null; then
		reject_source "$source_name" 'an ordinary email address'
	fi
}

tracked_paths="$scan_root/tracked-paths"
tracked_content="$scan_root/tracked-content"
if ! git ls-files -z > "$tracked_paths"; then
	printf '%s\n' 'release-safety scan could not enumerate tracked paths' >&2
	exit 2
fi
scan_source "$tracked_paths" 'tracked and staged filenames'
: > "$tracked_content"
if [ -s "$tracked_paths" ]; then
	worktree_status=0
	xargs -0 sh -c '
		for path do
			if [ -L "$path" ]; then
				readlink -- "$path" || exit 1
			elif [ -f "$path" ]; then
				cat -- "$path" || exit 1
			elif [ -e "$path" ]; then
				exit 1
			else
				continue
			fi
			printf "\\000"
		done
	' sh < "$tracked_paths" > "$tracked_content" || worktree_status=$?
	if [ "$worktree_status" -ne 0 ]; then
		printf '%s\n' 'release-safety scan could not inspect tracked working-tree bytes' >&2
		exit 2
	fi
fi
scan_source "$tracked_content" 'tracked working-tree content'

staged_content="$scan_root/staged-content"
staged_status=0
git grep --cached -a -h -e '' -- . > "$staged_content" || staged_status=$?
case "$staged_status" in
	0|1) ;;
	*)
		printf '%s\n' 'release-safety scan could not inspect staged content' >&2
		exit 2
		;;
esac
scan_source "$staged_content" 'staged content'

index_entries="$scan_root/index-entries"
index_objects="$scan_root/index-objects"
index_content="$scan_root/index-content"
if ! git ls-files -s -z > "$index_entries"; then
	printf '%s\n' 'release-safety scan could not enumerate index objects' >&2
	exit 2
fi
if ! perl -0ne 'chomp; if (/\A([0-9]+) ([0-9a-f]{40,64}) ([0-3])\t/s) { print "$2\n" if $1 ne "160000" } else { exit 2 }' < "$index_entries" > "$index_objects"; then
	printf '%s\n' 'release-safety scan could not parse index objects' >&2
	exit 2
fi
: > "$index_content"
if [ -s "$index_objects" ] && ! git cat-file --batch < "$index_objects" > "$index_content"; then
	printf '%s\n' 'release-safety scan could not inspect index blob bytes' >&2
	exit 2
fi
scan_source "$index_content" 'staged index blob content'

commit_content="$scan_root/commit-content"
commit_revisions="$scan_root/commit-revisions"
commit_range=HEAD
case "$base_ref" in
	'') ;;
	*[!0]*)
		if git rev-parse --verify "$base_ref^{commit}" >/dev/null 2>&1; then
			commit_range="$base_ref..HEAD"
		fi
		;;
	*) ;;
esac
if ! git rev-list "$commit_range" -- > "$commit_revisions"; then
	printf '%s\n' 'release-safety scan could not inspect commit metadata' >&2
	exit 2
fi
: > "$commit_content"
while IFS= read -r commit; do
	if ! git show -s --format='%an%n%ae%n%cn%n%ce' "$commit" >> "$commit_content"; then
		printf '%s\n' 'release-safety scan could not inspect commit metadata' >&2
		exit 2
	fi
	parents="$(git show -s --format='%P' "$commit")"
	committer_email="$(git show -s --format='%ce' "$commit")"
	subject="$(git show -s --format='%s' "$commit")"
	generated_merge=0
	case "$parents" in
		*' '*)
			case "$committer_email:$subject" in
				noreply@github.com:'Merge pull request #'*' from '*) generated_merge=1 ;;
			esac
			;;
	esac
	if [ "$generated_merge" -eq 0 ]; then
		if ! git show -s --format='%s%n%b' "$commit" >> "$commit_content"; then
			printf '%s\n' 'release-safety scan could not inspect commit metadata' >&2
			exit 2
		fi
	fi
done < "$commit_revisions"
scan_source "$commit_content" 'commit metadata'

change_content="$scan_root/change-content"
printf '%s' "${CHANGE_TEXT:-}" > "$change_content"
scan_source "$change_content" 'proposed change text'

scan_image_metadata() {
	image_paths=$1
	image_name=$2
	image_metadata=$3
	if [ ! -s "$image_paths" ]; then
		return
	fi
	if ! command -v exiftool >/dev/null 2>&1; then
		printf '%s\n' 'release-safety scan requires exiftool for tracked images' >&2
		exit 2
	fi
	if ! xargs -0 exiftool -json -- < "$image_paths" > "$image_metadata" 2>/dev/null; then
		printf '%s\n' 'release-safety scan could not inspect image metadata' >&2
		exit 2
	fi
	scan_source "$image_metadata" "$image_name"
}

worktree_image_paths="$scan_root/worktree-image-paths"
if ! perl -0ne 'chomp; print "$_\0" if /\.(?:avif|gif|jpe?g|png|tiff?|webp)\z/i' < "$tracked_paths" > "$worktree_image_paths"; then
	printf '%s\n' 'release-safety scan could not enumerate worktree images' >&2
	exit 2
fi
scan_image_metadata "$worktree_image_paths" 'tracked worktree image metadata' "$scan_root/worktree-image-metadata"

index_image_pairs="$scan_root/index-image-pairs"
if ! perl -0ne 'chomp; if (/\A([0-9]+) ([0-9a-f]{40,64}) ([0-3])\t(.*)\z/s) { my ($mode, $sha, $path) = ($1, $2, $4); print "$sha\0$path\0" if $mode ne "160000" && $path =~ /\.(?:avif|gif|jpe?g|png|tiff?|webp)\z/i } else { exit 2 }' < "$index_entries" > "$index_image_pairs"; then
	printf '%s\n' 'release-safety scan could not enumerate index images' >&2
	exit 2
fi
index_image_dir="$scan_root/index-images"
index_image_paths="$scan_root/index-image-paths"
mkdir "$index_image_dir"
: > "$index_image_paths"
if [ -s "$index_image_pairs" ]; then
	index_image_status=0
	RELEASE_SAFETY_IMAGE_DIR="$index_image_dir" xargs -0 -n 2 sh -c '
		sha=$0
		path=$1
		extension=${path##*.}
		output="$RELEASE_SAFETY_IMAGE_DIR/$sha.$extension"
		git cat-file blob "$sha" > "$output" || exit 1
		printf "%s\\000" "$output"
	' < "$index_image_pairs" > "$index_image_paths" || index_image_status=$?
	if [ "$index_image_status" -ne 0 ]; then
		printf '%s\n' 'release-safety scan could not export index images' >&2
		exit 2
	fi
fi
scan_image_metadata "$index_image_paths" 'staged index image metadata' "$scan_root/index-image-metadata"

if [ "$scan_failed" -ne 0 ]; then
	exit 1
fi

printf '%s\n' 'release-safety scan passed'
