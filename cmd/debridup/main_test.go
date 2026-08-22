package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

func TestLoadKeyReadsInheritedDescriptor(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	encoded := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, chacha20poly1305.KeySize))
	if _, err := io.WriteString(w, encoded); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEBRIDUP_ENCRYPTION_KEY", "")
	t.Setenv("DEBRIDUP_ENCRYPTION_KEY_FILE", "")
	t.Setenv("DEBRIDUP_ENCRYPTION_KEY_FD", strconv.FormatUint(uint64(r.Fd()), 10))

	got, err := loadKey()
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte{7}, chacha20poly1305.KeySize)) {
		t.Fatal("wrong key")
	}
}

func TestLoadKeyClosesInheritedDescriptor(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	encoded := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, chacha20poly1305.KeySize))
	if _, err := io.WriteString(w, encoded); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEBRIDUP_ENCRYPTION_KEY", "")
	t.Setenv("DEBRIDUP_ENCRYPTION_KEY_FILE", "")
	t.Setenv("DEBRIDUP_ENCRYPTION_KEY_FD", strconv.FormatUint(uint64(r.Fd()), 10))

	if _, err := loadKey(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(make([]byte, 1)); err == nil {
		t.Fatal("descriptor remained open after key load")
	}
	_ = r.Close()
}

func TestLoadKeyUsesDescriptorBeforeFileAndDirectValue(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	descriptorKey := bytes.Repeat([]byte{9}, chacha20poly1305.KeySize)
	if _, err := io.WriteString(w, base64.StdEncoding.EncodeToString(descriptorKey)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	fileKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{10}, chacha20poly1305.KeySize))
	keyFile := filepath.Join(t.TempDir(), "encryption-key")
	if err := os.WriteFile(keyFile, []byte(fileKey), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEBRIDUP_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{11}, chacha20poly1305.KeySize)))
	t.Setenv("DEBRIDUP_ENCRYPTION_KEY_FILE", keyFile)
	t.Setenv("DEBRIDUP_ENCRYPTION_KEY_FD", strconv.FormatUint(uint64(r.Fd()), 10))

	got, err := loadKey()
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, descriptorKey) {
		t.Fatal("descriptor did not take precedence")
	}
}

func TestLoadKeyDoesNotFallBackFromInvalidDescriptor(t *testing.T) {
	validFallback := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{13}, chacha20poly1305.KeySize))
	keyFile := filepath.Join(t.TempDir(), "encryption-key")
	if err := os.WriteFile(keyFile, []byte(validFallback), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEBRIDUP_ENCRYPTION_KEY", validFallback)
	t.Setenv("DEBRIDUP_ENCRYPTION_KEY_FILE", keyFile)
	t.Setenv("DEBRIDUP_ENCRYPTION_KEY_FD", "invalid-descriptor")

	if _, err := loadKey(); err != errInvalidEncryptionKey {
		t.Fatalf("invalid descriptor error = %v", err)
	}
}

func TestLoadKeyPreservesFileAndDevelopmentFallbacks(t *testing.T) {
	fileKey := bytes.Repeat([]byte{14}, chacha20poly1305.KeySize)
	directKey := bytes.Repeat([]byte{15}, chacha20poly1305.KeySize)
	keyFile := filepath.Join(t.TempDir(), "encryption-key")
	if err := os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(fileKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEBRIDUP_ENCRYPTION_KEY_FD", "")
	t.Setenv("DEBRIDUP_ENCRYPTION_KEY_FILE", keyFile)
	t.Setenv("DEBRIDUP_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(directKey))

	got, err := loadKey()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fileKey) {
		t.Fatal("file input did not take precedence over the development fallback")
	}

	t.Setenv("DEBRIDUP_ENCRYPTION_KEY_FILE", "")
	got, err = loadKey()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, directKey) {
		t.Fatal("development fallback stopped working")
	}
}

func TestLoadKeyRejectsUnsafeSourcesWithGenericErrors(t *testing.T) {
	tests := []struct {
		name   string
		direct string
		file   string
		fd     string
	}{
		{name: "missing"},
		{name: "negative descriptor", fd: "-1"},
		{name: "malformed descriptor", fd: "not-a-descriptor"},
		{name: "missing file", file: filepath.Join(t.TempDir(), "absent-key")},
		{name: "invalid direct value", direct: "invalid-key-input"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DEBRIDUP_ENCRYPTION_KEY", tc.direct)
			t.Setenv("DEBRIDUP_ENCRYPTION_KEY_FILE", tc.file)
			t.Setenv("DEBRIDUP_ENCRYPTION_KEY_FD", tc.fd)
			_, err := loadKey()
			if err == nil {
				t.Fatal("unsafe key source was accepted")
			}
			if err.Error() != "invalid encryption key" {
				t.Fatalf("unsafe error detail: %q", err)
			}
		})
	}
}

