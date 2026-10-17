export const ID_NUMBER_RE = /^\d{17}[\dX]$/
export const LICENSE_RE = /^[0-9A-HJ-NPQRTUWXY]{2}\d{6}[0-9A-HJ-NPQRTUWXY]{10}$/

export type PersonalErrors = Partial<Record<'name' | 'idNumber', string>>
export type EnterpriseErrors = Partial<Record<'company' | 'licenseNo' | 'fileKey', string>>

/** 个人实名：姓名必填；身份证 18 位，末位可为 X（小写按大写处理）。 */
export function validatePersonal(v: { name: string; idNumber: string }): PersonalErrors {
  const errors: PersonalErrors = {}
  if (!v.name.trim()) errors.name = '请填写真实姓名'
  if (!ID_NUMBER_RE.test(v.idNumber.trim().toUpperCase())) errors.idNumber = '身份证号格式不正确'
  return errors
}

/** 企业认证：企业名称必填；统一社会信用代码 18 位（小写按大写处理）；必须先上传营业执照。 */
export function validateEnterprise(v: { company: string; licenseNo: string; fileKey: string }): EnterpriseErrors {
  const errors: EnterpriseErrors = {}
  if (!v.company.trim()) errors.company = '请填写企业名称'
  if (!LICENSE_RE.test(v.licenseNo.trim().toUpperCase())) errors.licenseNo = '统一社会信用代码格式不正确'
  if (!v.fileKey) errors.fileKey = '请上传营业执照'
  return errors
}

/** 营业执照上传限制，与后端 storage.MaxUploadSize / 允许类型一致（后端按内容嗅探，仍以后端为准）。 */
export const MAX_LICENSE_BYTES = 10 << 20
const LICENSE_EXTS = ['jpg', 'jpeg', 'png', 'pdf']
const LICENSE_MIMES = ['image/jpeg', 'image/png', 'application/pdf']

/**
 * 选择文件后的本地预检：扩展名必须是 JPG / PNG / PDF；浏览器给出了 MIME 时也必须是这三种（部分系统 MIME 为空，此时只看扩展名）；
 * 不超过 10MB。通过返回空串，否则返回与后端相同的错误文案。
 */
export function checkLicenseFile(f: { name: string; type: string; size: number }): string {
  const ext = f.name.includes('.') ? f.name.split('.').pop()!.toLowerCase() : ''
  if (!LICENSE_EXTS.includes(ext) || (f.type !== '' && !LICENSE_MIMES.includes(f.type.toLowerCase()))) return '仅支持 JPG、PNG、PDF 文件'
  if (f.size > MAX_LICENSE_BYTES) return '文件不能超过 10MB'
  return ''
}
