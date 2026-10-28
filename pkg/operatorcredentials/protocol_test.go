package operatorcredentials

import (
	"os"
	"strconv"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPublishedChartDeclaresActualRuntimeAuthorityProtocol(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/helm/kyber/Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var chart struct {
		Annotations map[string]string `yaml:"annotations"`
	}
	if err := yaml.Unmarshal(raw, &chart); err != nil {
		t.Fatal(err)
	}
	if chart.Annotations["kyber.io/operator-credential-authority-protocol"] != strconv.Itoa(RuntimeAuthorityProtocolVersion) {
		t.Fatal("chart and runtime authority protocols differ")
	}
}