func TestLoadKeyRejectsOversizedDescriptorAndClosesIt(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := w.Write(bytes.Repeat([]byte{'x'}, 4097))
		if closeErr := w.Close(); writeErr == nil {
			writeErr = closeErr
		}
		writeDone <- writeErr
	}()
	t.Setenv("DEBRIDUP_ENCRYPTION_KEY", "")
	t.Setenv("DEBRIDUP_ENCRYPTION_KEY_FILE", "")
	t.Setenv("DEBRIDUP_ENCRYPTION_KEY_FD", strconv.FormatUint(uint64(r.Fd()), 10))

	_, err = loadKey()
	if err == nil || err.Error() != "invalid encryption key" {
		t.Fatalf("oversized descriptor error = %v", err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(make([]byte, 1)); err == nil {
		t.Fatal("oversized descriptor remained open")
	}
	_ = r.Close()
}

func TestEntrypointHandsOffSecretByDescriptorOnly(t *testing.T) {
	tempDir := t.TempDir()
	secretPath := filepath.Join(tempDir, "encryption-key")
	secret := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{12}, chacha20poly1305.KeySize))
	if err := os.WriteFile(secretPath, []byte(secret), 0o400); err != nil {
		t.Fatal(err)
	}
	probePath := filepath.Join(tempDir, "entrypoint-probe")
	fakeSuExec := filepath.Join(tempDir, "su-exec")
	probeScript := `#!/bin/sh
set -eu
[ "${DEBRIDUP_ENCRYPTION_KEY+x}" != x ] || exit 70
[ "${DEBRIDUP_ENCRYPTION_KEY_FILE+x}" != x ] || exit 71
[ "${DEBRIDUP_ENCRYPTION_KEY_FD:-}" = 3 ] || exit 72
{
  printf '%s\n' "$DEBRIDUP_ENCRYPTION_KEY_FD" "$#"
  printf '%s\n' "$@"
  cat <&3
} > "$ENTRYPOINT_PROBE_OUTPUT"
`
	if err := os.WriteFile(fakeSuExec, []byte(probeScript), 0o700); err != nil {
		t.Fatal(err)
	}
	entrypoint := executableEntrypointForTest(t)
	command := exec.Command("sh", entrypoint)
	command.Env = []string{
		"PATH=" + tempDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"PUID=99",
		"PGID=100",
		"DEBRIDUP_ENCRYPTION_KEY=" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{16}, chacha20poly1305.KeySize)),
		"DEBRIDUP_ENCRYPTION_KEY_FILE=" + secretPath,
		"ENTRYPOINT_PROBE_OUTPUT=" + probePath,
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("entrypoint failed: %v: %s", err, output)
	}

	probe, err := os.ReadFile(probePath)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := "3\n2\n99:100\n/usr/local/bin/debridup\n"
	if !bytes.Equal(probe, append([]byte(wantPrefix), []byte(secret)...)) {
		t.Fatal("entrypoint did not preserve the descriptor-only handoff and safe process arguments")
	}
}

