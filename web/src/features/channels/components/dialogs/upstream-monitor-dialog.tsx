/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { useEffect, useState } from 'react'
import { toast } from 'sonner'

import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { api } from '@/lib/api'

import { useChannels } from '../channels-provider'

type MonitorPrice = {
  model: string
  mode: string
  input: number
  output: number
  cache_read: number
  cache_write: number
  per_request: number
  raw_model_ratio?: number
  raw_group_ratio?: number
  comparable: boolean
  reason?: string
}

type MonitorForm = {
  enabled: boolean
  platform: 'newapi' | 'sub2api'
  base_url: string
  upstream_group: string
  user_id: string
  secret: string
  warning_balance: number
  has_secret?: boolean
  last_price_at?: number
  last_balance_at?: number
  last_balance?: number
  last_error?: string
  last_prices?: string
  balance_state?: string
  channel_state?: string
  probe_state?: string
}

const emptyForm: MonitorForm = {
  enabled: false,
  platform: 'newapi',
  base_url: '',
  upstream_group: 'default',
  user_id: '',
  secret: '',
  warning_balance: 0,
}

export function UpstreamMonitorDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (value: boolean) => void }) {
  const { currentRow } = useChannels()
  const [form, setForm] = useState<MonitorForm>(emptyForm)
  const [loading, setLoading] = useState(false)
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    if (!open || !currentRow) return
    let cancelled = false
    setLoading(true)
    api.get(`/api/upstream_monitor/channels/${currentRow.id}`)
      .then((response) => {
        if (cancelled) return
        const data = response.data?.data as Partial<MonitorForm> | null
        setForm({ ...emptyForm, base_url: currentRow.base_url?.replace(/\/v1\/?$/, '') ?? '', ...(data ?? {}), secret: '' })
      })
      .catch(() => toast.error('读取监控配置失败'))
      .finally(() => { if (!cancelled) setLoading(false) })
    return () => { cancelled = true }
  }, [open, currentRow])

  const set = <K extends keyof MonitorForm>(key: K, value: MonitorForm[K]) => setForm((previous) => ({ ...previous, [key]: value }))

  const save = async () => {
    if (!currentRow) return
    setSaving(true)
    try {
      const response = await api.put(`/api/upstream_monitor/channels/${currentRow.id}`, {
        enabled: form.enabled,
        platform: form.platform,
        base_url: form.base_url,
        upstream_group: form.upstream_group,
        user_id: form.user_id,
        secret: form.secret,
        warning_balance: form.warning_balance,
      })
      if (!response.data?.success) throw new Error(response.data?.message ?? '保存失败')
      toast.success('监控配置已保存')
      onOpenChange(false)
    } catch (error) {
      toast.error(error instanceof Error ? error.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  let prices: MonitorPrice[] = []
  try {
    prices = JSON.parse(form.last_prices || '[]') as MonitorPrice[]
  } catch {
    prices = []
  }

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={`上游监控 · ${currentRow?.name ?? ''}`}
      contentClassName='max-w-3xl'
      footer={<Button onClick={save} disabled={saving || loading}>{saving ? '保存中' : '保存'}</Button>}
    >
      {loading ? <p>加载中...</p> : (
        <div className='max-h-[70vh] space-y-4 overflow-y-auto pr-1'>
          <label className='flex items-center gap-2 text-sm'>
            <input type='checkbox' checked={form.enabled} onChange={(e) => set('enabled', e.target.checked)} />
            启用此渠道的监控
          </label>
          <div className='grid gap-3 sm:grid-cols-2'>
            <label className='space-y-1 text-sm'>上游类型
              <select className='border-input h-8 w-full rounded-md border bg-background px-2' value={form.platform} onChange={(e) => set('platform', e.target.value as MonitorForm['platform'])}>
                <option value='newapi'>NewAPI</option>
                <option value='sub2api'>sub2api</option>
              </select>
            </label>
            <label className='space-y-1 text-sm'>上游分组<Input value={form.upstream_group} onChange={(e) => set('upstream_group', e.target.value)} /></label>
            <label className='space-y-1 text-sm sm:col-span-2'>采集地址（HTTPS 根地址）<Input value={form.base_url} onChange={(e) => set('base_url', e.target.value)} placeholder='https://api.example.com' /></label>
            <label className='space-y-1 text-sm'>{form.platform === 'newapi' ? 'NewAPI 用户 ID' : 'sub2api 登录邮箱'}<Input value={form.user_id} onChange={(e) => set('user_id', e.target.value)} /></label>
            <label className='space-y-1 text-sm'>{form.platform === 'newapi' ? '访问令牌' : '登录密码'}<Input type='password' autoComplete='new-password' value={form.secret} onChange={(e) => set('secret', e.target.value)} placeholder={form.has_secret ? '已保存；留空表示不变' : '请输入凭据'} /></label>
            <label className='space-y-1 text-sm'>低余额提醒（USD）<Input type='number' min={0} step='0.01' value={form.warning_balance} onChange={(e) => set('warning_balance', Number(e.target.value))} /></label>
          </div>
          <div className='border-t pt-3 text-sm'>
            <div className='flex flex-wrap gap-x-5 gap-y-1'>
              <span>余额：{form.last_balance_at ? `${form.last_balance?.toFixed(4)} USD (${form.balance_state ?? '-'}) · ${new Date(form.last_balance_at * 1000).toLocaleString()}` : '尚未采集'}</span>
              <span>渠道：{form.channel_state ?? '-'}</span>
              <span>上游探针：{form.probe_state ?? '-'}</span>
              <span>价格采集：{form.last_price_at ? new Date(form.last_price_at * 1000).toLocaleString() : '尚未采集'}</span>
            </div>
            {form.last_error && <p className='mt-2 text-destructive'>{form.last_error}</p>}
            {prices.length > 0 && (
              <div className='mt-3 max-h-60 overflow-auto border'>
                <table className='w-full min-w-[900px] text-left text-xs'>
                  <thead><tr><th className='p-2'>原始模型</th><th>计费</th><th>模型倍率</th><th>分组倍率</th><th>输入/百万或按次</th><th>输出/百万</th><th>缓存读/写</th><th>状态</th></tr></thead>
                  <tbody>{prices.map((price, index) => (
                    <tr key={`${price.model}-${index}`} className='border-t'>
                      <td className='p-2'>{price.model}</td><td>{price.mode}</td>
                      <td>{price.raw_model_ratio ?? '-'}</td><td>{price.raw_group_ratio ?? '-'}</td>
                      <td>{price.mode === 'fixed' ? price.per_request : price.input}</td><td>{price.output}</td>
                      <td>{price.cache_read} / {price.cache_write}</td>
                      <td>{price.comparable ? '可比较' : price.reason || '需人工处理'}</td>
                    </tr>
                  ))}</tbody>
                </table>
              </div>
            )}
          </div>
        </div>
      )}
    </Dialog>
  )
}
