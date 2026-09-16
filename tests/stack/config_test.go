package stack_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const confidentialFixtureValue = "credential-value-must-not-leak"

const bundledDevelopmentConfiguration = `{
  "schema_version": "1",
  "deployment_id": "test-deployment",
  "public_origin": "https://pm.example.invalid",
  "postgres": {
    "ownership": "bundled",
    "database": "pm_app",
    "endpoint_ref": "secret://test/credential-value-must-not-leak/postgres-bundled"
  },
  "temporal": {
    "ownership": "bundled",
    "endpoint": "temporal.test.invalid:7233",
    "namespace": "pm-test",
    "client_identity_ref": "secret://test/credential-value-must-not-leak/temporal-bundled",
    "retention": "720h",
    "task_queues": [
      "pm-control",
      "pm-read",
      "pm-effect",
      "pm-binary-query"
    ]
  },
  "artifacts": {
    "ownership": "bundled",
    "driver": "s3",
    "endpoint": "https://objects.test.invalid",
    "bucket": "pm-test-artifacts",
    "credential_ref": "secret://test/credential-value-must-not-leak/artifacts-bundled",
    "attempt_key_policy": "unique_immutable"
  },
  "identity": {
    "issuer": "https://identity.test.invalid",
    "client_id": "pm-server",
    "client_secret_ref": "secret://test/credential-value-must-not-leak/identity"
  },
  "encryption": {
    "wrapping_key_ref": "secret://test/credential-value-must-not-leak/wrapping-key"
  },
  "admission": {
    "profile": "development"
  },
  "limits": {
    "workspace_active_runs": 4,
    "workflow_payload_bytes": 65536,
    "workflow_fanout": 32,
    "event_buffer": 128,
    "query_children": 1,
    "query_child_memory_bytes": 4294967296,
    "query_child_deadline": "5m",
    "request_bytes": 1048576,
    "metadata_deadline": "5s"
  }
}`

