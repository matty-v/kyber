import type { Machine, Agent } from './types'

// GCE machine type → (vCPU, memory in Gi). Covers the types the Kyber deployment
// actually uses. If the backend exposes an unknown type, we fall back to returning
// null and the UI shows "capacity unknown" rather than lying.
export type MachineCapacity = { cpu: number; memoryGi: number; diskGi?: number }

const GCE_TYPES: Record<string, MachineCapacity> = {
  'e2-small':        { cpu: 2,  memoryGi: 2  },
  'e2-medium':       { cpu: 2,  memoryGi: 4  },
  'e2-standard-2':   { cpu: 2,  memoryGi: 8  },
  'e2-standard-4':   { cpu: 4,  memoryGi: 16 },
  'e2-standard-8':   { cpu: 8,  memoryGi: 32 },
  'e2-standard-16':  { cpu: 16, memoryGi: 64 },
  'n2-standard-2':   { cpu: 2,  memoryGi: 8  },
  'n2-standard-4':   { cpu: 4,  memoryGi: 16 },
  'n2-standard-8':   { cpu: 8,  memoryGi: 32 },
  'n2-standard-16':  { cpu: 16, memoryGi: 64 },
}

// SystemOverhead approximates what's reserved on the node for non-agent workloads:
// k3s, kube-proxy, the kyber-node-agent DaemonSet, kubelet/container-runtime
// overhead. Subtracted from the nominal machine capacity so the badge reflects
// what's actually available for agent pods. Conservative — real allocatable
// varies by k3s version + kernel; a follow-up will plumb Node.status.allocatable
// from the API for accuracy.
export const SystemOverhead: MachineCapacity = { cpu: 0.25, memoryGi: 0.25 }

// lookupMachineCapacity returns the allocatable capacity (nominal minus reserved
// system overhead). Pods that match the SystemOverhead estimate will fit; pods
// that exceed it won't. Returns null when the machine type is unknown.
export function lookupMachineCapacity(machineType: string): MachineCapacity | null {
  const nominal = GCE_TYPES[machineType]
  if (!nominal) return null
  return {
    cpu: Math.max(0, nominal.cpu - SystemOverhead.cpu),
    memoryGi: Math.max(0, nominal.memoryGi - SystemOverhead.memoryGi),
  }
}

// Parse an agent resource quantity string — supports plain numbers, 'm' for
// millicpu, 'Gi' / 'Mi' for memory. Returns the normalized value:
//   CPU: in cores (so '500m' -> 0.5, '1' -> 1)
//   Memory: in Gi (so '2Gi' -> 2, '512Mi' -> 0.5, '1G' -> 0.931)
export function parseCpu(v: string): number {
  if (!v) return 0
  const s = v.trim()
  if (s.endsWith('m')) {
    const n = parseFloat(s.slice(0, -1))
    return isNaN(n) ? 0 : n / 1000
  }
  const n = parseFloat(s)
  return isNaN(n) ? 0 : n
}

export function parseMemoryGi(v: string): number {
  if (!v) return 0
  const s = v.trim()
  if (s.endsWith('Gi')) return parseFloat(s.slice(0, -2))
  if (s.endsWith('Mi')) return parseFloat(s.slice(0, -2)) / 1024
  if (s.endsWith('Ki')) return parseFloat(s.slice(0, -2)) / 1024 / 1024
  if (s.endsWith('G'))  return parseFloat(s.slice(0, -1)) * 1000 / 1024
  if (s.endsWith('M'))  return parseFloat(s.slice(0, -1)) / 1024
  if (s.endsWith('K'))  return parseFloat(s.slice(0, -1)) / 1024 / 1024
  // Plain number (bytes); unlikely in Kyber but handle gracefully.
  const n = parseFloat(s)
  return isNaN(n) ? 0 : n / (1024 ** 3)
}

