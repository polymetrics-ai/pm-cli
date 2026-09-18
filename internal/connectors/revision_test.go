package connectors_test

// This file owns the first-owned test entrypoint for issue #27
// (pm-github-direct-slice) cases S07 (pinned revision) and S08 (foreign cache):
// TestAtomicRevisionAndCacheScope/pinned_revision and
// TestAtomicRevisionAndCacheScope/foreign_cache.
//
// Production gap this file has to work around: package connectors deliberately
// exposes only immutable compiled bundles (LoadBundle, NewRegistry, Registry,
// Catalog, Connector, Operation). It has no cache, refresh, plan or
// invalidation type, and it carries no cache-scope identity. Those behaviors
// live elsewhere today: internal/api.CatalogCache with internal/api.CatalogBinding
// (client-side scoped catalog cache keyed by server instance, principal,
// workspace, connection and catalog revision) and internal/app.CatalogService
// (server-side catalog projection). This packet binds the cases to
// internal/connectors and forbids importing those packages here, so the atomic
// publication holder, the plan binding and the scoped cache are modelled by the
// clearly-labelled test-local harness in the "test harness" section below. The
// harness is not production API: when package connectors grows the real types,
// or when these cases move under internal/api / internal/app, these cases must
// bind to those owners instead.
//
// Every fixture and every expected value is authored in this file; no expected
// value is read back out of a response.

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/connectors"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestAtomicRevisionAndCacheScope(t *testing.T) {
	t.Run("pinned_revision", func(t *testing.T) {
		// Case S07-C01 (atomic_refresh). Catalog revision A and the refreshed
		// catalog revision B are two admitted immutable bundles; only the github
		// issues list read contract differs between them.
		githubV1 := mustBundle(t, githubBundleJSON(githubSemanticsRevisionV1, githubIssuesListResponsesV1))
		slack := mustBundle(t, slackBundleJSON())

		registryA := mustRegistry(t, githubV1, slack)
		catalogADigest := mustCatalogDigest(t, registryA.Catalog())
		listDigestA := mustOperationDigest(t, registryA, githubConnectorID, githubListOperationID)
		getDigestA := mustOperationDigest(t, registryA, githubConnectorID, githubGetOperationID)
		slackDigestA := mustOperationDigest(t, registryA, slackConnectorID, slackListOperationID)
		githubSemanticsA := mustConnector(t, registryA, githubConnectorID).SemanticsRevision
		slackSemanticsA := mustConnector(t, registryA, slackConnectorID).SemanticsRevision

		// Revision identity is content addressed: the same admitted content in a
		// different construction order keeps the revision.
		if reordered := mustRegistry(t, slack, githubV1); reordered.Revision() != registryA.Revision() {
			t.Fatalf("catalog revision depends on bundle construction order: %s != %s", reordered.Revision(), registryA.Revision())
		}
		if err := registryA.Revision().Validate(); err != nil {
			t.Fatalf("registry revision %q is not a canonical semantic digest: %v", registryA.Revision(), err)
		}
		if catalogRevision := registryA.Catalog().Revision; catalogRevision != registryA.Revision() {
			t.Fatalf("catalog revision %s does not match registry revision %s", catalogRevision, registryA.Revision())
		}
		// The registry projects the source-declared connector identities instead
		// of manufacturing its own.
		if githubSemanticsA != githubSemanticsRevisionV1 {
			t.Fatalf("github semantics revision = %s, want the admitted bundle identity %s", githubSemanticsA, githubSemanticsRevisionV1)
		}
		if slackSemanticsA != slackSemanticsRevision {
			t.Fatalf("slack semantics revision = %s, want the admitted bundle identity %s", slackSemanticsA, slackSemanticsRevision)
		}

		// The atomic refresh builds the next revision from changed content.
		githubV2 := mustBundle(t, githubBundleJSON(githubSemanticsRevisionV2, githubIssuesListResponsesV2))
		registryB := mustRegistry(t, githubV2, slack)
		if registryB.Revision() == registryA.Revision() {
			t.Fatalf("changed catalog content kept revision %s", registryA.Revision())
		}
		// Registry immutability: building the second registry must not change
		// anything the first registry already served.
		if got := mustCatalogDigest(t, registryA.Catalog()); got != catalogADigest {
			t.Fatalf("catalog of the first registry changed after a second registry was built: %s != %s", got, catalogADigest)
		}
		if got := mustOperationDigest(t, registryA, githubConnectorID, githubListOperationID); got != listDigestA {
			t.Fatalf("first registry issues list contract changed after a second registry was built: %s != %s", got, listDigestA)
		}
		if got := mustConnector(t, registryB, githubConnectorID).SemanticsRevision; got == githubSemanticsA {
			t.Fatalf("refreshed github connector kept semantics revision %s", got)
		} else if got != githubSemanticsRevisionV2 {
			t.Fatalf("refreshed github semantics revision = %s, want %s", got, githubSemanticsRevisionV2)
		}
		if got := mustConnector(t, registryB, slackConnectorID).SemanticsRevision; got != slackSemanticsA {
			t.Fatalf("unrelated slack connector semantics revision changed across the refresh: %s != %s", got, slackSemanticsA)
		}
		listDigestB := mustOperationDigest(t, registryB, githubConnectorID, githubListOperationID)
		if listDigestB == listDigestA {
			t.Fatalf("refreshed github issues list contract kept digest %s", listDigestA)
		}
		if getDigestB := mustOperationDigest(t, registryB, githubConnectorID, githubGetOperationID); getDigestB != getDigestA {
			t.Fatalf("github issues get contract changed across the refresh: %s != %s", getDigestB, getDigestA)
		}
		if slackDigestB := mustOperationDigest(t, registryB, slackConnectorID, slackListOperationID); slackDigestB != slackDigestA {
			t.Fatalf("slack conversations list contract changed across the refresh: %s != %s", slackDigestB, slackDigestA)
		}

		// Registry immutability: a caller that mutates a returned catalog snapshot
		// must not be able to reach into stores that are still served, and the
		// mutated identities must not become resolvable.
		catalogBDigest := mustCatalogDigest(t, registryB.Catalog())
		mutated := registryB.Catalog()
		for index := range mutated.Connectors {
			if mutated.Connectors[index].ConnectorID != githubConnectorID {
				continue
			}
			mutated.Connectors[index].ConnectorID = "mutated"
			for operation := range mutated.Connectors[index].Operations {
				mutated.Connectors[index].Operations[operation].Contract.OperationID = "mutated"
				mutated.Connectors[index].Operations[operation].Contract.Transport.Path = "/mutated"
			}
		}
		if _, err := registryB.Connector("mutated"); !errors.Is(err, connectors.ErrConnectorNotFound) {
			t.Fatalf("mutated connector identity resolved in the registry: %v", err)
		}
		if _, err := registryB.Operation("mutated", "mutated"); !errors.Is(err, connectors.ErrConnectorNotFound) {
			t.Fatalf("mutated connector identity resolved in the registry: %v", err)
		}
		if _, err := registryB.Operation(githubConnectorID, "mutated"); !errors.Is(err, connectors.ErrOperationNotFound) {
			t.Fatalf("mutated operation identity resolved under a known connector: %v", err)
		}
		githubAfterMutation, err := registryB.Operation(githubConnectorID, githubListOperationID)
		if err != nil {
			t.Fatalf("registry lost the github issues list operation after a caller mutated a returned catalog snapshot: %v", err)
		}
		if githubAfterMutation.Contract.Transport.Path != githubIssuesListPath {
			t.Fatalf("returned catalog snapshot shares storage with the registry: path = %s, want %s", githubAfterMutation.Contract.Transport.Path, githubIssuesListPath)
		}
		if got := mustCatalogDigest(t, registryB.Catalog()); got != catalogBDigest {
			t.Fatalf("registry served a catalog snapshot that shares storage with a mutated copy: %s != %s", got, catalogBDigest)
		}

		// Atomic refresh: the live publication swaps only after the whole next
		// revision is admitted and compiled.
		live := &catalogSource{}
		if err := live.attempt(func() (*connectors.Registry, error) { return registryA, nil }); err != nil {
			t.Fatalf("initial catalog publication failed: %v", err)
		}
		publicationA := live.snapshot()
		if publicationA.revision != registryA.Revision() {
			t.Fatalf("published revision = %s, want %s", publicationA.revision, registryA.Revision())
		}
		pinnedRunPlan := mustPlan(t, publicationA, "ada-pinned-run", githubConnectorID, githubListOperationID)
		mutablePlan := mustPlan(t, publicationA, "ada-mutable-plan", githubConnectorID, githubListOperationID)
		siblingPlan := mustPlan(t, publicationA, "ada-sibling-plan", githubConnectorID, githubGetOperationID)
		slackPlan := mustPlan(t, publicationA, "ada-slack-plan", slackConnectorID, slackListOperationID)

		// Case S07-C02 (failed_refresh). A refresh that is refused must leave the
		// prior publication untouched and complete. The live publication is still
		// revision A here, and every refused catalog below carries the changed
		// github issues list contract, so no part of it may become visible.
		wantInventory := []string{"github/issues/get-for-repo", "github/issues/list-for-repo", "slack/conversations/list"}
		if got := catalogOperationKeys(live.snapshot().catalog()); !slices.Equal(got, wantInventory) {
			t.Fatalf("live operation inventory before the failed refreshes = %v, want %v", got, wantInventory)
		}
		if got := unavailableOperationIDs(mustConnector(t, live.snapshot().registry, githubConnectorID)); !slices.Equal(got, []string{githubUnavailableOperationID}) {
			t.Fatalf("live unavailable inventory before the failed refreshes = %v, want [%s]", got, githubUnavailableOperationID)
		}

		refreshFailureCases := []struct {
			name  string
			build func() (*connectors.Registry, error)
		}{
			{
				name: "refused_bundle_document",
				build: func() (*connectors.Registry, error) {
					// The same sentry connector is admitted when its operation
					// inventory is unambiguous, so the refusal below is caused by
					// the duplicated operation identity alone.
					if _, err := connectors.LoadBundle(strings.NewReader(sentryBundleJSON(false))); err != nil {
						t.Fatalf("admitted sentry fixture was refused: %v", err)
					}
					githubNext := mustBundle(t, githubBundleJSON(githubSemanticsRevisionV2, githubIssuesListResponsesV2))
					slackNext := mustBundle(t, slackBundleJSON())
					sentryNext, err := connectors.LoadBundle(strings.NewReader(sentryBundleJSON(true)))
					if err != nil {
						return nil, err
					}
					return connectors.NewRegistry(githubNext, slackNext, sentryNext)
				},
			},
			{
				name: "refused_connector_identity",
				build: func() (*connectors.Registry, error) {
					// The replacement bundle is admitted on its own; only the
					// duplicated connector identity makes this refresh fail.
					githubReplacement := mustBundle(t, githubReplacementBundleJSON())
					if _, err := connectors.NewRegistry(githubReplacement); err != nil {
						t.Fatalf("admitted github replacement fixture was refused alone: %v", err)
					}
					githubNext := mustBundle(t, githubBundleJSON(githubSemanticsRevisionV2, githubIssuesListResponsesV2))
					slackNext := mustBundle(t, slackBundleJSON())
					return connectors.NewRegistry(githubNext, slackNext, githubReplacement)
				},
			},
		}
		for _, failure := range refreshFailureCases {
			revisionBefore := live.snapshot().revision
			digestBefore := mustCatalogDigest(t, live.snapshot().catalog())

			err := live.attempt(failure.build)
			if !errors.Is(err, connectors.ErrInvalidBundle) {
				t.Fatalf("%s: refresh error = %v, want a refused connector bundle", failure.name, err)
			}
			// Every connector, every executable operation, every unavailable
			// operation and the revision itself survive the refused refresh.
			if after := live.snapshot(); after.revision != revisionBefore {
				t.Fatalf("%s: live revision moved to %s after a refused refresh, want %s", failure.name, after.revision, revisionBefore)
			}
			if got := mustCatalogDigest(t, live.snapshot().catalog()); got != digestBefore {
				t.Fatalf("%s: retained catalog changed after a refused refresh: %s != %s", failure.name, got, digestBefore)
			}
			if got := catalogOperationKeys(live.snapshot().catalog()); !slices.Equal(got, wantInventory) {
				t.Fatalf("%s: retained operation inventory = %v, want %v", failure.name, got, wantInventory)
			}
			if got := unavailableOperationIDs(mustConnector(t, live.snapshot().registry, githubConnectorID)); !slices.Equal(got, []string{githubUnavailableOperationID}) {
				t.Fatalf("%s: retained unavailable inventory = %v, want [%s]", failure.name, got, githubUnavailableOperationID)
			}
			if got := mustOperationDigest(t, live.snapshot().registry, githubConnectorID, githubListOperationID); got != listDigestA {
				t.Fatalf("%s: github issues list contract = %s after the refused refresh, want the admitted %s", failure.name, got, listDigestA)
			}
			// Content from the refused catalog is not partially visible.
			if _, err := live.snapshot().registry.Operation(githubConnectorID, githubReplacementOperationID); !errors.Is(err, connectors.ErrOperationNotFound) {
				t.Fatalf("%s: refused refresh content is visible in the live catalog: %v", failure.name, err)
			}
			if _, err := live.snapshot().registry.Connector(sentryConnectorID); !errors.Is(err, connectors.ErrConnectorNotFound) {
				t.Fatalf("%s: connector from the refused catalog is visible: %v", failure.name, err)
			}
		}

		// The atomic refresh publishes the next catalog revision.
		if err := live.attempt(func() (*connectors.Registry, error) { return registryB, nil }); err != nil {
			t.Fatalf("atomic refresh to the next catalog revision failed: %v", err)
		}
		if live.snapshot().revision != registryB.Revision() {
			t.Fatalf("live revision after the refresh = %s, want %s", live.snapshot().revision, registryB.Revision())
		}

		// The accepted run keeps revision A and the exact operation contract it
		// was admitted with, even though the live catalog no longer serves it.
		pinnedRevision, pinnedOperation := mustAcceptedOperation(t, pinnedRunPlan)
		if pinnedRevision != registryA.Revision() {
			t.Fatalf("accepted run moved to revision %s, want %s", pinnedRevision, registryA.Revision())
		}
		if got := mustContractDigest(t, pinnedOperation); got != listDigestA || got != pinnedRunPlan.digest {
			t.Fatalf("accepted run operation digest = %s, want the accepted %s", got, listDigestA)
		}
		if got := mustOperationDigest(t, live.snapshot().registry, githubConnectorID, githubListOperationID); got == pinnedRunPlan.digest {
			t.Fatalf("refresh did not change the pinned operation, so the pin proves nothing: %s", got)
		}

		// Only a mutable plan whose bound operation actually changed is
		// invalidated; it was valid against its accepted revision.
		if err := mutablePlan.revalidate(publicationA); err != nil {
			t.Fatalf("mutable plan %q was already invalid against its accepted revision: %v", mutablePlan.name, err)
		}
		if err := mutablePlan.revalidate(live.snapshot()); !errors.Is(err, errPlanInvalidated) {
			t.Fatalf("mutable plan %q was not invalidated after its bound operation changed: %v", mutablePlan.name, err)
		}
		// Unrelated plans stay valid: their bound contracts are byte-identical
		// across the refresh and still resolve in the live catalog. The github
		// connector's own semantics revision did change, so this only holds
		// because each plan binds the operation contract, not the connector.
		for _, plan := range []planBinding{siblingPlan, slackPlan} {
			if err := plan.revalidate(live.snapshot()); err != nil {
				t.Fatalf("unrelated plan %q was invalidated by the refresh: %v", plan.name, err)
			}
			operation, err := live.snapshot().operation(plan.connectorID, plan.operationID)
			if err != nil {
				t.Fatalf("unrelated plan %q no longer resolves in the live catalog: %v", plan.name, err)
			}
			if got := mustContractDigest(t, operation.Contract); got != plan.digest {
				t.Fatalf("unrelated plan %q bound digest %s but the live contract is %s", plan.name, plan.digest, got)
			}
		}
	})

	t.Run("foreign_cache", func(t *testing.T) {
		// Cases S08-C01..C04. A scoped catalog cache entry belongs to one server
		// instance, principal, workspace, connection and catalog revision.
		registry := mustRegistry(t, mustBundle(t, githubBundleJSON(githubSemanticsRevisionV1, githubIssuesListResponsesV1)), mustBundle(t, slackBundleJSON()))
		refreshed := mustRegistry(t, mustBundle(t, githubBundleJSON(githubSemanticsRevisionV2, githubIssuesListResponsesV2)), mustBundle(t, slackBundleJSON()))
		if refreshed.Revision() == registry.Revision() {
			t.Fatalf("refreshed catalog kept revision %s", registry.Revision())
		}
		liveCatalog := registry.Catalog()
		liveListDigest := mustOperationDigest(t, registry, githubConnectorID, githubListOperationID)

		owner := cacheBinding{
			serverInstanceID: "server-a",
			principal:        cachePrincipal{kind: "user", id: "ada"},
			scope:            contract.Scope{OrgID: "org-1", WorkspaceID: "workspace-a"},
			connectionID:     "conn-github",
			catalogRevision:  registry.Revision(),
		}
		cache := newScopedCatalogCache()
		cache.store(owner, liveCatalog)

		// The exact binding is served, so the refusals below are about identity
		// and not about an empty cache.
		served, err := cache.load(owner)
		if err != nil {
			t.Fatalf("owner binding was refused: %v", err)
		}
		if served.Revision != registry.Revision() {
			t.Fatalf("served catalog revision = %s, want %s", served.Revision, registry.Revision())
		}
		if len(served.Connectors) != len(liveCatalog.Connectors) {
			t.Fatalf("served catalog carries %d connectors, want %d", len(served.Connectors), len(liveCatalog.Connectors))
		}
		servedOperation, err := catalogOperation(served, githubConnectorID, githubListOperationID)
		if err != nil {
			t.Fatalf("served catalog is incomplete: %v", err)
		}
		if got := mustContractDigest(t, servedOperation.Contract); got != liveListDigest {
			t.Fatalf("served catalog operation digest = %s, want %s", got, liveListDigest)
		}

		ownerWith := func(change func(binding *cacheBinding)) cacheBinding {
			binding := owner
			change(&binding)
			return binding
		}
		foreignBindings := []struct {
			name    string
			element string
			binding cacheBinding
		}{
			{"foreign_server", "server", ownerWith(func(binding *cacheBinding) { binding.serverInstanceID = "server-b" })},
			{"foreign_principal", "principal", ownerWith(func(binding *cacheBinding) {
				binding.principal = cachePrincipal{kind: "service_account", id: "gh-actions"}
			})},
			{"foreign_org", "org", ownerWith(func(binding *cacheBinding) { binding.scope.OrgID = "org-2" })},
			{"foreign_workspace", "workspace", ownerWith(func(binding *cacheBinding) { binding.scope.WorkspaceID = "workspace-b" })},
			{"foreign_connection", "connection", ownerWith(func(binding *cacheBinding) { binding.connectionID = "conn-github-read-only" })},
			{"stale_catalog_revision", "catalog_revision", ownerWith(func(binding *cacheBinding) { binding.catalogRevision = refreshed.Revision() })},
		}
		for _, foreign := range foreignBindings {
			differing := differingIdentityElements(owner, foreign.binding)
			if !slices.Equal(differing, []string{foreign.element}) {
				t.Fatalf("%s: fixture varies %v, want exactly [%s]", foreign.name, differing, foreign.element)
			}
			refused, err := cache.load(foreign.binding)
			if !errors.Is(err, errForeignCacheBinding) {
				t.Fatalf("%s: foreign binding was served with revision %s and error %v", foreign.name, refused.Revision, err)
			}
			if refused.Revision != "" || len(refused.Connectors) != 0 {
				t.Fatalf("%s: foreign binding exposed catalog content: revision %q with %d connectors", foreign.name, refused.Revision, len(refused.Connectors))
			}
			if _, err := catalogOperation(refused, githubConnectorID, githubListOperationID); err == nil {
				t.Fatalf("%s: foreign refusal still exposed the github issues list operation", foreign.name)
			}
			// A refused foreign lookup must not evict or damage the owner entry.
			afterRefusal, err := cache.load(owner)
			if err != nil {
				t.Fatalf("%s: owner entry was lost after a foreign binding was refused: %v", foreign.name, err)
			}
			if afterRefusal.Revision != liveCatalog.Revision {
				t.Fatalf("%s: owner entry revision = %s after a foreign refusal, want %s", foreign.name, afterRefusal.Revision, liveCatalog.Revision)
			}
		}
	})
}

