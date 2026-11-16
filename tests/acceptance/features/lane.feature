@stack
Feature: The acceptance lane runs the real PM processes on the job-owned stack
  The lane starts the real pm-server, with its API and relay, on the
  job-owned dependency stack. It drives the built pm binary. An independent
  observer reads PostgreSQL through its own read-only role and reads the lane
  server information through its own HTTPS client.

  Background:
    Given the lane runs pm-server on the job-owned stack
    And a pm server profile for the lane server

  Scenario: The CLI reaches the real server
    When I run "pm server info" against the lane server
    Then the lane command exits with code 0
    And the lane command stdout contains the lane deployment ID
    And the observer reads the lane server information through its own HTTPS client, and its instance ID equals the instance ID in the lane command stdout

  Scenario: The CLI refuses a command without a PM session, and the PM run and audit tables gain no row
    When I run "pm runs list" against the lane server
    Then the lane command exits with code 3
    And the lane command stderr contains "PM session is required"
    And the observer sees no new row in the PM run and audit tables

  # L2. Rule: the lane runs pm-server and pm-worker. Plan item: M1-01 ready signal, part b. Issue: #72.
  @stack
  Scenario: The lane starts pm-worker and waits for its ready signal
    Then the lane received the pm-server ready signal
    And the lane received the pm-worker ready signal
    And the lane pm-server is still running
    And the lane pm-worker is still running
