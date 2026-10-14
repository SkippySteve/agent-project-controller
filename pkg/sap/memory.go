package sap

import (
	"context"
	"errors"
	"maps"
	"sync"
)

// Memory is an in-memory catalog for tests and the local prototype.
type Memory struct {
	mu    sync.Mutex
	items map[string]Sandbox
}

// Put stores a sandbox. ID is set to Name.
func (m *Memory) Put(sandbox Sandbox) error {
	if !ValidLabel(sandbox.Namespace) || !ValidLabel(sandbox.Name) {
		return errors.New("invalid sandbox name")
	}
	sandbox.ID = sandbox.Name
	sandbox.Labels = maps.Clone(sandbox.Labels)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.items == nil {
		m.items = map[string]Sandbox{}
	}
	m.items[sandbox.Namespace+"/"+sandbox.Name] = sandbox
	return nil
}

// List implements Catalog.
func (m *Memory) List(context.Context) ([]Sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	items := make([]Sandbox, 0, len(m.items))
	for _, item := range m.items {
		item.Labels = maps.Clone(item.Labels)
		items = append(items, item)
	}
	return items, nil
}

// Get implements Catalog.
func (m *Memory) Get(_ context.Context, namespace, name string) (Sandbox, error) {
	if !ValidLabel(namespace) || !ValidLabel(name) {
		return Sandbox{}, ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.items[namespace+"/"+name]
	if !ok {
		return Sandbox{}, ErrNotFound
	}
	item.Labels = maps.Clone(item.Labels)
	return item, nil
}
