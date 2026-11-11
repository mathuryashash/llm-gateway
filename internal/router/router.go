// Package router resolves a client-requested model alias to an ordered list
// of (providerClient, modelName) pairs. The proxy handler iterates the list
// left to right, skipping circuit-open entries.
package router

import (
	"fmt"

	"github.com/mathuryashash/llm-gateway/internal/config"
	"github.com/mathuryashash/llm-gateway/internal/provider"
)

// Route is one resolved (client, model) pair for an alias.
type Route struct {
	Client    *provider.Client
	Model     string
	RouteKey  string // unique identifier for breaker/metrics: "alias/providerName"
}

// Router maps model aliases to ordered route lists.
type Router struct {
	routes map[string][]Route // alias → routes
}

// New builds a Router from config, using clients from the provided map.
func New(cfg *config.Config, clients map[string]*provider.Client) (*Router, error) {
	r := &Router{routes: make(map[string][]Route)}
	for _, route := range cfg.Routes {
		var resolved []Route
		for i, provName := range route.Providers {
			c, ok := clients[provName]
			if !ok {
				return nil, fmt.Errorf("router: route %q references unknown provider %q", route.Alias, provName)
			}
			resolved = append(resolved, Route{
				Client:   c,
				Model:    route.Models[i],
				RouteKey: route.Alias + "/" + provName,
			})
		}
		r.routes[route.Alias] = resolved
	}
	return r, nil
}

// Resolve returns the ordered routes for a model alias.
// If the alias is not found, it returns an error.
func (r *Router) Resolve(alias string) ([]Route, error) {
	routes, ok := r.routes[alias]
	if !ok {
		return nil, fmt.Errorf("model %q not found", alias)
	}
	return routes, nil
}

// KnownAliases returns all configured model aliases.
func (r *Router) KnownAliases() []string {
	out := make([]string, 0, len(r.routes))
	for k := range r.routes {
		out = append(out, k)
	}
	return out
}
