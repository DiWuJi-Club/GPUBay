// 品牌占位：替换品牌只改这一个文件与各应用 public/brand/ 下的图片。
export const brand = {
  name: 'Spark Cloud',
  shortName: 'Spark',
  logoUrl: 'brand/logo.svg', // 相对应用 base：console 为 /brand/logo.svg，admin 为 /admin/brand/logo.svg
  primaryColor: '#2563eb',
  supportText: '如需帮助请联系客服',
  /** SSH 网关主机密钥指纹（部署时由运维填 `platform admin ssh-host-key` 输出的指纹），帮助页公布供用户首次连接核对 */
  sshHostKeyFingerprint: '',
} as const
