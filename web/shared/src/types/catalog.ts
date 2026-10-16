export type Duration = '1d' | '1w' | '1m'
export const DURATIONS: readonly Duration[] = ['1d', '1w', '1m']

export interface Price {
  duration: Duration
  amount: number
}

export interface ProductSpec {
  gpu: string
  memory_gb: number
  disk_tb: number
  cpu: string
}

export interface Product {
  id: number
  code: string
  name: string
  spec: ProductSpec
  prices: Price[]
  idle_count: number
}

export interface Image {
  id: number
  name: string
  description: string
}
