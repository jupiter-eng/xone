// Package reqctx is the contract between xecho and its built-in middleware for passing along the
// ctx a request should be using.
//
// The built-in middleware runs on e.Pre, where the request must not be touched yet: echo's router
// matches on the *http.Request as it arrived, and so does a user's e.Pre(echomw.MethodOverride());
// swapping in an r.WithContext copy there makes the router match the wrong route. Mutating the
// request in place is no better — net/http's contract says a handler does not modify the request
// it was handed, and a handler that passes the request to a goroutine outliving it would race.
// So Pre only attaches the log scope and the span to a ctx and stores that in echo.Context; after
// routing, the first e.Use middleware xecho installs does the usual
// c.SetRequest(r.WithContext(ctx)).
//
// It lives under internal because it is strictly between two packages of the framework: callers
// neither need it nor should reach for it.
package reqctx

// Key is the key in echo.Context whose value is a context.Context: the ctx this request should be
// using so far.
//
// xecho writes the request's original ctx at the outermost layer of the Pre chain; built-in
// middleware that finds it only writes there and leaves the request alone. Without it (middleware
// used on its own, mounted on e.Use) the middleware swaps the request's ctx as usual.
const Key = "xone/xecho.ctx"
