// AI 管家回答的极简 Markdown 渲染（规格 §11.4、D9）。输出经 v-html 插入，所以：
// 1. 所有来自模型的文本都先 HTML 转义，标签只由本文件生成（不透传任何 HTML）；
// 2. 链接只允许站内路径与实例端口域名（白名单），其它一律输出为纯文本「文本 (url)」；
// 3. 属性值一律转义，且 href 只接受严格字符集（无空白、控制字符、反斜杠、引号、尖括号）。

export interface MarkdownOptions {
  /** 实例端口域名的父域（GET /butler/status 的 ingress_domain）；为空时不放行任何实例链接 */
  ingressDomain: string
}

const ESC: Record<string, string> = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }

export function escapeHtml(s: string): string {
  return s.replace(/[&<>"']/g, (c) => ESC[c])
}

function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\/-]/g, '\\$&')
}

/** href 允许的字符（RFC 3986 的非保留 + 保留字符，去掉 ' ( ) [ ] 以免与 Markdown 语法混淆）。 */
const URL_CHARS = "A-Za-z0-9\\-._~:/?#@!$&*+,;=%"
const SAFE_URL = new RegExp(`^[${URL_CHARS}]+$`)

type LinkKind = 'site' | 'instance'

/**
 * 链接白名单（D9）：
 * - 站内路径：以 / 开头且不含 //（防 //evil.example 这类协议相对地址）；
 * - 实例端口地址：https://ins-<id>-<port>.<ingressDomain> 后接结尾或 / ? #（防 .<domain>.evil.example、@evil.example）。
 */
export function linkKind(url: string, ingressDomain: string): LinkKind | null {
  if (!SAFE_URL.test(url)) return null
  if (url.startsWith('/')) return url.includes('//') ? null : 'site'
  const domain = ingressDomain.trim().toLowerCase()
  if (!domain || !/^[a-z0-9.-]+$/.test(domain)) return null
  const re = new RegExp(`^https://ins-[a-z0-9]+-[0-9]+\\.${escapeRegExp(domain)}(?:[/?#][${URL_CHARS}]*)?$`)
  return re.test(url) ? 'instance' : null
}

function anchor(url: string, kind: LinkKind, innerHtml: string): string {
  const target = kind === 'instance' ? ' target="_blank"' : ''
  return `<a href="${escapeHtml(url)}" rel="noopener noreferrer"${target}>${innerHtml}</a>`
}

/** 纯文本片段：转义后处理 **粗体**（转义不会产生 *，替换结果只含已转义文本）。 */
function plain(s: string): string {
  return escapeHtml(s).replace(/\*\*(?=\S)([^*]*?\S)\*\*/g, '<strong>$1</strong>')
}

/** 末尾的标点通常属于句子而不是 URL。 */
function splitTrailing(url: string): [string, string] {
  const m = /[.,;:!?]+$/.exec(url)
  return m ? [url.slice(0, m.index), m[0]] : [url, '']
}

// 行内记号：`代码`、[文本](url)、裸 http(s) URL
const INLINE = new RegExp(`\`([^\`\\n]+)\`|\\[([^\\]\\n]*)\\]\\(([^()\\s]*)\\)|(https?://[${URL_CHARS}]+)`, 'g')

function inline(src: string, opt: MarkdownOptions): string {
  let out = ''
  let last = 0
  for (const m of src.matchAll(INLINE)) {
    out += plain(src.slice(last, m.index))
    last = m.index + m[0].length
    const [, code, text, href, bare] = m
    if (code !== undefined) {
      out += `<code>${escapeHtml(code)}</code>`
    } else if (href !== undefined) {
      const kind = linkKind(href, opt.ingressDomain)
      if (kind) out += anchor(href, kind, plain(text || href))
      else out += text ? `${plain(text)} (${escapeHtml(href)})` : escapeHtml(href)
    } else {
      const [url, tail] = splitTrailing(bare)
      const kind = linkKind(url, opt.ingressDomain)
      out += (kind ? anchor(url, kind, escapeHtml(url)) : escapeHtml(url)) + escapeHtml(tail)
    }
  }
  return out + plain(src.slice(last))
}

const FENCE = /^ {0,3}```/
const HEADING = /^ {0,3}(#{1,3})\s+(.*?)(?:\s+#+)?\s*$/
const UL = /^ {0,3}[-*+]\s+(.*)$/
const OL = /^ {0,3}(\d{1,9})[.)]\s+(.*)$/

/** 渲染为 HTML 字符串（段落、粗体、行内代码、围栏代码块、无序 / 有序列表、# ～ ### 标题、白名单链接）。 */
export function renderMarkdown(src: string, opt: MarkdownOptions): string {
  const lines = src.replace(/\r\n?/g, '\n').split('\n')
  const out: string[] = []
  let para: string[] = []
  const flush = () => {
    if (para.length) out.push(`<p>${para.map((l) => inline(l, opt)).join('<br>')}</p>`)
    para = []
  }
  let i = 0
  while (i < lines.length) {
    const line = lines[i]
    if (FENCE.test(line)) {
      flush()
      const code: string[] = []
      i++
      while (i < lines.length && !FENCE.test(lines[i])) code.push(lines[i++])
      i++ // 跳过收尾的 ```；流式输出中未闭合时吃到末尾
      out.push(`<pre><code>${escapeHtml(code.join('\n'))}</code></pre>`)
      continue
    }
    const h = HEADING.exec(line)
    if (h) {
      flush()
      out.push(`<h${h[1].length}>${inline(h[2], opt)}</h${h[1].length}>`)
      i++
      continue
    }
    if (UL.test(line) || OL.test(line)) {
      flush()
      const ordered = !UL.test(line)
      const re = ordered ? OL : UL
      const items: string[] = []
      let start = 1
      while (i < lines.length) {
        const m = re.exec(lines[i])
        if (!m) break
        if (ordered && !items.length) start = Number(m[1])
        items.push(`<li>${inline(ordered ? m[2] : m[1], opt)}</li>`)
        i++
      }
      if (ordered) out.push(`<ol${start !== 1 ? ` start="${start}"` : ''}>${items.join('')}</ol>`)
      else out.push(`<ul>${items.join('')}</ul>`)
      continue
    }
    if (line.trim() === '') flush()
    else para.push(line.trim())
    i++
  }
  flush()
  return out.join('\n')
}
