package activity

import (
	"testing"
	"time"
)

func TestProject(t *testing.T) {
	now := time.Date(2026, 11, 22, 20, 0, 0, 0, time.UTC)
	base := Input{Running: true, ContactAt: now, Inventory: &Inventory{ObservedAt: now, LastWorkedAt: now, Complete: true}}
	cases := []struct {
		name   string
		change func(*Input)
		want   State
		reason string
	}{
		{"stopped", func(i *Input) { i.Running = false }, Excluded, "lifecycle"},
		{"stop requested before phase", func(i *Input) { i.StopPending = true }, Excluded, "lifecycle"},
		{"missing contact", func(i *Input) { i.ContactAt = time.Time{} }, Unavailable, "contact_missing"},
		{"contact boundary", func(i *Input) { i.ContactAt = now.Add(-ContactLease) }, Unavailable, "contact_stale"},
		{"future contact", func(i *Input) { i.ContactAt = now.Add(time.Second) }, Unavailable, "contact_stale"},
		{"legacy", func(i *Input) { i.Inventory = nil }, Unavailable, "legacy_reporting"},
		{"partial", func(i *Input) { i.Inventory.Complete = false }, Unavailable, "inventory_incomplete"},
		{"work lease boundary", func(i *Input) {
			i.Inventory.ObservedAt = now.Add(-WorkLease)
			i.Inventory.LastWorkedAt = i.Inventory.ObservedAt
		}, Unavailable, "reporter_stale"},
		{"future observation", func(i *Input) { i.Inventory.ObservedAt = now.Add(time.Second) }, Unavailable, "reporter_stale"},
		{"negative count", func(i *Input) { i.Inventory.Background = -1 }, Unavailable, "inventory_invalid"},
		{"unbounded count", func(i *Input) { i.Inventory.Background = MaxJobs + 1 }, Unavailable, "inventory_invalid"},
		{"future work", func(i *Input) { i.Inventory.LastWorkedAt = now.Add(time.Second) }, Unavailable, "inventory_invalid"},
		{"foreground wins", func(i *Input) { i.Inventory.Foreground = 1; i.Inventory.Background = 3 }, Working, ""},
		{"background", func(i *Input) { i.Inventory.Background = 2 }, Background, ""},
		{"waiting", func(i *Input) {}, Waiting, ""},
		{"never worked", func(i *Input) { i.Inventory.LastWorkedAt = time.Time{} }, Excluded, "outside_history"},
		{"history boundary", func(i *Input) { i.Inventory.LastWorkedAt = now.Add(-RecentWindow) }, Excluded, "outside_history"},
		{"old pending reply", func(i *Input) { i.Inventory.LastWorkedAt = now.Add(-2 * RecentWindow); i.Inventory.NeedsInput = true }, Waiting, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			i := base
			copy := *base.Inventory
			i.Inventory = &copy
			c.change(&i)
			v := Project(i, now)
			if v.State != c.want || v.Reason != c.reason {
				t.Fatalf("got %+v, want %s/%s", v, c.want, c.reason)
			}
		})
	}
}
func TestExpiryUsesEarlierContactBoundary(t *testing.T) {
	now := time.Now().UTC()
	v := Project(Input{Running: true, ContactAt: now.Add(-80 * time.Second), Inventory: &Inventory{ObservedAt: now, LastWorkedAt: now, Complete: true, Foreground: 1}}, now)
	if !v.ExpiresAt.Equal(now.Add(10 * time.Second)) {
		t.Fatal(v)
	}
}
