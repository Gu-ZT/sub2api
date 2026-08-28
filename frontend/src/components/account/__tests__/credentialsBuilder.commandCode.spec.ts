import { describe, it, expect } from 'vitest'
import {
  isCommandCodeBaseUrl,
  isCommandCodeAccount
} from '../credentialsBuilder'

describe('isCommandCodeBaseUrl', () => {
  it('accepts exact host with no path', () => {
    expect(isCommandCodeBaseUrl('https://api.commandcode.ai')).toBe(true)
  })

  it('accepts trailing slash', () => {
    expect(isCommandCodeBaseUrl('https://api.commandcode.ai/')).toBe(true)
  })

  it('accepts provider path', () => {
    expect(isCommandCodeBaseUrl('https://api.commandcode.ai/provider')).toBe(true)
  })

  it('accepts provider/v1 path', () => {
    expect(isCommandCodeBaseUrl('https://api.commandcode.ai/provider/v1')).toBe(true)
  })

  it('accepts deep path under provider/v1', () => {
    expect(isCommandCodeBaseUrl('https://api.commandcode.ai/provider/v1/anything')).toBe(true)
  })

  it('accepts http scheme', () => {
    expect(isCommandCodeBaseUrl('http://api.commandcode.ai/provider/v1')).toBe(true)
  })

  it('accepts uppercase host and path', () => {
    expect(isCommandCodeBaseUrl('HTTPS://API.COMMANDCODE.AI/PROVIDER/V1')).toBe(true)
  })

  it('accepts query string', () => {
    expect(isCommandCodeBaseUrl('https://api.commandcode.ai/provider/v1?x=1')).toBe(true)
  })

  it('rejects empty value', () => {
    expect(isCommandCodeBaseUrl('')).toBe(false)
    expect(isCommandCodeBaseUrl('   ')).toBe(false)
  })

  it('rejects other hosts', () => {
    expect(isCommandCodeBaseUrl('https://example.com/provider/v1')).toBe(false)
  })

  it('rejects subdomain confusion', () => {
    expect(isCommandCodeBaseUrl('https://api.commandcode.ai.evil.com/provider/v1')).toBe(false)
  })

  it('rejects path confusion', () => {
    expect(isCommandCodeBaseUrl('https://evil.com/api.commandcode.ai/provider/v1')).toBe(false)
  })

  it('rejects similar-prefix path', () => {
    expect(isCommandCodeBaseUrl('https://api.commandcode.ai/providers')).toBe(false)
  })

  it('rejects non-URL strings', () => {
    expect(isCommandCodeBaseUrl('not a url')).toBe(false)
    expect(isCommandCodeBaseUrl('api.commandcode.ai/provider/v1')).toBe(false)
  })

  it('rejects non-string values', () => {
    expect(isCommandCodeBaseUrl(null)).toBe(false)
    expect(isCommandCodeBaseUrl(undefined)).toBe(false)
    expect(isCommandCodeBaseUrl(123)).toBe(false)
  })
})

describe('isCommandCodeAccount', () => {
  it('detects account with commandcode base_url', () => {
    const account = { credentials: { base_url: 'https://api.commandcode.ai/provider/v1' } }
    expect(isCommandCodeAccount(account)).toBe(true)
  })

  it('rejects account with other base_url', () => {
    const account = { credentials: { base_url: 'https://api.openai.com/v1' } }
    expect(isCommandCodeAccount(account)).toBe(false)
  })

  it('rejects account without credentials', () => {
    expect(isCommandCodeAccount(null)).toBe(false)
    expect(isCommandCodeAccount(undefined)).toBe(false)
    expect(isCommandCodeAccount({})).toBe(false)
  })
})
