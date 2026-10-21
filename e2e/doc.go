// Package e2e tests xone against real services: a real PostgreSQL, MySQL, Redis, ClickHouse and
// Kafka (Docker), with real processes and real signals and nothing mocked.
//
//	scripts/e2e.sh               # brings up PG / MySQL / Redis / ClickHouse / Kafka and runs everything
//	scripts/e2e.sh -run Smoke    # the arguments go to go test as they are
//
// The directories:
//
//	service/    the service under test: every integration in use, all of it configured from service/application.yml
//	baseline/   a bare gin service to compare against, which tells the framework's overhead under load
//	harness/    building, starting processes, sending signals, reading logs / spans / metrics / /proc, a TCP proxy, downstream stubs, a load generator
//	*_test.go   the tests themselves
//
// Everything skips unless the XONE_E2E environment variable is 1: scripts/test.sh runs the tests
// of every module, and on a machine with no databases this module has to stay green all the same.
// ClickHouse runs in Docker, and when it does not come up only TestClickHouse_* skips
// (harness.RequireCH) while the rest runs; Kafka works the same way (TestKafka_*,
// harness.RequireKafka).
//
// The tests assert the behavior as it is documented. When one uncovers a bug in the framework the
// assertion is not bent to fit it: the failing branch calls knownBug instead (which prints
// KNOWN BUG and skips), with the evidence written into the comment.
package e2e
