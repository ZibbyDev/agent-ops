#!/usr/bin/env node
// Copyright 2026 Zibby Lab. Apache-2.0.
//
// publish-apt.mjs — generate + upload the dl.zibby.app/apt APT repository.
//
// Sibling of scripts/publish-binaries.mjs. The binary publisher dumps
// per-version artifacts under `agent-ops/v<ver>/`. This script then
// (re)builds a Debian APT repository from ALL `.deb` files currently in
// `agent-ops/v*/`, signs the Release metadata with the Zibby APT signing
// key from AWS Secrets Manager, and uploads it under `apt/`.
//
// Result on dl.zibby.app:
//
//   apt/
//   ├── key.gpg                      ← public GPG key (operator commits)
//   ├── dists/stable/
//   │   ├── Release  /  InRelease  /  Release.gpg
//   │   └── main/{binary-amd64,binary-arm64}/Packages{,.gz}
//   └── pool/main/a/agent-ops/agent-ops_<ver>_<arch>.deb
//
// End-user install (also documented in README.md):
//
//   curl -fsSL https://dl.zibby.app/apt/key.gpg | sudo gpg --dearmor -o /etc/apt/keyrings/zibby.gpg
//   echo "deb [signed-by=/etc/apt/keyrings/zibby.gpg] https://dl.zibby.app/apt stable main" \
//     | sudo tee /etc/apt/sources.list.d/zibby.list
//   sudo apt update && sudo apt install agent-ops
//
// Tool choice — apt-ftparchive (not aptly)
// ----------------------------------------
// Debian's `apt-ftparchive` (ships in the `dpkg-dev` package; on macOS:
// `brew install dpkg`) is the canonical thing Debian itself uses to
// generate Packages + Release metadata. aptly is friendlier for daily-use
// repo management (snapshots, mirrors, publishes), but it's a 30+MB Go
// binary with its own database state — overkill for what we want, which
// is "scan a flat dir of debs, emit standard repo metadata, sign it,
// upload it." apt-ftparchive is ~5 lines of shell-out from this script.
//
// GPG signing
// -----------
// One-time operator setup (NOT run by this script — documented in the
// runbook below). The script ITSELF:
//   1. Creates a fresh per-run GNUPGHOME at /tmp/agent-ops-apt-gpg-<pid>/.
//   2. Pulls the armored private key from
//      `aws secretsmanager get-secret-value --secret-id /zibby/prod/apt-signing-key`.
//   3. Pipes that key into `gpg --import` inside the per-run home.
//   4. Signs Release → InRelease (clearsign) + Release.gpg (detached).
//   5. Wipes the per-run GNUPGHOME on exit, success or failure.
//
// The private key NEVER touches disk outside that ephemeral GNUPGHOME —
// it lives as an in-memory string between `aws secretsmanager` and the
// spawned `gpg --import`. The wipe happens in a `finally` so a crash in
// signing still cleans up.
//
// Run it
// ------
//   node scripts/publish-apt.mjs --dry-run     # plan only, no S3/GPG
//   node scripts/publish-apt.mjs               # full build + sign + upload
//
// Required env / defaults
// -----------------------
//   AWS_PROFILE                    defaulted to "zibby"
//   AGENT_OPS_RELEASE_BUCKET       shared Studio downloads bucket
//   AGENT_OPS_CLOUDFRONT_ID        dl.zibby.app distribution
//   AGENT_OPS_APT_SECRET_ID        defaults to /zibby/prod/apt-signing-key
//   AGENT_OPS_APT_GPG_KEY_ID       optional explicit key id; if unset,
//                                  the script uses the first secret-key
//                                  in the imported keyring (which is the
//                                  one we just imported).
//
// =============================================================
// OPERATOR RUNBOOK — one-time setup (skip on subsequent runs)
// =============================================================
// 1. Generate a dedicated signing key:
//      gpg --quick-generate-key "Zibby APT Signing <noreply@zibby.dev>" rsa4096 default 5y
// 2. Export the private key armored:
//      gpg --armor --export-secret-keys "Zibby APT Signing" > zibby-apt-private.asc
// 3. Stash in AWS Secrets Manager:
//      AWS_PROFILE=zibby aws secretsmanager create-secret \
//        --name /zibby/prod/apt-signing-key \
//        --description "Armored private key used to sign dl.zibby.app/apt Release metadata" \
//        --secret-string file://zibby-apt-private.asc
// 4. Export the public key for users to download:
//      gpg --armor --export "Zibby APT Signing" > dist/apt/key.gpg
//    Commit dist/apt/key.gpg to the agent-ops repo so it ships with the
//    source. This script also re-uploads it on every run (idempotent) so
//    the dl.zibby.app/apt/key.gpg URL stays in sync.
// 5. Shred the local copies:
//      shred -u zibby-apt-private.asc
//      gpg --delete-secret-keys "Zibby APT Signing"   # optional — only if you
//                                                     # don't need to re-sign locally
// =============================================================

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const __filename = fileURLToPath(import.meta.url);
const scriptsDir = path.dirname(__filename);
const repoRoot = path.resolve(scriptsDir, '..');

