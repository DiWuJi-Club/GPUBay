import { describe, expect, it } from 'vitest'
import { isAuthorizeRedirect, looksLikePublicKey, postLoginTarget, safeRedirect, validatePort } from '../access'

describe('validatePort', () => {
  it('1～65535 且不为 22 的整数', () => {
    expect(validatePort('8888')).toEqual({ port: 8888 })
    expect(validatePort(' 80 ')).toEqual({ port: 80 })
    expect(validatePort('65535')).toEqual({ port: 65535 })
    expect(validatePort('1')).toEqual({ port: 1 })
    for (const bad of ['', '0', '22', '65536', '-1', '8.8', 'abc', '８８８８', '1e3', '0x50']) expect(validatePort(bad)).toHaveProperty('error')
    expect(validatePort('22')).toEqual({ error: '22 端口由 SSH 使用，不能作为 Web 端口' })
  })
})

describe('looksLikePublicKey', () => {
  it('前端粗校验：类型 + base64 两段，允许 CRLF 与注释', () => {
    expect(looksLikePublicKey('ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAbc me@mac\r\n')).toBe(true)
    expect(looksLikePublicKey('ssh-rsa AAAAB3NzaC1yc2E')).toBe(true)
    expect(looksLikePublicKey('ecdsa-sha2-nistp256 AAAAE2Vj')).toBe(true)
    expect(looksLikePublicKey('')).toBe(false)
    expect(looksLikePublicKey('ssh-ed25519')).toBe(false)
    expect(looksLikePublicKey('-----BEGIN OPENSSH PRIVATE KEY-----')).toBe(false)
    expect(looksLikePublicKey('ssh-dss AAAAB3')).toBe(false)
    expect(looksLikePublicKey('ssh-ed25519 AAAA\nssh-ed25519 BBBB')).toBe(false)
  })
})

describe('isAuthorizeRedirect', () => {
  it('只认 authorize 端点', () => {
    expect(isAuthorizeRedirect('/api/v1/ingress/authorize?target=x')).toBe(true)
    expect(isAuthorizeRedirect('/api/v1/ingress/authorize')).toBe(true)
    expect(isAuthorizeRedirect('/instances')).toBe(false)
    expect(isAuthorizeRedirect(null)).toBe(false)
    expect(isAuthorizeRedirect('/api/v1/ingress/authorizeX')).toBe(false)
    expect(isAuthorizeRedirect('/api/v1/ingress/authorize/../../x')).toBe(false)
  })
})

describe('safeRedirect', () => {
  it('只接受站内相对路径', () => {
    expect(safeRedirect('/instances')).toBe('/instances')
    expect(safeRedirect('//evil')).toBeNull()
    expect(safeRedirect('https://evil')).toBeNull()
    expect(safeRedirect('/\\evil')).toBeNull()
    expect(safeRedirect('javascript:alert(1)')).toBeNull()
    expect(safeRedirect('/\t/evil.com')).toBeNull() // 浏览器解析 URL 时剔除制表 / 换行，/\t/evil 会变成 //evil
    expect(safeRedirect('/\n/evil.com')).toBeNull()
    expect(safeRedirect(['/a'])).toBeNull()
    expect(safeRedirect(undefined)).toBeNull()
  })
})

describe('postLoginTarget（登录后去哪：authorize 整页跳转，其余站内路由）', () => {
  const encoded = '/api/v1/ingress/authorize?target=http%3A%2F%2Fins-a1b2c3d4-8888.spark.test%3A18080%2F'

  it('authorize 地址（含编码的 target）原样整页跳转', () => {
    expect(postLoginTarget(encoded)).toEqual({ kind: 'assign', url: encoded })
    expect(postLoginTarget('/api/v1/ingress/authorize')).toEqual({ kind: 'assign', url: '/api/v1/ingress/authorize' })
  })

  it('普通站内路径走路由', () => {
    expect(postLoginTarget('/instances/ins-a?x=1')).toEqual({ kind: 'route', path: '/instances/ins-a?x=1' })
    expect(postLoginTarget(undefined)).toEqual({ kind: 'route', path: '/market' })
  })

  it('开放重定向一律回落到 /market，绝不整页跳转', () => {
    for (const bad of [
      '//evil',
      '//evil.example/api/v1/ingress/authorize?target=x',
      'https://evil',
      'https://evil/api/v1/ingress/authorize?target=x',
      '/\\evil',
      '/\\evil/api/v1/ingress/authorize?target=x',
      'javascript:alert(1)',
      'javascript:/api/v1/ingress/authorize?target=x',
      '/\t/evil.com',
      ['/api/v1/ingress/authorize?target=x'],
    ]) {
      expect(postLoginTarget(bad)).toEqual({ kind: 'route', path: '/market' })
    }
  })

  it('形似 authorize 但不是该端点的路径不整页跳转', () => {
    expect(postLoginTarget('/api/v1/ingress/authorizeX?target=x')).toEqual({ kind: 'route', path: '/api/v1/ingress/authorizeX?target=x' })
    expect(postLoginTarget('/api/v1/ingress/authorize/../../logout')).toEqual({ kind: 'route', path: '/api/v1/ingress/authorize/../../logout' })
  })
})
