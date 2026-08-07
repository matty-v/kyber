import { createContext, useContext, type PropsWithChildren } from 'react'

// Capability strings are append-only — once a kyber release exposes a
// capability, it stays in the array forever, even if the underlying
// implementation is later refactored. New views feature-detect with
// useHasCapability(name) and degrade gracefully when absent.
export type ClusterCapability = string

export type Cluster = {
  /** Stable identifier. "local" in embedded mode; uuid in hub mode. */
  id: string
  /** User-facing display name (e.g. "kyber-gcp"). */
  name: string
  /** Absolute base URL of the cluster's control-plane API. Always ends with /. */
  baseURL: string
  /** API key for authenticating to baseURL. */
  apiKey: string
  /** Kyber version reported by the control plane (semver string). */
  version: string
  /** Capabilities reported by the control plane. */
  capabilities: ClusterCapability[]
}

export const ClusterContext = createContext<Cluster | null>(null)

export function ClusterProvider({ value, children }: PropsWithChildren<{ value: Cluster }>) {
  return <ClusterContext.Provider value={value}>{children}</ClusterContext.Provider>
}

export function useCluster(): Cluster {
  const c = useContext(ClusterContext)
  if (!c) {
    throw new Error(
      'No active cluster — wrap your tree in <ClusterProvider> (EmbeddedClusterProvider for the kyber binary, HubClusterProvider for holocron).',
    )
  }
  return c
}

export function useHasCapability(name: ClusterCapability): boolean {
  const cluster = useCluster()
  return cluster.capabilities.includes(name)
}
