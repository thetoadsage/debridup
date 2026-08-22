#!/bin/sh
set -eu

script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
scanner="$script_dir/check-release-safety.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/release-safety-test.XXXXXX")"
trap 'rm -rf "$test_root"' EXIT HUP INT TERM

test_number=0
repo_number=0
current_repo=
failures=0

ok() {
	test_number=$((test_number + 1))
	printf 'ok %s - %s\n' "$test_number" "$1"
}

fail() {
	test_number=$((test_number + 1))
	printf 'not ok %s - %s\n' "$test_number" "$1" >&2
	failures=$((failures + 1))
}

new_repo() {
	repo_number=$((repo_number + 1))
	current_repo="$test_root/repo-$repo_number"
	mkdir "$current_repo"
	git -C "$current_repo" init -q
	git -C "$current_repo" config core.autocrlf false
	git -C "$current_repo" config user.name "CI Fixture"
	git -C "$current_repo" config user.email "ci@users.noreply.github.com"
	printf 'neutral release notes\n' > "$current_repo/tracked.txt"
	git -C "$current_repo" add tracked.txt
	git -C "$current_repo" commit -q -m "test: add neutral fixture"
}

scan() {
	(cd "$current_repo" && "$scanner" "$@")
}

expect_pass() {
	label=$1
	shift
	if "$@" > "$test_root/output" 2>&1; then
		ok "$label"
	else
		fail "$label"
	fi
}

expect_fail() {
	label=$1
	shift
	if "$@" > "$test_root/output" 2>&1; then
		fail "$label"
	else
		ok "$label"
	fi
}

expect_reject() {
	label=$1
	shift
	set +e
	"$@" > "$test_root/output" 2>&1
	status=$?
	set -e
	if [ "$status" -eq 1 ]; then
		ok "$label"
	else
		sed -n '1p' "$test_root/output" >&2
		fail "$label (scanner exit $status)"
	fi
}

new_repo
expect_pass "neutral tracked content passes" scan

private_address="$(printf '%s.%s.%s.%s' 192 168 23 19)"
printf 'endpoint %s\n' "$private_address" > "$current_repo/tracked.txt"
expect_fail "working-tree private address fails" scan
git -C "$current_repo" restore tracked.txt

home_path="$(printf '/%s/%s/%s' home sample-user project)"
printf 'path %s\n' "$home_path" > "$current_repo/tracked.txt"
git -C "$current_repo" add tracked.txt
git -C "$current_repo" show HEAD:tracked.txt > "$current_repo/tracked.txt"
expect_fail "staged home path fails" scan
git -C "$current_repo" reset -q --hard HEAD

credential_assignment="$(printf '%s=%s' api_key runtime_fixture_value)"
printf '%s\n' "$credential_assignment" > "$current_repo/tracked.txt"
expect_fail "credential assignment fails" scan
git -C "$current_repo" restore tracked.txt

personal_email="$(printf '%s@%s.%s' person sample invalid)"
printf 'contact %s\n' "$personal_email" > "$current_repo/tracked.txt"
expect_fail "ordinary email fails" scan
git -C "$current_repo" restore tracked.txt

marker_one="$(printf '\143\157\144\145\170')"
printf 'attribution %s\n' "$marker_one" > "$current_repo/tracked.txt"
expect_fail "first runtime marker fails" scan
git -C "$current_repo" restore tracked.txt

marker_two="$(printf '\146\151\147\155\141')"
printf 'attribution %s\n' "$marker_two" > "$current_repo/tracked.txt"
expect_fail "second runtime marker fails" scan
git -C "$current_repo" restore tracked.txt

printf 'contact ci@users.noreply.github.com\n' > "$current_repo/tracked.txt"
expect_pass "repository no-reply fixture passes" scan
git -C "$current_repo" restore tracked.txt

private_pattern="$(printf '%s-%s' reserved runtime-deny-value)"
printf 'content %s\n' "$private_pattern" > "$current_repo/tracked.txt"
expect_fail "optional private pattern fails" sh -c 'export PRIVATE_PATTERNS="$1"; cd "$2"; "$3"' sh "$private_pattern" "$current_repo" "$scanner"
git -C "$current_repo" restore tracked.txt

change_text="$(printf 'notes mention %s' "$marker_one")"
expect_fail "provided change text fails" sh -c 'export CHANGE_TEXT="$1"; cd "$2"; "$3"' sh "$change_text" "$current_repo" "$scanner"

printf 'next neutral content\n' > "$current_repo/next.txt"
git -C "$current_repo" add next.txt
git -C "$current_repo" commit -q -m "$marker_one"
expect_fail "commit message range fails" scan HEAD~1

