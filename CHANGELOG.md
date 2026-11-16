# Changelog

## Unreleased

- `flint listen --forward-to URL` now creates or reuses a sandbox JWT endpoint and prints its stable ID for receiver verification. Existing endpoints passed with `--webhook-endpoint-id` keep their configuration and subscriptions. Event filters follow those subscriptions, and individual signing refusals produce `skipped` records without ending the session or counting toward `--max-events`.