func TestStrictStackConfiguration(t *testing.T) {
	t.Run("E01-C01 equal duplicate schema version is refused before mutation", func(t *testing.T) {
		raw := replaceExactly(t, bundledDevelopmentConfiguration,
			`  "schema_version": "1",`,
			`  "schema_version": "1",
  "schema_version": "1",`)
		launcher := new(recordingLauncher)
		_, err := validateBeforeMutation(raw, config.Prelaunch{}, launcher)
		requireDiagnostic(t, err, "schema_version", config.DiagnosticDuplicate)
		requireNoMutations(t, launcher)

		escaped := replaceExactly(t, bundledDevelopmentConfiguration,
			`  "schema_version": "1",`,
			`  "schema_version": "1",
  "schema\u005fversion": "1",`)
		escapedLauncher := new(recordingLauncher)
		_, err = validateBeforeMutation(escaped, config.Prelaunch{}, escapedLauncher)
		requireDiagnostic(t, err, "schema_version", config.DiagnosticDuplicate)
		requireNoMutations(t, escapedLauncher)
	})

	t.Run("E01-C02 strict JSON identifies the offending field", func(t *testing.T) {
		malformed := strings.TrimSuffix(bundledDevelopmentConfiguration, "}")
		deep := `{"schema_version":` + strings.Repeat("[", 10_001) + `"1"` + strings.Repeat("]", 10_001) + `}`
		cases := []struct {
			name  string
			raw   string
			field string
			kind  config.DiagnosticKind
		}{
			{
				name: "unknown field",
				raw: replaceExactly(t, bundledDevelopmentConfiguration,
					`  "schema_version": "1",`,
					`  "schema_version": "1",
  "unexpected": true,`),
				field: "unexpected",
				kind:  config.DiagnosticUnknown,
			},
			{
				name: "wrongly cased nested field",
				raw: replaceExactly(t, bundledDevelopmentConfiguration,
					`    "credential_ref": "secret://test/credential-value-must-not-leak/artifacts-bundled",`,
					`    "credential_Ref": "secret://test/credential-value-must-not-leak/artifacts-bundled",`),
				field: "artifacts.credential_Ref",
				kind:  config.DiagnosticUnknown,
			},
			{
				name: "wrong JSON type",
				raw: replaceExactly(t, bundledDevelopmentConfiguration,
					`  "schema_version": "1",`,
					`  "schema_version": 1,`),
				field: "schema_version",
				kind:  config.DiagnosticType,
			},
			{
				name: "forbidden null",
				raw: replaceExactly(t, bundledDevelopmentConfiguration,
					`  "encryption": {
    "wrapping_key_ref": "secret://test/credential-value-must-not-leak/wrapping-key"
  },`,
					`  "encryption": null,`),
				field: "encryption",
				kind:  config.DiagnosticType,
			},
			{
				name: "unsupported schema version",
				raw: replaceExactly(t, bundledDevelopmentConfiguration,
					`  "schema_version": "1",`,
					`  "schema_version": "2",`),
				field: "schema_version",
				kind:  config.DiagnosticValue,
			},
			{
				name:  "malformed document",
				raw:   malformed,
				field: "$",
				kind:  config.DiagnosticSyntax,
			},
			{
				name: "trailing JSON value",
				raw: bundledDevelopmentConfiguration + `
{}`,
				field: "$",
				kind:  config.DiagnosticSyntax,
			},
			{
				name: "unknown field value is not echoed",
				raw: replaceExactly(t, bundledDevelopmentConfiguration,
					`  "schema_version": "1",`,
					`  "schema_version": "1",
  "credential_value": "credential-value-must-not-leak",`),
				field: "credential_value",
				kind:  config.DiagnosticUnknown,
			},
			{
				name:  "excessive document size",
				raw:   strings.Repeat(" ", 16<<20) + bundledDevelopmentConfiguration,
				field: "$",
				kind:  config.DiagnosticBounds,
			},
			{
				name:  "excessive nesting",
				raw:   deep,
				field: "$",
				kind:  config.DiagnosticBounds,
			},
			{
				name: "invalid UTF-8 is not replaced",
				raw: replaceExactly(t, bundledDevelopmentConfiguration,
					`    "client_id": "pm-server",`,
					`    "client_id": "`+string([]byte{0xff})+`",`),
				field: "$",
				kind:  config.DiagnosticSyntax,
			},
			{
				name: "unpaired UTF-16 high surrogate is not replaced",
				raw: replaceExactly(t, bundledDevelopmentConfiguration,
					`    "client_id": "pm-server",`,
					`    "client_id": "\ud800",`),
				field: "$",
				kind:  config.DiagnosticSyntax,
			},
			{
				name: "unpaired UTF-16 low surrogate is not replaced",
				raw: replaceExactly(t, bundledDevelopmentConfiguration,
					`    "client_id": "pm-server",`,
					`    "client_id": "\udc00",`),
				field: "$",
				kind:  config.DiagnosticSyntax,
			},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				launcher := new(recordingLauncher)
				_, err := validateBeforeMutation(test.raw, config.Prelaunch{}, launcher)
				requireDiagnostic(t, err, test.field, test.kind)
				requireNoMutations(t, launcher)
			})
		}

		t.Run("valid replacement rune and surrogate pair remain lossless", func(t *testing.T) {
			raw := replaceExactly(t, bundledDevelopmentConfiguration,
				`    "wrapping_key_ref": "secret://test/credential-value-must-not-leak/wrapping-key"`,
				`    "wrapping_key_ref": "secret://test/�/\ud83d\ude00"`)
			resolved := requireConfiguration(t, raw)
			if got := resolved.Encryption().WrappingKeyRef; got != "secret://test/�/😀" {
				t.Fatalf("wrapping key reference = %q; want lossless Unicode decoding", got)
			}
		})
	})

	t.Run("E01-C03 every required credential or endpoint reference is present", func(t *testing.T) {
		cases := []struct {
			name  string
			raw   string
			field string
		}{
			{
				name: "missing wrapping key reference",
				raw: replaceExactly(t, bundledDevelopmentConfiguration,
					`  "encryption": {
    "wrapping_key_ref": "secret://test/credential-value-must-not-leak/wrapping-key"
  },`,
					`  "encryption": {},`),
				field: "encryption.wrapping_key_ref",
			},
			{
				name: "missing object credential reference",
				raw: replaceExactly(t, bundledDevelopmentConfiguration,
					`    "credential_ref": "secret://test/credential-value-must-not-leak/artifacts-bundled",
`, ""),
				field: "artifacts.credential_ref",
			},
			{
				name: "missing postgres endpoint reference",
				raw: replaceExactly(t, bundledDevelopmentConfiguration,
					`    "database": "pm_app",
    "endpoint_ref": "secret://test/credential-value-must-not-leak/postgres-bundled"`,
					`    "database": "pm_app"`),
				field: "postgres.endpoint_ref",
			},
			{
				name: "missing temporal client identity reference",
				raw: replaceExactly(t, bundledDevelopmentConfiguration,
					`    "client_identity_ref": "secret://test/credential-value-must-not-leak/temporal-bundled",
`, ""),
				field: "temporal.client_identity_ref",
			},
			{
				name: "missing identity client secret reference",
				raw: replaceExactly(t, bundledDevelopmentConfiguration,
					`    "client_id": "pm-server",
    "client_secret_ref": "secret://test/credential-value-must-not-leak/identity"`,
					`    "client_id": "pm-server"`),
				field: "identity.client_secret_ref",
			},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				launcher := new(recordingLauncher)
				_, err := validateBeforeMutation(test.raw, config.Prelaunch{}, launcher)
				requireDiagnostic(t, err, test.field, config.DiagnosticRequired)
				requireNoMutations(t, launcher)
			})
		}
	})

	t.Run("E01-C04 managed lifecycle is refused before the launcher records a mutation", func(t *testing.T) {
		managed := managedConfiguration(t)
		cases := []struct {
			name      string
			lifecycle config.LifecycleRequest
			field     string
		}{
			{
				name:      "managed postgres init",
				lifecycle: config.LifecycleRequest{Dependency: config.DependencyPostgres, Action: config.LifecycleInit},
				field:     "postgres.ownership",
			},
			{
				name:      "managed temporal stop",
				lifecycle: config.LifecycleRequest{Dependency: config.DependencyTemporal, Action: config.LifecycleStop},
				field:     "temporal.ownership",
			},
			{
				name:      "managed artifacts delete",
				lifecycle: config.LifecycleRequest{Dependency: config.DependencyArtifacts, Action: config.LifecycleDelete},
				field:     "artifacts.ownership",
			},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				launcher := new(recordingLauncher)
				prelaunch := config.Prelaunch{Lifecycle: []config.LifecycleRequest{test.lifecycle}}
				_, err := validateBeforeMutation(managed, prelaunch, launcher)
				requireDiagnostic(t, err, test.field, config.DiagnosticOwnership)
				requireNoMutations(t, launcher)
			})
		}

		t.Run("bundled lifecycle intent is released only after validation", func(t *testing.T) {
			prelaunch := config.Prelaunch{Lifecycle: []config.LifecycleRequest{{
				Dependency: config.DependencyPostgres,
				Action:     config.LifecycleInit,
			}}}
			launcher := new(recordingLauncher)
			if _, err := validateBeforeMutation(bundledDevelopmentConfiguration, prelaunch, launcher); err != nil {
				t.Fatalf("validate bundled lifecycle intent: %v", err)
			}
			if !reflect.DeepEqual(launcher.mutations, prelaunch.Lifecycle) {
				t.Fatalf("recorded lifecycle intents = %#v; want %#v", launcher.mutations, prelaunch.Lifecycle)
			}
		})

		t.Run("the complete request is checked before any earlier bundled action", func(t *testing.T) {
			mixed := managedTemporalConfiguration(t)
			prelaunch := config.Prelaunch{Lifecycle: []config.LifecycleRequest{
				{Dependency: config.DependencyPostgres, Action: config.LifecycleInit},
				{Dependency: config.DependencyTemporal, Action: config.LifecycleStop},
			}}
			launcher := new(recordingLauncher)
			_, err := validateBeforeMutation(mixed, prelaunch, launcher)
			requireDiagnostic(t, err, "temporal.ownership", config.DiagnosticOwnership)
			requireNoMutations(t, launcher)
		})
	})

	t.Run("E01-C05 bundled and managed endpoints retain one architecture", func(t *testing.T) {
		bundled := requireConfiguration(t, bundledDevelopmentConfiguration)
		managed := requireConfiguration(t, managedConfiguration(t))

		if bundled.Architecture() != config.ArchitecturePostgresTemporalS3 {
			t.Fatalf("bundled architecture = %q; want PostgreSQL/Temporal/S3", bundled.Architecture())
		}
		if managed.Architecture() != bundled.Architecture() {
			t.Fatalf("managed architecture = %q; want the bundled architecture %q", managed.Architecture(), bundled.Architecture())
		}

		requirePostgres(t, bundled.Postgres(), config.PostgresConfiguration{
			Ownership:   config.OwnershipBundled,
			Database:    "pm_app",
			EndpointRef: "secret://test/credential-value-must-not-leak/postgres-bundled",
		})
		requirePostgres(t, managed.Postgres(), config.PostgresConfiguration{
			Ownership:   config.OwnershipManaged,
			Database:    "pm_app",
			EndpointRef: "secret://test/credential-value-must-not-leak/postgres-managed",
		})
		requireTemporal(t, bundled.Temporal(), config.TemporalConfiguration{
			Ownership:         config.OwnershipBundled,
			Endpoint:          "temporal.test.invalid:7233",
			Namespace:         "pm-test",
			ClientIdentityRef: "secret://test/credential-value-must-not-leak/temporal-bundled",
			Retention:         720 * time.Hour,
			TaskQueues:        []string{"pm-control", "pm-read", "pm-effect", "pm-binary-query"},
		})
		requireTemporal(t, managed.Temporal(), config.TemporalConfiguration{
			Ownership:         config.OwnershipManaged,
			Endpoint:          "temporal.managed.test.invalid:7233",
			Namespace:         "pm-test",
			ClientIdentityRef: "secret://test/credential-value-must-not-leak/temporal-managed",
			Retention:         720 * time.Hour,
			TaskQueues:        []string{"pm-control", "pm-read", "pm-effect", "pm-binary-query"},
		})
		requireArtifacts(t, bundled.Artifacts(), config.ArtifactsConfiguration{
			Ownership:        config.OwnershipBundled,
			Driver:           "s3",
			Endpoint:         "https://objects.test.invalid",
			Bucket:           "pm-test-artifacts",
			CredentialRef:    "secret://test/credential-value-must-not-leak/artifacts-bundled",
			AttemptKeyPolicy: "unique_immutable",
		})
		requireArtifacts(t, managed.Artifacts(), config.ArtifactsConfiguration{
			Ownership:        config.OwnershipManaged,
			Driver:           "s3",
			Endpoint:         "https://objects.managed.test.invalid",
			Bucket:           "pm-test-artifacts",
			CredentialRef:    "secret://test/credential-value-must-not-leak/artifacts-managed",
			AttemptKeyPolicy: "unique_immutable",
		})

		view := bundled.Temporal()
		view.Ownership = config.OwnershipManaged
		view.TaskQueues[0] = "mutated"
		if got := bundled.Temporal(); got.Ownership != config.OwnershipBundled || got.TaskQueues[0] != "pm-control" {
			t.Fatalf("resolved configuration changed through a returned view: %#v", got)
		}
	})

	t.Run("accepted admission profiles and presence-aware overrides resolve once", func(t *testing.T) {
		successes := []struct {
			name      string
			admission string
			want      config.AdmissionLimits
		}{
			{
				name: "development profile",
				admission: `{
    "profile": "development"
  }`,
				want: config.AdmissionLimits{GlobalPending: 32, WorkspacePending: 8, WorkspaceMaxPendingAge: 2 * time.Minute},
			},
			{
				name: "standard profile",
				admission: `{
    "profile": "standard"
  }`,
				want: config.AdmissionLimits{GlobalPending: 256, WorkspacePending: 32, WorkspaceMaxPendingAge: 10 * time.Minute},
			},
			{
				name: "partial override retains other profile values",
				admission: `{
    "profile": "development",
    "overrides": {
      "workspace_pending": 7
    }
  }`,
				want: config.AdmissionLimits{GlobalPending: 32, WorkspacePending: 7, WorkspaceMaxPendingAge: 2 * time.Minute},
			},
			{
				name: "positive equality boundary",
				admission: `{
    "profile": "standard",
    "overrides": {
      "global_pending": 1,
      "workspace_pending": 1,
      "workspace_max_pending_age": "1s"
    }
  }`,
				want: config.AdmissionLimits{GlobalPending: 1, WorkspacePending: 1, WorkspaceMaxPendingAge: time.Second},
			},
		}
		for _, test := range successes {
			t.Run(test.name, func(t *testing.T) {
				resolved := requireConfiguration(t, withAdmission(t, test.admission))
				if got := resolved.Admission(); got != test.want {
					t.Fatalf("resolved admission = %#v; want %#v", got, test.want)
				}
			})
		}

		t.Run("override presence is retained separately from resolved defaults", func(t *testing.T) {
			resolved := requireConfiguration(t, withAdmission(t, `{
    "profile": "development",
    "overrides": {
      "workspace_pending": 7
    }
  }`))
			overrides := resolved.AdmissionOverrides()
			if value, present := overrides.GlobalPending(); present || value != 0 {
				t.Fatalf("global override = (%d, %t); want absent", value, present)
			}
			if value, present := overrides.WorkspacePending(); !present || value != 7 {
				t.Fatalf("workspace override = (%d, %t); want (7, true)", value, present)
			}
			if value, present := overrides.WorkspaceMaxPendingAge(); present || value != 0 {
				t.Fatalf("age override = (%s, %t); want absent", value, present)
			}
		})

		failures := []struct {
			name      string
			admission string
			field     string
			kind      config.DiagnosticKind
		}{
			{
				name:      "profile is required",
				admission: `{}`,
				field:     "admission.profile",
				kind:      config.DiagnosticRequired,
			},
			{
				name: "obsolete local profile",
				admission: `{
    "profile": "local"
  }`,
				field: "admission.profile",
				kind:  config.DiagnosticValue,
			},
			{
				name: "synthetic test profile",
				admission: `{
    "profile": "test"
  }`,
				field: "admission.profile",
				kind:  config.DiagnosticValue,
			},
			{
				name: "explicit zero global count is not absence",
				admission: `{
    "profile": "development",
    "overrides": {"global_pending": 0}
  }`,
				field: "admission.overrides.global_pending",
				kind:  config.DiagnosticValue,
			},
			{
				name: "explicit zero workspace count is not absence",
				admission: `{
    "profile": "development",
    "overrides": {"workspace_pending": 0}
  }`,
				field: "admission.overrides.workspace_pending",
				kind:  config.DiagnosticValue,
			},
			{
				name: "explicit zero age is not absence",
				admission: `{
    "profile": "development",
    "overrides": {"workspace_max_pending_age": "0s"}
  }`,
				field: "admission.overrides.workspace_max_pending_age",
				kind:  config.DiagnosticValue,
			},
			{
				name: "negative age",
				admission: `{
    "profile": "development",
    "overrides": {"workspace_max_pending_age": "-1s"}
  }`,
				field: "admission.overrides.workspace_max_pending_age",
				kind:  config.DiagnosticValue,
			},
			{
				name: "negative count",
				admission: `{
    "profile": "development",
    "overrides": {"global_pending": -1}
  }`,
				field: "admission.overrides.global_pending",
				kind:  config.DiagnosticValue,
			},
			{
				name: "fractional count is not coerced",
				admission: `{
    "profile": "development",
    "overrides": {"global_pending": 1.5}
  }`,
				field: "admission.overrides.global_pending",
				kind:  config.DiagnosticType,
			},
			{
				name: "integer outside the typed finite range",
				admission: `{
    "profile": "development",
    "overrides": {"global_pending": 18446744073709551616}
  }`,
				field: "admission.overrides.global_pending",
				kind:  config.DiagnosticBounds,
			},
			{
				name: "unbounded age alias",
				admission: `{
    "profile": "development",
    "overrides": {"workspace_max_pending_age": "unlimited"}
  }`,
				field: "admission.overrides.workspace_max_pending_age",
				kind:  config.DiagnosticValue,
			},
			{
				name: "workspace count exceeds deployment count",
				admission: `{
    "profile": "development",
    "overrides": {
      "global_pending": 4,
      "workspace_pending": 5
    }
  }`,
				field: "admission.overrides.workspace_pending",
				kind:  config.DiagnosticValue,
			},
			{
				name: "obsolete deployment count alias",
				admission: `{
    "profile": "development",
    "overrides": {"deployment_pending": 64}
  }`,
				field: "admission.overrides.deployment_pending",
				kind:  config.DiagnosticUnknown,
			},
		}
		for _, test := range failures {
			t.Run(test.name, func(t *testing.T) {
				launcher := new(recordingLauncher)
				_, err := validateBeforeMutation(withAdmission(t, test.admission), config.Prelaunch{}, launcher)
				requireDiagnostic(t, err, test.field, test.kind)
				requireNoMutations(t, launcher)
			})
		}
	})

	t.Run("illustration metadata and engine switches are not runtime configuration", func(t *testing.T) {
		cases := []struct {
			name  string
			raw   string
			field string
		}{
			{
				name: "synthetic label",
				raw: replaceExactly(t, bundledDevelopmentConfiguration,
					`  "schema_version": "1",`,
					`  "schema_version": "1",
  "label": "SYNTHETIC",`),
				field: "label",
			},
			{
				name: "illustrative resolved limits",
				raw: withAdmission(t, `{
    "profile": "development",
    "resolved_limits": {
      "global_pending": 32,
      "workspace_pending": 8,
      "workspace_max_pending_age": "2m"
    }
  }`),
				field: "admission.resolved_limits",
			},
			{
				name: "illustrative field spelling owner",
				raw: withAdmission(t, `{
    "profile": "development",
    "field_spelling_owner": "illustration only"
  }`),
				field: "admission.field_spelling_owner",
			},
			{
				name: "local distributed mode switch",
				raw: replaceExactly(t, bundledDevelopmentConfiguration,
					`  "schema_version": "1",`,
					`  "schema_version": "1",
  "mode": "local",`),
				field: "mode",
			},
			{
				name: "private autostart switch",
				raw: replaceExactly(t, bundledDevelopmentConfiguration,
					`  "schema_version": "1",`,
					`  "schema_version": "1",
  "autostart": true,`),
				field: "autostart",
			},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				launcher := new(recordingLauncher)
				_, err := validateBeforeMutation(test.raw, config.Prelaunch{}, launcher)
				requireDiagnostic(t, err, test.field, config.DiagnosticUnknown)
				requireNoMutations(t, launcher)
			})
		}
	})
}

