// Package web is the framework-independent half that the web integrations (xgin, xecho, xbeego)
// share.
//
// More than half of what a web integration does has nothing to do with which framework it uses:
//
//   - starting and stopping the HTTP server: the timeouts, h2c, TLS / mutual TLS, and waiting for
//     the handlers in flight to really return during a graceful shutdown (Server);
//   - which proxies are trusted: parsing TrustedProxies, the private keyword, and whether the
//     directly connected peer is trusted (Proxies);
//   - redaction: the credentials in the request body, the query string and the request headers
//     (RedactBody / RedactHeaders), with one list of sensitive words per process;
//   - the access log: the fields' names, their order and their values (AccessLog).
//
// These semantics should be the same everywhere. Spread across each integration they would become
// several implementations drifting apart -- the same service on another framework could differ in
// whether client_ip is trusted, whether a credential reaches the logs, and whether the handlers are
// waited for on exit. So they are written here once, and each integration only does the "take the
// values out of my framework" layer: how the route template, the status code and the bytes written
// are taken differs per framework, and the integration measures them and hands them in as ordinary
// values.
//
// It is internal: what a caller sees is still each integration's own API (xgin.Config, the
// functions of xgin/middleware), and the names here are not something a caller has to learn. It
// depends on the standard library and the core's packages alone, so the core module still has no
// third-party dependencies.
//
// The module name an error is reported under comes from the integration (Server.Module): whoever's
// Start / Stop the error came out of is who reported it, so a caller's xerror.Is(err, "xgin") still
// holds. A function that does not return an xerror returns a plain error, which the integration
// wraps once at its own boundary.
package web
