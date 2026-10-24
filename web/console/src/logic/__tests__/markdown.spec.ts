import { describe, expect, it } from 'vitest'
import { escapeHtml, linkKind, renderMarkdown } from '../markdown'

const opt = { ingressDomain: 'spark.test' }
const md = (s: string) => renderMarkdown(s, opt)

/** 解析成 DOM 后检查：没有任何会执行脚本的元素 / 属性，链接只有白名单内的。 */
function dom(html: string) {
  const div = document.createElement('div')
  div.innerHTML = html
  return div
}
function assertInert(html: string) {
  const d = dom(html)
  expect(d.querySelectorAll('script, img, iframe, svg, object, embed, style, form, input').length).toBe(0)
  for (const el of Array.from(d.querySelectorAll('*'))) {
    for (const a of Array.from(el.attributes)) {
      expect(a.name.startsWith('on')).toBe(false)
      expect(['href', 'rel', 'target', 'start']).toContain(a.name)
    }
  }
  for (const a of Array.from(d.querySelectorAll('a'))) {
    const href = a.getAttribute('href') ?? ''
    expect(linkKind(href, 'spark.test')).not.toBeNull()
  }
}

describe('renderMarkdown 基本语法', () => {
  it('段落与换行', () => {
    expect(md('第一行\n第二行\n\n第二段')).toBe('<p>第一行<br>第二行</p>\n<p>第二段</p>')
  })

  it('粗体与行内代码（代码内不解析粗体）', () => {
    expect(md('这是 **重要** 的 `**x** <b>`')).toBe('<p>这是 <strong>重要</strong> 的 <code>**x** &lt;b&gt;</code></p>')
  })

  it('围栏代码块原样转义，未闭合时吃到末尾', () => {
    expect(md('看这里：\n```bash\necho "<hi>" && ls\n```\n结束')).toBe(
      '<p>看这里：</p>\n<pre><code>echo &quot;&lt;hi&gt;&quot; &amp;&amp; ls</code></pre>\n<p>结束</p>',
    )
    expect(md('```\nnvidia-smi')).toBe('<pre><code>nvidia-smi</code></pre>')
  })

  it('无序 / 有序列表', () => {
    expect(md('- 甲\n- **乙**\n\n1. 一\n2. 二')).toBe('<ul><li>甲</li><li><strong>乙</strong></li></ul>\n<ol><li>一</li><li>二</li></ol>')
    expect(md('3. 三\n4. 四')).toBe('<ol start="3"><li>三</li><li>四</li></ol>')
  })

  it('标题 # ～ ###，#### 不是标题', () => {
    expect(md('# 一\n## 二\n### 三 ###\n#### 四')).toBe('<h1>一</h1>\n<h2>二</h2>\n<h3>三</h3>\n<p>#### 四</p>')
    expect(md('#无空格')).toBe('<p>#无空格</p>')
  })
})