type recordingLauncher struct {
	mutations []config.LifecycleRequest
}

func (l *recordingLauncher) record(requests []config.LifecycleRequest) {
	l.mutations = append(l.mutations, requests...)
}

func validateBeforeMutation(raw string, prelaunch config.Prelaunch, launcher *recordingLauncher) (config.Configuration, error) {
	loaded, err := config.Load(strings.NewReader(raw))
	if err != nil {
		return config.Configuration{}, err
	}
	resolved, err := config.Validate(loaded, prelaunch)
	if err != nil {
		return config.Configuration{}, err
	}
	launcher.record(prelaunch.Lifecycle)
	return resolved, nil
}

func requireConfiguration(t *testing.T, raw string) config.Configuration {
	t.Helper()
	resolved, err := validateBeforeMutation(raw, config.Prelaunch{}, new(recordingLauncher))
	if err != nil {
		t.Fatalf("validate configuration: %v", err)
	}
	return resolved
}

func requireDiagnostic(t *testing.T, err error, field string, kind config.DiagnosticKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("validation succeeded; want %s diagnostic for %s", kind, field)
	}
	var diagnostic *config.Diagnostic
	if !errors.As(err, &diagnostic) {
		t.Fatalf("error type = %T; want *config.Diagnostic", err)
	}
	if diagnostic.Field != field || diagnostic.Kind != kind {
		t.Fatalf("diagnostic = %#v; want field %q kind %q", diagnostic, field, kind)
	}
	if strings.Contains(err.Error(), confidentialFixtureValue) {
		t.Fatalf("diagnostic exposed credential fixture material")
	}
}

