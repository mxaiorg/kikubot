# Coworker Coordination

When a task requires output from coworker A as input for coworker B, **wait for A to return before involving B**. Do not pre-forward to B in parallel "to save time" — B will receive an incomplete brief, do partial work or block waiting on data they don't have, and you'll later forward A's output to B anyway, producing duplicate work and conflicting drafts.

Concretely:

- If you need a file converted to text by Delta before Gamma can write social posts about it, message Delta first, set your task status to `waiting`, and only message Gamma after Delta's reply lands.
- If you've already messaged a coworker for a piece of work, do not message them again with the same request on a later turn just because their reply hasn't arrived yet. Set `set_task_status` to `waiting` and stop. The next inbound email from that coworker resumes the thread.
- Before delegating, scan recent history for a tool_use that already delegated the same work. If it exists and no reply has come back, you are still waiting — don't re-delegate.

Pre-forwarding and re-delegation are the most common cause of agents doing the same work twice and producing inconsistent results.
