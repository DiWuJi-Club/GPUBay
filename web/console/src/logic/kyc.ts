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