// Test identity and fixture vocabulary -------------------------------------------------

const (
	githubConnectorID = "github"
	slackConnectorID  = "slack"
	sentryConnectorID = "sentry"

	githubListOperationID        = "issues/list-for-repo"
	githubGetOperationID         = "issues/get-for-repo"
	githubUnavailableOperationID = "issues/update-for-repo"
	githubReplacementOperationID = "teams/list-for-org"
	slackListOperationID         = "conversations/list"
	sentryListOperationID        = "projects/list"
	sentryUnavailableOperationID = "projects/create"

	githubIssuesListPath = "/repos/{owner}/{repo}/issues"
)

// Opaque source-declared connector identities. The connectors package validates
// only their canonical sha256 shape; the values are authored here so that the
// two catalog revisions under test carry distinct source identities.
const (
	githubSourceDigest                 = "sha256:1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a"
	githubSemanticsRevisionV1          = "sha256:2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b"
	githubSemanticsRevisionV2          = "sha256:3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c"
	githubReplacementSourceDigest      = "sha256:4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d"
	githubReplacementSemanticsRevision = "sha256:5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e"
	slackSourceDigest                  = "sha256:6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f"
	slackSemanticsRevision             = "sha256:7070707070707070707070707070707070707070707070707070707070707070"
	sentrySourceDigest                 = "sha256:8181818181818181818181818181818181818181818181818181818181818181"
	sentrySemanticsRevision            = "sha256:9292929292929292929292929292929292929292929292929292929292929292"
)

