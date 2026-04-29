# Knowledge

The Knowledge directory holds common and per agent knowledge in Markdown format.

Knowledge is loaded into the agent system prompt as context for the agent to use when answering questions.

Knowledge supplements the agent's system prompt provided by default or via the agent's environment variable, SYSTEM_PROMPT.

The 'common' directory holds knowledge that is common to all agents.

Agent specific knowledge is provided by directories named with the lowercase of the agent's email account (part before the @domain). For example, alice@agents.com would have a knowledge directory named 'alice'.

Mulitple knowledge files can be loaded. Files that are prefixed with a number will be loaded in their prefix order. For example, `01_file.md` will be loaded before `02_file.md`.

A few examples of knowledge files are provided.