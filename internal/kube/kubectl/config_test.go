package kubectl

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/kube"
)

// A real Secret shape: base64-encoded data values, and an ownerReference marking it as synced from
// Vault by the External Secrets Operator. "aHVudGVyMg==" decodes to "hunter2" (7 bytes), "YWRtaW4="
// to "admin" (5 bytes).
const secretObj = `{"metadata":{"name":"db-creds","namespace":"demo","creationTimestamp":"2026-01-01T00:00:00Z",
  "ownerReferences":[{"kind":"ExternalSecret","name":"db-creds"}]},
 "type":"Opaque",
 "data":{"password":"aHVudGVyMg==","username":"YWRtaW4="}}`

// TestSecretRedaction is the security-critical property: a Secret detail carries only key names and
// byte lengths - the decoded value NEVER leaves this package. Neither the marshaled detail nor the
// manifest may contain the plaintext or its base64 encoding.
func TestSecretRedaction(t *testing.T) {
	c := New(stubExecer{responses: map[string]string{"get secrets db-creds": secretObj}})

	d, err := c.Secret(context.Background(), cl, nil, kube.SecretRef{Namespace: "demo", Name: "db-creds"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Type != "Opaque" {
		t.Errorf("type = %q, want Opaque", d.Type)
	}
	if d.ManagedBy != "db-creds" {
		t.Errorf("managed_by = %q, want db-creds (ExternalSecret owner)", d.ManagedBy)
	}
	// Byte lengths are the ONLY thing revealed about a value.
	byKey := map[string]int{}
	for _, k := range d.Keys {
		byKey[k.Key] = k.Bytes
	}
	if byKey["password"] != 7 || byKey["username"] != 5 {
		t.Errorf("byte lengths = %v, want password:7 username:5", byKey)
	}

	// The marshaled detail must not contain the plaintext OR its base64 encoding.
	blob, _ := json.Marshal(d)
	for _, leak := range []string{"hunter2", "aHVudGVyMg==", "admin", "YWRtaW4="} {
		if strings.Contains(string(blob), leak) {
			t.Fatalf("secret detail leaked %q: %s", leak, blob)
		}
	}

	// The manifest must be scrubbed too - values replaced with the placeholder.
	y, err := c.SecretManifest(context.Background(), cl, nil, kube.SecretRef{Namespace: "demo", Name: "db-creds"})
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"hunter2", "aHVudGVyMg==", "YWRtaW4="} {
		if strings.Contains(y, leak) {
			t.Fatalf("secret manifest leaked %q:\n%s", leak, y)
		}
	}
	if !strings.Contains(y, "<redacted>") {
		t.Fatalf("secret manifest not redacted:\n%s", y)
	}
}