// Fixtures -----------------------------------------------------------------------------

// readLaneEligibilityJSON is the exact seven-lane disposition of an admitted
// direct read operation.
const readLaneEligibilityJSON = `
      {"lane":"direct_read","status":"eligible","reason":"source-declared-read"},
      {"lane":"direct_write","status":"unsupported","reason":"source-does-not-declare-write"},
      {"lane":"etl","status":"unproven","reason":"runtime-proof-not-yet-established"},
      {"lane":"reverse_etl","status":"not_applicable","reason":"read-operation-has-no-destination-write"},
      {"lane":"binary_download","status":"not_applicable","reason":"response-is-not-binary"},
      {"lane":"binary_upload","status":"not_applicable","reason":"request-is-not-binary"},
      {"lane":"sync_transport","status":"blocked","reason":"sync-semantics-are-not-declared"}`

// refusedLaneEligibilityJSON is the same lane set for an operation that the
// source did not admit; no lane may stay eligible.
const refusedLaneEligibilityJSON = `
      {"lane":"direct_read","status":"unproven","reason":"source-declared-read-without-proof"},
      {"lane":"direct_write","status":"unsupported","reason":"source-does-not-declare-write"},
      {"lane":"etl","status":"unproven","reason":"runtime-proof-not-yet-established"},
      {"lane":"reverse_etl","status":"not_applicable","reason":"read-operation-has-no-destination-write"},
      {"lane":"binary_download","status":"not_applicable","reason":"response-is-not-binary"},
      {"lane":"binary_upload","status":"not_applicable","reason":"request-is-not-binary"},
      {"lane":"sync_transport","status":"blocked","reason":"sync-semantics-are-not-declared"}`

