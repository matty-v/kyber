// Effective model list for PWA pickers — kyber#378 PR-D.
//
// Source ordering:
//   1. GET /api/v1/available (runtimedetect snapshot — includes
//      displayName, real contextWindow, contextWindowKnown). Updated
//      every cadenceSeconds by the control-plane poller.
//   2. GET /api/v1/config Models (cold-start fallback — knownModels
//      table, contextWindowKnown derived as `true` since the in-Go list
//      authoritatively pins those windows). PR-A guarantees /available
//      is empty until the first poll succeeds.
//
// The hook returns a stable shape (AvailableModel) so every picker reads
// the same fields. Loading flags from both queries roll up via `isLoading`
// so the consumer can render a skeleton until either source resolves.

import type { AvailableModel, AvailableResponse } from './types'
import { useAvailable } from '../hooks/useAPI'
import { useComputeConfig } from '../hooks/useAPI'

export interface EffectiveModelList {
  models: AvailableModel[]
  claudeCodeVersions: string[]
  codexVersions: string[]
  source: 'available' | 'config-fallback' | 'empty'
  isLoading: boolean
}

export function useEffectiveModelList(runtime = 'claude-code'): EffectiveModelList {
  const available = useAvailable()
  const config = useComputeConfig()

  const availableData: AvailableResponse | undefined = available.data
  const detectedModels = runtime === 'codex' ? (availableData?.codexModels ?? []) : (availableData?.models ?? [])
  if (availableData && detectedModels.length > 0) {
    return {
      models: detectedModels,
      claudeCodeVersions: availableData.claudeCodeVersions,
      codexVersions: availableData.codexVersions ?? [],
      source: 'available',
      isLoading: false,
    }
  }
  const cfgModels = config.data?.models ?? []
  if (runtime !== 'codex' && cfgModels.length > 0) {
    return {
      models: cfgModels.map((m): AvailableModel => ({
        id: m.id,
        displayName: m.id, // /config doesn't carry display names; surface the ID.
        contextWindow: m.contextWindow,
        // /config's source is the in-Go knownModels table, which pins
        // the real window for every entry it lists. So Known=true here
        // is honest: it means "we have an authoritative value." A
        // brand-new unmapped model never reaches this list — it only
        // shows up via /available, which carries its own Known flag.
        contextWindowKnown: true,
      })),
      // /config doesn't surface CC versions. PR-A's /available is the
      // only source; an empty cache means no version picker is shown.
      claudeCodeVersions: availableData?.claudeCodeVersions ?? [],
      codexVersions: availableData?.codexVersions ?? [],
      source: 'config-fallback',
      isLoading: false,
    }
  }
  return {
    models: [],
    claudeCodeVersions: [],
    codexVersions: availableData?.codexVersions ?? [],
    source: 'empty',
    isLoading: available.isLoading || config.isLoading,
  }
}
