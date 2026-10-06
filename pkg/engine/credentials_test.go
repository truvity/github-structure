package engine

import (
	"strings"
	"testing"

	"github.com/truvity/github-structure/pkg/registry"
)

var exampleProps = KVProperties{AppID: "app_id", InstallationID: "installation_id", PrivateKey: "private_key"}

func exampleSource() *registry.EngineCredentials {
	return &registry.EngineCredentials{
		OpenBAO: &registry.OpenBAOSecret{Namespace: "example-ns", Mount: "kv", Path: "apps/example-iac"},
	}
}

func TestCredentialsFromAPushedSecret(t *testing.T) {
	creds, err := CredentialsFromKV("acme", exampleSource(), map[string]string{
		"app_id": "5013323", "installation_id": "77", "private_key": "-----BEGIN RSA PRIVATE KEY-----",
	}, exampleProps, "")
	if err != nil {
		t.Fatalf("CredentialsFromKV: %v", err)
	}

	if creds.AppID != "5013323" || creds.InstallationID != "77" || creds.PrivateKey == "" {
		t.Fatalf("credentials = %+v", creds)
	}
}

// Every refusal names the org, the source and the path: an operator
// reading only the error knows which organization stopped and where to
// look for its credential.
func TestCredentialsRefusalsNameTheOrgAndThePlace(t *testing.T) {
	for _, tc := range []struct {
		name string
		data map[string]string
		want string
	}{
		{"nothing pushed yet", nil, "nothing there. create the app first"},
		{"only the id", map[string]string{"app_id": "5013323"}, "installation_id"},
		{"an empty key", map[string]string{"app_id": "5013323", "installation_id": "77", "private_key": ""}, "private_key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CredentialsFromKV("acme", exampleSource(), tc.data, exampleProps, "create the app first")
			if err == nil {
				t.Fatal("a malformed credential was accepted")
			}

			for _, want := range []string{`org "acme"`, "openbao", "apps/example-iac", tc.want} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}
}
