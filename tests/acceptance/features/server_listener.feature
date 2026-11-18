Feature: pm-server serves on a listener that its starter holds
  A starter can bind the pm-server address, keep the socket and pass it to
  pm-server with --listen-fd. pm-server then serves on that socket, so the
  port stays held from the bind of the starter to the first request.
  pm-server sends its ready line through --ready-fd when its accept loop
  first calls Accept. It needs no incoming connection for that line.
  Plan item: M1-01 ready signal, inherited listener (ADR 0008). Issue: #72.

  # Rule LF-A: with --listen-fd <n>, pm-server serves on the inherited listener after its checks. It refuses a descriptor that fails a check, and it never binds server.listen in its place. Its ready line keeps the boundary of ADR 0005.
  # Rule LF-B: without --listen-fd, pm-server binds server.listen itself, as before.
  # Rule LF-C: no refusal line holds the raw --listen-fd value or an address.

  # LF1. Rule LF-A, success. Plan item: M1-01 ready signal, inherited listener (ADR 0008). Issue: #72.
  @stack
  Scenario: pm-server sends its ready signal and then serves on the listener that its starter holds, while the starter keeps its copy open
    Given a new PM deployment configuration on the job-owned stack
    And the starter holds the listener at the configured pm-server address
    And a pm server profile for the started pm-server
    When the starter starts pm-server with --listen-fd on the held listener and waits for its ready signal or its exit
    Then the starter received the pm-server ready signal
    When I run "pm server info" against the started pm-server
    Then the lane command exits with code 0
    And the lane command stdout contains the deployment ID of the started pm-server
    And the starter still holds the listener at the configured pm-server address
    And pm-server is still running

  # LF2. Rule LF-B, refusal. Plan item: M1-01 ready signal, inherited listener (ADR 0008). Issue: #72.
  @stack
  Scenario: Without --listen-fd, pm-server stops when its starter holds the configured address
    Given a new PM deployment configuration on the job-owned stack
    And the starter holds the listener at the configured pm-server address
    When the starter starts pm-server without --listen-fd and waits for its ready signal or its exit
    Then the starter received no pm-server ready signal
    And pm-server exits with code 1
    And pm-server stderr is exactly the line: pm-server: configured server listener is unavailable

  # LF3. Rules LF-A and LF-C, refusal. Plan item: M1-01 ready signal, inherited listener (ADR 0008). Issue: #72.
  @stack
  Scenario: pm-server refuses an inherited listener at another address, with a line that holds no value and no address
    Given a new PM deployment configuration on the job-owned stack
    And the starter holds the listener at the configured pm-server address
    And the starter holds a second listener at another loopback port
    When the starter starts pm-server with --listen-fd on the second listener and waits for its ready signal or its exit
    Then the starter received no pm-server ready signal
    And pm-server exits with code 1
    And pm-server stderr is exactly the line: pm-server: --listen-fd requires an inherited listener at the configured server address
    And pm-server stdout is empty

  # LF4. Rule LF-C, refusal of a malformed value. Plan item: M1-01 ready signal, inherited listener (ADR 0008). Issue: #72.
  Scenario: pm-server refuses a malformed --listen-fd value, and no output holds the value
    When I run pm-server with "--config /nonexistent/pm-deployment.json --secrets-directory /nonexistent/secrets --listen-fd=LF_PRIVATE_TEXT_SENTINEL" and descriptor 3 closed
    Then pm-server exits with code 1
    And pm-server stderr is exactly the line: pm-server: --listen-fd requires only the digits 0-9, with no leading zero, for a number from 3 to the int maximum
    And pm-server stdout is empty
