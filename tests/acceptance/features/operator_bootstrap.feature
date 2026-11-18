Feature: An operator bootstraps the first workspace and its admin
  The operator runs pm-server bootstrap with the server configuration, the
  secrets directory and an input file. It creates the first workspace, the
  first user with its login binding and admin membership, and the provider
  accounts of the input. No step writes a PM row.
  Plan item: M1-01 ready signal, part b (proposed). Issue: #284 (sub-issue of #72).

  # K1a. Rule K, success. Slice 284b.
  @stack
  Scenario: The operator bootstraps a deployment
    Given a new PM deployment configuration on the job-owned stack
    And its secrets directory holds the bootstrap database credential
    And the deployment configuration declares the "postgres" connector bundle
    And a bootstrap input with a new organization, a workspace, the admin bound to the login user of the stack identity provider and a "postgres" provider account
    When the operator runs pm-server bootstrap with that input
    Then the bootstrap exits with code 0
    And the bootstrap stdout is one JSON document that names the workspace, the admin identity, the admin membership and the provider account
    And the bootstrap stdout holds no value from the secrets directory
    And the observer sees exactly one new workspace, one new identity with its login binding for this deployment, one admin membership, one provider account, one bootstrap record for this deployment and organization, and one audit record that names the operator bootstrap as its actor

  # K2. Rule K, refusal: once for each deployment and organization. Slice 284b.
  @stack
  Scenario: A second bootstrap of the same scope is refused and changes nothing
    Given a new PM deployment configuration on the job-owned stack
    And its secrets directory holds the bootstrap database credential
    And the deployment configuration declares the "postgres" connector bundle
    And the operator bootstrapped the deployment with a "postgres" provider account
    And the observer records the bootstrap state of the stack
    When the operator runs pm-server bootstrap again with the same input
    Then the bootstrap exits with code 1
    And the bootstrap stdout is empty
    And the bootstrap stderr is exactly the line: pm-server: bootstrap scope is already bootstrapped
    And the observer sees the bootstrap state of the stack unchanged since it recorded it, and no new audit record

  # K3. Rule K, refusal: invalid input. Slice 284b.
  @stack
  Scenario Outline: The bootstrap refuses <case>
    Given a new PM deployment configuration on the job-owned stack
    And its secrets directory holds the bootstrap database credential
    And the deployment configuration declares the "postgres" connector bundle
    And a bootstrap input with <change>
    And the observer records the bootstrap state of the stack
    When the operator runs pm-server bootstrap with that input
    Then the bootstrap exits with code 1
    And the bootstrap stdout is empty
    And the bootstrap stderr is exactly the line: <line>
    And the observer sees the bootstrap state of the stack unchanged since it recorded it, and no new audit record

    Examples:
      | case                    | change                                                                  | line                                                                |
      | an empty workspace name | an empty workspace name                                                 | pm-server: bootstrap workspace name is required                     |
      | an empty admin subject  | an admin subject that is empty                                          | pm-server: bootstrap admin subject is required                      |
      | an undeclared connector | a provider account for a connector that the deployment does not declare | pm-server: bootstrap provider account names an undeclared connector |

  # K4. Rule K, refusal: no operator authority. Slice 284b.
  @stack
  Scenario: The bootstrap refuses a caller without operator authority
    Given a new PM deployment configuration on the job-owned stack
    And its secrets directory does not hold the bootstrap database credential
    And the deployment configuration declares the "postgres" connector bundle
    And a bootstrap input with a new organization, a workspace, the admin bound to the login user of the stack identity provider and a "postgres" provider account
    And the observer records the bootstrap state of the stack
    When the operator runs pm-server bootstrap with that input
    Then the bootstrap exits with code 1
    And the bootstrap stdout is empty
    And the bootstrap stderr is exactly the line: pm-server: bootstrap requires operator authority
    And the observer sees the bootstrap state of the stack unchanged since it recorded it, and no new audit record

  # K5. Rule K, edge: another deployment on the same stack. Slice 284b.
  @stack
  Scenario: Another deployment on the same stack bootstraps its own scope
    Given a new PM deployment configuration on the job-owned stack
    And its secrets directory holds the bootstrap database credential
    And the deployment configuration declares the "postgres" connector bundle
    And the operator bootstrapped the deployment with a "postgres" provider account
    And the observer records the bootstrap state of the first scope
    And a second new PM deployment configuration on the job-owned stack, with the bootstrap database credential
    And the second deployment configuration declares the "postgres" connector bundle
    When the operator runs pm-server bootstrap for the second deployment
    Then the bootstrap exits with code 0
    And the observer sees one new workspace, identity, login binding, admin membership, provider account and bootstrap record for the second scope, and one new audit record
    And the observer sees the bootstrap state of the first scope unchanged since it recorded it

  # K6. Rule K, edge: a second organization in the same deployment. Slice 284b.
  @stack
  Scenario: A second organization in the same deployment bootstraps its own scope
    Given a new PM deployment configuration on the job-owned stack
    And its secrets directory holds the bootstrap database credential
    And the deployment configuration declares the "postgres" connector bundle
    And the operator bootstrapped the deployment with a "postgres" provider account
    And the observer records the bootstrap state of the first scope
    And a bootstrap input for a second organization in the same deployment, with another admin subject and a "postgres" provider account
    When the operator runs pm-server bootstrap with that input
    Then the bootstrap exits with code 0
    And the observer sees one new workspace, identity, login binding, admin membership, provider account and bootstrap record for the second scope, and one new audit record
    And the observer sees the bootstrap state of the first scope unchanged since it recorded it

  # K7. Rule K, refusal: an admin login binding that exists already. Slice 284b.
  @stack
  Scenario: A second organization with the admin issuer and subject of the first scope is refused
    Given a new PM deployment configuration on the job-owned stack
    And its secrets directory holds the bootstrap database credential
    And the deployment configuration declares the "postgres" connector bundle
    And the operator bootstrapped the deployment with a "postgres" provider account
    And a bootstrap input for a second organization in the same deployment, with the admin issuer and subject of the first scope
    And the observer records the bootstrap state of the stack
    When the operator runs pm-server bootstrap with that input
    Then the bootstrap exits with code 1
    And the bootstrap stdout is empty
    And the bootstrap stderr is exactly the line: pm-server: bootstrap admin login is already bound
    And the observer sees the bootstrap state of the stack unchanged since it recorded it, and no new audit record

  # K1b. Rule K, success: the bootstrapped admin can log in. Slice W1a. Feature: the lane.
  @stack
  Scenario: The bootstrapped admin logs in and sees its workspace
    Given the lane runs pm-server and pm-worker on the job-owned stack
    And the operator bootstrapped the deployment with a "postgres" provider account
    And the user is logged in through the product login flow
    When I run "pm workspaces list" as the user
    Then the lane command exits with code 0
    And the lane command stdout names the bootstrapped workspace and no other workspace