const DEFAULTS = {
  bucket: 'zibbystudiodownloadsprod-studiodownloadsbucketa7b7-xw8j7htykcp2',
  cloudFrontDistributionId: 'E2RXTLGIV1TGD9',
  awsProfile: 'zibby',
  s3SrcPrefix: 'agent-ops',           // where the .deb files live (per-version)
  s3AptPrefix: 'apt',                  // where the APT repo lives
  publicKeyRepoPath: 'dist/apt/key.gpg',
  secretsManagerId: '/zibby/prod/apt-signing-key',
  distSuite: 'stable',
  distComponent: 'main',
  distArches: ['amd64', 'arm64'],
  distOrigin: 'Zibby',
  distLabel: 'Zibby APT',
  distDescription: 'Zibby OSS packages — agent-ops and friends.',
};

function log(msg) { process.stdout.write(`${msg}\n`); }
function fail(msg) { process.stderr.write(`\n[publish-apt] ${msg}\n`); process.exit(1); }

function exists(p) {
  try { fs.accessSync(p, fs.constants.F_OK); return true; } catch { return false; }
}

function toCloudFrontPath(key) {
  const cleanKey = String(key || '').trim().replace(/^\/+/, '');
  if (!cleanKey) return '/';
  return `/${cleanKey.split('/').map((part) => encodeURIComponent(part)).join('/')}`;
}

function getCliOptions(argv) {
  const out = {
    dryRun: false,
    bucket: '',
    cloudFrontDistributionId: '',
    secretId: '',
    gpgKeyId: '',
  };
  for (let i = 0; i < argv.length; i++) {
    const a = String(argv[i] || '').trim();
    if (!a.startsWith('--')) continue;
    const [rawKey, inlineValue] = a.split('=', 2);
    const key = rawKey.replace(/^--/, '');
    const next = inlineValue ?? argv[i + 1];
    const hasValue = inlineValue !== undefined || (next && !String(next).startsWith('--'));
    switch (key) {
      case 'dry-run':                  out.dryRun = true; break;
      case 'bucket':
        out.bucket = hasValue ? String(next).trim() : '';
        if (inlineValue === undefined && hasValue) i++; break;
      case 'cloudfront-distribution-id':
        out.cloudFrontDistributionId = hasValue ? String(next).trim() : '';
        if (inlineValue === undefined && hasValue) i++; break;
      case 'secret-id':
        out.secretId = hasValue ? String(next).trim() : '';
        if (inlineValue === undefined && hasValue) i++; break;
      case 'gpg-key-id':
        out.gpgKeyId = hasValue ? String(next).trim() : '';
        if (inlineValue === undefined && hasValue) i++; break;
      default: break;
    }
  }
  return out;
}

function runCommand(cmd, args, { capture = false, input = null, env = process.env, stdioErr = 'inherit' } = {}) {
  return new Promise((resolve, reject) => {
    const stdio = [
      input != null ? 'pipe' : 'ignore',
      capture ? 'pipe' : 'inherit',
      capture ? 'pipe' : stdioErr,
    ];
    const child = spawn(cmd, args, { cwd: repoRoot, stdio, shell: false, env });
    let stdout = '';
    let stderr = '';
    if (capture) {
      child.stdout.on('data', (chunk) => { stdout += chunk.toString(); });
      child.stderr.on('data', (chunk) => { stderr += chunk.toString(); });
    }
    if (input != null) {
      child.stdin.write(input);
      child.stdin.end();
    }
    child.on('error', reject);
    child.on('close', (code) => {
      if (code === 0) resolve(stdout);
      else reject(new Error(`${cmd} ${args.join(' ')} failed (code ${code})${capture ? `: ${stderr || stdout}` : ''}`));
    });
  });
}