new_repo
printf 'synthetic image bytes\n' > "$current_repo/sample.png"
git -C "$current_repo" add sample.png
git -C "$current_repo" commit -q -m "test: add image fixture"
fake_bin="$test_root/fake-bin"
mkdir "$fake_bin"
cat > "$fake_bin/exiftool" <<'FAKE_EXIFTOOL'
#!/bin/sh
value="$(printf '\143\157\144\145\170')"
printf '[{"Comment":"%s"}]\n' "$value"
FAKE_EXIFTOOL
chmod 700 "$fake_bin/exiftool"
expect_fail "tracked image metadata fails" sh -c 'export PATH="$1:$PATH"; cd "$2"; "$3"' sh "$fake_bin" "$current_repo" "$scanner"

expect_pass "built-in scanner self-test passes" "$scanner" --self-test

new_repo
printf 'next neutral content\n' > "$current_repo/next.txt"
git -C "$current_repo" add next.txt
git -C "$current_repo" commit -q -m "$marker_one"
expect_reject "omitted base still scans commit messages" scan
expect_reject "unresolved base falls back to all reachable commits" scan refs/heads/missing-fixture

new_repo
newline_filename="$(printf 'line\n%s.txt' "$marker_one")"
printf 'neutral filename fixture\n' > "$current_repo/$newline_filename"
git -C "$current_repo" add -A
expect_reject "tracked newline filename is scanned without delimiter loss" scan
rm -f -- "$current_repo/$newline_filename"
git -C "$current_repo" reset -q --hard HEAD

printf 'neutral\000%s\n' "$marker_one" > "$current_repo/tracked.txt"
git -C "$current_repo" add tracked.txt
expect_reject "binary index and worktree bytes after NUL are scanned" scan
git -C "$current_repo" reset -q --hard HEAD

credential_value="$(printf '%s_%s' runtime fixture_value)"
printf '{"%s":"%s"}\n' api_key "$credential_value" > "$current_repo/tracked.txt"
expect_reject "JSON credential assignment fails" scan
git -C "$current_repo" restore tracked.txt

printf '%s: "%s"\n' access_token "$credential_value" > "$current_repo/tracked.txt"
expect_reject "YAML credential assignment fails" scan
git -C "$current_repo" restore tracked.txt

printf '%s: %s\n' access_token "$credential_value" > "$current_repo/tracked.txt"
expect_reject "unquoted YAML credential assignment fails" scan
git -C "$current_repo" restore tracked.txt

printf '%s = "%s"\n' password "$credential_value" > "$current_repo/tracked.txt"
expect_reject "TOML spaced credential assignment fails" scan
git -C "$current_repo" restore tracked.txt

printf '%s := "%s"\n' clientSecret "$credential_value" > "$current_repo/tracked.txt"
expect_reject "source literal credential assignment fails" scan
git -C "$current_repo" restore tracked.txt

new_repo
printf 'identity fixture\n' > "$current_repo/identity.txt"
git -C "$current_repo" add identity.txt
git -C "$current_repo" config user.name "$marker_one"
git -C "$current_repo" commit -q -m "test: add identity fixture"
expect_reject "commit author and committer names are scanned" scan HEAD~1

new_repo
ordinary_email="$(printf '%s@%s.%s' author sample invalid)"
printf 'identity fixture\n' > "$current_repo/identity.txt"
git -C "$current_repo" add identity.txt
git -C "$current_repo" config user.email "$ordinary_email"
git -C "$current_repo" commit -q -m "test: add identity fixture"
expect_reject "commit author and committer emails are scanned" scan HEAD~1

new_repo
printf 'staged-metadata-flag\n' > "$current_repo/sample.png"
git -C "$current_repo" add sample.png
printf 'clean worktree image\n' > "$current_repo/sample.png"
staged_fake_bin="$test_root/staged-fake-bin"
mkdir "$staged_fake_bin"
cat > "$staged_fake_bin/exiftool" <<'STAGED_EXIFTOOL'
#!/bin/sh
value="neutral"
for candidate do
	case "$candidate" in
		-json|--) continue ;;
	esac
	if grep -q 'staged-metadata-flag' "$candidate"; then
		value="$(printf '\143\157\144\145\170')"
	fi
done
printf '[{"Comment":"%s"}]\n' "$value"
STAGED_EXIFTOOL
chmod 700 "$staged_fake_bin/exiftool"
expect_reject "staged image metadata uses index bytes, not worktree replacement" sh -c 'export PATH="$1:$PATH"; cd "$2"; "$3"' sh "$staged_fake_bin" "$current_repo" "$scanner"

printf '1..%s\n' "$test_number"
if [ "$failures" -ne 0 ]; then
	exit 1
fi
