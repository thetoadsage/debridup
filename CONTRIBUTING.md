# Release checks

Before committing, configure your repository-local Git identity with your own GitHub username and the noreply address shown in your GitHub email settings. Check both identities:

```sh
git var GIT_AUTHOR_IDENT
git var GIT_COMMITTER_IDENT
```

Release verification scans author and committer metadata, commit subjects and bodies (including attribution trailers), tracked files, staged blobs, and proposed change text. Ordinary email addresses and prohibited attribution markers fail the scan, even when tests and the container build succeed.

After creating commits, run the same bounded scan used by CI against the intended base commit:

```sh
sh scripts/check-release-safety.sh BASE_COMMIT
```

Replace `BASE_COMMIT` with the actual pull-request base SHA. Also inspect the final squash-merge message: generated attribution trailers are part of the scanned commit body. Passing a branch scan does not validate a different squash message.

Container publishing requires verification to pass. Re-running a failed workflow uses the original commit range and cannot repair rejected metadata. Correct unmerged commit metadata before merging. If an invalid commit has already landed, a compliant follow-up commit can restore verification for the next push's bounded range without rewriting shared history or weakening the scanner. This does not sanitize older commits or make a full-history scan pass; historical metadata cleanup requires a separately coordinated history rewrite.
