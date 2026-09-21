import { describe, it, expect } from 'vitest'
import {
  isBasicsValid,
  isResourcesValid,
  isIdentityValid,
  isAuthValid,
  isReviewValid,
  earliestInvalidStep,
  WIZARD_STEPS,
} from './validation'
import { initialWizardState } from './types'
import type { WizardState } from './types'

const base = initialWizardState([])

describe('isBasicsValid', () => {
  it('not ok with reason when name is empty', () => {
    expect(isBasicsValid({ ...base, name: '', machine: 'razer' })).toEqual({
      ok: false,
      reason: 'Pick a name for the agent.',
    })
  })
  it('not ok with reason when machine is empty', () => {
    expect(isBasicsValid({ ...base, name: 'alice', machine: '' })).toEqual({
      ok: false,
      reason: 'Pick a machine.',
    })
  })
  it('ok when both name and machine are set', () => {
    expect(isBasicsValid({ ...base, name: 'alice', machine: 'razer' })).toEqual({ ok: true })
  })
})

describe('isResourcesValid', () => {
  it('ok with the default cpu/memory/disk values', () => {
    expect(isResourcesValid(base)).toEqual({ ok: true })
  })
  it('not ok with reason when cpu is empty', () => {
    expect(isResourcesValid({ ...base, cpu: '' })).toEqual({
      ok: false,
      reason: 'Pick a CPU value.',
    })
  })
  it('not ok with reason when memory is empty', () => {
    expect(isResourcesValid({ ...base, memory: '' })).toEqual({
      ok: false,
      reason: 'Pick a memory value.',
    })
  })
  it('not ok with reason when disk is empty', () => {
    expect(isResourcesValid({ ...base, disk: '' })).toEqual({
      ok: false,
      reason: 'Pick a disk value.',
    })
  })
})

describe('isIdentityValid', () => {
  it('ok for template mode regardless of slug', () => {
    expect(isIdentityValid({ ...base, identityRepoMode: 'template' })).toEqual({ ok: true })
  })
  it('ok for none mode', () => {
    expect(isIdentityValid({ ...base, identityRepoMode: 'none' })).toEqual({ ok: true })
  })
  it('not ok with reason when template mode flagged a name collision', () => {
    expect(
      isIdentityValid({
        ...base,
        identityRepoMode: 'template',
        identityRepoCollision: true,
      }),
    ).toEqual({
      ok: false,
      reason: 'A repo with that name already exists — change the agent name.',
    })
  })
  it('not ok with reason for existing mode with empty slug', () => {
    expect(
      isIdentityValid({ ...base, identityRepoMode: 'existing', identityRepoExisting: '' }),
    ).toEqual({
      ok: false,
      reason: 'Enter an existing repo as owner/repo (e.g. matty-v/dave-agent).',
    })
  })
  it('not ok with reason for existing mode with malformed slug', () => {
    expect(
      isIdentityValid({ ...base, identityRepoMode: 'existing', identityRepoExisting: '-foo/bar' }),
    ).toEqual({
      ok: false,
      reason: 'Enter an existing repo as owner/repo (e.g. matty-v/dave-agent).',
    })
  })
  it('ok for existing mode with valid slug', () => {
    expect(
      isIdentityValid({
        ...base,
        identityRepoMode: 'existing',
        identityRepoExisting: 'matty-v/agent',
      }),
    ).toEqual({ ok: true })
  })
})

describe('isAuthValid', () => {
  it('not ok with reason for oauth without pkceVerifier', () => {
    expect(
      isAuthValid({ ...base, authType: 'oauth', pkceVerifier: '', oauthCode: 'abc' }),
    ).toEqual({
      ok: false,
      reason: 'Open the login page to authorize.',
    })
  })
  it('not ok with reason for oauth without oauthCode', () => {
    expect(
      isAuthValid({ ...base, authType: 'oauth', pkceVerifier: 'v', oauthCode: '' }),
    ).toEqual({
      ok: false,
      reason: 'Paste the authorization code.',
    })
  })
  it('ok for oauth with both verifier and code', () => {
    expect(
      isAuthValid({ ...base, authType: 'oauth', pkceVerifier: 'v', oauthCode: 'c' }),
    ).toEqual({ ok: true })
  })
  it('not ok with reason for api-key with empty key', () => {
    expect(isAuthValid({ ...base, authType: 'api-key', anthropicApiKey: '' })).toEqual({
      ok: false,
      reason: 'Paste your Anthropic API Key.',
    })
  })
  it('ok for api-key with non-empty key', () => {
    expect(isAuthValid({ ...base, authType: 'api-key', anthropicApiKey: 'sk-ant-xxx' })).toEqual({
      ok: true,
    })
  })
  it('does not require telegramBotToken even when telegramEnabled is true', () => {
    expect(
      isAuthValid({
        ...base,
        authType: 'oauth',
        pkceVerifier: 'v',
        oauthCode: 'c',
        telegramEnabled: true,
        telegramBotToken: '',
      }),
    ).toEqual({ ok: true })
  })
  it('accepts Codex subscription mode without a pasted credential', () => {
    expect(isAuthValid({ ...base, runtime: 'codex', authType: 'oauth' })).toEqual({ ok: true })
  })
  it('requires an OpenAI key for Codex api-key mode', () => {
    expect(isAuthValid({ ...base, runtime: 'codex', authType: 'api-key', openaiApiKey: '' })).toEqual({
      ok: false,
      reason: 'Paste your OpenAI API key.',
    })
    expect(isAuthValid({ ...base, runtime: 'codex', authType: 'api-key', openaiApiKey: 'sk-test' })).toEqual({ ok: true })
  })
})

