// reactor.go - Deterministic event reactor for CogOS kernel.
//
// The Reactor subscribes to bus events and matches them against a rule set.
// When a rule fires, it invokes the rule's Action — no LLM in the loop.
// This enables infrastructure-level automation: startup notifications,
// health checks, capability refresh, etc.

package main

import (
	"log"
	"sync"
)

// ReactorRule defines a deterministic reaction to a bus event.
type ReactorRule struct {
	Name      string              // Human-readable rule name for logging
	EventType string              // Match on CogBlock.Type (exact match)
	BusFilter string              // Optional bus ID filter ("" = any bus)
	Action    func(block *CogBlock) // Callback when rule matches
}

// Reactor listens for bus events and dispatches matching rules.
// Rules are evaluated in registration order; all matching rules fire.
type Reactor struct {
	mu      sync.Mutex
	rules   []ReactorRule
	manager *busSessionManager
	running bool
}

// NewReactor creates a Reactor bound to the given bus session manager.
func NewReactor(manager *busSessionManager) *Reactor {
	return &Reactor{manager: manager}
}

// AddRule registers a new reactor rule. Must be called before Start.
func (r *Reactor) AddRule(rule ReactorRule) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rules = append(r.rules, rule)
}

// Start registers the reactor as a bus event handler.
// Matching rules are dispatched in goroutines to avoid blocking the bus pipeline.
func (r *Reactor) Start() {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	ruleCount := len(r.rules)
	r.mu.Unlock()

	r.manager.AddEventHandler("reactor", func(busID string, block *CogBlock) {
		r.mu.Lock()
		rules := make([]ReactorRule, len(r.rules))
		copy(rules, r.rules)
		r.mu.Unlock()

		for _, rule := range rules {
			if block.Type != rule.EventType {
				continue
			}
			if rule.BusFilter != "" && busID != rule.BusFilter {
				continue
			}
			ruleName := rule.Name
			action := rule.Action
			go func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Printf("[reactor] rule %q panicked: %v", ruleName, rec)
					}
				}()
				action(block)
			}()
		}
	})

	log.Printf("[reactor] started with %d rules", ruleCount)
}

// Stop removes the reactor's event handler from the bus.
func (r *Reactor) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return
	}
	r.running = false
	r.manager.RemoveEventHandler("reactor")
	log.Printf("[reactor] stopped")
}
