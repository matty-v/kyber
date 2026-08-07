// Shared "How secrets work" content — Format / Updates / Limits.
// Rendered inside the empty-state Collapsible AND the populated-state Popover
// so the two surfaces stay in sync by construction.

export function SecretsHelpContent() {
  return (
    <div className="space-y-2.5 text-xs text-text-secondary">
      <Section label="Format">
        <p>
          Key/value entries become{' '}
          <code className="text-text-primary">$USER_&lt;KEY&gt;</code> env vars.
        </p>
        <p>
          File entries mount at{' '}
          <code className="text-text-primary">/user-secrets/&lt;key&gt;.bin</code>.
        </p>
      </Section>
      <Section label="Updates">
        <p>Adding or replacing a secret rolls the pod.</p>
      </Section>
      <Section label="Limits">
        <p>256 KiB aggregate. 64 KiB per entry.</p>
      </Section>
    </div>
  )
}

function Section({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <p className="font-mono text-[10px] uppercase tracking-[0.2em] text-text-muted mb-0.5">
        {label}
      </p>
      <div className="space-y-1 leading-snug">{children}</div>
    </div>
  )
}