describe('renderMarkdown XSS（Review Focus 5）', () => {
  it('<script> 转义', () => {
    const html = md('<script>alert(1)</script>')
    expect(html).toBe('<p>&lt;script&gt;alert(1)&lt;/script&gt;</p>')
    assertInert(html)
  })

  it('<img onerror> 转义', () => {
    const html = md('<img src=x onerror="alert(1)">')
    expect(html).not.toContain('<img')
    expect(html).toContain('&lt;img src=x onerror=&quot;alert(1)&quot;&gt;')
    assertInert(html)
  })

  it('[x](javascript:…) 为纯文本', () => {
    for (const s of ['[x](javascript:alert(1))', '[x](javascript:alert`1`)', '[x](JaVaScRiPt:alert)', '[x](&#106;avascript:alert)', '[x](%6Aavascript:alert)']) {
      const html = md(s)
      expect(html).not.toContain('<a')
      assertInert(html)
    }
    expect(md('[点我](javascript:void)')).toBe('<p>点我 (javascript:void)</p>')
  })

  it('[x](data:…) 为纯文本', () => {
    const html = md('[x](data:text/html,<script>alert(1)</script>)')
    expect(html).not.toContain('<a')
    expect(html).not.toContain('<script')
    assertInert(html)
    expect(md('[x](data:text/html;base64,PHNjcmlwdD4=)')).toBe('<p>x (data:text/html;base64,PHNjcmlwdD4=)</p>')
  })

  it('站外链接（Markdown 与裸 URL）为纯文本', () => {
    expect(md('[官网](https://evil.example/login)')).toBe('<p>官网 (https://evil.example/login)</p>')
    expect(md('打开 https://evil.example/ 看看')).toBe('<p>打开 https://evil.example/ 看看</p>')
    expect(md('http://ins-abc-8000.spark.test/')).not.toContain('<a') // 只允许 https
  })

  it('协议相对与反斜杠地址为纯文本', () => {
    for (const s of ['[x](//evil.example)', '[x](/\\evil.example)', '[x](/a//evil.example)', '[x](/\\/evil.example)']) {
      const html = md(s)
      expect(html).not.toContain('<a')
      assertInert(html)
    }
  })

  it('仿冒实例域名为纯文本', () => {
    for (const u of [
      'https://ins-abc-8000.spark.test.evil.example/',
      'https://ins-abc-8000.spark.test@evil.example/',
      'https://ins-abc-8000.sparkXtest/',
      'https://evil.example/ins-abc-8000.spark.test/',
      'https://x.ins-abc-8000.spark.test/',
      'https://ins-abc-8000.spark.test:443@evil.example/',
      'https://ins-ABC-8000.spark.test/',
      'https://ins-abc.spark.test/',
    ]) {
      expect(md(`[x](${u})`)).not.toContain('<a')
      expect(md(u)).not.toContain('<a')
    }
  })

  it('链接文本里的 HTML 与引号被转义，不能逃出属性', () => {
    const html = md('[<b onmouseover=alert(1)>"x"</b>](/instances/ins-abc)')
    expect(html).toBe('<p><a href="/instances/ins-abc" rel="noopener noreferrer">&lt;b onmouseover=alert(1)&gt;&quot;x&quot;&lt;/b&gt;</a></p>')
    assertInert(html)
    // 引号 / 尖括号不在 href 允许字符里：整段按纯文本
    const q = md('[x](/a"onclick="alert(1))')
    expect(q).not.toContain('<a')
    assertInert(q)
  })

  it('/api、/admin、/hooks 与 . / .. 路径段不给链接（纵深防御）', () => {
    for (const u of ['/api/v1/butler/conversations/cv-1', '/api', '/admin/', '/ADMIN/users', '/hooks/butler/reply', '/api?x=1',
      '/x/../api/v1/me', '/./admin', '/x/%2e%2e/hooks/a', '/x/%2E/', '/x/..']) {
      expect(linkKind(u, 'spark.test')).toBeNull()
      expect(md(`[x](${u})`)).not.toContain('<a')
    }
    // 仅前缀相同的普通路径不受影响
    expect(linkKind('/apis', 'spark.test')).toBe('site')
    expect(linkKind('/admins/x', 'spark.test')).toBe('site')
    expect(linkKind('/help#api', 'spark.test')).toBe('site')
    expect(linkKind('/apps/vllm.v1', 'spark.test')).toBe('site')
  })

  it('大量未闭合的 [ 不会退化成平方级耗时', () => {
    const t0 = performance.now()
    md('['.repeat(20000) + 'x')
    md('[a](b'.repeat(20000))
    expect(performance.now() - t0).toBeLessThan(500)
  })

  it('ingressDomain 为空时不放行实例链接；站内路径仍可用', () => {
    expect(renderMarkdown('https://ins-abc-8000.spark.test/', { ingressDomain: '' })).not.toContain('<a')
    expect(renderMarkdown('[实例](/instances/ins-abc)', { ingressDomain: '' })).toContain('<a href="/instances/ins-abc"')
  })

  it('域名里的 . 按字面量匹配（正则已转义）', () => {
    expect(linkKind('https://ins-abc-8000.sparkxtest/', 'spark.test')).toBeNull()
    expect(linkKind('https://ins-abc-8000.a-b.example.com/x', 'a-b.example.com')).toBe('instance')
  })
})

describe('renderMarkdown 链接白名单（D9）', () => {
  it('站内路径 → <a>，同窗口打开', () => {
    expect(md('[查看实例](/instances/ins-abc)')).toBe('<p><a href="/instances/ins-abc" rel="noopener noreferrer">查看实例</a></p>')
    expect(md('[充值](/wallet/recharge?amount=100&from=butler)')).toBe(
      '<p><a href="/wallet/recharge?amount=100&amp;from=butler" rel="noopener noreferrer">充值</a></p>',
    )
    expect(md('[帮助](/help#ssh)')).toContain('<a href="/help#ssh"')
  })

  it('实例端口地址 → <a target="_blank">（Markdown 与裸 URL）', () => {
    expect(md('[文档](https://ins-abc-8000.spark.test/docs)')).toBe(
      '<p><a href="https://ins-abc-8000.spark.test/docs" rel="noopener noreferrer" target="_blank">文档</a></p>',
    )
    expect(md('打开 https://ins-abc-8000.spark.test/docs。')).toBe(
      '<p>打开 <a href="https://ins-abc-8000.spark.test/docs" rel="noopener noreferrer" target="_blank">https://ins-abc-8000.spark.test/docs</a>。</p>',
    )
    expect(md('地址：https://ins-abc-8000.spark.test/.')).toBe(
      '<p>地址：<a href="https://ins-abc-8000.spark.test/" rel="noopener noreferrer" target="_blank">https://ins-abc-8000.spark.test/</a>.</p>',
    )
    expect(md('https://ins-abc-8000.spark.test')).toContain('target="_blank"')
  })

  it('列表与标题里的链接也走白名单', () => {
    const html = md('## [实例](/instances/ins-abc)\n- [坏](https://evil.example)\n- https://ins-abc-8000.spark.test/')
    expect(html).toContain('<h2><a href="/instances/ins-abc"')
    expect(html).toContain('<li>坏 (https://evil.example)</li>')
    expect(html).toContain('<li><a href="https://ins-abc-8000.spark.test/"')
    assertInert(html)
  })
})

describe('escapeHtml', () => {
  it('五个字符', () => {
    expect(escapeHtml(`<a href="x" title='y'>&</a>`)).toBe('&lt;a href=&quot;x&quot; title=&#39;y&#39;&gt;&amp;&lt;/a&gt;')
  })
})
