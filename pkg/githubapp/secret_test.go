package githubapp

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
)

func pemForTest(t *testing.T) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(k),
	})
}

func TestConfigFromSecretData(t *testing.T) {
	goodPEM := pemForTest(t)

	t.Run("happy path", func(t *testing.T) {
		cfg, err := configFromSecretData(map[string][]byte{
			"app-id":          []byte("3421339"),
			"installation-id": []byte("125001155\n"), // tolerates trailing newline
			"private-key.pem": goodPEM,
		})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AppID != 3421339 {
			t.Errorf("appID: got %d", cfg.AppID)
		}
		if cfg.InstallationID != 125001155 {
			t.Errorf("installationID: got %d", cfg.InstallationID)
		}
		if cfg.PrivateKey == nil {
			t.Error("private key nil")
		}
	})

	cases := []struct {
		name    string
		data    map[string][]byte
		wantErr string
	}{
		{
			"nil data",
			nil,
			"no data",
		},
		{
			"missing app-id",
			map[string][]byte{"installation-id": []byte("1"), "private-key.pem": goodPEM},
			`missing key "app-id"`,
		},
		{
			"missing installation-id",
			map[string][]byte{"app-id": []byte("1"), "private-key.pem": goodPEM},
			`missing key "installation-id"`,
		},
		{
			"missing pem",
			map[string][]byte{"app-id": []byte("1"), "installation-id": []byte("2")},
			`missing key "private-key.pem"`,
		},
		{
			"unparseable app-id",
			map[string][]byte{"app-id": []byte("not-a-number"), "installation-id": []byte("2"), "private-key.pem": goodPEM},
			"parsing app-id",
		},
		{
			"zero app-id",
			map[string][]byte{"app-id": []byte("0"), "installation-id": []byte("2"), "private-key.pem": goodPEM},
			`key "app-id" is zero`,
		},
		{
			"bad pem",
			map[string][]byte{"app-id": []byte("1"), "installation-id": []byte("2"), "private-key.pem": []byte("not pem")},
			"parsing private-key.pem",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := configFromSecretData(tc.data)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
		})
	}
}