const (
	githubIssuesListResponseJSON = `{
      "status":"200",
      "no_content":false,
      "media_type":"application/json",
      "schema":{"type":"array","items":{"type":"object","required":["id","number","title"],"properties":{"id":{"type":"integer"},"number":{"type":"integer"},"title":{"type":"string"},"state":{"type":"string"}}}},
      "source_refs":[{"source_id":"github-rest-api","pointer":"#/paths/~1repos~1{owner}~1{repo}~1issues/get/responses/200"}]
    }`

	githubIssuesErrorResponseJSON = `{
      "status":"default",
      "no_content":false,
      "media_type":"application/json",
      "schema":{"type":"object","required":["message"],"properties":{"message":{"type":"string"},"documentation_url":{"type":"string"}}},
      "source_refs":[{"source_id":"github-rest-api","pointer":"#/paths/~1repos~1{owner}~1{repo}~1issues/get/responses/default"}]
    }`

	githubIssueGetResponseJSON = `{
      "status":"200",
      "no_content":false,
      "media_type":"application/json",
      "schema":{"type":"object","required":["id","number","title"],"properties":{"id":{"type":"integer"},"number":{"type":"integer"},"title":{"type":"string"},"state":{"type":"string"}}},
      "source_refs":[{"source_id":"github-rest-api","pointer":"#/paths/~1repos~1{owner}~1{repo}~1issues~1{issue_number}/get/responses/200"}]
    }`

	githubTeamsListResponseJSON = `{
      "status":"200",
      "no_content":false,
      "media_type":"application/json",
      "schema":{"type":"array","items":{"type":"object","required":["id","name"],"properties":{"id":{"type":"integer"},"name":{"type":"string"}}}},
      "source_refs":[{"source_id":"github-rest-api","pointer":"#/paths/~1orgs~1{org}~1teams/get/responses/200"}]
    }`

	slackConversationsListResponseJSON = `{
      "status":"200",
      "no_content":false,
      "media_type":"application/json",
      "schema":{"type":"object","required":["ok","channels"],"properties":{"ok":{"type":"bool"},"channels":{"type":"array","items":{"type":"object","required":["id","name"],"properties":{"id":{"type":"string"},"name":{"type":"string"}}}}}},
      "source_refs":[{"source_id":"slack-web-api","pointer":"#/paths/~1api~1conversations.list/get/responses/200"}]
    }`

	sentryProjectsListResponseJSON = `{
      "status":"200",
      "no_content":false,
      "media_type":"application/json",
      "schema":{"type":"array","items":{"type":"object","required":["id","slug"],"properties":{"id":{"type":"string"},"slug":{"type":"string"}}}},
      "source_refs":[{"source_id":"sentry-api","pointer":"#/paths/~1api~10~1organizations~1{organization_slug}~1projects~1/get/responses/200"}]
    }`
)

