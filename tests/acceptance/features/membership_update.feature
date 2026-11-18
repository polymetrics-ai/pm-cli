Feature: An admin adds a connection to a membership
  An authorized user adds a connection to a membership with the revision that
  the user read. Each update is audited. No step writes a PM row.
  Plan item: M1-01 ready signal, part b (proposed). Issue: #284 (sub-issue of #72), slice 284c.

  Background:
    Given the lane runs pm-server and pm-worker on the job-owned stack
    And the operator bootstrapped the deployment with a "postgres" provider account
    And the user is logged in through the product login flow
    And the user created a PostgreSQL connection with "pm connections create"

  # M1. Rule M, success.
  @stack
  Scenario: The admin adds the new connection to its own membership
    Given the credential page refuses a credential of the user for that connection, and the observer sees no stored credential for it
    When the user runs "pm access memberships update" to add that connection to its own membership, with the current membership revision
    Then the lane command exits with code 0
    And the observer sees that the membership grants list that connection, that its other grants did not change, and that its revision is one higher
    And the observer sees one audit record of the update that names the user as its actor
    And the user can store the customer role password through the product credential page for that connection

  # M2. Rule M, refusal: a stale revision.
  @stack
  Scenario: The membership update refuses a stale revision
    Given the user added that connection to its own membership with "pm access memberships update"
    And the observer records the membership state of the workspace
    When the user runs "pm access memberships update" on its own membership, with the revision before the current revision
    Then the lane command exits with code 4
    And the lane command stderr starts with "error: control_revision_mismatch:"
    And the observer sees the membership state of the workspace unchanged since it recorded it, and no new audit record

  # M3. Rule M, refusal: no revision.
  @stack
  Scenario: The membership update refuses a command with no revision
    Given the observer records the membership state of the workspace
    When the user runs "pm access memberships update" to add that connection to its own membership, with no revision
    Then the lane command exits with code 2
    And the lane command stderr is exactly the line: error: --revision is required
    And the observer sees the membership state of the workspace unchanged since it recorded it, and no new audit record