func TestEntrypointFailsBeforePrivilegeDropWhenSecretFileIsMissing(t *testing.T) {
	tempDir := t.TempDir()
	probePath := filepath.Join(tempDir, "privilege-drop-probe")
	fakeSuExec := filepath.Join(tempDir, "su-exec")
	if err := os.WriteFile(fakeSuExec, []byte("#!/bin/sh\n: > \"$ENTRYPOINT_PROBE_OUTPUT\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	entrypoint := executableEntrypointForTest(t)
	command := exec.Command("sh", entrypoint)
	command.Env = []string{
		"PATH=" + tempDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"PUID=99",
		"PGID=100",
		"DEBRIDUP_ENCRYPTION_KEY_FILE=" + filepath.Join(tempDir, "absent-key"),
		"ENTRYPOINT_PROBE_OUTPUT=" + probePath,
	}
	if err := command.Run(); err == nil {
		t.Fatal("entrypoint accepted a missing secret file")
	}
	if _, err := os.Stat(probePath); !os.IsNotExist(err) {
		t.Fatal("entrypoint dropped privileges before rejecting the missing secret")
	}
}

func TestEntrypointCheckoutIsLFOnlyAndValidShell(t *testing.T) {
	entrypoint, err := filepath.Abs(filepath.Join("..", "..", "docker-entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	entrypointBytes, err := os.ReadFile(entrypoint)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.ContainsRune(entrypointBytes, '\r') {
		t.Fatal("docker-entrypoint.sh contains a carriage return")
	}
	if runtime.GOOS == "windows" {
		return
	}
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("entrypoint syntax check requires a POSIX shell")
	}
	if output, err := exec.Command(shell, "-n", entrypoint).CombinedOutput(); err != nil {
		t.Fatalf("docker-entrypoint.sh syntax check failed: %v: %s", err, output)
	}
}

func TestComposeDocumentationKeepsDirectoryTraversableAndKeyRootOnly(t *testing.T) {
	readmePath, err := filepath.Abs(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readmeBytes, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatal(err)
	}
	readme := string(readmeBytes)
	for _, required := range []string{
		"install -d -m 0700 secrets",
		"sudo install -m 0400 -o root -g root /dev/stdin secrets/encryption_key",
	} {
		if !strings.Contains(readme, required) {
			t.Errorf("Compose secret setup is missing %q", required)
		}
	}
	if strings.Contains(readme, "chmod 600 secrets/encryption_key") {
		t.Fatal("Compose secret setup leaves the key owned by the invoking user")
	}
	if strings.Contains(readme, "install -d -m 0700 -o root -g root secrets") {
		t.Fatal("Compose secret setup makes the tracked directory inaccessible to the invoking user")
	}
}

func TestGitIgnoresGeneratedComposeSecretsButTracksPlaceholder(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	gitExecutable := "git"
	gitPrefix := []string{"-c", "safe.directory=" + filepath.ToSlash(repoRoot)}
	if _, err := exec.LookPath("git.exe"); err == nil {
		gitExecutable = "git.exe"
		gitPrefix = nil
	}
	gitCommand := func(args ...string) *exec.Cmd {
		t.Helper()
		args = append(append([]string{}, gitPrefix...), args...)
		command := exec.Command(gitExecutable, args...)
		command.Dir = repoRoot
		return command
	}
	checkIgnored := gitCommand("check-ignore", "--quiet", "--no-index", "secrets/encryption_key")
	if err := checkIgnored.Run(); err != nil {
		t.Fatalf("generated Compose secret is not ignored: %v", err)
	}
	checkPlaceholder := gitCommand("check-ignore", "--quiet", "--no-index", "secrets/.gitkeep")
	if err := checkPlaceholder.Run(); err == nil {
		t.Fatal("tracked secrets placeholder is ignored")
	}
	trackedPlaceholder := gitCommand("ls-files", "--error-unmatch", "secrets/.gitkeep")
	if output, err := trackedPlaceholder.CombinedOutput(); err != nil {
		t.Fatalf("secrets placeholder is not tracked: %v: %s", err, output)
	}
	if _, err := os.ReadFile(filepath.Join(repoRoot, "secrets", ".gitkeep")); err != nil {
		t.Fatalf("tracked secrets placeholder is not readable: %v", err)
	}
	dockerIgnore, err := os.ReadFile(filepath.Join(repoRoot, ".dockerignore"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dockerIgnore), "secrets/*") || !strings.Contains(string(dockerIgnore), "!secrets/.gitkeep") {
		t.Fatal("Docker context does not exclude generated secrets while retaining the placeholder")
	}
}

func executableEntrypointForTest(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("entrypoint requires a POSIX shell")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("entrypoint requires a POSIX shell")
	}
	entrypointSource, err := filepath.Abs(filepath.Join("..", "..", "docker-entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return entrypointSource
}

func TestSendNtfy(t *testing.T) {
	var gotTitle, gotTags, gotEventID, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTitle = r.Header.Get("Title")
		gotTags = r.Header.Get("Tags")
		gotEventID = r.Header.Get("X-Event-ID")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	a := &app{client: server.Client()}
	err := a.sendNtfy(server.URL+"/topic/", "DebridUp test", "It works", "white_check_mark", "test-1")
	if err != nil {
		t.Fatalf("sendNtfy returned an error: %v", err)
	}
	if gotTitle != "DebridUp test" || gotTags != "white_check_mark" || gotEventID != "test-1" || gotBody != "It works" {
		t.Fatalf("unexpected ntfy request: title=%q tags=%q event=%q body=%q", gotTitle, gotTags, gotEventID, gotBody)
	}
}

func TestHealthz(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	(&app{}).routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "{\"ok\":true}\n" {
		t.Fatalf("health check returned %d: %q", response.Code, response.Body.String())
	}
}

func TestThemeBootstrapIsAvailableBeforeAuthentication(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/theme-init.js", nil)
	response := httptest.NewRecorder()
	(&app{}).routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("theme bootstrap returned %d: %q", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "debridup-theme") {
		t.Fatal("theme bootstrap asset was not served")
	}
}

func TestDashboardHTMLContainsAccessibleLandmarks(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, required := range []string{
		`<nav aria-label="Primary">`, `id="range-controls"`, `id="summary"`,
		`id="provider-pulse"`, `id="provider-table-body"`, `id="latency-chart"`,
		`id="incidents"`, `id="provider-drawer"`, `id="dashboard-status"`,
		`aria-live="polite"`, `id="theme-select"`,
	} {
		if !strings.Contains(html, required) {
			t.Errorf("missing %s", required)
		}
	}
}

func TestDashboardAssetsContainResponsiveMonitorDialog(t *testing.T) {
	htmlBytes, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(htmlBytes), `<dialog id="monitor-dialog">`) {
		t.Fatal("monitor management dialog is missing")
	}

	cssBytes, err := webFS.ReadFile("web/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cssBytes), "dialog .form { min-width: 0; width: 100%; }") {
		t.Fatal("monitor dialog form must shrink to the dialog content width")
	}
}

func TestProviderDrawerLabelsTheLatestCheckAccurately(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	if !strings.Contains(html, `<dt>Last check</dt>`) {
		t.Fatal("provider drawer must label the API's lastCheck value as Last check")
	}
	if strings.Contains(html, `<dt>Last successful check</dt>`) {
		t.Fatal("provider drawer must not describe lastCheck as successful")
	}
}

func TestPulseStylesBoundMaximumHistoryWithoutColorOnlyMeaning(t *testing.T) {
	b, err := webFS.ReadFile("web/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	if !strings.Contains(css, `.pulse-track { display: grid; min-width: 0; max-width: 100%; overflow: hidden; grid-template-columns: repeat(var(--pulse-bucket-count, 1), minmax(0, 1fr));`) {
		t.Fatal("pulse track must scale every server bucket inside its available width")
	}
	if !strings.Contains(css, `.pulse-rows { display: grid; gap: 5px; margin-top: 16px; }`) ||
		!strings.Contains(css, `.pulse-bucket { display: grid; width: 100%; min-width: 0; height: 14px;`) {
		t.Fatal("pulse rows must remain compact as the provider count grows")
	}
	if strings.Count(css, "repeating-linear-gradient") < 4 {
		t.Fatal("pulse states must use distinct visible patterns in addition to color")
	}
}

func TestStatsResetsRefreshMonitorSettingsAndDashboard(t *testing.T) {
	b, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	app := string(b)
	perProviderStart := strings.Index(app, "$('#reset-monitor-stats').addEventListener")
	globalStart := strings.Index(app, "$('#reset-all-stats').addEventListener")
	notificationStart := strings.Index(app, "$('#ntfy-form').addEventListener")
	if perProviderStart < 0 || globalStart <= perProviderStart || notificationStart <= globalStart {
		t.Fatal("could not locate reset handlers")
	}
	for name, block := range map[string]string{
		"per-provider reset": app[perProviderStart:globalStart],
		"global reset":       app[globalStart:notificationStart],
	} {
		if !strings.Contains(block, "loadMonitorSettings()") || !strings.Contains(block, "dashboard.refresh({supersede: true})") {
			t.Errorf("%s must refresh both monitor settings and dashboard data", name)
		}
	}
}

func TestSendNtfyReportsHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	a := &app{client: server.Client()}
	err := a.sendNtfy(server.URL+"/topic", "Test", "Test", "warning", "test-2")
	if err == nil || err.Error() != "ntfy returned HTTP 401" {
		t.Fatalf("expected safe HTTP status error, got %v", err)
	}
}

func TestNormalizeNtfyURL(t *testing.T) {
	got, err := normalizeNtfyURL(" https://ntfy.sh/private-topic/ ")
	if err != nil {
		t.Fatalf("normalizeNtfyURL returned an error: %v", err)
	}
	if got != "https://ntfy.sh/private-topic" {
		t.Fatalf("expected trailing slash to be removed, got %q", got)
	}
	if _, err = normalizeNtfyURL("https://ntfy.sh/"); err == nil {
		t.Fatal("expected a topic-less ntfy URL to be rejected")
	}
}

func TestIncidentSummary(t *testing.T) {
	tests := []struct {
		name   string
		result checkResult
		want   string
	}{
		{"authentication", checkResult{State: stateAuthFailed}, "The provider rejected the configured credential. Verify or replace it."},
		{"timeout", checkResult{State: stateConnection, ErrorCode: "timeout"}, "The authenticated API request timed out before the provider responded."},
		{"server status", checkResult{State: stateAPI, ErrorCode: "server_error", HTTPStatus: 503}, "The provider API returned HTTP 503, indicating a server-side failure."},
		{"invalid response", checkResult{State: stateAPI, ErrorCode: "invalid_response"}, "The provider API was reachable, but its response was invalid or could not be understood."},
		{"rate limited", checkResult{State: stateAPI, ErrorCode: "rate_limited"}, "The provider API rate-limited the authenticated health check."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := incidentSummary(test.result); got != test.want {
				t.Fatalf("incidentSummary() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestConfirmedAvailability(t *testing.T) {
	tests := []struct {
		name    string
		start   int64
		now     int64
		periods []incidentPeriod
		want    *float64
	}{
		{name: "no monitoring history", start: 0, now: 1100},
		{name: "isolated failures do not count", start: 100, now: 1100, want: floatPointer(100)},
		{name: "confirmed resolved incident", start: 100, now: 1100, periods: []incidentPeriod{{OpenedAt: 200, ResolvedAt: 300}}, want: floatPointer(90)},
		{name: "incident is clipped to window", start: 100, now: 1100, periods: []incidentPeriod{{OpenedAt: 50, ResolvedAt: 200}}, want: floatPointer(90)},
		{name: "open incident runs to now", start: 100, now: 1100, periods: []incidentPeriod{{OpenedAt: 900}}, want: floatPointer(80)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := confirmedAvailability(test.start, test.now, test.periods)
			if test.want == nil {
				if got != nil {
					t.Fatalf("confirmedAvailability() = %.2f, want nil", *got)
				}
				return
			}
			if got == nil || *got != *test.want {
				t.Fatalf("confirmedAvailability() = %v, want %.2f", got, *test.want)
			}
		})
	}
}

func floatPointer(value float64) *float64 { return &value }

func TestProviderDefinitions(t *testing.T) {
	want := []string{"torbox", "premiumize", "alldebrid", "realdebrid", "torrin", "pikpak", "offcloud", "debridlink", "easydebrid", "debrider", "deepbrid"}
	for _, id := range want {
		provider, ok := providerDefinitions[id]
		if !ok {
			t.Fatalf("provider %q is missing", id)
		}
		if !strings.HasPrefix(provider.Endpoint, "https://") || !strings.HasPrefix(provider.PublicEndpoint, "https://") {
			t.Fatalf("provider %q must use HTTPS endpoints: %#v", id, provider)
		}
	}
	if got := providerDefinitions["torrin"]; got.Endpoint != "https://torrin.app/api/stats" || got.PublicEndpoint != "https://torrin.app/api/stats/public" {
		t.Fatalf("Torrin must use its authenticated and public stats endpoints: %#v", got)
	}
}

func TestClassifyProviderPayload(t *testing.T) {
	tests := []struct {
		name, provider string
		payload        any
		state, code    string
	}{
		{"torbox success", "torbox", map[string]any{"success": true}, "", ""},
		{"torbox invalid token", "torbox", map[string]any{"success": false, "detail": "Invalid token"}, stateAuthFailed, "authentication_rejected"},
		{"premiumize API failure", "premiumize", map[string]any{"status": "error", "message": "backend unavailable"}, stateAPI, "api_error"},
		{"alldebrid invalid key", "alldebrid", map[string]any{"status": "error", "error": map[string]any{"code": "AUTH_BAD_APIKEY"}}, stateAuthFailed, "authentication_rejected"},
		{"offcloud invalid key", "offcloud", map[string]any{"error": "Invalid API key"}, stateAuthFailed, "authentication_rejected"},
		{"deepbrid application failure", "deepbrid", map[string]any{"error": float64(1), "message": "Internal failure"}, stateAPI, "api_error"},
		{"generic account response", "realdebrid", map[string]any{"id": float64(42)}, "", ""},
		{"torrin stats response", "torrin", map[string]any{"stats": map[string]any{}, "plan": map[string]any{}}, "", ""},

		// Classification reads only the provider's declared error fields.
		// Previously the whole encoded payload was searched, so an unrelated
		// field name or value containing "auth"/"token" forced auth_failed.
		{
			"rate limit is not an auth failure despite an unrelated auth field",
			"premiumize",
			map[string]any{"status": "error", "message": "rate limit exceeded", "authMethod": "apikey"},
			stateAPI, "api_error",
		},
		{
			"field names alone do not imply an auth failure",
			"torbox",
			map[string]any{"success": false, "detail": "queue is full", "tokenBucket": float64(0)},
			stateAPI, "api_error",
		},
		{
			"nested provider error text is still inspected",
			"debridlink",
			map[string]any{"success": false, "error": map[string]any{"error_description": "invalid access token"}},
			stateAuthFailed, "authentication_rejected",
		},
		{
			"error text outside the declared fields is ignored",
			"offcloud",
			map[string]any{"error": "server busy", "hint": "check your api key"},
			stateAPI, "api_error",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, code := classifyProviderPayload(test.provider, test.payload)
			if state != test.state || code != test.code {
				t.Fatalf("classifyProviderPayload() = (%q, %q), want (%q, %q)", state, code, test.state, test.code)
			}
		})
	}
}

func TestAuthCheckHandlesBearerAndRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bearer":
			if r.Header.Get("Authorization") != "Bearer correct-credential" {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":1}`))
		default:
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("login"))
		}
	}))
	defer server.Close()
	providerDefinitions["test-bearer"] = providerDefinition{Endpoint: server.URL + "/bearer", Method: http.MethodGet}
	defer delete(providerDefinitions, "test-bearer")
	a := &app{client: server.Client()}

	if result := a.authCheck(monitor{Provider: "test-bearer", TimeoutSeconds: 3}, "correct-credential"); result.State != stateHealthy {
		t.Fatalf("bearer check returned %q (%q)", result.State, result.ErrorCode)
	}
	if result := a.authCheck(monitor{Provider: "test-bearer", TimeoutSeconds: 3}, "wrong-credential"); result.State != stateAuthFailed || result.ErrorCode != "authentication_redirect" {
		t.Fatalf("redirected auth check returned %q (%q)", result.State, result.ErrorCode)
	}
}