// githubIssuesListResponsesV1 and V2 are the only admitted content difference
// between catalog revision A and B: the refreshed source admitted a documented
// error response variant beside the existing 200 variant.
const (
	githubIssuesListResponsesV1 = githubIssuesListResponseJSON
	githubIssuesListResponsesV2 = githubIssuesListResponseJSON + ",\n    " + githubIssuesErrorResponseJSON
)

type readOperationFixture struct {
	operationID   string
	sourceID      string
	sourcePointer string
	path          string
	inputs        []string
	responses     string
}

func (fixture readOperationFixture) json() string {
	return fmt.Sprintf(`{
    "schema_version":"pm.operation/v1",
    "operation_id":%q,
    "source_refs":[{"source_id":%q,"pointer":%q}],
    "transport":{"kind":"http","method":"GET","path":%q},
    "inputs":[%s],
    "request_variants":[],
    "responses":[%s],
    "capability_requirements":[
      {"capability":"field-binding","foundation":{"id":"FND-03","version":"1.0.0"}},
      {"capability":"typed-values","foundation":{"id":"FND-03","version":"1.0.0"}}
    ],
    "lane_eligibility":[%s]
  }`,
		fixture.operationID,
		fixture.sourceID,
		fixture.sourcePointer,
		fixture.path,
		strings.Join(fixture.inputs, ",\n      "),
		fixture.responses,
		readLaneEligibilityJSON,
	)
}

