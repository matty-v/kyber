import { beforeEach, describe, it, expect, vi } from 'vitest'
import { render, screen, act } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import type { Agent, AgentWorkView } from '../lib/types'
import { ClusterContext } from '../lib/cluster-context'
import { recordActivityReceipt } from '../lib/activity'
import { RecentlyActiveList } from './RecentlyActiveList'
function agent(id: string, work: Partial<AgentWorkView> = {}): Agent {
 const now=Date.now()
 return recordActivityReceipt([{ id, phase:'Running', activity:{work:{state:'working',serverTime:new Date(now).toISOString(),observedAt:new Date(now).toISOString(),expiresAt:new Date(now+60000).toISOString(),foregroundCount:1,backgroundCount:0,...work}} } as Agent])[0]
}
const view=(agents:Agent[],id='one')=><MemoryRouter><ClusterContext.Provider value={{id,name:id,baseURL:'https://example.invalid/',apiKey:'',version:'',capabilities:[]}}><RecentlyActiveList agents={agents}/></ClusterContext.Provider></MemoryRouter>
beforeEach(()=>localStorage.clear())
describe('Agent activity groups',()=>{
 it('shows four independent headers, counts, real work and no stopped agents',()=>{
  render(view([agent('mando'),agent('sol',{state:'background',foregroundCount:0,backgroundCount:2}),{...agent('stopped'),phase:'Stopped'}, {id:'legacy',phase:'Running'} as Agent]))
  expect(screen.getByRole('heading',{name:'Agent activity'})).toBeInTheDocument()
  expect(screen.getAllByRole('button')).toHaveLength(4)
  expect(screen.getByText('2 background jobs running')).toBeVisible()
  expect(screen.queryByText('stopped')).toBeNull()
  expect(screen.getByText('Legacy reporting')).toBeVisible()
 })
 it('remembers collapse across count changes, zero counts and remount',async()=>{
  const user=userEvent.setup();const a=agent('mando');const r=render(view([a]))
  await user.click(screen.getByRole('button',{name:/Working now/}))
  expect(screen.getByText('mando')).not.toBeVisible()
  r.rerender(view([]));expect(screen.getByRole('button',{name:/Working now 0/})).toHaveAttribute('aria-expanded','false')
  r.rerender(view([a]));expect(screen.getByText('mando')).not.toBeVisible()
  r.unmount();render(view([a]));expect(screen.getByRole('button',{name:/Working now/})).toHaveAttribute('aria-expanded','false')
 })
 it('keeps cluster preferences independent and waiting collapsed initially',async()=>{
  const user=userEvent.setup();const r=render(view([agent('a')]))
  expect(screen.getByRole('button',{name:/Waiting/})).toHaveAttribute('aria-expanded','false')
  await user.click(screen.getByRole('button',{name:/Working now/}))
  r.rerender(view([agent('b')],'two'));expect(screen.getByRole('button',{name:/Working now/})).toHaveAttribute('aria-expanded','true')
  r.rerender(view([agent('a')],'one'));expect(screen.getByRole('button',{name:/Working now/})).toHaveAttribute('aria-expanded','false')
 })
 it('supports interaction when browser storage is unavailable',async()=>{
  const get=vi.spyOn(Storage.prototype,'getItem').mockImplementation(()=>{throw Error('denied')})
  const set=vi.spyOn(Storage.prototype,'setItem').mockImplementation(()=>{throw Error('denied')})
  try {const user=userEvent.setup();render(view([agent('mando')]));await user.click(screen.getByRole('button',{name:/Working now/}));expect(screen.getByRole('button',{name:/Working now/})).toHaveAttribute('aria-expanded','false')}finally{get.mockRestore();set.mockRestore()}
 })
 it('ages a cached working lease even when no refetch succeeds',()=>{
  vi.useFakeTimers({toFake:['Date','performance','setInterval','clearInterval']})
  try{render(view([agent('mando')]));expect(screen.getByText('Working')).toBeVisible();act(()=>vi.advanceTimersByTime(65000));expect(screen.queryByText('Working')).toBeNull();expect(screen.getByText('No recent activity signal')).toBeVisible()}finally{vi.useRealTimers()}
 })
 it('moves focus to the destination header without opening a collapsed group',()=>{
  const r=render(view([agent('mando')]))
  screen.getByRole('link',{name:/mando/}).focus()
  r.rerender(view([agent('mando',{state:'waiting',foregroundCount:0})]))
  const waiting=screen.getByRole('button',{name:/Waiting/})
  expect(waiting).toHaveAttribute('aria-expanded','false')
  expect(waiting).toHaveFocus()
 })
 it('shows a verified pending reply alongside other work',()=>{
  render(view([agent('mando',{needsInput:true,backgroundCount:1})]))
  expect(screen.getByText('Needs your reply')).toBeVisible()
  expect(screen.getByText(/Working · \+1 background jobs/)).toBeVisible()
 })

})


it('opens the complete request independently of agent navigation', async () => {
 const user=userEvent.setup();const a=agent('mando');a.goal={summary:'Read every part of this request '+ 'longword'.repeat(60), source:'agent',acceptedAt:new Date().toISOString()}
 render(view([a]))
 const full=screen.getByTestId('full-request')
 expect(full).not.toBeVisible()
 await user.click(screen.getByText('Read full request'))
 expect(full).toBeVisible()
 expect(full).toHaveTextContent(a.goal.summary)
 expect(full.closest('a')).toBeNull()
 expect(screen.getByRole('link',{name:/mando/})).toHaveAttribute('href','/agents/mando')
})

it('recovers focus when a focused request moves into a collapsed group', () => {
 const a=agent('mando');a.goal={summary:'Read the complete request',source:'agent',acceptedAt:new Date().toISOString()}
 const r=render(view([a]));screen.getByText('Read full request').closest('summary')!.focus()
 r.rerender(view([{...agent('mando',{state:'waiting',foregroundCount:0}),goal:a.goal}]))
 expect(screen.getByRole('button',{name:/Waiting/})).toHaveFocus()
 expect(screen.getByRole('button',{name:/Waiting/})).toHaveAttribute('aria-expanded','false')
})
