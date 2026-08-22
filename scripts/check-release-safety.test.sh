#!/bin/sh
set -eu

script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
scanner="$script_dir/check-release-safety.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/release-safety-test.XXXXXX")"
trap 'rm -rf "$test_root"' EXIT HUP INT TERM

test_number=0
repo_number=0
current_repo=

ok() {
	test_number=$((test_number + 1))
	printf 'ok %s - %s\n' "$test_number" "$1"
}

fail() {
	test_number=$((test_number + 1))
	printf 'not ok %s - %s\n' "$test_number" "$1" >&2
	exit 1
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

printf '1..%s\n' "$test_number"
