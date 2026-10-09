import { expect, test } from 'bun:test'
import { pilotCases, setupAgent } from 'ori/eval'

if (process.env.ORI_EVAL_OPT_IN !== '1') throw new Error('Set ORI_EVAL_OPT_IN=1 to run this opt-in synthetic eval')

const models = [
  'cohere/north-mini-code:free',
  'nvidia/nemotron-3.5-lightning:free',
]

const cases = pilotCases([
  {
    name: 'coding',
    prompt: 'Return only a JavaScript function named clamp that limits a number x to the inclusive bounds lo and hi. Do not call tools or edit files.',
    mustMention: 'clamp',
  },
  {
    name: 'support',
    prompt: 'Synthetic policy: purchases within 30 days qualify for a refund; older purchases must be escalated. A customer bought an item 10 days ago and asks for a refund. Reply with exactly one label: APPROVE_REFUND or ESCALATE. Do not call tools.',
    mustMention: 'APPROVE_REFUND',
  },
])

for (const model of models) {
  for (const scenario of cases) {
    test(`${scenario.name} on ${model}`, async () => {
      const run = await setupAgent({ model }).run({
        prompt: scenario.prompt,
        parameters: { reasoning: { effort: 'none' }, max_output_tokens: 256 },
      })
      run.toComplete()
      run.toMention(scenario.mustMention)
      if (scenario.name === 'support') expect(run.text.trim()).toBe('APPROVE_REFUND')
      run.toCostAtMost(0.001)
      run.toFinishWithin(90_000)
    }, { timeout: 90_000 })
  }
}
