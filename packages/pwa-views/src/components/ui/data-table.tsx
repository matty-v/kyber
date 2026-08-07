import { useState } from 'react'
import type { ReactNode } from 'react'
import {
  type ColumnDef,
  type SortingState,
  flexRender,
  getCoreRowModel,
  getSortedRowModel,
  useReactTable,
} from '@tanstack/react-table'
import { ChevronDown, ChevronUp, ChevronsUpDown } from 'lucide-react'
import { cn } from '@/lib/utils'

interface Props<T> {
  columns: ColumnDef<T, unknown>[]
  data: T[]
  onRowClick?: (row: T) => void
  getRowId?: (row: T) => string
  emptyState?: ReactNode
  className?: string
  initialSorting?: SortingState
}

export function DataTable<T>({
  columns,
  data,
  onRowClick,
  getRowId,
  emptyState,
  className,
  initialSorting,
}: Props<T>) {
  const [sorting, setSorting] = useState<SortingState>(initialSorting ?? [])
  const table = useReactTable({
    data,
    columns,
    state: { sorting },
    onSortingChange: setSorting,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
    getRowId,
  })

  const rows = table.getRowModel().rows

  return (
    <div
      className={cn(
        'overflow-hidden rounded-xl border border-border-subtle bg-surface-raised',
        className,
      )}
    >
      <table className="kyber-data-table w-full text-sm">
        <thead className="border-b border-border-subtle bg-surface-overlay/40">
          {table.getHeaderGroups().map((hg) => (
            <tr key={hg.id}>
              {hg.headers.map((h) => {
                const canSort = h.column.getCanSort()
                const sorted = h.column.getIsSorted()
                return (
                  <th
                    key={h.id}
                    className={cn(
                      'px-4 py-2.5 text-left font-mono text-[10px] uppercase tracking-[0.15em] text-text-muted',
                      canSort && 'cursor-pointer select-none hover:text-text-secondary',
                    )}
                    onClick={h.column.getToggleSortingHandler()}
                  >
                    <span className="inline-flex items-center gap-1.5">
                      {flexRender(h.column.columnDef.header, h.getContext())}
                      {canSort &&
                        (sorted === 'asc' ? (
                          <ChevronUp className="h-3 w-3" />
                        ) : sorted === 'desc' ? (
                          <ChevronDown className="h-3 w-3" />
                        ) : (
                          <ChevronsUpDown className="h-3 w-3 opacity-40" />
                        ))}
                    </span>
                  </th>
                )
              })}
            </tr>
          ))}
        </thead>
        <tbody>
          {rows.length === 0 && emptyState && (
            <tr>
              <td
                colSpan={table.getAllColumns().length}
                className="px-4 py-10 text-center text-sm text-text-muted"
              >
                {emptyState}
              </td>
            </tr>
          )}
          {rows.map((row) => (
            <tr
              key={row.id}
              onClick={onRowClick ? () => onRowClick(row.original) : undefined}
              className={cn(
                'border-b border-border-subtle/60 transition-colors last:border-0',
                onRowClick && 'cursor-pointer hover:bg-surface-overlay/30',
              )}
            >
              {row.getVisibleCells().map((cell) => (
                <td key={cell.id} className="px-4 py-3 align-middle">
                  {flexRender(cell.column.columnDef.cell, cell.getContext())}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
