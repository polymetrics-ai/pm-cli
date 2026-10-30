Feature: The pm CLI runs without a server for local commands
  The pm CLI is a thin client of PM Server. Version and help need no server.
  A command that needs a server is refused when no server profile is set.

  Scenario: The CLI prints its version
    When I run "pm version"
    Then the exit code is 0
    And stdout is not empty

  Scenario: The CLI prints help without a server
    When I run "pm --help"
    Then the exit code is 0
    And stdout contains "PM API thin client"

  Scenario: A server command is refused without a server profile
    When I run "pm no-such-command"
    Then the exit code is 2
    And stderr contains "--server-profile or PM_SERVER_PROFILE is required"
