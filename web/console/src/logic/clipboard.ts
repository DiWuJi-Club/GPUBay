import { ElMessage } from 'element-plus'

/**
 * 写剪贴板。navigator.clipboard 只在安全上下文（https / localhost）里有，局域网 http 访问时不存在；
 * 没有或被拒绝时退回隐藏 textarea + execCommand('copy')。textarea 挂在当前焦点所在的弹窗里（没有则 body），
 * 免得弹窗的焦点锁把焦点抢回去导致选不中。成功返回 true。
 */
export async function writeClipboard(text: string, doc: Document = document): Promise<boolean> {
  const nav = doc.defaultView?.navigator
  if (nav?.clipboard) {
    try {
      await nav.clipboard.writeText(text)
      return true
    } catch {
      // 权限被拒等：继续走 execCommand
    }
  }
  const host = (doc.activeElement?.closest('[role="dialog"]') as HTMLElement | null) ?? doc.body
  const ta = doc.createElement('textarea')
  ta.value = text
  ta.setAttribute('readonly', '')
  ta.style.position = 'fixed'
  ta.style.top = '0'
  ta.style.left = '0'
  ta.style.opacity = '0'
  host.appendChild(ta)
  try {
    ta.focus()
    ta.select()
    ta.setSelectionRange(0, text.length)
    return doc.execCommand('copy')
  } catch {
    return false
  } finally {
    ta.remove()
  }
}

/** 复制到剪贴板并提示；两种方式都失败时提示手动复制。 */
export async function copyText(text: string): Promise<void> {
  if (await writeClipboard(text)) ElMessage.success('已复制')
  else ElMessage.error('复制失败，请手动选择复制')
}
