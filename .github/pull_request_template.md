## Summary

<!-- What changed and why, in a few short sentences. -->

## How it was tested

<!-- Commands you ran and what they showed. -->

## Data dependence

<!--
Required for every PR that adds or changes a component or feature. See docs/data-dependence.md.
Pick one and fill it in. Give the reason in plain words, not a file or line number.
-->

- [ ] **No change** to what any component or feature stores.
- [ ] **Metadata only**: it keeps <hashes / counts / ids …>, because <reason>.
- [ ] **Content**: it keeps <what>, in <where>, because <reason>.

What it does at each level (see docs/data-dependence.md):
- **Off**: <works / skips / works with less: what is lost>
- **Metadata only**: <…>
- **Everything**: <…>

<!--
If this PR CHANGES a row's Kind or any of its three level columns (in either direction), say so in the
first line of the PR and ask the person in that row's "Decided by" to confirm before merge.
-->
