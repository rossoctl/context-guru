# Test data for the accountant comparison

`known-differences.json` lists the cases where cg's saving logic (`dash.Event.Price`) is **meant** to
give a different figure from the shared test cases of
[`github.com/rossoctl/accountant`](https://github.com/rossoctl/accountant).

`dash/accountant_play_test.go` plays every accountant case through cg and compares the result:

- A case that is **not** in this file must agree with cg to within 1 µ$.
- A case that **is** in this file must really differ. If cg starts to agree, the test fails, so remove
  the entry.
- An entry whose case no longer exists in accountant also fails the test.

One entry:

| Key | Meaning |
|---|---|
| `case` | The id of an accountant case. |
| `kind` | `differs`: same question, different answer on purpose. `notCovered`: cg prices something the model does not, or the reverse. |
| `reason` | Why they differ. Cites a spec section and the cg function. |
| `allow` | The exact places (`step`, and `change` when one change differs) where cg may differ. A difference anywhere else fails. |
| `total` | Whether the thread total may differ. It follows from the steps. |

The file stays in context-guru because it compares the cases with cg code. The figures and the root
causes are in [`docs/accountant/`](../../../docs/accountant/cg-differences.md).
