import { Fragment, useState } from 'react'
import type { ReactNode } from 'react'
import {
  type ColumnDef,
  type ExpandedState,
  type Row,
  type RowData,
  type SortingState,
  columnVisibilityFeature,
  createExpandedRowModel,
  createSortedRowModel,
  flexRender,
  rowExpandingFeature,
  rowSortingFeature,
  sortFn_alphanumeric,
  sortFn_basic,
  sortFn_datetime,
  sortFn_text,
  tableFeatures,
  useTable,
} from '@tanstack/react-table'
import { ChevronDown, ChevronUp, ChevronsUpDown } from 'lucide-react'
import { cn } from '@/lib/utils'

// The table features every DataTable uses. TanStack Table v9 bundles nothing
// by default: sorting and row expansion are registered here with their row
// models, and the sort functions v8 bundled for auto-detected column types
// (alphanumeric, text, datetime, basic) keep sorting behaviour unchanged.
// Column visibility provides the visible-column and visible-cell accessors
// the markup reads.
export const dataTableFeatures = tableFeatures({
  columnVisibilityFeature,
  rowSortingFeature,
  rowExpandingFeature,
  sortedRowModel: createSortedRowModel(),
  expandedRowModel: createExpandedRowModel(),
  sortFns: {
    alphanumeric: sortFn_alphanumeric,
    text: sortFn_text,
    datetime: sortFn_datetime,
    basic: sortFn_basic,
  },
})

export type DataTableFeatures = typeof dataTableFeatures

// DataTableColumn is the column definition type pages pass to DataTable.
export type DataTableColumn<T extends RowData> = ColumnDef<DataTableFeatures, T, unknown>

interface Props<T extends RowData> {
  columns: DataTableColumn<T>[]
  data: T[]
  onRowClick?: (row: T) => void
  getRowId?: (row: T) => string
  emptyState?: ReactNode
  className?: string
  initialSorting?: SortingState
  renderExpandedRow?: (row: Row<DataTableFeatures, T>) => ReactNode
  columnWidths?: readonly string[]
}

export function DataTable<T extends RowData>({
  columns,
  data,
  onRowClick,
  getRowId,
  emptyState,
  className,
  initialSorting,
  renderExpandedRow,
  columnWidths,
}: Props<T>) {
  const [sorting, setSorting] = useState<SortingState>(initialSorting ?? [])
  const [expanded, setExpanded] = useState<ExpandedState>({})
  const table = useTable({
    features: dataTableFeatures,
    data,
    columns,
    state: { sorting, expanded },
    onSortingChange: setSorting,
    onExpandedChange: setExpanded,
    getRowId,
    getRowCanExpand: () => Boolean(renderExpandedRow),
  })

  const rows = table.getRowModel().rows

  return (
    <div
      className={cn(
        'overflow-hidden rounded-xl border border-border-subtle bg-surface-raised',
        className,
      )}
    >
      <table className={cn('kyber-data-table w-full text-sm', columnWidths && 'table-fixed')}>
        {columnWidths && (
          <colgroup>
            {table.getVisibleLeafColumns().map((column, index) => (
              <col key={column.id} style={{ width: columnWidths[index] }} />
            ))}
          </colgroup>
        )}
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
            <Fragment key={row.id}>
              <tr
                onClick={onRowClick ? () => onRowClick(row.original) : undefined}
                className={cn(
                  'border-b border-border-subtle/60 transition-colors',
                  !row.getIsExpanded() && 'last:border-0',
                  onRowClick && 'cursor-pointer hover:bg-surface-overlay/30',
                )}
              >
                {row.getVisibleCells().map((cell) => (
                  <td key={cell.id} className="px-4 py-3 align-middle">
                    {flexRender(cell.column.columnDef.cell, cell.getContext())}
                  </td>
                ))}
              </tr>
              {row.getIsExpanded() && renderExpandedRow && (
                <tr className="border-b border-border-subtle/60 last:border-0">
                  <td colSpan={row.getVisibleCells().length} className="bg-surface-overlay/20 px-4 py-3">
                    {renderExpandedRow(row)}
                  </td>
                </tr>
              )}
            </Fragment>
          ))}
        </tbody>
      </table>
    </div>
  )
}
