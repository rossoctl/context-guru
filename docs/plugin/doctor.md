# Doctor

**Question: is it set up right?**

![/context-guru:doctor](img/doctor-dark.svg#only-dark)
![/context-guru:doctor](img/doctor-light.svg#only-light)

```text
All 7 checks pass.

  ✓ This session is routed through a local proxy
  ✓ The proxy answers
  ✓ Dashboard API is on (these commands need it)
  ✓ Prices are loaded (dollars are real, not zero)
  ✓ Context Guru status line is installed
  ✓ Status line refreshes on a timer (live cache countdown)
  ✓ Cache mode is set
```

When something fails, the first line names the first fix and each failing check says what to run:

| Check | If it fails |
|---|---|
| This session is routed through a local proxy | `/context-guru:install` |
| The proxy answers | `/context-guru:update`, or see [troubleshooting](configuration.md) |
| Dashboard API is on | start the proxy with `--dashboard` |
| Prices are loaded | point `MODEL_PRICES` at your gateway's price file; otherwise dollars show as zero |
| Context Guru status line is installed (shown only when it is: since 0.6.1 the band above the prompt shows its figures) | `/context-guru:statusline` |
| Status line refreshes on a timer | set `"refreshInterval"` on `statusLine` (see [Status line](statusline.md#refresh)) |
| Cache mode is set | `/context-guru:cache-strategy-picker` |
