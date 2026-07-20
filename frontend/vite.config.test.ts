import { describe, expect, it } from 'vitest'

import { canonicalDevClientIP, overwriteDevClientIPHeaders } from './vite.config'

class HeaderRecorder {
  private values = new Map<string, string[]>()

  constructor(entries: Array<[string, string]> = []) {
    for (const [name, value] of entries) {
      const key = name.toLowerCase()
      this.values.set(key, [...(this.values.get(key) || []), value])
    }
  }

  removeHeader(name: string) {
    this.values.delete(name.toLowerCase())
  }

  setHeader(name: string, value: string) {
    this.values.set(name.toLowerCase(), [value])
  }

  getAll(name: string) {
    return this.values.get(name.toLowerCase()) || []
  }
}

describe('development proxy client IP', () => {
  it.each([
    ['192.0.2.7', '192.0.2.7'],
    ['2001:0db8:0:0:0:0:0:7', '2001:db8::7'],
    ['fe80::1%eth0', 'fe80::1'],
    ['::ffff:192.0.2.9', '192.0.2.9'],
    ['::ffff:c000:020a', '192.0.2.10'],
    ['', null],
    ['not-an-ip', null],
    ['192.0.2.1, 198.51.100.2', null],
    ['fe80::1%', null],
  ])('canonicalizes %s', (input, expected) => {
    expect(canonicalDevClientIP(input)).toBe(expected)
  })

  it('replaces mixed-case and repeated forged values with one canonical value', () => {
    const headers = new HeaderRecorder([
      ['x-forwarded-for', 'attacker-one'],
      ['X-Forwarded-For', 'attacker-two'],
      ['x-real-ip', 'attacker-three'],
      ['X-FileTrans-Dev-Client-IP', 'attacker-four'],
    ])
    overwriteDevClientIPHeaders(headers, '::ffff:198.51.100.12')
    expect(headers.getAll('X-Forwarded-For')).toEqual(['198.51.100.12'])
    expect(headers.getAll('X-Real-IP')).toEqual(['198.51.100.12'])
    expect(headers.getAll('X-FileTrans-Dev-Client-IP')).toEqual(['198.51.100.12'])
  })

  it('removes every forged value when the socket address is invalid', () => {
    const headers = new HeaderRecorder([
      ['X-Forwarded-For', 'attacker-one'],
      ['x-forwarded-for', 'attacker-two'],
      ['X-Real-IP', 'attacker-three'],
      ['x-filetrans-dev-client-ip', 'attacker-four'],
    ])
    overwriteDevClientIPHeaders(headers, 'invalid')
    expect(headers.getAll('X-Forwarded-For')).toEqual([])
    expect(headers.getAll('X-Real-IP')).toEqual([])
    expect(headers.getAll('X-FileTrans-Dev-Client-IP')).toEqual([])
  })
})
