Feature: pm-server and pm-worker tell their starter when they are ready
  A starter waits for one ready signal from each process, with one time
  limit. It does not repeat an observation until one passes.
  Plan item: M1-01 ready signal (proposed). Issue: #72.

  # Rule A: pm-server sends one ready signal after it answers requests.
  # Rule B: pm-worker sends one ready signal after its own Temporal checks passed and its pollers started.
  # Rule C: the ready signal holds only its fixed text.
  # Rule E: the starter asks for the signal with --ready-fd <n>. The value has only ASCII digits, has no leading zero, fits in a 32-bit signed int and is 3 or more.
  # Edges of rules A and B: one signal in the life of the process, the close after the signal, and a closed channel.

  # B1. Rule B, success. Slice S4. Plan item: M1-01 ready signal. Issue: #72.
  @stack
  Scenario: pm-worker sends its ready signal after it starts in its Temporal namespace
    Given a new PM deployment configuration on the job-owned stack
    When the starter starts pm-worker and waits for its ready signal or its exit
    Then the starter received the pm-worker ready signal
    And pm-worker is still running
