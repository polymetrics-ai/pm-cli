@pmgen
Feature: pmgen keeps the write identity and the read-back of a write operation
  A write operation tells PM how to find its provider effect again. It declares
  a provider dedup field, or none, and a read-back operation. Each fact has a
  source witness. A write that is eligible without these facts must have an
  explicit unsafe classification. pmgen refuses to compile any other eligible
  write. Each scenario runs the real pmgen process on a synthetic connector
  source. An observer reads the bundle that pmgen writes.

  Background:
    Given a connector source "items" with the write operation "items/create" and the read operation "items/get"

  Scenario Outline: A write declares its dedup field and its read-back operation
    Given the semantics mark "items/create" eligible for "direct_write"
    And the semantics declare the dedup field "<dedup field>" for "items/create" with a source witness
    And the semantics declare the read-back operation "items/get" for "items/create" with a source witness
    When pmgen compiles the connector
    Then pmgen succeeds
    And the bundle operation "items/create" has the dedup field "<dedup field>" with its source witness
    And the bundle operation "items/create" has the read-back operation "items/get" with its source witness

    Examples:
      | dedup field                                     |
      | request.application-json.body.client-request-id |
      | none                                            |

  Scenario Outline: An eligible write without a write identity is refused
    Given the semantics mark "items/create" eligible for "<lane>"
    And the semantics declare no write identity for "items/create"
    When pmgen compiles the connector
    Then pmgen refuses to compile "items/create"
    And pmgen writes no bundle

    Examples:
      | lane         |
      | direct_write |
      | reverse_etl  |

  Scenario: A safe retry classification does not replace the write identity
    Given the semantics mark "items/create" eligible for "direct_write"
    And the semantics declare the retry classification "safe" for "items/create" with a source witness
    And the semantics declare no write identity for "items/create"
    When pmgen compiles the connector
    Then pmgen refuses to compile "items/create"
    And pmgen writes no bundle

  Scenario: An explicit unsafe classification admits a write without a write identity
    Given the semantics mark "items/create" eligible for "direct_write"
    And the semantics declare the retry classification "unsafe" for "items/create" with a source witness
    And the semantics declare no write identity for "items/create"
    When pmgen compiles the connector
    Then pmgen succeeds
    And the bundle operation "items/create" has the retry classification "unsafe"

  Scenario: A read operation needs no write identity
    Given the semantics mark "items/get" eligible for "direct_read"
    When pmgen compiles the connector
    Then pmgen succeeds