func TestMigrateExpandsProviderConstraint(t *testing.T) {
	db, err := sql.Open("sqlite", "file:provider-migration?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE monitors (
 id INTEGER PRIMARY KEY, provider TEXT NOT NULL CHECK(provider IN ('torbox','premiumize')), name TEXT NOT NULL,
 enabled INTEGER NOT NULL DEFAULT 1, interval_seconds INTEGER NOT NULL DEFAULT 60, timeout_seconds INTEGER NOT NULL DEFAULT 15,
 failure_threshold INTEGER NOT NULL DEFAULT 3, recovery_threshold INTEGER NOT NULL DEFAULT 2, public_check INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
); INSERT INTO monitors(provider,name,created_at,updated_at) VALUES('torbox','Existing TorBox',1,1);`)
	if err != nil {
		t.Fatal(err)
	}
	if err = migrateDatabase(context.Background(), db); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}
	if _, err = db.Exec(`INSERT INTO monitors(provider,name,created_at,updated_at) VALUES('realdebrid','Real-Debrid',2,2)`); err != nil {
		t.Fatalf("new provider rejected after migration: %v", err)
	}
	var count int
	if err = db.QueryRow(`SELECT COUNT(*) FROM monitors`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("monitor data was not preserved: count=%d err=%v", count, err)
	}
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("provider constraint migration left a foreign key violation")
	}
}

func TestResetHistoryScopesAndPreservesConfiguration(t *testing.T) {
	db, err := sql.Open("sqlite", "file:reset-history?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	a := &app{db: db}
	if err = migrateDatabase(context.Background(), db); err != nil {
		t.Fatal(err)
	}

	for id, provider := range []string{"torbox", "torrin"} {
		monitorID := id + 1
		if _, err = db.Exec(`INSERT INTO monitors(id,provider,name,created_at,updated_at) VALUES(?,?,?,?,?)`, monitorID, provider, provider, 1, 1); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO monitor_secrets(monitor_id,nonce,ciphertext,updated_at) VALUES(?,?,?,?)`, monitorID, []byte{1}, []byte{2}, 1); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO monitor_states(monitor_id,current_state,state_since,last_raw_state,last_check_at) VALUES(?,?,?,?,?)`, monitorID, stateAPI, 1, stateAPI, 1); err != nil {
			t.Fatal(err)
		}
		check, err := db.Exec(`INSERT INTO check_results(monitor_id,source,state,duration_ms,checked_at) VALUES(?,?,?,?,?)`, monitorID, "authenticated", stateAPI, 10, 1)
		if err != nil {
			t.Fatal(err)
		}
		checkID, _ := check.LastInsertId()
		incident, err := db.Exec(`INSERT INTO incidents(monitor_id,opened_at,detected_at,initial_state,latest_state,summary) VALUES(?,?,?,?,?,?)`, monitorID, 1, 1, stateAPI, stateAPI, "test")
		if err != nil {
			t.Fatal(err)
		}
		incidentID, _ := incident.LastInsertId()
		if _, err = db.Exec(`INSERT INTO incident_events(incident_id,type,new_state,created_at,check_id) VALUES(?,?,?,?,?)`, incidentID, "opened", stateAPI, 1, checkID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`INSERT INTO notification_channels(id,kind,updated_at) VALUES(1,'ntfy',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO notification_outbox(channel_id,incident_id,event_type,payload,next_attempt_at) SELECT 1,id,'opened','{}',1 FROM incidents`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO notification_outbox(channel_id,event_type,payload,next_attempt_at) VALUES(1,'test','{}',1)`); err != nil {
		t.Fatal(err)
	}

	firstID := int64(1)
	if err = a.resetHistory(&firstID); err != nil {
		t.Fatalf("reset provider history: %v", err)
	}
	assertCount := func(query string, want int) {
		t.Helper()
		var got int
		if err := db.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: count=%d err=%v, want %d", query, got, err, want)
		}
	}
	assertCount(`SELECT COUNT(*) FROM check_results WHERE monitor_id=1`, 0)
	assertCount(`SELECT COUNT(*) FROM incidents WHERE monitor_id=1`, 0)
	assertCount(`SELECT COUNT(*) FROM monitor_states WHERE monitor_id=1`, 0)
	assertCount(`SELECT COUNT(*) FROM check_results WHERE monitor_id=2`, 1)
	assertCount(`SELECT COUNT(*) FROM monitors`, 2)
	assertCount(`SELECT COUNT(*) FROM monitor_secrets`, 2)

	if err = a.resetHistory(nil); err != nil {
		t.Fatalf("reset all history: %v", err)
	}
	assertCount(`SELECT COUNT(*) FROM check_results`, 0)
	assertCount(`SELECT COUNT(*) FROM incidents`, 0)
	assertCount(`SELECT COUNT(*) FROM incident_events`, 0)
	assertCount(`SELECT COUNT(*) FROM monitor_states`, 0)
	assertCount(`SELECT COUNT(*) FROM notification_outbox WHERE incident_id IS NOT NULL`, 0)
	assertCount(`SELECT COUNT(*) FROM notification_outbox WHERE event_type='test'`, 1)
	assertCount(`SELECT COUNT(*) FROM monitors`, 2)
	assertCount(`SELECT COUNT(*) FROM monitor_secrets`, 2)
}

