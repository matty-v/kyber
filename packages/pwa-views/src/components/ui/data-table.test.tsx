import { describe, it, expect } from 'vitest'
import { fireEvent, render, screen, within } from '@testing-library/react'
import { DataTable, type DataTableColumn } from './data-table'

type Item = { name: string; count: number }

const items: Item[] = [
  { name: 'agent-10', count: 1 },
  { name: 'agent-2', count: 30 },
  { name: 'agent-1', count: 200 },
]

const columns: DataTableColumn<Item>[] = [
  { accessorKey: 'name', header: 'Name' },
  { accessorKey: 'count', header: 'Count' },
  { id: 'note', header: 'Note', enableSorting: false, cell: () => 'x' },
]

function names() {
  return screen.getAllByRole('row').slice(1).map((r) => within(r).getAllByRole('cell')[0].textContent)
}

describe('DataTable sorting', () => {
  // v8 sampled rows.slice(10) instead of slice(0, 10) to pick a sort function,
  // so tables under ten rows compared names as plain strings (agent-10 before
  // agent-2). v9 samples correctly and sorts names naturally.
  it('applies the initial sort with natural string order', () => {
    render(<DataTable columns={columns} data={items} initialSorting={[{ id: 'name', desc: false }]} />)
    expect(names()).toEqual(['agent-1', 'agent-2', 'agent-10'])
  })

  it('toggles sorting from the header and sorts numbers numerically', () => {
    render(<DataTable columns={columns} data={items} />)
    expect(names()).toEqual(['agent-10', 'agent-2', 'agent-1'])
    fireEvent.click(screen.getByText('Count'))
    const first = names()
    fireEvent.click(screen.getByText('Count'))
    const second = names()
    // Numbers start descending (the v8/v9 default for non-strings), then flip.
    expect(first).toEqual(['agent-1', 'agent-2', 'agent-10'])
    expect(second).toEqual(['agent-10', 'agent-2', 'agent-1'])
  })

  it('leaves a non-sortable column inert', () => {
    render(<DataTable columns={columns} data={items} />)
    fireEvent.click(screen.getByText('Note'))
    expect(names()).toEqual(['agent-10', 'agent-2', 'agent-1'])
  })
})
