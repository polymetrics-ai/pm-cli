Feature: An operator sets the device poll interval of pm-server
  The deployment configuration can set the device poll interval, in whole
  seconds from 1 to 60. Without the setting, the interval is 5 seconds.
  pm-server and pm-server bootstrap refuse any other value.
  Plan item: device poll interval setting. Issue: #366. Refs #325.

  # P1. Rule PI-3. Issue #366.
  @stack
  Scenario Outline: pm-server refuses an invalid device poll interval: <row>
    Given a new PM deployment configuration on the job-owned stack
    And its secrets directory holds the bootstrap database credential
    And the deployment configuration sets the device poll interval to the JSON value <value>
    When the starter starts pm-server and waits for its ready signal or its exit
    Then the starter received no pm-server ready signal
    And pm-server exits with code 1
    And pm-server stderr is exactly the line: pm-server: configuration <kind> at identity.device_poll_interval

    Examples:
      | row | value                  | kind   |
      | P1a | "0s"                   | bounds |
      | P1b | "61s"                  | bounds |
      | P1c | "1500ms"               | bounds |
      | P1d | "-5s"                  | bounds |
      | P1e | "5"                    | value  |
      | P1f | 60                     | type   |
      | P1g | null                   | type   |
      | P1h | "+5s"                  | value  |
      | P1i | "0000000000000000000000000000000000000000000000000000000000000001s" | value  |
      | P1j | "9999999999999999999s" | value  |

  # P2. Rule PI-4. Issue #366.
  @stack
  Scenario: pm-server bootstrap refuses an invalid device poll interval
    Given a new PM deployment configuration on the job-owned stack
    And its secrets directory holds the bootstrap database credential
    And the deployment configuration sets the device poll interval to the JSON value "61s"
    And a bootstrap input with a new organization, a workspace, the admin bound to the login user of the stack identity provider and a "postgres" provider account
    When the operator runs pm-server bootstrap with that input
    Then the bootstrap exits with code 1
    And the bootstrap stderr is exactly the line: pm-server: configuration bounds at identity.device_poll_interval

  # P3. Rule PI-2: the default. Issue #366.
  @stack
  Scenario: Without the setting, a device login start answer states a poll interval of 5 seconds
    Given a new PM deployment configuration on the job-owned stack
    And its secrets directory holds the bootstrap database credential
    And the deployment configuration declares the "postgres" connector bundle
    And the operator bootstrapped the deployment with a "postgres" provider account
    And a pm server profile for the started pm-server
    And the starter starts pm-server and waits for its ready signal or its exit
    And the starter received the pm-server ready signal
    When I start a device login on the started pm-server and keep only its poll interval
    Then the device login start answer states a poll interval of 5 seconds

  # P4. Rules PI-1, PI-4 and PI-5. Issue #366.
  @stack
  Scenario Outline: A device login start answer states the configured poll interval: <seconds> seconds
    Given a new PM deployment configuration on the job-owned stack
    And its secrets directory holds the bootstrap database credential
    And the deployment configuration sets the device poll interval to <seconds> seconds
    And the deployment configuration declares the "postgres" connector bundle
    And a bootstrap input with a new organization, a workspace, the admin bound to the login user of the stack identity provider and a "postgres" provider account
    When the operator runs pm-server bootstrap with that input
    Then the bootstrap exits with code 0
    And a pm server profile for the started pm-server
    When the starter starts pm-server and waits for its ready signal or its exit
    Then the starter received the pm-server ready signal
    When I start a device login on the started pm-server and keep only its poll interval
    Then the device login start answer states a poll interval of <seconds> seconds

    Examples:
      | seconds |
      | 60      |
      | 1       |
