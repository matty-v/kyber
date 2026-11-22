import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { AgentActivityBadge } from './AgentActivityBadge'
import { recordActivityReceipt } from '../lib/activity'
import type { Agent } from '../lib/types'
describe('AgentActivityBadge', () => {
 it('hides stopped activity',()=>{const {container}=render(<AgentActivityBadge agent={{phase:'Stopped',activity:{state:'working'}} as Agent}/>);expect(container).toBeEmptyDOMElement()})
 it('shows legacy working as unavailable',()=>{render(<AgentActivityBadge agent={{phase:'Running',activity:{state:'working'}} as Agent}/>);expect(screen.getByText('Activity unavailable')).toBeVisible()})
 it('shows a native job count',()=>{const now=Date.now();const a=recordActivityReceipt([{phase:'Running',activity:{work:{state:'background',serverTime:new Date(now).toISOString(),expiresAt:new Date(now+60000).toISOString(),foregroundCount:0,backgroundCount:2}}} as Agent])[0];render(<AgentActivityBadge agent={a}/>);expect(screen.getByText('2 shells running')).toBeVisible()})
})
