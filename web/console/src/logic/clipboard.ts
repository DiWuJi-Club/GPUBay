import { ElMessage } from 'element-plus'

/** 复制到剪贴板并提示；浏览器拒绝（非 HTTPS、无权限）时提示手动复制。 */
export async function copyText(text: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(text)
    ElMessage.success('已复制')
  } catch {
    ElMessage.error('复制失败，请手动选择复制')
  }
}
