# Changelog

## [Unreleased]

- Added a **History** page: past consumption from 7 days to 12 months, with spend/tokens charts, a per-account
  split, per-model totals, daily or monthly breakdowns and the recorded subscription limit history.
- Reorganised **Accounts** into Subscriptions, Pay-as-you-go and Local tools, with a 30-day summary per account.
- Linked the dashboard summary tiles and account pages to the new History view.
- Added History to the main navigation.

## [0.1.1] - 2026-11-14

- Fixed: OpenCode Go weekly and monthly limit forecasts said "stays near X%" on a new install. They now
  work out the pace from when the window started, like the 5-hour window.
- Limit forecasts no longer claim usage will stay flat when the readings don't go back to the window's start.
- Added a favicon and home-screen icon.

## [0.1.0] - 2026-11-14

First public release.

- Tracks subscription limits for Claude Pro/Max, ChatGPT Plus/Pro (Codex) and OpenCode Go, and daily spend and
  tokens for the Anthropic and OpenAI APIs, OpenRouter, OpenCode Console budgets and local OpenCode sessions.
- Sign-in with admin and read-only viewer users, a one-time setup code for new installs, and per-IP lockout.
- 19 editor themes in dark and light versions, follow-system mode and a compact layout.
- Spend forecasts (burn rate, month projection, when a balance or key limit runs out) and limit forecasts
  ("hits 100% before it resets") that work from the first check.
- Settings for check interval, history retention, warning levels, users, sessions and read-only API tokens.
- Multi-arch Docker image (amd64, arm64) and binaries for Linux, macOS and Windows.
