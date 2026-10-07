/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { RefreshCw } from 'lucide-react'
import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { channelsQueryKeys } from '@/features/channels/lib'
import { toIntlLocale } from '@/i18n/languages'
import { api } from '@/lib/api'
import { formatNumber } from '@/lib/format'
import { requireServerSuccess } from '@/lib/server-error-message'

import { getSystemTask, listSystemTasks } from '../api'
import type { SystemTask } from '../types'

type MonitorResult = {
  channels: number
  prices: number
  balances: number
  errors: number
  groups: number
  issues?: { channel_id: number; phase: string; reason: string }[]
}
type MonitorTask = SystemTask<
  { force?: boolean },
  Record<string, unknown>,
  MonitorResult
>

export function UpstreamMonitorRunStatus(props: { enabled: boolean }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const client = useQueryClient()
  const [taskId, setTaskId] = useState<string>()
  const taskQuery = useQuery({
    queryKey: ['upstream-monitor', 'collection-task', taskId],
    queryFn: async () => {
      if (taskId) {
        return requireServerSuccess(await getSystemTask<MonitorTask>(taskId))
          .data
      }
      const response = requireServerSuccess(
        await listSystemTasks(1, { type: 'upstream_monitor' })
      )
      return response.data?.[0] as MonitorTask | undefined
    },
    refetchInterval: (query) => {
      const status = query.state.data?.status
      return status === 'pending' || status === 'running' ? 2000 : false
    },
    retry: false,
  })
  const task = taskQuery.data
  const active = task?.status === 'pending' || task?.status === 'running'
  const run = useMutation({
    mutationFn: async () => {
      const response = await api.post<{
        success: boolean
        message?: string
        data: { task_id: string; created: boolean }
      }>('/api/upstream_monitor/run')
      return requireServerSuccess(response.data).data
    },
    onSuccess: (result) => {
      setTaskId(result.task_id)
      toast.success(
        t(
          result.created
            ? 'Collection queued'
            : 'Collection already queued or running'
        )
      )
      void client.invalidateQueries({
        queryKey: ['upstream-monitor', 'collection-task'],
      })
    },
    onError: (error) => toast.error(t(error.message || 'Collection failed')),
  })
  useEffect(() => {
    if (task?.status === 'succeeded' || task?.status === 'failed') {
      void client.invalidateQueries({ queryKey: channelsQueryKeys.all })
    }
  }, [client, task?.task_id, task?.status])

  return (
    <section className='space-y-3 border-t pt-4' aria-live='polite'>
      <div className='flex flex-wrap items-center gap-3'>
        <h3 className='text-base font-semibold'>{t('Collection task')}</h3>
        <Button
          variant='outline'
          onClick={() => run.mutate()}
          disabled={!props.enabled || run.isPending || active}
        >
          <RefreshCw
            className={run.isPending || active ? 'animate-spin' : undefined}
          />
          {t('Collect now')}
        </Button>
        <Button
          variant='ghost'
          size='sm'
          onClick={() => void taskQuery.refetch()}
          disabled={taskQuery.isFetching}
        >
          <RefreshCw />
          {t('Refresh')}
        </Button>
      </div>
      {taskQuery.isError && (
        <p className='text-destructive text-sm'>
          {t('Could not load collection task')}
        </p>
      )}
      {task && (
        <div className='space-y-2 text-sm'>
          <div className='flex flex-wrap items-center gap-2'>
            <Badge
              variant={task.status === 'failed' ? 'destructive' : 'secondary'}
            >
              {t(task.status)}
            </Badge>
            <span className='min-w-0 font-mono text-xs break-all'>
              {task.task_id}
            </span>
          </div>
          {task.result && (
            <dl className='flex flex-wrap gap-x-5 gap-y-2'>
              {(
                [
                  ['Channels monitored', task.result.channels],
                  ['Price samples', task.result.prices],
                  ['Balance samples', task.result.balances],
                  ['Groups repriced', task.result.groups],
                  ['Monitor errors', task.result.errors],
                ] as const
              ).map(([label, count]) => (
                <div key={label}>
                  <dt className='text-muted-foreground'>{t(label)}</dt>
                  <dd className='tabular-nums'>
                    {formatNumber(count, locale)}
                  </dd>
                </div>
              ))}
            </dl>
          )}
          {task.error && (
            <p className='text-destructive break-words'>{t(task.error)}</p>
          )}
          {task.result?.issues?.map((issue) => (
            <p
              key={`${issue.channel_id}-${issue.phase}`}
              className='text-destructive break-words'
            >
              #{issue.channel_id} · {issue.phase} · {t(issue.reason)}
            </p>
          ))}
        </div>
      )}
    </section>
  )
}
