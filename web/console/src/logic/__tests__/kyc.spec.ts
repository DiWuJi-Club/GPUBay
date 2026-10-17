import { describe, expect, it } from 'vitest'
import { checkLicenseFile, MAX_LICENSE_BYTES, validateEnterprise, validatePersonal } from '../kyc'

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
  it('信用代码不含 I、O、S、V、Z；首尾空白去掉后再校验', () => {
    for (const ch of ['I', 'O', 'S', 'V', 'Z']) {
      expect(validateEnterprise({ company: 'x', licenseNo: `91110000MA01ABCD2${ch}`, fileKey: 'k' })).toEqual({ licenseNo: '统一社会信用代码格式不正确' })
      expect(validateEnterprise({ company: 'x', licenseNo: `${ch}1110000MA01ABCD2X`, fileKey: 'k' })).toEqual({ licenseNo: '统一社会信用代码格式不正确' })
    }
    expect(validateEnterprise({ company: ' x ', licenseNo: '  91110000MA01ABCD2X\t', fileKey: 'k' })).toEqual({})
    expect(validateEnterprise({ company: '   ', licenseNo: '91110000MA01ABCD2X', fileKey: 'k' })).toEqual({ company: '请填写企业名称' })
    expect(validatePersonal({ name: ' 张三 ', idNumber: ' 11010119900101123X ' })).toEqual({})
    expect(validatePersonal({ name: '  ', idNumber: '11010119900101123X' })).toEqual({ name: '请填写真实姓名' })
  })
  it('执照文件预检：类型按扩展名与 MIME、大小不超过 10MB', () => {
    expect(checkLicenseFile({ name: 'a.PNG', type: 'image/png', size: 1 })).toBe('')
    expect(checkLicenseFile({ name: 'a.jpeg', type: 'image/jpeg', size: MAX_LICENSE_BYTES })).toBe('')
    expect(checkLicenseFile({ name: 'a.pdf', type: '', size: 1 })).toBe('') // MIME 为空时只看扩展名
    expect(checkLicenseFile({ name: 'a.gif', type: 'image/gif', size: 1 })).toBe('仅支持 JPG、PNG、PDF 文件')
    expect(checkLicenseFile({ name: 'a.txt', type: 'text/plain', size: 1 })).toBe('仅支持 JPG、PNG、PDF 文件')
    expect(checkLicenseFile({ name: 'a.png', type: 'text/plain', size: 1 })).toBe('仅支持 JPG、PNG、PDF 文件')
    expect(checkLicenseFile({ name: 'png', type: 'image/png', size: 1 })).toBe('仅支持 JPG、PNG、PDF 文件')
    expect(checkLicenseFile({ name: 'a.png', type: 'image/png', size: MAX_LICENSE_BYTES + 1 })).toBe('文件不能超过 10MB')
  })
})
