package dns

import "testing"

func lab() Settings {
	return Settings{
		BaseDomain:  "kaas.example.internal",
		Server:      "dc01.example.internal",
		KrbUsername: "svc-kaas",
		KrbRealm:    "EXAMPLE.INTERNAL",
		KrbPassword: "pw",
	}
}

func TestNaming(t *testing.T) {
	s, err := lab().Validate()
	if err != nil {
		t.Fatal(err)
	}
	if got := s.ClusterDomain("dev"); got != "dev.kaas.example.internal" {
		t.Fatalf("cluster domain = %q", got)
	}
	if got := s.AppsDomain("dev"); got != "apps.dev.kaas.example.internal" {
		t.Fatalf("apps domain = %q", got)
	}
	if got := Wildcard(s.AppsDomain("dev")); got != "*.apps.dev.kaas.example.internal" {
		t.Fatalf("wildcard = %q", got)
	}
	// The zone defaults to the base domain: a deployment that delegated exactly what it hands out
	// needs to say it once.
	if s.Zone != s.BaseDomain {
		t.Fatalf("zone = %q, want %q", s.Zone, s.BaseDomain)
	}
}

// Disabled is the whole of "this deployment publishes no DNS": no error, no names, and every caller
// sees the same empty pair rather than a special case.
func TestDisabled(t *testing.T) {
	s, err := Settings{}.Validate()
	if err != nil {
		t.Fatal(err)
	}
	if s.Enabled() {
		t.Fatal("empty settings are enabled")
	}
	cd, ad, err := s.AdmitCluster("dev")
	if err != nil || cd != "" || ad != "" {
		t.Fatalf("AdmitCluster on disabled = %q/%q/%v", cd, ad, err)
	}
}

func TestAdmitClusterRejectsUnpublishableName(t *testing.T) {
	s, _ := lab().Validate()
	for _, name := range []string{"Dev", "dev cluster", "-dev", "dev.prod"} {
		if _, _, err := s.AdmitCluster(name); err == nil {
			t.Fatalf("name %q was accepted", name)
		}
	}
	if _, _, err := s.AdmitCluster("dev-1"); err != nil {
		t.Fatalf("dev-1 rejected: %v", err)
	}
}

// The base domain must sit inside the zone we send updates to, or every record we write lands
// somewhere the server won't accept. Catch it at boot, not per cluster.
func TestValidateRejectsBaseDomainOutsideZone(t *testing.T) {
	s := lab()
	s.Zone = "other.internal"
	if _, err := s.Validate(); err == nil {
		t.Fatal("base domain outside the zone was accepted")
	}
}

// Only a process that WRITES records needs a server and a credential. The API derives names and
// nothing else, so Validate must not demand them - otherwise the API container would have to hold
// the credential that can rewrite the site's zone.
func TestValidateDoesNotRequireCredentials(t *testing.T) {
	s := Settings{BaseDomain: "kaas.example.internal"}
	if _, err := s.Validate(); err != nil {
		t.Fatalf("naming-only settings rejected: %v", err)
	}
	if _, err := s.ValidateUpdate(); err == nil {
		t.Fatal("ValidateUpdate accepted settings with no server")
	}
}

func TestValidateUpdateAuth(t *testing.T) {
	// A realm can be left out only when the principal names one - guessing it from the delegated
	// zone would produce a realm that does not exist.
	s := lab()
	s.KrbRealm = ""
	if _, err := s.ValidateUpdate(); err == nil {
		t.Fatal("missing realm accepted")
	}
	s.KrbUsername = "svc-kaas@EXAMPLE.INTERNAL"
	got, err := s.ValidateUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if got.KrbRealm != "EXAMPLE.INTERNAL" {
		t.Fatalf("realm = %q", got.KrbRealm)
	}

	bad := lab()
	bad.Auth = "kerberos"
	if _, err := bad.ValidateUpdate(); err == nil {
		t.Fatal("unknown auth mode accepted")
	}
}
