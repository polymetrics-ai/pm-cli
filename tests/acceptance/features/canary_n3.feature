Feature: Canary N3 for issue 350
  This feature is a deliberate fault on the throwaway branch
  agent/286c-canary-n3-3. It never merges. A Before hook skips its tagged
  scenario as a whole.

  @canary-n3-skip
  Scenario: Canary N3 is skipped by a hook
    When I run "pm --help"
    Then the exit code is 0