type bundleFixture struct {
	connectorID       string
	sourceDigest      string
	semanticsRevision string
	operations        []string
	unavailable       []string
}

func (fixture bundleFixture) json() string {
	return fmt.Sprintf(`{
    "schema_version":"pm.bundle/v1",
    "connector_id":%q,
    "source_digest":%q,
    "semantics_revision":%q,
    "operations":[%s],
    "commands":[],
    "unavailable_operations":[%s]
  }`,
		fixture.connectorID,
		fixture.sourceDigest,
		fixture.semanticsRevision,
		strings.Join(fixture.operations, ",\n      "),
		strings.Join(fixture.unavailable, ",\n      "),
	)
}

func pathInputJSON(fieldID, wireName string) string {
	return fmt.Sprintf(`{"field_id":%q,"wire_name":%q,"location":"path","json_pointer":%q,"required":true,"nullable":false,"style":"simple","explode":false,"allow_reserved":false,"sensitivity":"public","schema":{"type":"string"}}`,
		fieldID, wireName, "/path/"+fieldID)
}

func queryInputJSON(fieldID, wireName string) string {
	return fmt.Sprintf(`{"field_id":%q,"wire_name":%q,"location":"query","json_pointer":%q,"required":false,"nullable":false,"style":"form","explode":true,"allow_reserved":false,"sensitivity":"public","schema":{"type":"integer"}}`,
		fieldID, wireName, "/query/"+fieldID)
}

func authorizationInputJSON() string {
	return `{"field_id":"authorization","wire_name":"Authorization","location":"header","json_pointer":"/headers/Authorization","required":true,"nullable":false,"style":"simple","explode":false,"allow_reserved":false,"sensitivity":"secret","schema":{"type":"string"}}`
}

func unavailableOperationJSON(sourceID, sourcePointer, operationID string) string {
	return fmt.Sprintf(`{
      "operation_id":%q,
      "source_refs":[{"source_id":%q,"pointer":%q}],
      "diagnostic_codes":["lane-not-admitted"],
      "lane_eligibility":[%s]
    }`, operationID, sourceID, sourcePointer, refusedLaneEligibilityJSON)
}

// githubBundleJSON builds the github connector as it was admitted at catalog
// revision A (githubSemanticsRevisionV1) or revision B
// (githubSemanticsRevisionV2); listResponses selects the admitted read variants.
func githubBundleJSON(semanticsRevision, listResponses string) string {
	return bundleFixture{
		connectorID:       githubConnectorID,
		sourceDigest:      githubSourceDigest,
		semanticsRevision: semanticsRevision,
		operations: []string{
			readOperationFixture{
				operationID:   githubListOperationID,
				sourceID:      "github-rest-api",
				sourcePointer: "#/paths/~1repos~1{owner}~1{repo}~1issues/get",
				path:          githubIssuesListPath,
				inputs: []string{
					pathInputJSON("owner", "owner"),
					pathInputJSON("repo", "repo"),
					queryInputJSON("per-page", "per_page"),
					authorizationInputJSON(),
				},
				responses: listResponses,
			}.json(),
			readOperationFixture{
				operationID:   githubGetOperationID,
				sourceID:      "github-rest-api",
				sourcePointer: "#/paths/~1repos~1{owner}~1{repo}~1issues~1{issue_number}/get",
				path:          "/repos/{owner}/{repo}/issues/{issue_number}",
				inputs: []string{
					pathInputJSON("owner", "owner"),
					pathInputJSON("repo", "repo"),
					pathInputJSON("issue-number", "issue_number"),
					authorizationInputJSON(),
				},
				responses: githubIssueGetResponseJSON,
			}.json(),
		},
		unavailable: []string{
			unavailableOperationJSON("github-rest-api", "#/paths/~1repos~1{owner}~1{repo}~1issues~1{issue_number}/patch", githubUnavailableOperationID),
		},
	}.json()
}

func slackBundleJSON() string {
	return bundleFixture{
		connectorID:       slackConnectorID,
		sourceDigest:      slackSourceDigest,
		semanticsRevision: slackSemanticsRevision,
		operations: []string{
			readOperationFixture{
				operationID:   slackListOperationID,
				sourceID:      "slack-web-api",
				sourcePointer: "#/paths/~1api~1conversations.list/get",
				path:          "/api/conversations.list",
				inputs: []string{
					queryInputJSON("limit", "limit"),
					authorizationInputJSON(),
				},
				responses: slackConversationsListResponseJSON,
			}.json(),
		},
	}.json()
}

