// Package peer is the contract between xecho and its built-in middleware for passing along
// whether the directly connected peer is trusted.
//
// It lives under internal because it is strictly between two packages of the framework: callers
// neither need it nor should reach for it. Whether a peer is trusted has exactly one source,
// XEcho.TrustedProxies, and no switch of its own.
package peer

// TrustedKey is the key in echo.Context whose value is true when the directly connected peer is
// in XEcho.TrustedProxies.
//
// It is only ever written when the peer is trusted: the reading side sees false when nothing was
// written, which is the safe side — forgetting to write it trusts less, never more.
const TrustedKey = "xone/xecho.trusted_peer"
