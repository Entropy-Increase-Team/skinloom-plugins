import { readFileSync } from 'node:fs'
import path from 'node:path'
import { describe, expect, it } from 'vitest'

describe('theme-atelier Level 5 manifest', () => {
  it('declares L1-L5 theme capabilities with valid overrides and extends default', () => {
    const raw = JSON.parse(readFileSync(path.resolve(import.meta.dirname, '../plugin.json'), 'utf8'))
    expect(raw.kind).toBe('theme')
    expect(raw.extends).toBe('default')
    expect(raw.web).toBe('web/index.js')
    expect(raw.theme?.css).toBe('theme.css')
    expect(raw.theme?.overrides).toEqual(['site.footer', 'texture.card'])
    expect(raw.theme?.tokens?.light?.primary).toBeTruthy()
    expect(raw.theme?.tokens?.dark?.primary).toBeTruthy()
    expect(Object.keys(raw.theme?.settings ?? {})).toContain('accentColor')
  })
})