func requireNoMutations(t *testing.T, launcher *recordingLauncher) {
	t.Helper()
	if len(launcher.mutations) != 0 {
		t.Fatalf("launcher recorded mutations before validation completed: %#v", launcher.mutations)
	}
}

func requirePostgres(t *testing.T, got, want config.PostgresConfiguration) {
	t.Helper()
	if got != want {
		t.Fatalf("postgres configuration = %#v; want %#v", got, want)
	}
}

func requireTemporal(t *testing.T, got, want config.TemporalConfiguration) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("temporal configuration = %#v; want %#v", got, want)
	}
}

func requireArtifacts(t *testing.T, got, want config.ArtifactsConfiguration) {
	t.Helper()
	if got != want {
		t.Fatalf("artifacts configuration = %#v; want %#v", got, want)
	}
}

func withAdmission(t *testing.T, admission string) string {
	t.Helper()
	return replaceExactly(t, bundledDevelopmentConfiguration,
		`  "admission": {
    "profile": "development"
  },`,
		`  "admission": `+admission+`, `)
}

func managedConfiguration(t *testing.T) string {
	t.Helper()
	raw := bundledDevelopmentConfiguration
	if count := strings.Count(raw, `"ownership": "bundled"`); count != 3 {
		t.Fatalf("bundled fixture ownership count = %d; want 3", count)
	}
	raw = strings.ReplaceAll(raw, `"ownership": "bundled"`, `"ownership": "managed"`)
	replacements := [][2]string{
		{"secret://test/credential-value-must-not-leak/postgres-bundled", "secret://test/credential-value-must-not-leak/postgres-managed"},
		{"temporal.test.invalid:7233", "temporal.managed.test.invalid:7233"},
		{"secret://test/credential-value-must-not-leak/temporal-bundled", "secret://test/credential-value-must-not-leak/temporal-managed"},
		{"https://objects.test.invalid", "https://objects.managed.test.invalid"},
		{"secret://test/credential-value-must-not-leak/artifacts-bundled", "secret://test/credential-value-must-not-leak/artifacts-managed"},
	}
	for _, replacement := range replacements {
		raw = replaceExactly(t, raw, replacement[0], replacement[1])
	}
	return raw
}

