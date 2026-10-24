export type LedgerKind = 'recharge' | 'order_pay' | 'refund' | 'admin_adjust' | 'llm_pay' | (string & {})

export interface LedgerRow {
  id: number
  amount: number
  balance_after: number
  kind: LedgerKind
  ref_type: string
  ref_id: string
  remark: string
  created_at: string
}
