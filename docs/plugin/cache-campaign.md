# Cache campaign

!!! note
    To pick and apply a keep-alive strategy for your own account, use `/context-guru:save-more cache`.
    This command is the manager and operations view: any tenant, the model builder, the mode of any
    campaign, and a campaign's full evidence.

`/context-guru:cache-campaign` builds a keep-alive plan from a tenant's own history, says whether it
beats the best fixed rule on that history, and switches it on in shadow or active mode. It talks to
the proxy's campaign routes (`/api/cache/campaigns`), which are **manager only**.

```
/context-guru:cache-campaign                        latest campaign
/context-guru:cache-campaign build --id TENANT      build one (model builder)
/context-guru:cache-campaign build --id TENANT --builder auto [--session-end]
/context-guru:cache-campaign show ID [details]      one campaign; details lists every strategy compared
/context-guru:cache-campaign mode ID off|shadow|active [--force]
```

- **Login.** Set `CG_DASH_COOKIE` to the dashboard login cookie (`name=value`). Without it the routes
  answer 401 and the command says so.
- **`build`** waits up to 80 s, then prints the campaign id to check later. `--builder auto` picks a
  per-tenant fixed plan rebuilt daily and needs nothing beyond the proxy; the default builder trains a
  predictor and needs the build pipeline's Python on the proxy host. `--session-end` scores the auto
  plan as if the [client lifecycle signals](../how-to/cache-keepalive.md) are on.
- **`mode`**: `shadow` decides on live traffic and spends nothing; `active` sets the ping count and is
  refused unless the build recommended it (`--force` overrides).
- **Older proxy.** A proxy without the campaign routes gets a sample screen labelled `EXAMPLE, not your data`.

Dollars compare strategies on past traffic. They are not your bill; shadow dollars are estimates.
