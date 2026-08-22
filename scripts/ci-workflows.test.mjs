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
  assert.doesNotMatch(readWorkflow('container.yml'), /^  workflow_dispatch:\s*$/m);
});

test('verification job runs every required gate with fixed toolchains', () => {
  const workflow = readWorkflow('test.yml');
  const verify = jobBlock(workflow, 'verify');
  const dockerfile = readFileSync(resolve(root, 'Dockerfile'), 'utf8');

  for (const expected of [
    'go-version: "1.25.14"',
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
  assert.match(verify, /0000000000000000000000000000000000000000/);

  const workflowGo = verify.match(/go-version:\s*"([^"]+)"/)?.[1];
  const imageGo = dockerfile.match(/^FROM golang:([0-9.]+)-alpine AS build$/m)?.[1];
  assert.equal(imageGo, workflowGo, 'Docker builder Go version must match the CI security toolchain');
});

test('container publication depends on the reusable verification workflow', () => {
  const workflow = readWorkflow('container.yml');
  const verify = jobBlock(workflow, 'verify');
  const publish = jobBlock(workflow, 'publish');

  assert.match(verify, /uses: \.\/\.github\/workflows\/test\.yml/);
  assert.match(publish, /needs: verify/);
  assert.doesNotMatch(workflow, /secrets:\s*inherit/);
  assert.match(verify, /secrets:[\s\S]*PRIVATE_PATTERNS:\s*\$\{\{ secrets\.PRIVATE_PATTERNS \}\}/);
});

test('every external action is pinned to a reviewed release commit', () => {
  const workflows = [readWorkflow('test.yml'), readWorkflow('container.yml')].join('\n');
  const uses = [...workflows.matchAll(/^\s*uses:\s+([^\s#]+)(?:\s+#\s+(v\d+\.\d+\.\d+))?\s*$/gm)]
    .map((match) => ({ reference: match[1], version: match[2] }));

  const expected = new Map([
    ['actions/checkout', ['d23441a48e516b6c34aea4fa41551a30e30af803', 'v6.1.0']],
    ['actions/setup-go', ['924ae3a1cded613372ab5595356fb5720e22ba16', 'v6.5.0']],
    ['actions/setup-node', ['249970729cb0ef3589644e2896645e5dc5ba9c38', 'v6.5.0']],
    ['docker/setup-qemu-action', ['96fe6ef7f33517b61c61be40b68a1882f3264fb8', 'v4.2.0']],
    ['docker/setup-buildx-action', ['37fe631027851001ddb9b187196cc803df7f5f0e', 'v4.3.0']],
    ['docker/login-action', ['dbcb813823bdd20940b903addbd779551569679f', 'v4.6.0']],
    ['docker/metadata-action', ['dc802804100637a589fabce1cb79ff13a1411302', 'v6.2.0']],
    ['docker/build-push-action', ['53b7df96c91f9c12dcc8a07bcb9ccacbed38856a', 'v7.3.0']],
  ]);

  for (const [name, [sha, version]] of expected) {
    const matches = uses.filter(({ reference }) => reference === `${name}@${sha}`);
    assert.ok(matches.length >= 1, `missing pinned action ${name}`);
    assert.ok(matches.every((entry) => entry.version === version), `missing release comment for ${name}`);
  }

  for (const { reference, version } of uses) {
    if (reference.startsWith('./')) continue;
    assert.match(reference, /^[^@]+@[0-9a-f]{40}$/);
    assert.match(version ?? '', /^v\d+\.\d+\.\d+$/);
  }
});