// machineCapacity picks the right source for a Machine's declared capacity:
//   1. spec.capacity (Phase B — authoritative; user-supplied on mock, server-
//      stamped on gce). Returned as-is — the user/server chose this number;
//      system overhead is NOT re-subtracted.
//   2. spec.machineType lookup (pre-Phase-B / legacy fallback). Goes through
//      lookupMachineCapacity which subtracts SystemOverhead.
//   3. null when neither is available.
export function machineCapacity(m: Machine): MachineCapacity | null {
  if (m.spec.capacity?.cpu && m.spec.capacity?.memory) {
    return {
      cpu: parseCpu(m.spec.capacity.cpu),
      memoryGi: parseMemoryGi(m.spec.capacity.memory),
      diskGi: m.spec.capacity.ephemeralStorage
        ? parseMemoryGi(m.spec.capacity.ephemeralStorage)
        : undefined,
    }
  }
  if (m.spec.machineType) {
    return lookupMachineCapacity(m.spec.machineType)
  }
  return null
}

/**
 * Live-availability triple. `diskGi` mirrors `cpu`/`memoryGi` — Gi units, parsed
 * from the wire's resource.Quantity string. Falls back to 0 (NOT null/undefined)
 * when ephemeral-storage isn't on the wire (pre-#129 PR-C clusters / machines
 * with no node yet) so callers can render a zero-bar instead of branching.
 */
export type MachineAvailability = { cpu: number; memoryGi: number; diskGi: number }

/**
 * availableFromMachine returns the live remaining capacity for a machine.
 * Three-tier preference: status.availableCapacity → status.{availableCPU,availableMemory}
 * → spec.capacity minus agents.
 *
 * Returns null when neither path can produce a number — caller should
 * treat as "capacity unknown" and disable the form. Disk follows the same
 * three-tier preference as CPU/memory but degrades to 0 (not null) when
 * a tier is missing — pre-PR-C clusters and pending machines.
 */
export function availableFromMachine(m: Machine, agents: Agent[]): MachineAvailability | null {
  const avail = m.status?.availableCapacity
  if (avail && avail.cpu && avail.memory) {
    return {
      cpu: parseCpu(avail.cpu),
      memoryGi: parseMemoryGi(avail.memory),
      diskGi: avail.ephemeralStorage ? parseMemoryGi(avail.ephemeralStorage) : 0,
    }
  }
  // Tier 2: legacy availableCPU/availableMemory strings (server-side enrichMachineResponse,
  // counts ALL pods on the node — more accurate than agents-only). Used during transitional
  // states where the controller hasn't published Status.AvailableCapacity yet.
  const legacyCPU = m.status?.availableCPU
  const legacyMem = m.status?.availableMemory
  if (legacyCPU && legacyMem) {
    // Legacy tier predates ephemeral-storage tracking — no disk number to surface.
    return { cpu: parseCpu(legacyCPU), memoryGi: parseMemoryGi(legacyMem), diskGi: 0 }
  }
  // Legacy fallback: spec-derived budget minus agents.
  const total = machineCapacity(m)
  if (!total) return null
  let usedCpu = 0
  let usedMem = 0
  let usedDisk = 0
  for (const a of agents) {
    if (a.machine !== m.id) continue
    if (a.resources?.cpu) usedCpu += parseCpu(a.resources.cpu)
    if (a.resources?.memory) usedMem += parseMemoryGi(a.resources.memory)
    if (a.resources?.disk) usedDisk += parseMemoryGi(a.resources.disk)
  }
  // Spec.Capacity.ephemeralStorage was added in #129 PR-C. When unset,
  // surface 0 so the bar renders as "no headroom known" rather than lying.
  const totalDisk = total.diskGi ?? 0
  return {
    cpu:      Math.max(0, total.cpu - usedCpu),
    memoryGi: Math.max(0, total.memoryGi - usedMem),
    diskGi:   Math.max(0, totalDisk - usedDisk),
  }
}
