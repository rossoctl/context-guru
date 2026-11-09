# Example tokens: for `compose.yaml` only

The two files here hold ONE made-up token each, so a developer can start the stack and push to it.
Anyone who reads this repository knows them. Never use them anywhere a stranger can reach.

A real server gets its tokens from `install.sh token add`, which makes a random one and keeps it in
`/etc/central-metrics/tokens/`, outside this repository.

| File | Used by |
|---|---|
| `cortex.txt` | Cortex proxies (port 4318) |
| `context-guru.txt` | context-guru proxies (port 4319) |
