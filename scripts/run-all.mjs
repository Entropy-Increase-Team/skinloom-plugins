#!/usr/bin/env node
/**
 * Workspace task runner for official SkinLoom plugins & themes.
 * Uses the sibling ../skinloom toolchain when available (zero duplicated node_modules).
 */

import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const ROOT = path.resolve(__dirname, '..')
const SKINLOOM_DIR = process.env.SKINLOOM_REPO_DIR
  ? path.resolve(process.env.SKINLOOM_REPO_DIR)
  : path.resolve(ROOT, '..', 'skinloom')
const SKINLOOM_WEB = path.join(SKINLOOM_DIR, 'web')

const PACKAGES = [
  'invitation-codes',
  'mojang-verification',
  'texture-description',
  'theme-atelier',
  'bs-importer',
  'mua-union',
]

function resolveBin(name, relPath) {
  const candidate = path.join(SKINLOOM_WEB, relPath)
  if (!fs.existsSync(candidate)) {
    throw new Error(
      `Missing ${name} at ${candidate}. Run "cd ../skinloom/web && bun install && bun run --filter @skinloom/cli build" first.`,
    )
  }
  return candidate
}

function run(cmd, args, cwd, extraEnv = {}) {
  console.log(`\n$ (${path.relative(ROOT, cwd) || '.'}) ${cmd} ${args.join(' ')}`)
  execFileSync(cmd, args, {
    cwd,
    stdio: 'inherit',
    env: { ...process.env, ...extraEnv },
  })
}

const task = process.argv[2]
if (!task) {
  console.error('Usage: node scripts/run-all.mjs <lint|typecheck|test|test:go|build|pack>')
  process.exit(1)
}

const existing = PACKAGES.filter((p) => fs.existsSync(path.join(ROOT, p, 'plugin.json')))

switch (task) {
  case 'lint': {
    const cliBin = resolveBin('@skinloom/cli', 'packages/cli/bin/skinloom-plugin.mjs')
    for (const pkg of existing) {
      run(process.execPath, [cliBin, 'lint', '--dir', path.join(ROOT, pkg)], ROOT)
    }
    break
  }

  case 'typecheck': {
    const tscBin = resolveBin('typescript', 'node_modules/typescript/bin/tsc')
    for (const pkg of existing) {
      const tsconfig = path.join(ROOT, pkg, 'tsconfig.json')
      if (fs.existsSync(tsconfig)) {
        run(process.execPath, [tscBin, '-p', tsconfig], path.join(ROOT, pkg))
      }
    }
    break
  }

  case 'test': {
    const vitestBin = resolveBin('vitest', 'node_modules/vitest/vitest.mjs')
    for (const pkg of existing) {
      const pkgDir = path.join(ROOT, pkg)
      const hasTsTest =
        fs.existsSync(path.join(pkgDir, 'src', 'server.test.ts')) ||
        fs.existsSync(path.join(pkgDir, 'src', 'theme.test.ts'))
      if (hasTsTest) {
        run(process.execPath, [vitestBin, 'run', '--config', path.join(ROOT, 'vitest.config.mjs')], pkgDir)
      }
    }
    break
  }

  case 'test:go': {
    for (const pkg of existing) {
      const pkgDir = path.join(ROOT, pkg)
      if (fs.existsSync(path.join(pkgDir, 'go.mod'))) {
        run('go', ['vet', './...'], pkgDir, { CGO_ENABLED: '0' })
        run('go', ['test', '-v', '-count=1', './...'], pkgDir, { CGO_ENABLED: '0' })
      }
    }
    break
  }

  case 'build': {
    const cliBin = resolveBin('@skinloom/cli', 'packages/cli/bin/skinloom-plugin.mjs')
    for (const pkg of existing) {
      const pkgDir = path.join(ROOT, pkg)
      const manifest = JSON.parse(fs.readFileSync(path.join(pkgDir, 'plugin.json'), 'utf8'))
      const args = [cliBin, 'build', '--dir', pkgDir]
      if (manifest.runtime === 'native') {
        args.push('--native')
      }
      run(process.execPath, args, ROOT, { CGO_ENABLED: '0' })
    }
    break
  }

  case 'pack': {
    const cliBin = resolveBin('@skinloom/cli', 'packages/cli/bin/skinloom-plugin.mjs')
    const outRoot = path.join(ROOT, 'dist')
    fs.mkdirSync(outRoot, { recursive: true })
    for (const pkg of existing) {
      const pkgDir = path.join(ROOT, pkg)
      const manifest = JSON.parse(fs.readFileSync(path.join(pkgDir, 'plugin.json'), 'utf8'))
      const args = [cliBin, 'pack', '--dir', pkgDir]
      if (manifest.runtime === 'native') {
        args.push('--native', '--split-platforms', '--out-dir', path.join(outRoot, pkg))
      } else {
        args.push('--out', path.join(outRoot, `${manifest.id}-${manifest.version}.slp`))
      }
      run(process.execPath, args, ROOT, { CGO_ENABLED: '0' })
    }
    break
  }

  default:
    console.error(`Unknown task "${task}"`)
    process.exit(1)
}
