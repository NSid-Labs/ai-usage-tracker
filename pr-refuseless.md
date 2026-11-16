Adds a `refuseless` API-key provider that reads `GET /v1/credits` and reports the prepaid credit
balance (and a key spending cap, when one is configured), plus a README section.

Refuseless is OpenAI-compatible and bills per token out of credit, so there is no reset window and no
daily history to backfill: the account shows what is left.

Tests: `go test ./internal/providers/ -run Refuseless` (balance, key limit, missing key).