// sentryBundleJSON builds the connector of a refused refresh. The duplicate
// inventory variant reuses projects/list as an unavailable operation identity,
// which the bundle validator refuses across inventories.
func sentryBundleJSON(duplicateInventory bool) string {
	unavailableID := sentryUnavailableOperationID
	if duplicateInventory {
		unavailableID = sentryListOperationID
	}
	return bundleFixture{
		connectorID:       sentryConnectorID,
		sourceDigest:      sentrySourceDigest,
		semanticsRevision: sentrySemanticsRevision,
		operations: []string{
			readOperationFixture{
				operationID:   sentryListOperationID,
				sourceID:      "sentry-api",
				sourcePointer: "#/paths/~1api~10~1organizations~1{organization_slug}~1projects~1/get",
				path:          "/api/0/organizations/{organization_slug}/projects/",
				inputs: []string{
					pathInputJSON("organization", "organization_slug"),
					authorizationInputJSON(),
				},
				responses: sentryProjectsListResponseJSON,
			}.json(),
		},
		unavailable: []string{
			unavailableOperationJSON("sentry-api", "#/paths/~1api~10~1organizations~1{organization_slug}~1projects~1/post", unavailableID),
		},
	}.json()
}

// githubReplacementBundleJSON declares the already installed connector
// identity with a different operation inventory; the registry refuses the
// duplicated identity, so a refresh containing it must not publish.
func githubReplacementBundleJSON() string {
	return bundleFixture{
		connectorID:       githubConnectorID,
		sourceDigest:      githubReplacementSourceDigest,
		semanticsRevision: githubReplacementSemanticsRevision,
		operations: []string{
			readOperationFixture{
				operationID:   githubReplacementOperationID,
				sourceID:      "github-rest-api",
				sourcePointer: "#/paths/~1orgs~1{org}~1teams/get",
				path:          "/orgs/{org}/teams",
				inputs: []string{
					pathInputJSON("org", "org"),
					authorizationInputJSON(),
				},
				responses: githubTeamsListResponseJSON,
			}.json(),
		},
	}.json()
}

// Test harness (not production API) ----------------------------------------------------
//
// The types below model the required behaviors with the smallest surface that
// the connectors and contract packages can express. They are deliberately not
// exported and not production code: see the production-gap note at the top of
// this file.

var (
	errPlanInvalidated     = errors.New("plan is invalidated by the refreshed catalog revision")
	errForeignCacheBinding = errors.New("catalog cache has no entry for this complete binding")
)

// catalogPublication is one immutable published catalog revision.
type catalogPublication struct {
	revision contract.SemanticDigest
	registry *connectors.Registry
}

func (publication *catalogPublication) catalog() connectors.Catalog {
	return publication.registry.Catalog()
}

func (publication *catalogPublication) operation(connectorID, operationID string) (connectors.Operation, error) {
	operation, err := publication.registry.Operation(connectorID, operationID)
	if err != nil {
		return connectors.Operation{}, fmt.Errorf("catalog revision %s: %w", publication.revision, err)
	}
	return operation, nil
}

// catalogSource is the atomically published catalog holder: a refresh is
// admitted only when the whole next catalog is built, and the live pointer is
// swapped once, so a refused refresh cannot partially publish.
type catalogSource struct {
	current *catalogPublication
}

func (source *catalogSource) attempt(build func() (*connectors.Registry, error)) error {
	next, err := build()
	if err != nil {
		return err
	}
	source.current = &catalogPublication{revision: next.Revision(), registry: next}
	return nil
}

func (source *catalogSource) snapshot() *catalogPublication {
	return source.current
}

// planBinding is the catalog binding a stored plan holds: the publication it
// was accepted against, the bound operation identity, and the canonical digest
// of that operation contract.
type planBinding struct {
	name        string
	connectorID string
	operationID string
	digest      contract.SemanticDigest
	publication *catalogPublication
}

// revalidate reports whether a mutable plan may still execute against the
// currently published catalog. Only a changed bound operation contract
// invalidates it.
func (plan planBinding) revalidate(live *catalogPublication) error {
	operation, err := live.operation(plan.connectorID, plan.operationID)
	if err != nil {
		return fmt.Errorf("%w: plan %q: %v", errPlanInvalidated, plan.name, err)
	}
	digest, err := operationDigest(operation.Contract)
	if err != nil {
		return err
	}
	if digest != plan.digest {
		return fmt.Errorf("%w: plan %q bound %s/%s as %s but the live contract is %s",
			errPlanInvalidated, plan.name, plan.connectorID, plan.operationID, plan.digest, digest)
	}
	return nil
}

// acceptedOperation returns the revision and the operation contract a pinned
// plan keeps executing; it never consults the live catalog.
func (plan planBinding) acceptedOperation() (contract.SemanticDigest, contract.Operation, error) {
	operation, err := plan.publication.operation(plan.connectorID, plan.operationID)
	if err != nil {
		return "", contract.Operation{}, err
	}
	return plan.publication.revision, operation.Contract, nil
}

// cachePrincipal is the principal identity part of a cache binding.
type cachePrincipal struct {
	kind string
	id   string
}

// cacheBinding is the complete identity a client cache entry is stored under,
// modelled on the production binding (internal/api.CatalogBinding): server
// instance, principal, workspace scope, connection and catalog revision.
type cacheBinding struct {
	serverInstanceID string
	principal        cachePrincipal
	scope            contract.Scope
	connectionID     string
	catalogRevision  contract.SemanticDigest
}

// scopedCatalogCache is the client-side scoped catalog cache: an entry is
// stored and served only under its complete binding.
type scopedCatalogCache struct {
	entries map[cacheBinding]connectors.Catalog
}

func newScopedCatalogCache() *scopedCatalogCache {
	return &scopedCatalogCache{entries: make(map[cacheBinding]connectors.Catalog)}
}

func (cache *scopedCatalogCache) store(binding cacheBinding, catalog connectors.Catalog) {
	cache.entries[binding] = catalog
}

