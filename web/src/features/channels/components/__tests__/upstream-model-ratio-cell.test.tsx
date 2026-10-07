/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, expect, it } from 'vitest'

import { channelSchema, type UpstreamMonitorPriceSummary } from '../../types'
import { ChannelsProvider, useChannels } from '../channels-provider'
import { UpstreamModelRatioCell } from '../upstream-model-ratio-cell'

const i18n = createInstance()
await i18n.init({
  lng: 'en',
  resources: { en: { translation: {} } },
  initAsync: false,
})
const channel = channelSchema.parse({
  id: 14,
  type: 1,
  key: '',
  name: 'upstream',
  status: 1,
  created_time: 1,
  test_time: 0,
  response_time: 0,
  balance_updated_time: 0,
})
const clients: QueryClient[] = []
afterEach(() => {
  cleanup()
  clients.splice(0).forEach((client) => client.clear())
})

function DialogState() {
  const { open, currentRow, setSensitiveVisible } = useChannels()
  return (
    <>
      <output>
        {open}:{currentRow?.id}
      </output>
      <button type='button' onClick={() => setSensitiveVisible(false)}>
        Hide prices
      </button>
    </>
  )
}

function renderCell(summary?: UpstreamMonitorPriceSummary, failed = false) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <ChannelsProvider>
          <UpstreamModelRatioCell
            channel={channel}
            summary={summary}
            failed={failed}
          />
          <DialogState />
        </ChannelsProvider>
      </QueryClientProvider>
    </I18nextProvider>
  )
}

function snapshot(ratio: number | null): UpstreamMonitorPriceSummary {
  return {
    channel_id: 14,
    enabled: true,
    first_model: 'gpt-first',
    model_ratio: ratio,
    last_price_at: 100,
    last_error: '',
  }
}

it('displays the first model and small ratios, and opens its existing monitor dialog', async () => {
  renderCell(snapshot(0.00000125))
  expect(screen.getByText('0.00000125×')).toBeInTheDocument()
  expect(screen.getByText('gpt-first')).toBeInTheDocument()
  await userEvent.click(
    screen.getByRole('button', { name: 'Upstream model ratio' })
  )
  expect(screen.getByRole('status')).toHaveTextContent('upstream-monitor:14')
  await userEvent.click(screen.getByRole('button', { name: 'Hide prices' }))
  expect(screen.getByText('******')).toBeInTheDocument()
})

it('preserves a valid zero ratio', () => {
  renderCell(snapshot(0))
  expect(screen.getByText('0×')).toBeInTheDocument()
})

it('shows a dash when the first model has no model ratio', () => {
  renderCell(snapshot(null))
  expect(screen.getByText('-')).toBeInTheDocument()
  expect(screen.getByText('gpt-first')).toBeInTheDocument()
})

it('shows unsampled channels without inventing a multiplier', () => {
  renderCell()
  expect(screen.getByText('-')).toBeInTheDocument()
  expect(screen.getByText('Not sampled')).toBeInTheDocument()
})

it('keeps the last sample visible but marks snapshot refresh failures', async () => {
  renderCell(snapshot(0.125), true)
  expect(screen.getByText('0.125×')).toBeInTheDocument()
  await userEvent.hover(
    screen.getByRole('button', { name: 'Upstream model ratio' })
  )
  expect(
    await screen.findByText('Price snapshot unavailable')
  ).toBeInTheDocument()
})
