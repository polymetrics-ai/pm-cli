Feature: pm-server and pm-worker tell their starter when they are ready
  A starter waits for one ready signal from each process, with one time
  limit. It does not repeat an observation until one passes.
  Plan item: M1-01 ready signal (proposed). Issue: #72.

  # Rule A: pm-server sends one ready signal after it answers requests.
  # Rule B: pm-worker sends one ready signal after its own Temporal checks passed and its pollers started.
  # Rule C: the ready signal holds only its fixed text.
  # Rule E: the starter asks for the signal with --ready-fd <n>. The value has only ASCII digits, has no leading zero, fits in a 32-bit signed int and is 3 or more.
  # Edges of rules A and B: one signal in the life of the process, the close after the signal, and a closed channel.

  # A1. Rule A, success. Slice S1. Plan item: M1-01 ready signal. Issue: #72.
  @stack
  Scenario: pm-server answers a request at once after its ready signal
    Given a new PM deployment configuration on the job-owned stack
    And a pm server profile for the started pm-server
    When the starter starts pm-server and waits for its ready signal or its exit
    And I run "pm server info" against the started pm-server
    Then the starter received the pm-server ready signal
    And the lane command exits with code 0
    And the lane command stdout contains the deployment ID of the started pm-server
    And the observer reads the started pm-server information through its own HTTPS client, and its instance ID equals the instance ID in the lane command stdout
    And pm-server is still running

  # B1. Rule B, success. Slice S4. Plan item: M1-01 ready signal. Issue: #72.
  @stack
  Scenario: pm-worker sends its ready signal after it starts in its Temporal namespace
    Given a new PM deployment configuration on the job-owned stack
    When the starter starts pm-worker and waits for its ready signal or its exit
    Then the starter received the pm-worker ready signal
    And pm-worker is still running
