---
kind: project
name: airline-refund
policy:
  tools:
    web_read: allow
    gmail_search: allow
    gmail_read: allow
    gmail_draft: allow
    gmail_send:
      mode: approval
      resources: [refunds@airline.example]
    ask_owner: allow
    notify: allow
    schedule_followup: allow
  limits:
    max_llm_spend_usd: 5
completion:
  criteria:
    - id: refund
      description: The refund is posted to my card
    - id: receipt
      description: The airline confirmed the refund in writing
schedules:
  - title: Chase the refund
    every: 72h
    objective: Check for a reply from the airline. If there is none, send a short, polite follow-up.
---

Get the airline to refund my cancelled flight LH123 (booking reference XY123), which was due to
leave Frankfurt on 3 March.

Find the airline's refund policy first and quote it. Write to the refunds desk politely but firmly,
always mention the booking reference, and ask me before anything that costs money or commits me to
something. Tell me when the airline answers.
