import { describe, expect, it } from 'vitest'
import { validateEnterprise, validatePersonal } from '../kyc'

describe('kyc validation', () => {
  it('个人：姓名必填、身份证 18 位末位可 X', () => {
    expect(validatePersonal({ name: '张三', idNumber: '11010119900101123X' })).toEqual({})
    expect(validatePersonal({ name: '张三', idNumber: '11010119900101123x' })).toEqual({})
    expect(validatePersonal({ name: '', idNumber: '110101199001011234' })).toEqual({ name: '请填写真实姓名' })
    expect(validatePersonal({ name: '张三', idNumber: '11010119900101123' })).toEqual({ idNumber: '身份证号格式不正确' })
  })
  it('企业：名称、信用代码、执照', () => {
    expect(validateEnterprise({ company: '测试科技有限公司', licenseNo: '91110000MA01ABCD2X', fileKey: 'k' })).toEqual({})
    expect(validateEnterprise({ company: '测试科技有限公司', licenseNo: '123', fileKey: 'k' })).toEqual({ licenseNo: '统一社会信用代码格式不正确' })
    expect(validateEnterprise({ company: '', licenseNo: '91110000MA01ABCD2X', fileKey: '' })).toEqual({ company: '请填写企业名称', fileKey: '请上传营业执照' })
    expect(validateEnterprise({ company: 'x', licenseNo: '91110000ma01abcd2x', fileKey: 'k' })).toEqual({})
  })
})
