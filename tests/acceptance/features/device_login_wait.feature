Feature: A device login exchange waits for the browser decision
  pm sessions login-exchange --wait=true repeats the exchange request while
  pm-server answers device_authorization_pending or device_poll_too_soon.
  Before each wait, it writes one notice line to stderr with the wait in
  seconds. During the wait, the browser session of the deployment's Keycloak
  user approves or denies the device login, or an interrupt ends the command.
  The proof from login-start stays in memory and goes to login-exchange on
  stdin. The step adds only --challenge-id with the challenge ID from the
  login-start output to the login-exchange command line.
  Plan item: L-d (device login wait). Issue: #325. Criteria: D4 of the login proposal.

  # Row 1. D4-2, D4-4, D4-9. Rule PI-5. Slice L-d.
  # The deployment sets the device poll interval to 60 s. So the first poll
  # time of the challenge is its insertion time plus 60 s. The wait check
  # below needs the first exchange less than 55 s after the challenge
  # insertion (proof limit 1). A late exchange fails the notice step. It
  # never passes falsely.
  @stack
  Scenario: A device login exchange waits after a too-soon answer and then succeeds
    Given a new PM deployment configuration on the job-owned stack
    And its secrets directory holds the bootstrap database credential
    And the deployment configuration sets the device poll interval to 60 seconds
    And the deployment configuration declares the "postgres" connector bundle
    And the operator bootstrapped the deployment with a "postgres" provider account
    And a pm server profile for the started pm-server
    And the starter starts pm-server and waits for its ready signal or its exit
    And the starter received the pm-server ready signal
    And a PM browser session of the deployment's Keycloak user
    And I run "pm sessions login-start --device-id acceptance-device" against the started pm-server and keep its stdout in memory
    And the device login challenge states a poll interval of 60 seconds
    When I start "pm sessions login-exchange --wait=true --request-timeout=2m0s" against the started pm-server with the device proof on stdin
    Then the device login exchange writes the wait notice line for device_poll_too_soon with a wait of at least 11 seconds to stderr
    When the browser session approves the device login
    Then the device login exchange exits with code 0
    When I run "pm sessions inspect" against the started pm-server with the stored PM session
    Then the lane command exits with code 0

  # Row 2. D4-5: the wait stops at success after a browser approval. Slice L-d.
  @stack
  Scenario: A device login approved in the browser during the wait gives a PM session
    Given a new PM deployment configuration on the job-owned stack
    And its secrets directory holds the bootstrap database credential
    And the deployment configuration declares the "postgres" connector bundle
    And the operator bootstrapped the deployment with a "postgres" provider account
    And a pm server profile for the started pm-server
    And the starter starts pm-server and waits for its ready signal or its exit
    And the starter received the pm-server ready signal
    And a PM browser session of the deployment's Keycloak user
    And I run "pm sessions login-start --device-id acceptance-device" against the started pm-server and keep its stdout in memory
    When I start "pm sessions login-exchange --wait=true" against the started pm-server with the device proof on stdin
    Then the device login exchange writes the wait notice line to stderr
    When the browser session approves the device login
    Then the device login exchange exits with code 0
    When I run "pm sessions inspect" against the started pm-server with the stored PM session
    Then the lane command exits with code 0

  # Row 3. D4-5: the wait stops at 403 device_authorization_denied, with the
  # existing exit code for a 403 answer. Slice L-d.
  @stack
  Scenario: A device login denied in the browser during the wait stops with the denied exit code
    Given a new PM deployment configuration on the job-owned stack
    And its secrets directory holds the bootstrap database credential
    And the deployment configuration declares the "postgres" connector bundle
    And the operator bootstrapped the deployment with a "postgres" provider account
    And a pm server profile for the started pm-server
    And the starter starts pm-server and waits for its ready signal or its exit
    And the starter received the pm-server ready signal
    And a PM browser session of the deployment's Keycloak user
    And I run "pm sessions login-start --device-id acceptance-device" against the started pm-server and keep its stdout in memory
    When I start "pm sessions login-exchange --wait=true" against the started pm-server with the device proof on stdin
    Then the device login exchange writes the wait notice line to stderr
    When the browser session denies the device login
    Then the device login exchange exits with code 3
    And the device login exchange stderr names device_authorization_denied

  # Row 4. D4-8: an interrupt during the wait ends the command with the
  # existing exit code for a cancelled command. A Go developer check on the
  # change shows that no exchange request follows the cancellation. Slice L-d.
  @stack
  Scenario: An interrupt during the device login wait ends the command
    Given a new PM deployment configuration on the job-owned stack
    And its secrets directory holds the bootstrap database credential
    And the deployment configuration declares the "postgres" connector bundle
    And the operator bootstrapped the deployment with a "postgres" provider account
    And a pm server profile for the started pm-server
    And the starter starts pm-server and waits for its ready signal or its exit
    And the starter received the pm-server ready signal
    And a PM browser session of the deployment's Keycloak user
    And I run "pm sessions login-start --device-id acceptance-device" against the started pm-server and keep its stdout in memory
    When I start "pm sessions login-exchange --wait=true" against the started pm-server with the device proof on stdin
    Then the device login exchange writes the wait notice line to stderr
    When I send SIGINT to the device login exchange
    Then the device login exchange exits with code 6
    And the device login exchange process has ended
