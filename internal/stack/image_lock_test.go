package stack

import (
	"path/filepath"
	"testing"
)

// keycloakIndexDigest is the OCI image index digest that a registry read of
// docker.io/keycloak/keycloak:26.8.1 returned on 2026-11-16. quay.io returned
// the same index bytes.
const keycloakIndexDigest = "sha256:3f3c5f8e03c614774b4bd9173b88391fefa4153bfc010d8012d59ad825377332"

func TestImageLockPinsKeycloakByItsIndexDigest(t *testing.T) {
	repository, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	images, err := loadImageLock(repository)
	if err != nil {
		t.Fatalf("load deployment image lock: %v", err)
	}
	if got, want := images["keycloak"], "docker.io/keycloak/keycloak@"+keycloakIndexDigest; got != want {
		t.Fatalf("keycloak image reference = %q, want %q", got, want)
	}
}
