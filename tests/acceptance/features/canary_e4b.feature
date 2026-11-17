Feature: Canary E4b for issue 272
  This feature is a deliberate fault on the throwaway branch
  agent/286b-canary-e4b-3. It never merges. It uses existing step
  definitions. Its last expectation is wrong, so the scenario fails while
  the stack run passes.

  Scenario: Canary E4b expects a help text that pm does not print
    When I run "pm --help"
    Then the exit code is 0
    And stdout contains "canary E4b expects a text that pm help does not print"
