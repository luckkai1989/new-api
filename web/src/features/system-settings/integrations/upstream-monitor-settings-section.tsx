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

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { api } from '@/lib/api'

import { UpstreamMonitorPriceLogs } from './upstream-monitor-price-logs'

type Policy = {
  enabled: boolean
  auto_price: boolean
  auto_disable_balance: boolean
  price_interval_minutes: number
  balance_interval_minutes: number
  spike_percent: number
  dingtalk_keyword: string
  dingtalk_webhook: string
  has_dingtalk_webhook?: boolean
}

const defaults: Policy = {
  enabled: false,
  auto_price: false,
  auto_disable_balance: false,
  price_interval_minutes: 60,
  balance_interval_minutes: 10,
  spike_percent: 30,
  dingtalk_keyword: '',
  dingtalk_webhook: '',
}

export function UpstreamMonitorSettingsSection() {
  const [policy, setPolicy] = useState<Policy>(defaults)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [running, setRunning] = useState(false)

  useEffect(() => {
    api.get('/api/upstream_monitor/settings')
      .then((response) => { if (response.data?.success) setPolicy({ ...defaults, ...response.data.data }) })
      .catch(() => toast.error('读取上游监控设置失败'))
      .finally(() => setLoading(false))
  }, [])

  const set = <K extends keyof Policy>(key: K, value: Policy[K]) => setPolicy((current) => ({ ...current, [key]: value }))
  const save = async () => {
    setSaving(true)
    try {
      const response = await api.put('/api/upstream_monitor/settings', policy)
      if (!response.data?.success) throw new Error(response.data?.message || '保存失败')
      setPolicy((current) => ({ ...current, has_dingtalk_webhook: Boolean(current.dingtalk_webhook) || current.has_dingtalk_webhook, dingtalk_webhook: '' }))
      toast.success('上游监控设置已保存')
    } catch (error) { toast.error(error instanceof Error ? error.message : '保存失败') }
    finally { setSaving(false) }
  }

  const run = async () => {
    setRunning(true)
    try {
      const response = await api.post('/api/upstream_monitor/run')
      if (!response.data?.success) throw new Error(response.data?.message || '启动失败')
      toast.success('监控任务已加入队列')
    } catch (error) { toast.error(error instanceof Error ? error.message : '启动失败') }
    finally { setRunning(false) }
  }

  if (loading) return <div>加载中...</div>
  return <div className='max-w-5xl space-y-5 py-4'>
    <h2 className='text-lg font-semibold'>上游监控与自动调价</h2>
    <div className='space-y-3 text-sm'>
      <label className='flex items-center gap-2'><input type='checkbox' checked={policy.enabled} onChange={(e) => set('enabled', e.target.checked)} />启用定时监控</label>
      <label className='flex items-center gap-2'><input type='checkbox' checked={policy.auto_price} onChange={(e) => set('auto_price', e.target.checked)} />自动调整分组倍率</label>
      <label className='flex items-center gap-2'><input type='checkbox' checked={policy.auto_disable_balance} onChange={(e) => set('auto_disable_balance', e.target.checked)} />余额耗尽时自动下线单 Key 渠道</label>
    </div>
    <div className='grid gap-4 sm:grid-cols-2'>
      <label className='space-y-1 text-sm'>价格采集间隔（分钟）<Input type='number' min={5} value={policy.price_interval_minutes} onChange={(e) => set('price_interval_minutes', Number(e.target.value))} /></label>
      <label className='space-y-1 text-sm'>余额采集间隔（分钟）<Input type='number' min={1} value={policy.balance_interval_minutes} onChange={(e) => set('balance_interval_minutes', Number(e.target.value))} /></label>
      <label className='space-y-1 text-sm'>异常涨价阈值（%）<Input type='number' min={0} max={1000} step={1} value={policy.spike_percent} onChange={(e) => set('spike_percent', Number(e.target.value))} /></label>
      <label className='space-y-1 text-sm'>钉钉关键字<Input value={policy.dingtalk_keyword} onChange={(e) => set('dingtalk_keyword', e.target.value)} /></label>
      <label className='space-y-1 text-sm sm:col-span-2'>钉钉 Webhook<Input type='password' autoComplete='new-password' value={policy.dingtalk_webhook} onChange={(e) => set('dingtalk_webhook', e.target.value)} placeholder={policy.has_dingtalk_webhook ? '已保存；留空表示不变' : 'https://oapi.dingtalk.com/robot/send?...'} /></label>
    </div>
    <p className='border-l-2 border-amber-500 pl-3 text-sm text-muted-foreground'>超过阈值时暂停自动调价并告警，渠道仍可接单，可能按旧售价产生亏损。</p>
    <div className='flex gap-2'><Button onClick={save} disabled={saving}>{saving ? '保存中' : '保存'}</Button><Button variant='outline' onClick={run} disabled={running || !policy.enabled}>{running ? '启动中' : '立即采集'}</Button></div>
    <UpstreamMonitorPriceLogs />
  </div>
}
