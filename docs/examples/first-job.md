---
kind: responsibility
name: daily-go-news
policy:
  tools:
    web_read:
      mode: allow
      resources: [go.dev]
    notify: allow
  limits:
    max_llm_spend_usd: 1
    budget_period: 24h
notifications:
  min_severity: info
  dedup_window: 24h
  max_per_day: 3
schedules:
  - title: Daily Go blog check
    every: 24h
    objective: Read https://go.dev/blog, find the newest post, and notify me with a two-sentence summary.
---

Once a day, read https://go.dev/blog and tell me in two sentences what the newest post is about.