func TestUpdateMonitorRetriesImmediateCheckAfterRejection(t *testing.T) {
	for _, rejection := range []string{"overlap", "capacity"} {
		t.Run(rejection, func(t *testing.T) {
			db := migratedTestDB(t)
			monitorID := insertSyntheticMonitor(t, db, "torbox")
			a := &app{db: db, runs: newRunCoordinator(1)}
			now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
			if got := a.runs.Claim(monitorID, now, time.Hour); got != claimAccepted {
				t.Fatalf("initial monitor claim = %v, want accepted", got)
			}
			blockerID := monitorID
			if rejection == "capacity" {
				a.runs.Release(monitorID)
				blockerID = 99
				if got := a.runs.Claim(blockerID, now, time.Hour); got != claimAccepted {
					t.Fatalf("capacity holder claim = %v, want accepted", got)
				}
			}

			body := strings.NewReader(`{"name":"Updated","apiKey":"","enabled":true,"intervalSeconds":3600,"timeoutSeconds":15,"failureThreshold":3,"recoveryThreshold":2,"publicCheck":false}`)
			request := httptest.NewRequest(http.MethodPut, "/api/monitors/1", body)
			request.Header.Set("Content-Type", "application/json")
			request.SetPathValue("id", "1")
			response := httptest.NewRecorder()
			a.updateMonitor(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("update status = %d, body = %q", response.Code, response.Body.String())
			}

			a.runs.Release(blockerID)
			if got := a.runs.Claim(monitorID, now.Add(time.Minute), time.Hour); got != claimAccepted {
				t.Fatalf("post-update retry = %v, want accepted", got)
			}
		})
	}
}

