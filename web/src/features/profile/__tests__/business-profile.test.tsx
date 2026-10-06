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
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import type { TFunction } from 'i18next'
import { Toaster } from 'sonner'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import { BusinessProfileCard } from '../components/business-profile-card'
import { getBusinessProfileSchema } from '../lib/business-profile'

const clients: QueryClient[] = []
afterEach(() => {
  vi.restoreAllMocks()
  clients.splice(0).forEach((client) => client.clear())
})

function renderProfile(onUpdate = vi.fn()) {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false } },
  })
  clients.push(client)
  render(
    <QueryClientProvider client={client}>
      <BusinessProfileCard
        businessSystemId='old-business'
        onUpdate={onUpdate}
      />
      <Toaster />
    </QueryClientProvider>
  )
  return onUpdate
}

describe('business account profile', () => {
  it('saves a trimmed account identifier and refreshes only after server success', async () => {
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({
        data: { success: true, data: { business_system_id: 'new-business' } },
      })
    const onUpdate = renderProfile()
    fireEvent.input(screen.getByLabelText('Business system ID'), {
      target: { value: '  new-business  ' },
    })
    fireEvent.click(screen.getByRole('button', { name: 'Save changes' }))
    await waitFor(() => expect(onUpdate).toHaveBeenCalledTimes(1))
    expect(put).toHaveBeenCalledWith('/api/business/profile', {
      business_system_id: 'new-business',
    })
    expect(screen.getByLabelText('Business system ID')).toHaveValue(
      'new-business'
    )
    expect(screen.getByRole('button', { name: 'Save changes' })).toBeDisabled()
  })

  it('preserves unsaved edits and does not report success when the server refuses an update', async () => {
    vi.spyOn(api, 'put').mockResolvedValue({
      data: { success: false, message: 'business profile rejected' },
    })
    const onUpdate = renderProfile()
    fireEvent.input(screen.getByLabelText('Business system ID'), {
      target: { value: 'new-business' },
    })
    fireEvent.click(screen.getByRole('button', { name: 'Save changes' }))
    await screen.findByText('business profile rejected')
    expect(onUpdate).not.toHaveBeenCalled()
    expect(screen.getByLabelText('Business system ID')).toHaveValue(
      'new-business'
    )
    expect(screen.getByRole('button', { name: 'Save changes' })).toBeEnabled()
  })

  it('allows an empty identifier and counts Unicode characters consistently with the server', () => {
    const t = ((key: string) => key) as TFunction
    const schema = getBusinessProfileSchema(t)
    expect(schema.parse({ business_system_id: '   ' })).toEqual({
      business_system_id: '',
    })
    expect(
      schema.safeParse({ business_system_id: '😀'.repeat(128) }).success
    ).toBe(true)
    expect(
      schema.safeParse({ business_system_id: '😀'.repeat(129) }).success
    ).toBe(false)
  })
})
