---
kind: responsibility
name: portfolio-intelligence
policy:
  tools:
    await_workers: allow
    delegate: allow
    notify: allow
    portfolio_positions: allow
    schedule_followup: allow
    task_events: allow
    update_state: allow
    web_read: allow
  limits:
    max_llm_spend_usd: 20
    budget_period: 720h
    tool_rate_limits:
      web_read:
        max: 200
        per: 24h
notifications:
  min_severity: attention
  dedup_window: 72h
  max_per_day: 5
schedules:
  - title: Daily portfolio brief
    every: 24h
    skills:
      - portfolio-report
    objective: Write a short brief of what changed today across the portfolio. Notify at info severity only if something is worth reading.
routes:
  - kind: price.moved
    title: Investigate price move
    source: market
    match:
      threshold_crossed: "true"
    coalesce_field: symbol
    skills:
      - market-research
    objective: A holding moved past its alert threshold. Find out why, judge whether it changes the thesis in your state, and notify only if it does.
  - kind: article.published
    title: Assess news
    source: news
    coalesce_field: symbol
    skills:
      - market-research
    objective: News mentions a holding. Decide whether it is material; most news is not.
obligations:
  - description: Portfolio picture refreshed at least daily
    every: 24h
    grace: 6h
    evidence:
      tool: update_state
    skills:
      - portfolio-report
  - description: Market events keep arriving
    every: 72h
    evidence:
      event:
        kind: price.moved
        source: market
---

Continuously monitor my portfolio and relevant market developments. Investigate meaningful events, keep a concise picture of each holding in your state, and tell me when something needs my attention or action. Never trade.
