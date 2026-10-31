@pmgen
Feature: The checked-in connectors compile without drift
  The PostgreSQL operation "relation/mapped-upsert" is eligible for
  direct_write. A change to the write rules of pmgen must keep each checked-in
  connector compilable. The generated files must stay equal to the checkout.

  Scenario: pmgen checks every checked-in connector
    When pmgen checks the checked-in connectors
    Then pmgen succeeds
