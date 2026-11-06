import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { rollup } from 'rollup';
import { nodeResolve, DEFAULTS } from '@rollup/plugin-node-resolve';
import cloneDefaults from './index.cjs';

const require = createRequire(import.meta.url);
const resolverRequire = createRequire(require.resolve('@rollup/plugin-node-resolve'));

test('installed resolver uses the private clone adapter and retains frozen defaults', () => {
  const lock = JSON.parse(readFileSync(new URL('../../package-lock.json', import.meta.url), 'utf8'));
  const consumers = Object.entries(lock.packages).filter(([, pkg]) => pkg.dependencies?.deepmerge);
  assert.deepEqual(consumers.map(([path, pkg]) => [path, pkg.version]),
    [['node_modules/@rollup/plugin-node-resolve', '16.0.3']]);
  const manifest = JSON.parse(readFileSync(resolverRequire.resolve('deepmerge/package.json'), 'utf8'));
  assert.equal(manifest.name, '@kyber/rollup-defaults-clone');
  assert.equal(resolverRequire('deepmerge'), cloneDefaults);
  assert.deepEqual(DEFAULTS.extensions, ['.mjs', '.js', '.json', '.node']);
  assert.deepEqual(DEFAULTS.moduleDirectories, ['node_modules']);
  assert.ok(Object.isFrozen(DEFAULTS));
  assert.ok(Object.isFrozen(DEFAULTS.extensions));
  assert.equal(typeof resolverRequire('@rollup/plugin-node-resolve').nodeResolve, 'function');
});

test('cloning leaves source arrays and nested records independent; unexpected merge use fails', () => {
  const source = { extensions: ['.mjs', '.js'], flags: { enabled: true } };
  const snapshot = cloneDefaults({}, source);
  assert.deepEqual(snapshot, source);
  Object.freeze(snapshot.extensions);
  source.extensions.push('.custom');
  source.flags.enabled = false;
  assert.deepEqual(snapshot.extensions, ['.mjs', '.js']);
  assert.equal(snapshot.flags.enabled, true);
  assert.equal(Object.getPrototypeOf(snapshot), Object.prototype);
  assert.throws(() => cloneDefaults({ existing: true }, source), /empty plain target/);
  assert.throws(() => cloneDefaults({}, source, { clone: false }), /empty plain target/);
});

test('actual Rollup resolves package exports and custom extensions with user options', async () => {
  const root = mkdtempSync(join(tmpdir(), 'kyber-resolver-'));
  try {
    const pkg = join(root, 'node_modules', 'resolver-fixture');
    mkdirSync(pkg, { recursive: true });
    writeFileSync(join(pkg, 'package.json'), JSON.stringify({
      name: 'resolver-fixture', type: 'module', exports: { import: './modern.mjs', default: './fallback.mjs' },
    }));
    writeFileSync(join(pkg, 'modern.mjs'), 'export const value = 41;');
    writeFileSync(join(pkg, 'fallback.mjs'), 'export const value = 0;');
    writeFileSync(join(root, 'extra.custom'), 'export default 1;');
    const input = join(root, 'entry.mjs');
    writeFileSync(input, "import { value } from 'resolver-fixture'; import extra from './extra'; export default value + extra;");
    const bundle = await rollup({ input, plugins: [nodeResolve({ extensions: ['.mjs', '.custom'] })] });
    try {
      const { output } = await bundle.generate({ format: 'es' });
      assert.deepEqual(output[0].imports, []);
      const result = await import(`data:text/javascript;base64,${Buffer.from(output[0].code).toString('base64')}`);
      assert.equal(result.default, 42);
    } finally {
      await bundle.close();
    }
    // Creating a resolver with custom options must not alter the shared defaults.
    assert.deepEqual(DEFAULTS.extensions, ['.mjs', '.js', '.json', '.node']);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
