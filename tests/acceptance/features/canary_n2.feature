Feature: Canary N2 for issue 350
  This feature is a deliberate fault on the throwaway branch
  agent/286c-canary-n2-3. It never merges. Its last step returns a skip, and
  its other steps pass.

  Scenario: Canary N2 skips its last step
    When I run "pm --help"
    Then the exit code is 0
    And the canary N2 step is skipped
