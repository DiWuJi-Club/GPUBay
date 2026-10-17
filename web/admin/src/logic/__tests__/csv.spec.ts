import { describe, expect, it } from 'vitest'
import { IMPORT_HEADER, MAX_IMPORT_BYTES, readCSVFile, templateCSV } from '../csv'

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
})
