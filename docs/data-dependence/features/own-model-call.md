---
name: **Where Context Guru's own model call goes** (`agent.ModelTarget`, IBM Bob)
kind: metadata
off: ✅ Works. It keeps nothing.
metadata_only: ✅ Works
everything: ✅ Works
order: 155
---

A summary call and the sweep's question go to the model of the request's own route, with the caller's own key. The route's agent names only the path and the scheme of the auth header. Bob's agent names Bob's model path and `Apikey`. So on Bob's route these calls go to Bob's own server, which already has the conversation. Before, they went to a path that Bob does not serve, and they failed. The agent never sees the key, and it keeps nothing. Such a route never uses the operator's OpenAI key: with no key of its own, the request makes no model call. What each component sends and stores does not change: see its row above. Decided by: Vitaly Zabershinsky, #1074.