describe('isReviewValid', () => {
  it('always ok (read-only step)', () => {
    expect(isReviewValid(base)).toEqual({ ok: true })
  })
})

describe('WIZARD_STEPS', () => {
  it('is a 5-element list with ids 1..5 in order and matching labels', () => {
    expect(WIZARD_STEPS.length).toBe(5)
    expect(WIZARD_STEPS.map((s) => s.id)).toEqual([1, 2, 3, 4, 5])
    expect(WIZARD_STEPS.map((s) => s.label)).toEqual([
      'Basics',
      'Resources',
      'Identity',
      'Auth',
      'Review',
    ])
  })
})

describe('earliestInvalidStep', () => {
  it('returns 1 on a fresh state (basics is invalid)', () => {
    expect(earliestInvalidStep(base)).toBe(1)
  })
  it('returns 4 when basics + resources + identity are valid but auth is not', () => {
    expect(
      earliestInvalidStep({
        ...base,
        name: 'alice',
        machine: 'razer',
        identityRepoMode: 'template',
      }),
    ).toBe(4)
  })
  it('returns 5 (Review) when all gates pass', () => {
    expect(
      earliestInvalidStep({
        ...base,
        name: 'alice',
        machine: 'razer',
        identityRepoMode: 'template',
        authType: 'api-key',
        anthropicApiKey: 'sk-ant-xxx',
      }),
    ).toBe(5)
  })
})

describe('isAuthValid — custom inference endpoint', () => {
  // A half-filled endpoint produces an agent that comes up, reports healthy,
  // and fails every turn. Gate the step rather than let the API reject it
  // after the rest of the form is gone.
  const base = () => ({
    ...initialWizardState([]),
    runtime: 'hermes',
    authType: 'api-key' as const,
    runtimeApiKey: 'placeholder-runtime-key',
    runtimeContract: {
      id: 'hermes',
      name: 'Hermes',
      contractVersion: '1.0',
      profile: 'interactive-tmux-v1',
      cancellation: 'notify_only',
      features: ['custom-inference-endpoint'],
      authModes: [{ id: 'api-key', name: 'OpenRouter API key', flow: 'api-key', inputField: 'openrouterApiKey', channels: ['telegram'] }],
    },
    inferenceEnabled: true,
    inferenceBaseURL: 'https://llm.example.com/v1',
    inferenceSecretName: 'falcon-llm',
    inferenceSecretKey: 'token',
  }) as WizardState

  it('accepts a fully specified endpoint', () => {
    expect(isAuthValid(base()).ok).toBe(true)
  })

  it('blocks on a missing URL, secret name, or secret key', () => {
    expect(isAuthValid({ ...base(), inferenceBaseURL: '  ' }).ok).toBe(false)
    expect(isAuthValid({ ...base(), inferenceSecretName: '' }).ok).toBe(false)
    expect(isAuthValid({ ...base(), inferenceSecretKey: '' }).ok).toBe(false)
  })

  it('ignores the endpoint fields entirely when the operator did not opt in', () => {
    const off = { ...base(), inferenceEnabled: false, inferenceBaseURL: '', inferenceSecretName: '', inferenceSecretKey: '' }
    expect(isAuthValid(off).ok).toBe(true)
  })

  // An agent on its own endpoint authenticates with that endpoint's Secret
  // and never reaches the harness's built-in provider, so requiring the
  // provider's key forced a dummy string on operators without an account.
  it('does not require the built-in provider key when an endpoint is configured', () => {
    const noProviderKey = { ...base(), runtimeApiKey: '' }
    expect(isAuthValid(noProviderKey).ok).toBe(true)
  })

  it('still requires the provider key when no endpoint is configured', () => {
    const off = { ...base(), inferenceEnabled: false, runtimeApiKey: '' }
    expect(isAuthValid(off).ok).toBe(false)
  })
})