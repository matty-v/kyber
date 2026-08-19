export const terminalExtraKeys = [
  { label: 'Esc', data: '\x1b', title: 'Escape' },
  { label: 'Tab', data: '\t', title: 'Tab' },
  { label: '⇧Tab', data: '\x1b[Z', title: 'Shift+Tab' },
  { label: 'Ctrl-C', data: '\x03', title: 'Interrupt' },
  { label: 'Ctrl-B', data: '\x02', title: 'tmux prefix' },
  { label: '←', data: '\x1b[D', title: 'Left arrow' },
  { label: '↑', data: '\x1b[A', title: 'Up arrow' },
  { label: '↓', data: '\x1b[B', title: 'Down arrow' },
  { label: '→', data: '\x1b[C', title: 'Right arrow' },
  { label: 'Enter', data: '\r', title: 'Enter' },
] as const