async function checkBinary(cmd, hint, { dryRun }) {
  if (dryRun) return;
  try {
    await runCommand(cmd, ['--version'], { capture: true });
  } catch {
    fail(`Required binary not found: ${cmd}. ${hint}`);
  }
}

// List every .deb under agent-ops/v*/ in S3. The publish-binaries.mjs script
// is the only writer of that prefix, so this is the source-of-truth set of
// packages the APT repo should serve.
async function listDebsInBucket({ bucket }) {
  const out = await runCommand('aws', ['s3', 'ls', `s3://${bucket}/${DEFAULTS.s3SrcPrefix}/`, '--recursive'], { capture: true });
  const debs = [];
  for (const rawLine of out.split('\n')) {
    const line = rawLine.trim();
    if (!line) continue;
    // `aws s3 ls --recursive` lines: "2026-05-30 12:34:56  5421000 agent-ops/v0.2.0/agent-ops_0.2.0_linux_amd64.deb"
    const match = line.match(/^\S+\s+\S+\s+\d+\s+(.+)$/);
    if (!match) continue;
    const key = match[1];
    if (!key.endsWith('.deb')) continue;
    // Only pick canonically-named files we recognise. nfpms emits the linux+arch
    // permutations; we filter on that pattern to avoid surprises if random
    // .deb files end up under the prefix.
    const base = path.posix.basename(key);
    if (!/^agent-ops_[0-9][^_]*_(linux)_(amd64|arm64)\.deb$/.test(base)) continue;
    // Skip alias copies under agent-ops/latest/ — the canonical source is
    // agent-ops/v<ver>/. Without this filter every deb would be discovered
    // twice and downloaded twice into the same pool/ slot.
    if (key.startsWith(`${DEFAULTS.s3SrcPrefix}/latest/`)) continue;
    debs.push({ key, base });
  }
  return debs;
}

// Mock for dry-run mode: pretend we found one of each arch at v0.2.0 so the
// downstream plan output looks realistic. Operators see the layout the live
// run would produce, just without touching S3.
function mockDebs() {
  return [
    { key: 'agent-ops/v0.2.0/agent-ops_0.2.0_linux_amd64.deb', base: 'agent-ops_0.2.0_linux_amd64.deb' },
    { key: 'agent-ops/v0.2.0/agent-ops_0.2.0_linux_arm64.deb', base: 'agent-ops_0.2.0_linux_arm64.deb' },
  ];
}

function archOfDeb(base) {
  const m = base.match(/_(amd64|arm64)\.deb$/);
  return m ? m[1] : null;
}

// Pull the armored private key out of AWS Secrets Manager as a JS string.
// Returns the SecretString verbatim — we pipe it into `gpg --import` via
// stdin, never writing it to disk.
async function fetchPrivateKey({ secretId }) {
  const raw = await runCommand('aws', [
    'secretsmanager', 'get-secret-value',
    '--secret-id', secretId,
    '--query', 'SecretString',
    '--output', 'text',
  ], { capture: true });
  const trimmed = raw.replace(/\r?\n$/, '');
  if (!trimmed || !trimmed.includes('BEGIN PGP')) {
    fail(`Secret at ${secretId} doesn't look like an armored PGP private key.`);
  }
  return trimmed;
}

// Returns the first secret-key fingerprint in `gpgHome`. We need the
// fingerprint to point apt-ftparchive's `gpg --local-user` at it. If the
// operator passed --gpg-key-id we trust that and skip the lookup.
async function firstSecretKeyId({ gpgHome }) {
  const env = { ...process.env, GNUPGHOME: gpgHome };
  const out = await runCommand('gpg', ['--list-secret-keys', '--with-colons'], { capture: true, env });
  for (const line of out.split('\n')) {
    const cols = line.split(':');
    if (cols[0] === 'fpr' && cols[9]) return cols[9];
  }
  return '';
}

function aptFtpArchiveConfig({ stagingDir }) {
  // apt-ftparchive's "generate" command wants a config file describing
  // which dirs hold which arches. This is the canonical example from the
  // Debian docs, trimmed to what we actually need (no source packages,
  // no .udeb).
  return `Dir {
  ArchiveDir "${stagingDir}";
};

Default {
  Packages::Compress ". gzip";
  Contents::Compress ". gzip";
};

TreeDefault {
  Directory "pool/${DEFAULTS.distComponent}";
};

BinDirectory "pool/${DEFAULTS.distComponent}/a/agent-ops" {
  Packages "dists/${DEFAULTS.distSuite}/${DEFAULTS.distComponent}/binary-amd64/Packages";
};

Tree "dists/${DEFAULTS.distSuite}" {
  Sections "${DEFAULTS.distComponent}";
  Architectures "${DEFAULTS.distArches.join(' ')}";
};
`;
}

