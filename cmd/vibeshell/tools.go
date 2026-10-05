package main

import (
	"j0s.at/vibeshell/internal/application"
	"j0s.at/vibeshell/internal/simulation"
)

// buildToolAdapter wires the simulation tool layer behind the application's
// ToolExecutor seam. The second result reports whether a real adapter was
// built: a build missing a dependency the registry needs falls back to the
// rejecting executor, so an incomplete composition keeps the honest
// "tools unavailable" behavior instead of panicking inside a session.
func buildToolAdapter(c *component) (application.ToolExecutor, bool) {
	if c.db == nil || c.events == nil || c.world == nil || c.durable == nil {
		return rejectTools{}, false
	}
	registry := simulation.NewRegistry(simulation.Dependencies{
		World:     c.db,
		Events:    c.events,
		Retrieval: c.events,
		Content:   c.db,
		Apps:      c.durable.apps,
		Sandbox:   c.sandbox,
		Redactor:  simulation.DefaultRedactor{},
		Random:    c.random,
	})
	return &simulation.ToolAdapter{
		Registry:   registry,
		Namespaces: c.world,
		Clock:      c.clock,
	}, true
}
