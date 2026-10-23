package main

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// The shape almost every real API has and the rest of this service did not: a version prefix, with
// an authentication middleware in front of it that answers before the handler ever runs.
//
// Two things about it reach the framework. The route label has to be the whole template, prefix
// included, or /api/v1/users/:id and a later /api/v2/users/:id share one series in the metrics and
// one name in the spans. And a request the middleware turns away has to be recorded under that
// template as well: a 401 is an answer this service gave on a route it has, not a route it does not
// have, and recording it as unmatched would hide every authentication failure behind one label.
//
//	GET /api/v1/users/:id   200 with Authorization: Bearer e2e-token
//	                        401 with no Authorization at all
//	                        403 with any other credential
const v1Token = "Bearer e2e-token"

func v1Routes(e *gin.Engine) {
	g := e.Group("/api/v1", v1Auth)
	g.GET("/users/:id", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"id": c.Param("id"), "api": "v1"})
	})
}

// v1Auth answers before the handler runs. The credential it reads is in the Authorization header,
// which the access log masks by name -- what this endpoint is for is seeing that happen on a real
// request rather than in a unit test's recorder.
func v1Auth(c *gin.Context) {
	switch c.GetHeader("Authorization") {
	case v1Token:
		return
	case "":
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "no credential"})
	default:
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "not allowed"})
	}
}
