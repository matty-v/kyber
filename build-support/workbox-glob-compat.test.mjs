import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import test from 'node:test';
import { getManifest } from 'workbox-build';

const require = createRequire(import.meta.url);
const workboxRequire = createRequire(require.resolve('workbox-build'));

// Workbox still declares glob ^11. This gate exercises the real consumer of
// the scoped major upgrade, rather than relying on a matcher-only test.
test('Workbox resolves glob 14 and a patched supported minimatch 11', () => {
  const glob = workboxRequire('glob');
  assert.equal(typeof glob.globSync, 'function');
  assert.equal(workboxRequire('glob/package.json').version, '14.0.4');
  const globRequire = createRequire(workboxRequire.resolve('glob'));
  const [major, minor, patch] = globRequire('minimatch/package.json').version.split('.').map(Number);
  assert.ok(major === 11 && (minor > 0 || patch >= 1));
});

test('actual Workbox manifest preserves asset filtering, ignores, content revisions and byte totals', async () => {
  const root = mkdtempSync(join(tmpdir(), 'kyber-workbox-'));
  const files = {
    'index.html': '<main>Kyber</main>',
    'assets/main.js': 'export const value = 42;',
    'assets/nested/app.css': 'body { color: white; }',
    'space file.js': 'export default true;',
    'assets/ignored.js': 'ignored',
    '.hidden.js': 'hidden',
    'notes.txt': 'not an asset',
  };
  try {
    for (const [name, content] of Object.entries(files)) {
      const path = join(root, name);
      mkdirSync(dirname(path), { recursive: true });
      writeFileSync(path, content);
    }
    mkdirSync(join(root, 'directory.js'));
    const result = await getManifest({
      globDirectory: root,
      globPatterns: ['**/*.{js,css,html}'],
      globIgnores: ['**/ignored.js'],
      globFollow: false,
    });
    const expected = ['index.html', 'assets/main.js', 'assets/nested/app.css', 'space file.js'].sort();
    assert.deepEqual(result.manifestEntries.map(entry => entry.url).sort(), expected);
    assert.deepEqual(result.warnings, []);
    assert.equal(result.count, expected.length);
    assert.equal(result.size, expected.reduce((sum, name) => sum + Buffer.byteLength(files[name]), 0));
    for (const entry of result.manifestEntries) {
      assert.equal(entry.revision, createHash('md5').update(files[entry.url]).digest('hex'));
    }
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test('actual Workbox reports an unmatched normal asset pattern', async () => {
  const root = mkdtempSync(join(tmpdir(), 'kyber-workbox-empty-'));
  try {
    const result = await getManifest({ globDirectory: root, globPatterns: ['missing/**/*.js'] });
    assert.equal(result.count, 0);
    assert.deepEqual(result.manifestEntries, []);
    assert.equal(result.warnings.length, 1);
    assert.match(result.warnings[0], /missing/);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
