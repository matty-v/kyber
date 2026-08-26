package adapters

import (
	"context"
	"strings"
	"testing"
)

func validEKSConfig() ProviderConfig {
	return ProviderConfig{
		EKSConfigRegion: "us-east-1", EKSConfigCluster: "kyber",
		EKSConfigNodeRoleARN:  "arn:aws:iam::123456789012:role/kyber-node",
		EKSConfigAllowedZones: `["us-east-1a","us-east-1b"]`,
		EKSConfigProfiles:     `[{"id":"small","cpu":"2","memory":"8Gi","instanceTypes":["m7i.large"],"diskSizeGb":100,"availabilityClasses":["reliable"]}]`,
	}
}

func TestParseEKSConfig(t *testing.T) {
	a, err := parseEKSConfig(validEKSConfig())
	if err != nil {
		t.Fatalf("parseEKSConfig: %v", err)
	}
	if a.region != "us-east-1" || len(a.profiles) != 1 || len(a.allowedZones) != 2 {
		t.Fatalf("adapter = %+v", a)
	}
	if err := a.Validate(context.Background(), DesiredMachine{Profile: "small", Location: "us-east-1a"}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestParseEKSConfigFailsClosed(t *testing.T) {
	for _, mutate := range []func(ProviderConfig){
		func(c ProviderConfig) { delete(c, EKSConfigRegion) },
		func(c ProviderConfig) { c[EKSConfigProfiles] = `[]` },
		func(c ProviderConfig) { c[EKSConfigAllowedZones] = `[]` },
		func(c ProviderConfig) { c[EKSConfigProfiles] = `[{` },
	} {
		cfg := validEKSConfig()
		mutate(cfg)
		if _, err := parseEKSConfig(cfg); err == nil {
			t.Fatal("parseEKSConfig unexpectedly succeeded")
		}
	}
}

func TestEKSValidateRejectsUnknownZoneAndCostOptimized(t *testing.T) {
	a, _ := parseEKSConfig(validEKSConfig())
	if err := a.Validate(context.Background(), DesiredMachine{Profile: "small", Location: "us-west-2a"}); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("zone error = %v", err)
	}
	if err := a.Validate(context.Background(), DesiredMachine{Profile: "small", Location: "us-east-1a", Interruptible: true}); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("cost error = %v", err)
	}
}

func TestEKSProviderRefRoundTripAndHostileRefs(t *testing.T) {
	a, _ := parseEKSConfig(validEKSConfig())
	ref := a.providerRef("kyber-worker-1")
	if got, err := a.parseProviderRef(ref); err != nil || got != "kyber-worker-1" {
		t.Fatalf("round trip = %q, %v", got, err)
	}
	for _, ref := range []ProviderRef{"", "gke://kyber/pool", "eks://other/pool", "eks://kyber/a/b", "eks://kyber/pool?x=1", "eks://kyber/%2F"} {
		if _, err := a.parseProviderRef(ref); err == nil {
			t.Errorf("parseProviderRef(%q) unexpectedly succeeded", ref)
		}
	}
}
