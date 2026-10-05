// Package inference owns runtime admission control for model requests (PLAN 9.3,
// 11): the instance-wide and per-account concurrency bounds, the bounded wait
// queue in front of them, and the observable evidence of both.
//
// Dependency direction: the package depends on the domain and on the outbound
// ModelGateway port it decorates. It performs no I/O and imports no adapter, so
// the composition root can wrap any provider adapter with it without changing
// routing, failover, or shell behavior.
//
// Ownership of the limits themselves stays with the configuration adapter: the
// inference group of the configuration snapshot is validated there
// (global_concurrency, max_account_concurrency, wait_queue_depth) and this
// package enforces the values it is handed. A rejected request is local
// backpressure, never a provider fault: no provider time was spent, so no health
// record may be penalized because of it.
package inference