func managedTemporalConfiguration(t *testing.T) string {
	t.Helper()
	raw := replaceExactly(t, bundledDevelopmentConfiguration,
		`"temporal": {
    "ownership": "bundled"`,
		`"temporal": {
    "ownership": "managed"`)
	raw = replaceExactly(t, raw, "temporal.test.invalid:7233", "temporal.managed.test.invalid:7233")
	return replaceExactly(t, raw,
		"secret://test/credential-value-must-not-leak/temporal-bundled",
		"secret://test/credential-value-must-not-leak/temporal-managed")
}

func replaceExactly(t *testing.T, input, old, replacement string) string {
	t.Helper()
	if count := strings.Count(input, old); count != 1 {
		t.Fatalf("fixture fragment %q occurs %d times; want exactly once", old, count)
	}
	return strings.Replace(input, old, replacement, 1)
}

func TestInstallerRejectsDestinationFileDirectoryConflict(t *testing.T) {
	source := t.TempDir()
	prefix := filepath.Join(t.TempDir(), "install")
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	writeJSON := func(name string, value any) string {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%x", sha256.Sum256(data))
	}
	files := []map[string]any{
		{"source": "NOTICE", "destination": "share/NOTICE", "role": "notice", "mode": "0644"},
		{"source": "pm", "destination": "bin/pm", "role": "cli", "mode": "0755"},
	}
	artifacts := make([]map[string]any, 0, len(files))
	for _, file := range files {
		data := []byte("disposable installer payload\n")
		name := file["source"].(string)
		if err := os.WriteFile(filepath.Join(source, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
		file["sha256"] = fmt.Sprintf("%x", sha256.Sum256(data))
		artifacts = append(artifacts, map[string]any{
			"role": file["role"], "path": name, "version": "fixture",
			"sha256": file["sha256"], "media_type": "application/octet-stream",
			"executable": file["role"] == "cli", "platforms": []string{"any"},
		})
	}
	releaseDigest := writeJSON("release.json", map[string]any{
		"schema_version": "pm.release-pair/v1", "release_id": "fixture",
		"backend_architecture": "postgresql-temporal-s3", "protocol_major": 1,
		"required_features": []string{}, "artifacts": artifacts,
	})
	manifest := map[string]any{
		"schema_version": "pm.install-manifest/v1", "release_id": "fixture",
		"arrangement": "client-only", "platform": "any", "files": files,
		"release_pair": map[string]any{"path": "release.json", "sha256": releaseDigest},
	}
	check := func() ([]byte, error) {
		t.Helper()
		writeJSON("manifest.json", manifest)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, script,
			"--manifest", filepath.Join(source, "manifest.json"),
			"--source", source, "--prefix", prefix, "--check").CombinedOutput()
	}
	if output, err := check(); err != nil {
		t.Fatalf("valid install plan was rejected: %v\n%s", err, output)
	}
	// Put the child first so refusal cannot depend on manifest ordering.
	files[0]["destination"] = "bin/NOTICE"
	files[1]["destination"] = "bin"
	output, err := check()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() <= 0 {
		t.Fatalf("conflicting install plan did not fail validation: %v\n%s", err, output)
	}
	if _, err := os.Stat(prefix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("check mode changed its destination: %v", err)
	}
}

func TestPackagingPathSchemas(t *testing.T) {
	for _, name := range []string{"release-pair.schema.json", "install-manifest.schema.json"} {
		t.Run(name, func(t *testing.T) {
			path, err := filepath.Abs(filepath.Join("..", "..", "packaging", name))
			if err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			document, err := jsonschema.UnmarshalJSON(file)
			if err != nil {
				t.Fatal(err)
			}
			compiler := jsonschema.NewCompiler()
			compiler.DefaultDraft(jsonschema.Draft2020)
			compiler.AssertFormat()
			compiler.UseLoader(jsonschema.SchemeURLLoader{})
			if err := compiler.AddResource(path, document); err != nil {
				t.Fatal(err)
			}
			schema, err := compiler.Compile(path + "#/$defs/relativePath")
			if err != nil {
				t.Fatal(err)
			}
			for _, valid := range []string{"bin/pm", ".pm/config.json"} {
				if err := schema.Validate(valid); err != nil {
					t.Errorf("valid relative path %q was rejected: %v", valid, err)
				}
			}
			for _, invalid := range []string{"/etc/pm", "bin/../pm", "./pm", "bin//pm", `bin\pm`} {
				if err := schema.Validate(invalid); err == nil {
					t.Errorf("unsafe or noncanonical path %q was accepted", invalid)
				}
			}
		})
	}
}

func TestOIDCClientIdentifierRemainsOpaque(t *testing.T) {
	clientID := " urn:registered:client/" + strings.Repeat("x", 300) + "?audience=pm "
	encodedID, err := json.Marshal(clientID)
	if err != nil {
		t.Fatal(err)
	}
	raw := replaceExactly(t, bundledDevelopmentConfiguration,
		`"client_id": "pm-server"`, `"client_id": `+string(encodedID))
	t.Run("typed_configuration", func(t *testing.T) {
		document, err := config.Load(strings.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := config.Validate(document, config.Prelaunch{})
		if err != nil {
			t.Fatalf("registered opaque client identifier was refused: %v", err)
		}
		if resolved.Identity().ClientID != clientID {
			t.Fatal("registered client identifier was changed")
		}
		for _, invalid := range []string{"pm\n", "pm\u0080"} {
			encoded, err := json.Marshal(invalid)
			if err != nil {
				t.Fatal(err)
			}
			invalidRaw := replaceExactly(t, bundledDevelopmentConfiguration,
				`"client_id": "pm-server"`, `"client_id": `+string(encoded))
			document, err := config.Load(strings.NewReader(invalidRaw))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := config.Validate(document, config.Prelaunch{}); err == nil {
				t.Errorf("non-VSCHAR client identifier %q was accepted", invalid)
			}
		}
	})
	t.Run("published_schema", func(t *testing.T) {
		path, err := filepath.Abs(filepath.Join("..", "..", "schemas", "server-config.schema.json"))
		if err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		document, err := jsonschema.UnmarshalJSON(file)
		if err != nil {
			t.Fatal(err)
		}
		compiler := jsonschema.NewCompiler()
		compiler.DefaultDraft(jsonschema.Draft2020)
		compiler.UseLoader(jsonschema.SchemeURLLoader{})
		if err := compiler.AddResource(path, document); err != nil {
			t.Fatal(err)
		}
		schema, err := compiler.Compile(path + "#/properties/identity/properties/client_id")
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(clientID); err != nil {
			t.Fatalf("registered opaque client identifier was refused: %v", err)
		}
		for _, invalid := range []string{"pm\n", "pm\u0080"} {
			if schema.Validate(invalid) == nil {
				t.Errorf("non-VSCHAR client identifier %q was accepted", invalid)
			}
		}
	})
}
