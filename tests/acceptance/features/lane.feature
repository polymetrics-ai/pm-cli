@stack
Feature: The acceptance lane runs the real PM processes on the job-owned stack
  The lane starts the real pm-server, with its API and relay, on the
  job-owned dependency stack. It drives the built pm binary. An independent
  observer reads PostgreSQL through its own read-only role.

  Background:
    Given the lane runs pm-server on the job-owned stack
    And a pm server profile for the lane server

  Scenario: The CLI reaches the real server
    When I run "pm server info" against the lane server
    Then the lane command exits with code 0
    And the lane command stdout contains the lane deployment ID

  Scenario: A command without a PM session is refused and saves nothing
    When I run "pm runs list" against the lane server
    Then the lane command exits with code 3
    And the lane command stderr contains "PM session is required"
    And the observer sees no new row in the PM run and audit tables
