// Package reqfacts reads the facts a request body states: the metadata the client chose
// (Meta, MetaFromBody) and the tool results the request answers (Lifecycle, ScanLifecycle).
// One rule, one place, so every host reads a request the same way. It reads wire format only,
// keeps counts and short enums, and holds no transport and no stored customer data. It does
// not import dash: dash.Meta and dash.LifecycleReq are aliases of these types.
package reqfacts