func (cache *scopedCatalogCache) load(binding cacheBinding) (connectors.Catalog, error) {
	catalog, present := cache.entries[binding]
	if !present {
		return connectors.Catalog{}, fmt.Errorf("%w: server %q, principal %s/%s, org %s, workspace %s, connection %q, catalog revision %s",
			errForeignCacheBinding, binding.serverInstanceID, binding.principal.kind, binding.principal.id,
			binding.scope.OrgID, binding.scope.WorkspaceID, binding.connectionID, binding.catalogRevision)
	}
	return catalog, nil
}

// differingIdentityElements names every binding element that differs between
// two bindings; each foreign-cache fixture row must vary exactly one.
func differingIdentityElements(owner, candidate cacheBinding) []string {
	var differing []string
	if owner.serverInstanceID != candidate.serverInstanceID {
		differing = append(differing, "server")
	}
	if owner.principal != candidate.principal {
		differing = append(differing, "principal")
	}
	if owner.scope.OrgID != candidate.scope.OrgID {
		differing = append(differing, "org")
	}
	if owner.scope.WorkspaceID != candidate.scope.WorkspaceID {
		differing = append(differing, "workspace")
	}
	if owner.connectionID != candidate.connectionID {
		differing = append(differing, "connection")
	}
	if owner.catalogRevision != candidate.catalogRevision {
		differing = append(differing, "catalog_revision")
	}
	return differing
}

// Helpers ------------------------------------------------------------------------------

func mustBundle(t *testing.T, document string) connectors.Bundle {
	t.Helper()
	bundle, err := connectors.LoadBundle(strings.NewReader(document))
	if err != nil {
		t.Fatalf("LoadBundle(fixture): %v", err)
	}
	return bundle
}

func mustRegistry(t *testing.T, bundles ...connectors.Bundle) *connectors.Registry {
	t.Helper()
	registry, err := connectors.NewRegistry(bundles...)
	if err != nil {
		t.Fatalf("NewRegistry(fixture): %v", err)
	}
	return registry
}

func mustConnector(t *testing.T, registry *connectors.Registry, connectorID string) connectors.Connector {
	t.Helper()
	connector, err := registry.Connector(connectorID)
	if err != nil {
		t.Fatalf("Connector(%q): %v", connectorID, err)
	}
	return connector
}

func mustPlan(t *testing.T, publication *catalogPublication, name, connectorID, operationID string) planBinding {
	t.Helper()
	operation, err := publication.operation(connectorID, operationID)
	if err != nil {
		t.Fatalf("bind plan %q: %v", name, err)
	}
	digest, err := operationDigest(operation.Contract)
	if err != nil {
		t.Fatalf("bind plan %q: %v", name, err)
	}
	return planBinding{name: name, connectorID: connectorID, operationID: operationID, digest: digest, publication: publication}
}

func mustAcceptedOperation(t *testing.T, plan planBinding) (contract.SemanticDigest, contract.Operation) {
	t.Helper()
	revision, operation, err := plan.acceptedOperation()
	if err != nil {
		t.Fatalf("accepted operation for plan %q: %v", plan.name, err)
	}
	return revision, operation
}

func mustOperationDigest(t *testing.T, registry *connectors.Registry, connectorID, operationID string) contract.SemanticDigest {
	t.Helper()
	operation, err := registry.Operation(connectorID, operationID)
	if err != nil {
		t.Fatalf("Operation(%q, %q): %v", connectorID, operationID, err)
	}
	return mustContractDigest(t, operation.Contract)
}

func mustContractDigest(t *testing.T, operation contract.Operation) contract.SemanticDigest {
	t.Helper()
	digest, err := operationDigest(operation)
	if err != nil {
		t.Fatalf("digest operation contract: %v", err)
	}
	return digest
}

func operationDigest(operation contract.Operation) (contract.SemanticDigest, error) {
	document, err := json.Marshal(operation)
	if err != nil {
		return "", fmt.Errorf("encode operation contract: %w", err)
	}
	digest, err := contract.CanonicalDigest(document)
	if err != nil {
		return "", fmt.Errorf("compute operation contract digest: %w", err)
	}
	return digest, nil
}

func mustCatalogDigest(t *testing.T, catalog connectors.Catalog) contract.SemanticDigest {
	t.Helper()
	document, err := json.Marshal(catalog)
	if err != nil {
		t.Fatalf("encode catalog snapshot: %v", err)
	}
	digest, err := contract.CanonicalDigest(document)
	if err != nil {
		t.Fatalf("compute catalog snapshot digest: %v", err)
	}
	return digest
}

func catalogOperation(catalog connectors.Catalog, connectorID, operationID string) (connectors.Operation, error) {
	for _, connector := range catalog.Connectors {
		if connector.ConnectorID != connectorID {
			continue
		}
		for _, operation := range connector.Operations {
			if operation.Contract.OperationID == operationID {
				return operation, nil
			}
		}
	}
	return connectors.Operation{}, fmt.Errorf("catalog snapshot does not contain %s/%s", connectorID, operationID)
}

func catalogOperationKeys(catalog connectors.Catalog) []string {
	keys := make([]string, 0, len(catalog.Connectors))
	for _, connector := range catalog.Connectors {
		for _, operation := range connector.Operations {
			keys = append(keys, connector.ConnectorID+"/"+operation.Contract.OperationID)
		}
	}
	slices.Sort(keys)
	return keys
}

func unavailableOperationIDs(connector connectors.Connector) []string {
	identities := make([]string, 0, len(connector.UnavailableOperations))
	for _, unavailable := range connector.UnavailableOperations {
		identities = append(identities, unavailable.OperationID)
	}
	slices.Sort(identities)
	return identities
}
