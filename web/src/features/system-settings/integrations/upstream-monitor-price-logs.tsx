/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { useCallback, useEffect, useState } from 'react'
import { toast } from 'sonner'

import { Button } from '@/components/ui/button'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { api } from '@/lib/api'

type PriceLog = {
  id: number
  task_id: string
  group: string
  old_ratio: number
  new_ratio: number
  required_ratio: number
  evidence: string
  created_at: number
}

type PriceLogPage = { items: PriceLog[]; total: number }
const pageSize = 20

export function UpstreamMonitorPriceLogs() {
  const [page, setPage] = useState(1)
  const [data, setData] = useState<PriceLogPage>({ items: [], total: 0 })
  const [loading, setLoading] = useState(false)

  const load = useCallback(async (signal?: AbortSignal) => {
    setLoading(true)
    try {
      const response = await api.get('/api/upstream_monitor/price_logs', {
        params: { p: page, page_size: pageSize },
        signal,
      })
      if (!response.data?.success) {
        throw new Error(response.data?.message || '读取调价日志失败')
      }
      if (!signal?.aborted) setData(response.data.data)
    } catch (error) {
      if (!signal?.aborted) {
        toast.error(error instanceof Error ? error.message : '读取调价日志失败')
      }
    } finally {
      if (!signal?.aborted) setLoading(false)
    }
  }, [page])

  useEffect(() => {
    const controller = new AbortController()
    void load(controller.signal)
    return () => controller.abort()
  }, [load])

  return (
    <section className='space-y-3'>
      <div className='flex items-center justify-between gap-3'>
        <h3 className='text-base font-semibold'>分组自动调价日志</h3>
        <Button variant='outline' size='sm' onClick={() => void load()} disabled={loading}>刷新</Button>
      </div>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>时间</TableHead><TableHead>分组</TableHead>
            <TableHead>调整</TableHead><TableHead>任务</TableHead>
            <TableHead>依据</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {data.items.map((log) => (
            <TableRow key={log.id}>
              <TableCell>{new Date(log.created_at * 1000).toLocaleString()}</TableCell>
              <TableCell>{log.group}</TableCell>
              <TableCell className='tabular-nums'>{log.old_ratio} → {log.new_ratio}</TableCell>
              <TableCell><span className='block max-w-40 truncate' title={log.task_id}>{log.task_id}</span></TableCell>
              <TableCell>
                <details>
                  <summary className='cursor-pointer'>查看渠道价格</summary>
                  <pre className='max-h-60 max-w-[480px] overflow-auto whitespace-pre-wrap break-all text-xs'>{formatEvidence(log.evidence)}</pre>
                </details>
              </TableCell>
            </TableRow>
          ))}
          {!data.items.length && (
            <TableRow><TableCell colSpan={5} className='text-muted-foreground text-center'>{loading ? '加载中...' : '暂无自动调价记录'}</TableCell></TableRow>
          )}
        </TableBody>
      </Table>
      <div className='flex items-center justify-end gap-3 text-sm'>
        <span>共 {data.total} 条</span>
        <Button variant='outline' size='sm' disabled={loading || page <= 1} onClick={() => setPage(page - 1)}>上一页</Button>
        <span>第 {page} 页</span>
        <Button variant='outline' size='sm' disabled={loading || page * pageSize >= data.total} onClick={() => setPage(page + 1)}>下一页</Button>
      </div>
    </section>
  )
}

function formatEvidence(value: string): string {
  try {
    return JSON.stringify(JSON.parse(value), null, 2)
  } catch {
    return value
  }
}
