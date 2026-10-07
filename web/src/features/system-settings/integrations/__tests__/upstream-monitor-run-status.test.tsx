/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { toast } from 'sonner'
import { afterEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import type { SystemTask } from '../../types'
import { UpstreamMonitorRunStatus } from '../upstream-monitor-run-status'

const i18n = createInstance()
await i18n.init({
  lng: 'en',
  resources: { en: { translation: {} } },
  initAsync: false,
})
const clients: QueryClient[] = []
afterEach(() => {
  cleanup()
  clients.splice(0).forEach((client) => client.clear())
  vi.restoreAllMocks()
})

function task(status: SystemTask['status']): SystemTask {
  return {
    id: 1,
    task_id: 'systask_monitor_test',
    type: 'upstream_monitor',
    status,
    created_at: 100,
    updated_at: 100,
  }
}

function renderStatus(enabled = true) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <UpstreamMonitorRunStatus enabled={enabled} />
      </QueryClientProvider>
    </I18nextProvider>
  )
  return client
}

it('tracks the queued task and shows failed model diagnostics instead of reporting collection success', async () => {
  let current = task('pending')
  vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: { success: true, data: String(url).endsWith('/list') ? [] : current },
  }))
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({
      data: {
        success: true,
        data: { task_id: current.task_id, created: true },
      },
    })
  renderStatus()
  await userEvent.click(screen.getByRole('button', { name: 'Collect now' }))
  expect(post).toHaveBeenCalledWith('/api/upstream_monitor/run')
  expect(await screen.findByText('pending')).toBeInTheDocument()
  expect(screen.getByRole('button', { name: 'Collect now' })).toBeDisabled()
  current = {
    ...task('failed'),
    error: 'upstream monitor had 1 errors; see task result issues',
    result: {
      channels: 1,
      prices: 0,
      balances: 1,
      errors: 1,
      groups: 0,
      issues: [
        {
          channel_id: 14,
          phase: 'prices',
          reason: 'gpt-y: model is missing from the configured upstream group',
        },
      ],
    },
  }
  await userEvent.click(screen.getByRole('button', { name: 'Refresh' }))
  expect(await screen.findByText('failed')).toBeInTheDocument()
  expect(
    screen.getByText(/#14 · prices · gpt-y: model is missing/)
  ).toBeInTheDocument()
  expect(screen.getByText('Price samples').parentElement).toHaveTextContent('0')
  expect(screen.getByText('Balance samples').parentElement).toHaveTextContent(
    '1'
  )
})

it('loads the latest result after a page reload even when monitoring is disabled', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: [
        {
          ...task('succeeded'),
          result: { channels: 2, prices: 2, balances: 2, groups: 0, errors: 0 },
        },
      ],
    },
  })
  renderStatus(false)
  expect(await screen.findByText('succeeded')).toBeInTheDocument()
  expect(screen.getByText('Price samples').parentElement).toHaveTextContent('2')
  expect(screen.getByRole('button', { name: 'Collect now' })).toBeDisabled()
})

it('reports a reused active task honestly and refreshes channel snapshots after completion', async () => {
  let current = task('running')
  vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: { success: true, data: String(url).endsWith('/list') ? [] : current },
  }))
  vi.spyOn(api, 'post').mockResolvedValue({
    data: { success: true, data: { task_id: current.task_id, created: false } },
  })
  const success = vi.spyOn(toast, 'success')
  const client = renderStatus()
  const invalidate = vi.spyOn(client, 'invalidateQueries')
  await userEvent.click(screen.getByRole('button', { name: 'Collect now' }))
  expect(await screen.findByText('running')).toBeInTheDocument()
  expect(success).toHaveBeenCalledWith('Collection already queued or running')
  current = {
    ...task('succeeded'),
    result: { channels: 1, prices: 1, balances: 1, groups: 0, errors: 0 },
  }
  await userEvent.click(screen.getByRole('button', { name: 'Refresh' }))
  expect(await screen.findByText('succeeded')).toBeInTheDocument()
  await waitFor(() =>
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['channels'] })
  )
})

it('surfaces preflight errors without pretending a task was queued', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({ data: { success: true, data: [] } })
  vi.spyOn(api, 'post').mockResolvedValue({
    data: {
      success: false,
      message: 'no channels have upstream monitoring enabled',
    },
  })
  const error = vi.spyOn(toast, 'error')
  renderStatus()
  await userEvent.click(screen.getByRole('button', { name: 'Collect now' }))
  await waitFor(() =>
    expect(error).toHaveBeenCalledWith(
      'no channels have upstream monitoring enabled'
    )
  )
  expect(screen.queryByText('pending')).not.toBeInTheDocument()
})
