#!/bin/sh
# Stand-in for scripts/campaign/build_campaign.py in tests: same arguments, same output files, no Python.
while [ $# -gt 0 ]; do case "$1" in --out=*) OUT="${1#--out=}"; shift;; *) shift;; esac; done
cp "$CG_CAMPAIGN_TEST_MODEL" "$OUT/model.json"
SHA=$(sha256sum "$OUT/model.json" | cut -d' ' -f1)
REC=${CG_CAMPAIGN_TEST_REC:-shadow}
cat > "$OUT/spec.json" <<JSON
{"schema":"cg.cache.campaign/2","version":"2.0.0",
 "menu":{"none":{"gaps_s":[],"max_pings":0,"tier":"5m"},"u280k2":{"gaps_s":[280,280],"max_pings":2,"tier":"5m"}},
 "policy":{"kind":"model+segments","model":{"ref":"model.json","sha256":"$SHA","schema":"cg.cache.predictor/1","n_features":37,"rule":{"lam":1,"x_s":280,"kmax":6,"tc_s":295}},
  "segments":[{"id":"family-haiku","when":[["family","==","haiku"]],"rule":{"kind":"none"}},{"id":"big-opus","when":[["family","==","opus"],["prefix_tok","<",1000]],"rule":{"kind":"plan","plan":"u280k2"}},{"id":"all","when":[],"rule":{"kind":"model"}}],
  "fallback":{"kind":"none"}},
 "evidence":{"recommendation":"$REC","best_simple_baseline":"blanket-K3","oracle_net_usd":10,
  "table":[{"strategy":"predictive(model)","net_usd":5},{"strategy":"blanket-K3","net_usd":4}],
  "paired_vs_baselines":[{"vs":"blanket-K3","diff_usd":1,"ci95":[-1,3]}]}}
JSON
echo "{\"state\":\"done\",\"recommendation\":\"$REC\",\"seconds\":0.1}" > "$OUT/status.json"
