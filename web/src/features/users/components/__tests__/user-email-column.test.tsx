/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import {
  flexRender,
  getCoreRowModel,
  useReactTable,
} from '@tanstack/react-table'
import { cleanup, render, screen } from '@testing-library/react'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, expect, it } from 'vitest'

import type { User } from '../../types'
import { useUsersColumns } from '../users-columns'

const i18n = createInstance()
await i18n.init({
  lng: 'en',
  resources: { en: { translation: {} } },
  initAsync: false,
})

function EmailTable({ email }: { email?: string }) {
  const columns = useUsersColumns().filter((column) =>
    'accessorKey' in column && column.accessorKey === 'email'
  )
  const table = useReactTable({
    columns,
    data: [{
      id: 1, username: 'test', email, quota: 0, used_quota: 0,
      request_count: 0, group: 'default', status: 1, role: 1,
    } as User],
    getCoreRowModel: getCoreRowModel(),
  })
  return (
    <table>
      <thead>{table.getHeaderGroups().map((group) => (
        <tr key={group.id}>{group.headers.map((header) => (
          <th key={header.id}>{flexRender(header.column.columnDef.header, header.getContext())}</th>
        ))}</tr>
      ))}</thead>
      <tbody>{table.getRowModel().rows.map((row) => (
        <tr key={row.id}>{row.getVisibleCells().map((cell) => (
          <td key={cell.id}>{flexRender(cell.column.columnDef.cell, cell.getContext())}</td>
        ))}</tr>
      ))}</tbody>
    </table>
  )
}

afterEach(cleanup)

it('shows the existing email field in the admin user list', () => {
  render(
    <I18nextProvider i18n={i18n}>
      <EmailTable email='user@example.com' />
    </I18nextProvider>
  )
  expect(screen.getByRole('columnheader', { name: 'Email' })).toBeInTheDocument()
  expect(screen.getByRole('cell')).toHaveTextContent('user@example.com')
})

it('shows a placeholder for accounts without an email', () => {
  render(
    <I18nextProvider i18n={i18n}>
      <EmailTable />
    </I18nextProvider>
  )
  expect(screen.getByRole('cell')).toHaveTextContent('—')
})