function releaseFileSettings({ stagingDir }) {
  // Settings for `apt-ftparchive release dists/<suite>` to stamp the
  // top-level Release metadata. The hashes for Packages files inside the
  // suite are filled in automatically by apt-ftparchive.
  return `APT::FTPArchive::Release::Origin "${DEFAULTS.distOrigin}";
APT::FTPArchive::Release::Label "${DEFAULTS.distLabel}";
APT::FTPArchive::Release::Suite "${DEFAULTS.distSuite}";
APT::FTPArchive::Release::Codename "${DEFAULTS.distSuite}";
APT::FTPArchive::Release::Architectures "${DEFAULTS.distArches.join(' ')}";
APT::FTPArchive::Release::Components "${DEFAULTS.distComponent}";
APT::FTPArchive::Release::Description "${DEFAULTS.distDescription}";
Dir { ArchiveDir "${stagingDir}"; };
`;
}

async function awsCpFile(localPath, s3Url, { dryRun, extraArgs = [] }) {
  if (dryRun) {
    log(`  [dry-run] aws s3 cp ${path.relative(repoRoot, localPath) || localPath} ${s3Url}${extraArgs.length ? ' ' + extraArgs.join(' ') : ''}`);
    return;
  }
  await runCommand('aws', ['s3', 'cp', localPath, s3Url, '--no-progress', ...extraArgs]);
}

async function awsCpS3ToS3(srcS3Url, dstS3Url, { dryRun }) {
  if (dryRun) {
    log(`  [dry-run] aws s3 cp ${srcS3Url} ${dstS3Url}`);
    return;
  }
  await runCommand('aws', ['s3', 'cp', srcS3Url, dstS3Url, '--no-progress']);
}

async function awsSyncDir(localDir, s3Url, { dryRun }) {
  if (dryRun) {
    log(`  [dry-run] aws s3 sync ${path.relative(repoRoot, localDir) || localDir} ${s3Url} --delete`);
    return;
  }
  // --delete keeps the bucket clean if a previous arch / version drops out.
  await runCommand('aws', ['s3', 'sync', localDir, s3Url, '--no-progress', '--delete']);
}

async function cloudFrontInvalidate({ distributionId, paths, dryRun }) {
  if (!distributionId) {
    log('Skipping CloudFront invalidation (no distribution id).');
    return;
  }
  if (paths.length === 0) return;
  const maxPathsPerInvalidation = 900;
  for (let i = 0; i < paths.length; i += maxPathsPerInvalidation) {
    const batch = paths.slice(i, i + maxPathsPerInvalidation);
    if (dryRun) {
      log(`  [dry-run] aws cloudfront create-invalidation --distribution-id ${distributionId} --paths ${batch.join(' ')}`);
      continue;
    }
    await runCommand('aws', [
      'cloudfront', 'create-invalidation',
      '--distribution-id', distributionId,
      '--paths', ...batch,
    ]);
  }
}

