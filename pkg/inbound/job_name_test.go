package inbound

import (
	"regexp"
	"testing"
)

func TestDispatchJobNameMatchesInPodDispatcherContract(t *testing.T) {
	got := DispatchJobName("req_ee4909ceb05790b2e33ac2370fa4d146")
	if want := "inbound-req-ee4909ceb05790b2e33ac2370fa4d146"; got != want {
		t.Fatalf("DispatchJobName() = %q, want %q", got, want)
	}
	if !regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`).MatchString(got) {
		t.Fatalf("DispatchJobName() = %q, rejected by kyber-job-dispatch", got)
	}
}
