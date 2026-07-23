// Package agenthealth routes agent availability observations to their
// authoritative store. Globally registered agents share health through the
// default coordination database; board-only agents keep board-local health.
package agenthealth

import (
	"context"
	"errors"
	"fmt"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

// Router resolves the authoritative health store for one board operation.
type Router struct {
	manager *boards.Manager
	local   *store.Store
	global  *store.Store
}

func New(manager *boards.Manager, local *store.Store) Router {
	return Router{manager: manager, local: local}
}

// NewWithGlobal reuses an already-open coordination store for callers that
// need several health lookups in one bounded operation.
func NewWithGlobal(manager *boards.Manager, local, global *store.Store) Router {
	return Router{manager: manager, local: local, global: global}
}

// Get returns health from the shared coordination store for a global agent,
// or from the board database for a board-local agent.
func (r Router) Get(ctx context.Context, agentID string, global bool) (model.AgentHealth, error) {
	target, closeTarget, err := r.openTarget(ctx, global)
	if err != nil {
		return model.AgentHealth{}, err
	}
	value, getErr := target.GetAgentHealth(ctx, agentID)
	return value, errors.Join(getErr, closeTarget())
}

// Set records health in the shared coordination store for a global agent, or
// in the board database for a board-local agent.
func (r Router) Set(ctx context.Context, input store.SetAgentHealthInput, global bool) (model.AgentHealth, error) {
	target, closeTarget, err := r.openTarget(ctx, global)
	if err != nil {
		return model.AgentHealth{}, err
	}
	value, setErr := target.SetAgentHealth(ctx, input)
	return value, errors.Join(setErr, closeTarget())
}

// Begin reserves the causal generation for a health check in its authoritative
// store. The returned observation must be routed with the same global flag.
func (r Router) Begin(ctx context.Context, agentID string, global bool) (store.AgentHealthObservation, error) {
	target, closeTarget, err := r.openTarget(ctx, global)
	if err != nil {
		return store.AgentHealthObservation{}, err
	}
	observation, beginErr := target.BeginAgentHealthObservation(ctx, agentID)
	return observation, errors.Join(beginErr, closeTarget())
}

// Apply records an observed result if a later-started check has not already
// updated the authoritative store.
func (r Router) Apply(
	ctx context.Context,
	observation store.AgentHealthObservation,
	input store.SetAgentHealthInput,
	global bool,
) (store.AgentHealthUpdate, error) {
	target, closeTarget, err := r.openTarget(ctx, global)
	if err != nil {
		return store.AgentHealthUpdate{}, err
	}
	update, applyErr := target.ApplyAgentHealthObservation(ctx, observation, input)
	return update, errors.Join(applyErr, closeTarget())
}

// WorkerRecord reports a non-authoritative local audit failure separately from
// the authoritative health write.
type WorkerRecord struct {
	AuditError error
	Applied    bool
}

// RecordWorker keeps the run reference when the default board and authoritative
// health share a database. For a non-default board it omits the run ID from the
// global observation because that run cannot satisfy the coordination
// database's foreign key, then tries to keep the linkage in a local audit
// observation. An audit failure must not undo an authoritative global
// observation or reclaim an otherwise valid worker result.
func (r Router) RecordWorker(ctx context.Context, input store.SetAgentHealthInput, global bool) (WorkerRecord, error) {
	if !global {
		_, err := r.Set(ctx, input, false)
		return WorkerRecord{Applied: err == nil}, err
	}
	if r.local != nil && r.local.Board() == "default" {
		_, err := r.Set(ctx, input, true)
		return WorkerRecord{Applied: err == nil}, err
	}
	globalInput := input
	globalInput.LastRunID = nil
	_, globalErr := r.Set(ctx, globalInput, true)
	if r.local == nil {
		return WorkerRecord{Applied: globalErr == nil}, globalErr
	}
	_, localErr := r.local.SetAgentHealth(ctx, input)
	return WorkerRecord{AuditError: localErr, Applied: globalErr == nil}, globalErr
}

// RecordWorkerObservation applies an invocation result in causal start order.
// A stale authoritative result is not copied into the board-local audit view.
func (r Router) RecordWorkerObservation(
	ctx context.Context,
	observation store.AgentHealthObservation,
	input store.SetAgentHealthInput,
	global bool,
) (WorkerRecord, error) {
	if !global {
		update, err := r.Apply(ctx, observation, input, false)
		return WorkerRecord{Applied: update.Applied}, err
	}
	if r.local != nil && r.local.Board() == "default" {
		update, err := r.Apply(ctx, observation, input, true)
		return WorkerRecord{Applied: update.Applied}, err
	}
	globalInput := input
	globalInput.LastRunID = nil
	update, globalErr := r.Apply(ctx, observation, globalInput, true)
	if globalErr != nil || !update.Applied || r.local == nil {
		return WorkerRecord{Applied: update.Applied}, globalErr
	}
	_, localErr := r.local.SetAgentHealth(ctx, input)
	return WorkerRecord{AuditError: localErr, Applied: true}, nil
}

func (r Router) openTarget(ctx context.Context, global bool) (*store.Store, func() error, error) {
	if r.local == nil {
		return nil, nil, errors.New("agent health requires a board store")
	}
	if !global || r.local.Board() == "default" {
		return r.local, func() error { return nil }, nil
	}
	if r.global != nil {
		return r.global, func() error { return nil }, nil
	}
	if r.manager == nil {
		return nil, nil, fmt.Errorf("global agent health for board %s requires a board manager", r.local.Board())
	}
	opened, err := r.manager.OpenCoordinationStore(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("open global agent health store: %w", err)
	}
	return opened, opened.Close, nil
}
