package usagestore

import "testing"

func TestDailyAggregatePartialAndCorrection(t *testing.T) {
	a := DailyAggregate{}
	first := validEvent()
	first.InputUncached = n(10)
	first.CacheRead = nil
	second := validEvent()
	second.InputUncached = n(0)
	second.CacheRead = n(5)
	if err := a.apply(first, 1); err != nil {
		t.Fatal(err)
	}
	if err := a.apply(second, 1); err != nil {
		t.Fatal(err)
	}
	if a.EventCount != 2 || a.InputUncached.Sum == nil || *a.InputUncached.Sum != 10 || a.InputUncached.ReportedEvents != 2 ||
		a.CacheRead.Sum == nil || *a.CacheRead.Sum != 5 || a.CacheRead.ReportedEvents != 1 || a.CacheRead.UnknownEvents != 1 {
		t.Fatalf("partial aggregate=%+v", a)
	}
	if err := a.apply(first, -1); err != nil {
		t.Fatal(err)
	}
	if a.EventCount != 1 || a.InputUncached.Sum == nil || *a.InputUncached.Sum != 0 || a.CacheRead.UnknownEvents != 0 {
		t.Fatalf("after supersession=%+v", a)
	}
	if err := a.apply(second, -1); err != nil {
		t.Fatal(err)
	}
	if a.EventCount != 0 || a.InputUncached.Sum != nil || a.CacheRead.Sum != nil {
		t.Fatalf("empty aggregate=%+v", a)
	}
	if err := a.apply(second, -1); err == nil {
		t.Fatal("negative event count accepted")
	}
}
