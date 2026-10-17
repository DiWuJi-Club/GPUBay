export const IMPORT_HEADER = 'serial,rack,slot,lan_ip,product_code,pdu_name,pdu_outlet'
export const MAX_IMPORT_BYTES = 1 << 20
/** 后端 httpx.DecodeJSON 的请求体上限（含 JSON 包装与转义）。 */
export const MAX_BODY_BYTES = 1 << 20

/** 导入模板：带 BOM（Excel 打开不乱码）+ 固定表头。 */
export function templateCSV(): string {
  return `﻿${IMPORT_HEADER}\n`
}

/** 前端读取 CSV 文本直接提交（后端接收 {csv}），限制 1MB；超限不读取、不发请求。 */
export async function readCSVFile(file: File): Promise<string> {
  if (file.size > MAX_IMPORT_BYTES) throw new Error('文件不能超过 1MB')
  return file.text()
}

/**
 * 导入请求体 {csv}。文件本身不超过 1MB 还不够：JSON 包装加 10 字节，换行 / 引号 / 反斜杠各转义成 2 字节，
 * 超过后端请求体上限时后端只会回「请求参数格式不正确」。这里按实际要发送的 UTF-8 字节数精确校验，超限给出明确提示。
 */
export function importBody(csv: string): { csv: string } {
  const body = { csv }
  if (new Blob([JSON.stringify(body)]).size > MAX_BODY_BYTES) throw new Error('文件内容过大：提交时超过 1MB，请分批导入')
  return body
}

export function downloadText(filename: string, text: string, type = 'text/csv;charset=utf-8') {
  const url = URL.createObjectURL(new Blob([text], { type }))
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.click()
  // 下载由浏览器异步开始：同步 revoke 在部分浏览器会让下载拿到失效的 URL，推迟到下一个任务
  setTimeout(() => URL.revokeObjectURL(url), 0)
}
