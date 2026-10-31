@query-child
Feature: A Parquet query result has the schema that pm-query returns
  pm-query is the restricted query child. For a selection with a Parquet
  output, it returns the column names and kinds of the result. The file that it
  writes must have the same column names, in the same order, with the same
  types. An observer reads the file with its own DuckDB.

  Background:
    Given the observer writes a Parquet relation "calls" with these rows:
      | call_id:VARCHAR | title:VARCHAR | duration:BIGINT | amount:DECIMAL(18,2) | started_at:TIMESTAMPTZ |
      | c1              | First         | 61              | 10.50                | 2026-10-01T10:00:00Z   |
      | c2              |               | 120             | 3.00                 | 2026-10-02T11:30:00Z   |

  Scenario: Aliased columns keep their alias names, order and types in the file
    When pm-query selects "call_id AS id, title AS name, amount AS total" from "calls" into a Parquet output
    Then pm-query succeeds and returns the columns "id, name, total"
    And the observer reads the footer columns "id, name, total" in that order
    And the footer column types equal the returned column types
    And the observer reads the same rows from the file as pm-query returns, with "name" null for "c2"

  Scenario: Columns without an alias keep their names
    When pm-query selects "call_id, duration, started_at" from "calls" into a Parquet output
    Then the observer reads the footer columns "call_id, duration, started_at" in that order

  Scenario: A duplicate alias is refused before anything is written
    When pm-query selects "call_id AS x, title AS x" from "calls" into a Parquet output
    Then pm-query refuses the request
    And the output file does not exist