async function main() {
  if (!process.env.AWS_PROFILE) process.env.AWS_PROFILE = DEFAULTS.awsProfile;

  const cli = getCliOptions(process.argv.slice(2));
  const bucket = cli.bucket || process.env.AGENT_OPS_RELEASE_BUCKET || DEFAULTS.bucket;
  const cloudFrontDistributionId =
    cli.cloudFrontDistributionId
    || process.env.AGENT_OPS_CLOUDFRONT_ID
    || DEFAULTS.cloudFrontDistributionId;
  const secretId = cli.secretId || process.env.AGENT_OPS_APT_SECRET_ID || DEFAULTS.secretsManagerId;
  const explicitGpgKeyId = cli.gpgKeyId || process.env.AGENT_OPS_APT_GPG_KEY_ID || '';
  const dryRun = cli.dryRun;

  log('\n=== agent-ops APT repository publish ===');
  log(`Mode:        ${dryRun ? 'dry-run (no S3 / Secrets / GPG mutations)' : 'live'}`);
  log(`Bucket:      ${bucket}`);
  log(`Src prefix:  ${DEFAULTS.s3SrcPrefix}/`);
  log(`APT prefix:  ${DEFAULTS.s3AptPrefix}/`);
  log(`CloudFront:  ${cloudFrontDistributionId || '(none)'}`);
  log(`Secret id:   ${secretId}`);
  log(`Suite:       ${DEFAULTS.distSuite}`);
  log(`Component:   ${DEFAULTS.distComponent}`);
  log(`Architectures: ${DEFAULTS.distArches.join(', ')}`);

  await checkBinary('aws', 'Install AWS CLI v2.', { dryRun });
  await checkBinary('apt-ftparchive', 'macOS: `brew install dpkg`. Debian/Ubuntu: `apt install apt-utils`.', { dryRun });
  await checkBinary('gpg', 'macOS: `brew install gnupg`. Debian/Ubuntu: `apt install gnupg`.', { dryRun });

  // 0. Fail fast in live mode if the public-key file isn't a real PGP key
  //    yet. Avoids spending 5+ minutes building / staging / signing only
  //    to error at the very last upload step. Dry-run still proceeds so the
  //    operator can see the plan without setting up the key first.
  if (!dryRun) {
    const repoKeyPath = path.join(repoRoot, DEFAULTS.publicKeyRepoPath);
    if (!exists(repoKeyPath)) {
      fail(`${DEFAULTS.publicKeyRepoPath} not found. Generate it per the runbook at the top of this script, then commit + re-run.`);
    }
    const firstBytes = fs.readFileSync(repoKeyPath, 'utf8').slice(0, 64);
    if (!firstBytes.includes('BEGIN PGP PUBLIC KEY BLOCK')) {
      fail(`${DEFAULTS.publicKeyRepoPath} looks like the placeholder, not a real PGP public key. See the runbook in this script's header.`);
    }
  }

  // 1. Discover .deb files (live: from S3; dry-run: synthesised).
  const debs = dryRun ? mockDebs() : await listDebsInBucket({ bucket });
  if (debs.length === 0) {
    fail(`No .deb files found under s3://${bucket}/${DEFAULTS.s3SrcPrefix}/v*/. Run publish-binaries.mjs first.`);
  }
  log(`\nDiscovered ${debs.length} .deb file(s):`);
  for (const d of debs) log(`  - ${d.key}`);

  // 2. Staging dir holds the repo tree we'll upload. Wiped at the end.
  const stagingDir = fs.mkdtempSync(path.join(os.tmpdir(), 'agent-ops-apt-'));
  log(`\nStaging dir: ${stagingDir}`);

  // GNUPGHOME for this run — fresh + wiped on exit. Per-run home so we never
  // contaminate the operator's keychain with the publish key.
  const gpgHome = fs.mkdtempSync(path.join(os.tmpdir(), 'agent-ops-apt-gpg-'));
  fs.chmodSync(gpgHome, 0o700);
  const cleanup = () => {
    try { fs.rmSync(stagingDir, { recursive: true, force: true }); } catch { /* ignore */ }
    try { fs.rmSync(gpgHome, { recursive: true, force: true }); } catch { /* ignore */ }
  };
  process.on('exit', cleanup);
  process.on('SIGINT', () => { cleanup(); process.exit(130); });

  try {
    // 3. Lay out pool/ + dists/.
    const poolDir = path.join(stagingDir, 'pool', DEFAULTS.distComponent, 'a', 'agent-ops');
    fs.mkdirSync(poolDir, { recursive: true });
    for (const arch of DEFAULTS.distArches) {
      fs.mkdirSync(path.join(stagingDir, 'dists', DEFAULTS.distSuite, DEFAULTS.distComponent, `binary-${arch}`), { recursive: true });
    }

    // 4. Download every .deb into pool/main/a/agent-ops/ (or skip in dry-run).
    log(`\nStaging .deb files into pool/${DEFAULTS.distComponent}/a/agent-ops/ ...`);
    for (const deb of debs) {
      const dst = path.join(poolDir, deb.base);
      if (dryRun) {
        log(`  [dry-run] aws s3 cp s3://${bucket}/${deb.key} ${path.relative(repoRoot, dst) || dst}`);
        // Drop a small placeholder so the rest of the pipeline can still walk.
        fs.writeFileSync(dst, '');
      } else {
        await runCommand('aws', ['s3', 'cp', `s3://${bucket}/${deb.key}`, dst, '--no-progress']);
      }
    }

    // 5. Generate Packages + Packages.gz for each binary-<arch>. We call
    //    apt-ftparchive directly per-arch rather than via the `generate`
    //    config because apt-ftparchive's `packages` subcommand has a
    //    --arch filter that scans the pool and emits arch-specific output
    //    from a single dir — simpler than maintaining an arch-per-dir layout.
    log('\nGenerating Packages indexes ...');
    for (const arch of DEFAULTS.distArches) {
      const binDir = path.join(stagingDir, 'dists', DEFAULTS.distSuite, DEFAULTS.distComponent, `binary-${arch}`);
      const packagesPath = path.join(binDir, 'Packages');
      const packagesGzPath = path.join(binDir, 'Packages.gz');
      if (dryRun) {
        log(`  [dry-run] apt-ftparchive --arch ${arch} packages pool/ > ${path.relative(stagingDir, packagesPath)}`);
        fs.writeFileSync(packagesPath, '');
        fs.writeFileSync(packagesGzPath, '');
        continue;
      }
      // Pass the absolute staging-pool path: runCommand's default cwd is
      // repoRoot, which has no `pool/` of its own. apt-ftparchive then
      // emits absolute Filename: lines that we rewrite to be repo-relative
      // below.
      const poolAbs = path.join(stagingDir, 'pool');
      const out = await runCommand('apt-ftparchive', ['--arch', arch, 'packages', poolAbs], { capture: true, env: { ...process.env }, stdioErr: 'inherit' });
      // Rewrite Filename: prefix so absolute paths in the staging dir
      // become relative repo paths (apt-ftparchive emits them with the
      // prefix it was given, i.e. the absolute poolAbs).
      const stagingPrefix = stagingDir.endsWith('/') ? stagingDir : `${stagingDir}/`;
      const normalised = out.replaceAll(stagingPrefix, '');
      fs.writeFileSync(packagesPath, normalised, 'utf8');
      await runCommand('gzip', ['-9k', '-f', packagesPath]);
      log(`  ${arch}: ${packagesPath} (${fs.statSync(packagesPath).size} bytes)`);
    }

    // 6. Generate Release. apt-ftparchive computes MD5/SHA hashes for every
    //    Packages{,.gz} file in the suite tree and embeds them in Release.
    log('\nGenerating dists/stable/Release ...');
    const releaseConfPath = path.join(stagingDir, 'release.conf');
    fs.writeFileSync(releaseConfPath, releaseFileSettings({ stagingDir }), 'utf8');
    const releasePath = path.join(stagingDir, 'dists', DEFAULTS.distSuite, 'Release');
    if (dryRun) {
      log(`  [dry-run] apt-ftparchive -c release.conf release dists/${DEFAULTS.distSuite}/ > ${path.relative(stagingDir, releasePath)}`);
      fs.writeFileSync(releasePath, '');
    } else {
      // apt-ftparchive needs to be invoked with the staging root as cwd so
      // relative paths in the embedded checksums are stable.
      const releaseOut = await new Promise((resolve, reject) => {
        const child = spawn('apt-ftparchive', ['-c', releaseConfPath, 'release', `dists/${DEFAULTS.distSuite}/`], { cwd: stagingDir, stdio: ['ignore', 'pipe', 'inherit'] });
        let buf = '';
        child.stdout.on('data', (c) => { buf += c.toString(); });
        child.on('error', reject);
        child.on('close', (code) => code === 0 ? resolve(buf) : reject(new Error(`apt-ftparchive release failed (code ${code})`)));
      });
      fs.writeFileSync(releasePath, releaseOut, 'utf8');
      log(`  Release written (${fs.statSync(releasePath).size} bytes)`);
    }

    // 7. Import private signing key into the per-run GNUPGHOME and sign.
    let gpgKeyId = explicitGpgKeyId;
    if (!dryRun) {
      log(`\nImporting signing key from Secrets Manager (${secretId}) into ${gpgHome} ...`);
      const armoredKey = await fetchPrivateKey({ secretId });
      await runCommand('gpg', ['--batch', '--import'], {
        input: armoredKey,
        env: { ...process.env, GNUPGHOME: gpgHome },
      });
      if (!gpgKeyId) {
        gpgKeyId = await firstSecretKeyId({ gpgHome });
        if (!gpgKeyId) fail('Imported the secret but couldn\'t locate a secret-key fingerprint.');
      }
      log(`Signing with key ${gpgKeyId} ...`);
      const gpgEnv = { ...process.env, GNUPGHOME: gpgHome };
      const inReleasePath = path.join(stagingDir, 'dists', DEFAULTS.distSuite, 'InRelease');
      const releaseGpgPath = path.join(stagingDir, 'dists', DEFAULTS.distSuite, 'Release.gpg');
      // Clearsigned (InRelease) — apt prefers this since Debian buster.
      await runCommand('gpg', [
        '--batch', '--yes', '--pinentry-mode', 'loopback',
        '--local-user', gpgKeyId,
        '--armor', '--output', inReleasePath,
        '--clearsign', releasePath,
      ], { env: gpgEnv });
      // Detached sig — kept for older apt clients.
      await runCommand('gpg', [
        '--batch', '--yes', '--pinentry-mode', 'loopback',
        '--local-user', gpgKeyId,
        '--armor', '--output', releaseGpgPath,
        '--detach-sign', releasePath,
      ], { env: gpgEnv });
      log(`  InRelease + Release.gpg written.`);
    } else {
      log('\n  [dry-run] would: fetch signing key from Secrets Manager, import into temp GNUPGHOME, sign Release → InRelease + Release.gpg.');
    }

    // 8. Upload pool/ + dists/ to s3://<bucket>/apt/. We `aws s3 sync --delete`
    //    so dropped versions disappear from the published repo on the next
    //    run. dl.zibby.app/apt/key.gpg is also re-uploaded here (idempotent).
    const aptS3Base = `s3://${bucket}/${DEFAULTS.s3AptPrefix}`;
    log(`\nUploading APT repo to ${aptS3Base}/ ...`);
    await awsSyncDir(path.join(stagingDir, 'pool'), `${aptS3Base}/pool/`, { dryRun });
    await awsSyncDir(path.join(stagingDir, 'dists'), `${aptS3Base}/dists/`, { dryRun });

    const repoKeyPath = path.join(repoRoot, DEFAULTS.publicKeyRepoPath);
    if (!exists(repoKeyPath)) {
      log(`\n  WARNING: ${DEFAULTS.publicKeyRepoPath} not present in repo. The dl.zibby.app/apt/key.gpg URL won't be updated.`);
      log('  Operator: generate the key (see comments at top of this script), export public,');
      log('            and commit to dist/apt/key.gpg.');
    } else {
      // Refuse to upload the placeholder marker — operator hasn't replaced it yet.
      const firstBytes = fs.readFileSync(repoKeyPath, 'utf8').slice(0, 64);
      const looksLikePlaceholder = !firstBytes.includes('BEGIN PGP PUBLIC KEY BLOCK');
      if (looksLikePlaceholder && !dryRun) {
        fail(`${DEFAULTS.publicKeyRepoPath} looks like the placeholder, not a real PGP public key. See the runbook in this script's header.`);
      }
      log(`Uploading public key ${DEFAULTS.publicKeyRepoPath} -> ${aptS3Base}/key.gpg ...`);
      await awsCpFile(repoKeyPath, `${aptS3Base}/key.gpg`, { dryRun });
    }

    // 9. CloudFront invalidate just /apt/*. Studio's /download/ paths are
    //    untouched.
    await cloudFrontInvalidate({
      distributionId: cloudFrontDistributionId,
      paths: [toCloudFrontPath(`${DEFAULTS.s3AptPrefix}/*`)],
      dryRun,
    });

    log('\nDone.');
    log(`\nAPT consumer install:`);
    log(`  curl -fsSL https://dl.zibby.app/apt/key.gpg | sudo gpg --dearmor -o /etc/apt/keyrings/zibby.gpg`);
    log(`  echo "deb [signed-by=/etc/apt/keyrings/zibby.gpg] https://dl.zibby.app/apt ${DEFAULTS.distSuite} ${DEFAULTS.distComponent}" \\`);
    log(`    | sudo tee /etc/apt/sources.list.d/zibby.list`);
    log(`  sudo apt update && sudo apt install agent-ops`);
  } finally {
    // Cleanup is also registered as a process.exit listener for the crash
    // case, but call it here so logs show "wiped GPG home" before the
    // process actually exits.
    cleanup();
    log('\nWiped staging dir and per-run GNUPGHOME.');
  }
}

main().catch((e) => fail(e?.message || String(e)));
