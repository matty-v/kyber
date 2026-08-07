import { useEffect, useState } from 'react'

// True while the tab is visible (Page Visibility API). The Dashboard uses this
// to suspend the live terminal attach when backgrounded so we don't hold an
// open pod WebSocket for a tab nobody is looking at.
export function usePageVisible(): boolean {
  const [visible, setVisible] = useState(() =>
    typeof document === 'undefined' ? true : document.visibilityState !== 'hidden',
  )
  useEffect(() => {
    const onChange = () => setVisible(document.visibilityState !== 'hidden')
    document.addEventListener('visibilitychange', onChange)
    return () => document.removeEventListener('visibilitychange', onChange)
  }, [])
  return visible
}
