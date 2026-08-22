import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');

function readWorkflow(name) {
  return readFileSync(resolve(root, '.github', 'workflows', name), 'utf8');
}

function topLevelBlock(source, key) {
  const lines = source.split(/\r?\n/);
  const start = lines.findIndex((line) => line === `${key}:`);
  assert.notEqual(start, -1, `missing top-level ${key}`);
  const body = [];
  for (let index = start + 1; index < lines.length; index += 1) {
    const line = lines[index];
    if (line && !line.startsWith(' ')) break;
    body.push(line);
  }
  return body.join('\n');
}

function jobBlock(source, name) {
  const jobs = topLevelBlock(source, 'jobs');
  const lines = jobs.split(/\r?\n/);
  const start = lines.findIndex((line) => line === `  ${name}:`);
  assert.notEqual(start, -1, `missing ${name} job`);
  const body = [];
  for (let index = start + 1; index < lines.length; index += 1) {
    const line = lines[index];
    if (/^  [A-Za-z0-9_-]+:\s*$/.test(line)) break;
    body.push(line);
  }
  return body.join('\n');
}

test('verification workflow supports direct and reusable execution', () => {
  const workflow = readWorkflow('test.yml');
  const triggers = topLevelBlock(workflow, 'on');

  assert.match(triggers, /^  pull_request:\s*$/m);
  assert.match(triggers, /^  push:\s*$/m);
  assert.match(triggers, /^      - main\s*$/m);
  assert.match(triggers, /^  workflow_call:\s*$/m);
  assert.match(triggers, /PRIVATE_PATTERNS:[\s\S]*required: false/);
  assert.equal(topLevelBlock(workflow, 'permissions').trim(), 'contents: read');
});

test('verification job runs every required gate with fixed toolchains', () => {
  const workflow = readWorkflow('test.yml');
  const verify = jobBlock(workflow, 'verify');

  for (const expected of [
    'go-version: "1.24.x"',
    'node-version: "22"',
    'gofmt -l',
    'go test ./...',
    'go test -race ./...',
    'go vet ./...',
    'node --test cmd/debridup/web/dashboard-model.test.mjs',
    'golang.org/x/vuln/cmd/govulncheck@v1.7.0',
    'govulncheck ./...',
    'docker build -t debridup:test .',
    'libimage-exiftool-perl',
    'sh scripts/check-release-safety.sh "$BASE_SHA"',
  ]) {
    assert.ok(verify.includes(expected), `missing verification contract: ${expected}`);
  }

  assert.match(verify, /PRIVATE_PATTERNS:\s*\$\{\{ secrets\.PRIVATE_PATTERNS \}\}/);
  assert.match(verify, /github\.event\.pull_request\.title/);
  assert.match(verify, /github\.event\.pull_request\.body/);
  assert.match(verify, /BASE_SHA:/);
});

test('container publication depends on the reusable verification workflow', () => {
  const workflow = readWorkflow('container.yml');
  const verify = jobBlock(workflow, 'verify');
  const publish = jobBlock(workflow, 'publish');

  assert.match(verify, /uses: \.\/\.github\/workflows\/test\.yml/);
  assert.match(publish, /needs: verify/);
});