func TestManualMonitorCheckDistinguishesOverlapFromCapacity(t *testing.T) {
	db := migratedTestDB(t)
	monitorID := insertSyntheticMonitor(t, db, "torbox")
	key := bytes.Repeat([]byte{1}, 32)
	a := &app{db: db, key: key, runs: newRunCoordinator(1)}
	nonce, ciphertext, err := a.encrypt([]byte("test-credential"), "monitor:1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO monitor_secrets(monitor_id,nonce,ciphertext,updated_at) VALUES(?,?,?,?)`, monitorID, nonce, ciphertext, 1); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if got := a.runs.Claim(monitorID, now, time.Hour); got != claimAccepted {
		t.Fatalf("monitor claim = %v, want accepted", got)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/monitors/1/test", nil)
	request.SetPathValue("id", "1")
	overlap := httptest.NewRecorder()
	a.testMonitor(overlap, request)
	if overlap.Code != http.StatusConflict {
		t.Fatalf("overlap status = %d, want %d", overlap.Code, http.StatusConflict)
	}
	a.runs.Release(monitorID)

	if got := a.runs.Claim(99, now, time.Hour); got != claimAccepted {
		t.Fatalf("capacity holder claim = %v, want accepted", got)
	}
	capacity := httptest.NewRecorder()
	a.testMonitor(capacity, request)
	if capacity.Code != http.StatusServiceUnavailable {
		t.Fatalf("capacity status = %d, want %d", capacity.Code, http.StatusServiceUnavailable)
	}
}

func TestAuthenticatedEndpoint(t *testing.T) {
	bearer := providerDefinition{Endpoint: "https://example.test/api/user", Auth: authBearer}
	got, err := authenticatedEndpoint(bearer, "secret-value")
	if err != nil {
		t.Fatalf("authenticatedEndpoint() error = %v", err)
	}
	if got != "https://example.test/api/user" {
		t.Fatalf("bearer endpoint must be unchanged, got %q", got)
	}

	query := providerDefinition{Endpoint: "https://example.test/v4/user?agent=debridup", Auth: authQueryParam, AuthParam: "apikey"}
	got, err = authenticatedEndpoint(query, "secret value/with+chars")
	if err != nil {
		t.Fatalf("authenticatedEndpoint() error = %v", err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("result is not a valid URL: %v", err)
	}
	if parsed.Query().Get("apikey") != "secret value/with+chars" {
		t.Fatalf("credential was not carried verbatim, got %q", parsed.Query().Get("apikey"))
	}
	if parsed.Query().Get("agent") != "debridup" {
		t.Fatalf("existing query parameters must be preserved, got %q", got)
	}
}

func TestEveryProviderDeclaresAuthAndErrorFields(t *testing.T) {
	for id, definition := range providerDefinitions {
		if definition.Auth == authQueryParam && definition.AuthParam == "" {
			t.Fatalf("provider %q uses query-parameter auth without naming the parameter", id)
		}
		if len(definition.ErrorFields) == 0 {
			t.Fatalf("provider %q declares no error fields, so payload errors cannot be classified", id)
		}
	}
}

func TestListIncidentsGroupsEventsOntoTheirIncident(t *testing.T) {
	a := testApp(t)
	if err := migrateDatabase(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := a.db.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`INSERT INTO monitors(id,provider,name,created_at,updated_at) VALUES(1,'torbox','TorBox',1,1),(2,'realdebrid','Real-Debrid',1,1)`)
	exec(`INSERT INTO check_results(id,monitor_id,source,state,duration_ms,http_status,error_code,checked_at) VALUES(10,1,'authenticated','api_issue',5,503,'server_error',100)`)
	// Two incidents on one monitor plus one on another, so a mis-grouped join
	// would show up as events landing on the wrong incident.
	exec(`INSERT INTO incidents(id,monitor_id,opened_at,detected_at,resolved_at,initial_state,latest_state,summary) VALUES
		(1,1,100,105,180,'api_issue','healthy','First outage'),
		(2,1,200,205,NULL,'auth_failed','auth_failed',NULL),
		(3,2,300,305,NULL,'connection_issue','connection_issue','Beta down')`)
	exec(`INSERT INTO incident_events(id,incident_id,type,new_state,created_at,check_id) VALUES
		(1,1,'opened','api_issue',105,10),
		(2,1,'recovered','healthy',180,NULL),
		(3,2,'opened','auth_failed',205,NULL),
		(4,3,'opened','connection_issue',305,NULL)`)

	response := httptest.NewRecorder()
	a.listIncidents(response, httptest.NewRequest(http.MethodGet, "/api/incidents", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}

	var incidents []struct {
		ID          int64  `json:"ID"`
		Name        string `json:"Name"`
		LatestState string `json:"LatestState"`
		Summary     string `json:"summary"`
		ResolvedAt  *int64 `json:"resolvedAt"`
		Events      []struct {
			Type      string `json:"type"`
			State     string `json:"state"`
			Summary   string `json:"summary"`
			CreatedAt int64  `json:"createdAt"`
		} `json:"events"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &incidents); err != nil {
		t.Fatalf("decode: %v (body %s)", err, response.Body.String())
	}
	if len(incidents) != 3 {
		t.Fatalf("got %d incidents, want 3", len(incidents))
	}
	// Ordered by opened_at DESC.
	if incidents[0].ID != 3 || incidents[1].ID != 2 || incidents[2].ID != 1 {
		t.Fatalf("unexpected incident order: %d,%d,%d", incidents[0].ID, incidents[1].ID, incidents[2].ID)
	}

	byID := map[int64]int{}
	for i, incident := range incidents {
		byID[incident.ID] = i
	}
	first := incidents[byID[1]]
	if len(first.Events) != 2 {
		t.Fatalf("incident 1 got %d events, want 2", len(first.Events))
	}
	if first.Events[0].Type != "opened" || first.Events[1].Type != "recovered" {
		t.Fatalf("incident 1 events out of order: %+v", first.Events)
	}
	// The joined check row supplies the HTTP status used in the summary.
	if !strings.Contains(first.Events[0].Summary, "503") {
		t.Fatalf("incident 1 opened summary lost its check detail: %q", first.Events[0].Summary)
	}
	if first.Events[1].Summary != "Authenticated checks recovered and the incident was resolved." {
		t.Fatalf("unexpected recovery summary: %q", first.Events[1].Summary)
	}
	// A resolved incident reports its initial state, not 'healthy'.
	if first.LatestState != "api_issue" {
		t.Fatalf("resolved incident LatestState = %q, want api_issue", first.LatestState)
	}

	second := incidents[byID[2]]
	if len(second.Events) != 1 || second.Events[0].State != stateAuthFailed {
		t.Fatalf("incident 2 events = %+v", second.Events)
	}
	// A NULL summary falls back to the state description.
	if second.Summary != incidentStateDescription(stateAuthFailed) {
		t.Fatalf("incident 2 summary = %q", second.Summary)
	}
	if second.ResolvedAt != nil {
		t.Fatalf("incident 2 must be unresolved")
	}

	third := incidents[byID[3]]
	if third.Name != "Real-Debrid" || len(third.Events) != 1 {
		t.Fatalf("incident 3 = %+v", third)
	}
}

func TestListIncidentsWithNoIncidentsReturnsEmptyArray(t *testing.T) {
	a := testApp(t)
	if err := migrateDatabase(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	a.listIncidents(response, httptest.NewRequest(http.MethodGet, "/api/incidents", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if body := strings.TrimSpace(response.Body.String()); body != "[]" {
		t.Fatalf("body = %q, want []", body)
	}
}

// TestOverviewMatchesReferenceImplementation pins the rewritten batched
// overview to the per-monitor algorithm it replaced. The reference below is a
// direct transcription of the original implementation.
func TestOverviewMatchesReferenceImplementation(t *testing.T) {
	a := testApp(t)
	if err := migrateDatabase(context.Background(), a.db); err != nil {
		t.Fatal(err)
	}

	now := time.Now().Unix()
	cutoff := now - int64(30*24*time.Hour/time.Second)
	random := rand.New(rand.NewSource(20260822))

	tx, err := a.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	providers := []string{"torbox", "premiumize", "alldebrid", "realdebrid", "torrin"}
	for id := 1; id <= len(providers); id++ {
		if _, err := tx.Exec(`INSERT INTO monitors(id,provider,name,created_at,updated_at) VALUES(?,?,?,?,?)`,
			id, providers[id-1], providers[id-1], now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO monitor_states(monitor_id,current_state,state_since,last_raw_state,last_check_at) VALUES(?,?,?,?,?)`,
			id, stateHealthy, now, stateHealthy, now); err != nil {
			t.Fatal(err)
		}
		// More than 1000 healthy samples on one monitor so the recency cap is
		// actually exercised, and some samples deliberately outside the window.
		samples := 300 + id*400
		for i := 0; i < samples; i++ {
			state := stateHealthy
			switch {
			case i%53 == 0:
				state = stateAuthFailed
			case i%37 == 0:
				state = stateAPI
			}
			checkedAt := now - int64(i)*900
			if _, err := tx.Exec(`INSERT INTO check_results(monitor_id,source,state,duration_ms,checked_at) VALUES(?,'authenticated',?,?,?)`,
				id, state, int64(random.Intn(900)+10), checkedAt); err != nil {
				t.Fatal(err)
			}
		}
		// Public rows must never influence overview.
		if _, err := tx.Exec(`INSERT INTO check_results(monitor_id,source,state,duration_ms,checked_at) VALUES(?,'public','healthy',1,?)`, id, now-60); err != nil {
			t.Fatal(err)
		}
		for incident := 0; incident < id; incident++ {
			opened := now - int64(incident+1)*86400
			var resolved any
			if incident%2 == 0 {
				resolved = opened + 3600
			}
			if _, err := tx.Exec(`INSERT INTO incidents(monitor_id,opened_at,detected_at,resolved_at,initial_state,latest_state,summary) VALUES(?,?,?,?,?,?,?)`,
				id, opened, opened+60, resolved, stateAPI, stateAPI, "seeded"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Checks were inserted in bulk rather than through recordResult, so the
	// rollups that coverage is summed from are built here.
	if err := rebuildRollups(context.Background(), a.db, nil); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	a.overview(response, httptest.NewRequest(http.MethodGet, "/api/overview", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		GeneratedAt int64 `json:"generatedAt"`
		Monitors    []struct {
			ID           int64    `json:"id"`
			State        string   `json:"State"`
			Availability *float64 `json:"availability"`
			Coverage     *float64 `json:"coverage"`
			P95MS        *int64   `json:"p95Ms"`
			OpenIncident bool     `json:"openIncident"`
		} `json:"monitors"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Monitors) != len(providers) {
		t.Fatalf("got %d monitors, want %d", len(payload.Monitors), len(providers))
	}
	// Availability is measured against the handler's own clock. Use the
	// timestamp it reported so the reference below is compared at the same
	// instant rather than one captured before the request ran.
	now = payload.GeneratedAt
	cutoff = now - int64(30*24*time.Hour/time.Second)

	for _, got := range payload.Monitors {
		id := got.ID

		// --- reference: coverage ---
		var total, eligible int
		if err := a.db.QueryRow(`SELECT COUNT(*),COUNT(CASE WHEN state!='auth_failed' THEN 1 END) FROM check_results WHERE monitor_id=? AND source='authenticated' AND checked_at>=?`, id, cutoff).Scan(&total, &eligible); err != nil {
			t.Fatal(err)
		}
		var wantCoverage *float64
		if total > 0 {
			v := float64(eligible) / float64(total) * 100
			wantCoverage = &v
		}
		assertFloatPointer(t, "coverage", id, got.Coverage, wantCoverage)

		// --- reference: p95 over the most recent 1000 healthy checks ---
		latencyRows, err := a.db.Query(`SELECT duration_ms FROM check_results WHERE monitor_id=? AND source='authenticated' AND checked_at>=? AND state='healthy' ORDER BY checked_at DESC LIMIT 1000`, id, cutoff)
		if err != nil {
			t.Fatal(err)
		}
		var latencies []int64
		for latencyRows.Next() {
			var ms int64
			if err := latencyRows.Scan(&ms); err != nil {
				t.Fatal(err)
			}
			latencies = append(latencies, ms)
		}
		latencyRows.Close()
		var wantP95 *int64
		if len(latencies) > 0 {
			sorted := append([]int64(nil), latencies...)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
			v := sorted[(len(sorted)*95+99)/100-1]
			wantP95 = &v
		}
		if (got.P95MS == nil) != (wantP95 == nil) {
			t.Fatalf("monitor %d p95 presence mismatch: got %v want %v", id, got.P95MS, wantP95)
		}
		if wantP95 != nil && *got.P95MS != *wantP95 {
			t.Fatalf("monitor %d p95 = %d, want %d (over %d samples)", id, *got.P95MS, *wantP95, len(latencies))
		}

		// --- reference: confirmed availability ---
		var firstCheck sql.NullInt64
		if err := a.db.QueryRow(`SELECT MIN(checked_at) FROM check_results WHERE monitor_id=? AND source='authenticated'`, id).Scan(&firstCheck); err != nil {
			t.Fatal(err)
		}
		var wantAvailability *float64
		if firstCheck.Valid {
			observedStart := max(firstCheck.Int64, cutoff)
			periodRows, err := a.db.Query(`SELECT opened_at,resolved_at FROM incidents WHERE monitor_id=? AND opened_at<? AND (resolved_at IS NULL OR resolved_at>?) ORDER BY opened_at`, id, now, observedStart)
			if err != nil {
				t.Fatal(err)
			}
			var periods []incidentPeriod
			for periodRows.Next() {
				var period incidentPeriod
				var resolved sql.NullInt64
				if err := periodRows.Scan(&period.OpenedAt, &resolved); err != nil {
					t.Fatal(err)
				}
				if resolved.Valid {
					period.ResolvedAt = resolved.Int64
				}
				periods = append(periods, period)
			}
			periodRows.Close()
			wantAvailability = confirmedAvailability(observedStart, now, periods)
		}
		assertFloatPointer(t, "availability", id, got.Availability, wantAvailability)

		// --- reference: open incident flag ---
		var open int
		if err := a.db.QueryRow(`SELECT COUNT(*) FROM incidents WHERE monitor_id=? AND resolved_at IS NULL`, id).Scan(&open); err != nil {
			t.Fatal(err)
		}
		if got.OpenIncident != (open > 0) {
			t.Fatalf("monitor %d openIncident = %t, want %t", id, got.OpenIncident, open > 0)
		}
	}
}

func assertFloatPointer(t *testing.T, label string, id int64, got, want *float64) {
	t.Helper()
	if (got == nil) != (want == nil) {
		t.Fatalf("monitor %d %s presence mismatch: got %v want %v", id, label, got, want)
	}
	if want != nil && math.Abs(*got-*want) > 1e-9 {
		t.Fatalf("monitor %d %s = %v, want %v", id, label, *got, *want)
	}
}

func TestSecurityHeadersIncludeContentSecurityPolicy(t *testing.T) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/login.html", nil)
	(&app{}).routes().ServeHTTP(response, request)

	policy := response.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'self'", "object-src 'none'", "base-uri 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(policy, directive) {
			t.Fatalf("CSP missing %q: %q", directive, policy)
		}
	}
	// The app has no inline scripts or styles, so neither escape hatch belongs.
	if strings.Contains(policy, "unsafe-inline") || strings.Contains(policy, "unsafe-eval") {
		t.Fatalf("CSP must not relax script or style execution: %q", policy)
	}
}

func TestStaticAssetsAreCacheableAndRevalidate(t *testing.T) {
	routes := (&app{}).routes()

	response := httptest.NewRecorder()
	routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/app.css", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	etag := response.Header().Get("ETag")
	if etag == "" {
		t.Fatal("static assets must carry an ETag")
	}
	if cacheControl := response.Header().Get("Cache-Control"); strings.Contains(cacheControl, "no-store") {
		t.Fatalf("static assets must be cacheable, got %q", cacheControl)
	}

	// A conditional request revalidates instead of resending the body.
	conditional := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/app.css", nil)
	request.Header.Set("If-None-Match", etag)
	routes.ServeHTTP(conditional, request)
	if conditional.Code != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want 304", conditional.Code)
	}
	if conditional.Body.Len() != 0 {
		t.Fatalf("304 must not carry a body, got %d bytes", conditional.Body.Len())
	}
	if conditional.Header().Get("Content-Encoding") != "" {
		t.Fatalf("304 must not be gzip encoded, got %q", conditional.Header().Get("Content-Encoding"))
	}
}

func TestAPIResponsesStayUncached(t *testing.T) {
	response := httptest.NewRecorder()
	(&app{}).routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/dashboard?range=24h", nil))
	if cacheControl := response.Header().Get("Cache-Control"); cacheControl != "no-store" {
		t.Fatalf("API Cache-Control = %q, want no-store", cacheControl)
	}
}

func TestCompressionAppliesToTextAndAnnouncesVary(t *testing.T) {
	routes := (&app{}).routes()

	request := httptest.NewRequest(http.MethodGet, "/app.css", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()
	routes.ServeHTTP(response, request)

	if response.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", response.Header().Get("Content-Encoding"))
	}
	if !strings.Contains(response.Header().Get("Vary"), "Accept-Encoding") {
		t.Fatalf("Vary = %q, want Accept-Encoding", response.Header().Get("Vary"))
	}
	reader, err := gzip.NewReader(response.Body)
	if err != nil {
		t.Fatalf("body is not valid gzip: %v", err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	if !bytes.Contains(decoded, []byte("--bg")) {
		t.Fatalf("decompressed stylesheet does not look like app.css (%d bytes)", len(decoded))
	}

	// Vary is announced even when the client does not accept gzip, so shared
	// caches never hand a gzipped body to a client that cannot read it.
	plain := httptest.NewRecorder()
	routes.ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/app.css", nil))
	if plain.Header().Get("Content-Encoding") != "" {
		t.Fatalf("unrequested Content-Encoding = %q", plain.Header().Get("Content-Encoding"))
	}
	if !strings.Contains(plain.Header().Get("Vary"), "Accept-Encoding") {
		t.Fatalf("Vary must be set even without gzip, got %q", plain.Header().Get("Vary"))
	}
}

func TestCompressibleType(t *testing.T) {
	for _, contentType := range []string{"text/css", "text/html; charset=utf-8", "application/json", "image/svg+xml"} {
		if !compressibleType(contentType) {
			t.Fatalf("%q should be compressible", contentType)
		}
	}
	for _, contentType := range []string{"image/png", "application/octet-stream", "font/woff2", ""} {
		if compressibleType(contentType) {
			t.Fatalf("%q should not be compressed", contentType)
		}
	}
}
