import { describe, expect, it } from 'vitest'
import { IMPORT_HEADER, importBody, MAX_BODY_BYTES, MAX_IMPORT_BYTES, readCSVFile, templateCSV } from '../csv'

describe('csv', () => {
  it('模板带 BOM 与固定表头', () => {
    expect(IMPORT_HEADER).toBe('serial,rack,slot,lan_ip,product_code,pdu_name,pdu_outlet')
    expect(templateCSV()).toBe(`﻿${IMPORT_HEADER}\n`)
    expect(templateCSV().charCodeAt(0)).toBe(0xfeff)
  })
  it('读取文本并限制 1MB', async () => {
    expect(MAX_IMPORT_BYTES).toBe(1024 * 1024)
    const small = new File(['serial,rack\nA,B\n'], 'm.csv', { type: 'text/csv' })
    expect(await readCSVFile(small)).toBe('serial,rack\nA,B\n')
    const big = new File([new Uint8Array(MAX_IMPORT_BYTES + 1)], 'big.csv')
    await expect(readCSVFile(big)).rejects.toThrow('文件不能超过 1MB')
  })
  it('恰好 1MB 可以读取（上限含等号）', async () => {
    const exact = new File([new Uint8Array(MAX_IMPORT_BYTES).fill(0x61)], 'exact.csv')
    expect((await readCSVFile(exact)).length).toBe(MAX_IMPORT_BYTES)
  })
  it('请求体（JSON 包装后）不超过后端 1MB 上限：恰好上限可以，多 1 字节拒绝', () => {
    expect(MAX_BODY_BYTES).toBe(1024 * 1024)
    const wrap = '{"csv":""}'.length
    const exact = 'x'.repeat(MAX_BODY_BYTES - wrap)
    expect(importBody(exact)).toEqual({ csv: exact })
    expect(() => importBody(exact + 'x')).toThrow('文件内容过大：提交时超过 1MB，请分批导入')
  })
  it('换行、引号在 JSON 里转义成 2 字节：文件不到 1MB 也可能超过请求体上限', () => {
    const text = '\n'.repeat(600 * 1024) // 600KB 文件 → 1.2MB 请求体
    expect(text.length).toBeLessThan(MAX_IMPORT_BYTES)
    expect(() => importBody(text)).toThrow('文件内容过大')
  })
  it('中文按 UTF-8 字节计（JSON.stringify 不转义非 ASCII）', () => {
    const wrap = '{"csv":""}'.length
    const n = Math.floor((MAX_BODY_BYTES - wrap) / 3)
    expect(() => importBody('机'.repeat(n))).not.toThrow()
    expect(() => importBody('机'.repeat(n + 1))).toThrow('文件内容过大')
  })
})
