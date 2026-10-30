package core

import (
	"context"
	"sync"
)

// runtimeGenerations tracks the newest runtime generation per data
// directory: crash recovery goes only to the newest owner.
type runtimeGenerations struct {
	mu     sync.Mutex
	owners map[string]int64 // data directory -> newest runtime start
}

// runtimeGeneration orders starts and gives crash recovery only to the newest
// instance using a data directory. A draining worker must not restart with a
// newer generation and thereby make its obsolete source authoritative again.
func (s *Service) runtimeGeneration(ctx context.Context, dataDir string) (int64, func(context.Context) (int64, error), func(), error) {
	s.runtime.mu.Lock()
	defer s.runtime.mu.Unlock()
	generation, err := s.st.NextRuntimeGeneration(ctx)
	if err != nil {
		return 0, nil, nil, err
	}
	previous := s.runtime.owners[dataDir]
	s.runtime.owners[dataDir] = generation
	recover := func(ctx context.Context) (int64, error) {
		s.runtime.mu.Lock()
		defer s.runtime.mu.Unlock()
		if s.runtime.owners[dataDir] != generation {
			return 0, ErrRuntimeUnavailable
		}
		return s.st.NextRuntimeGeneration(ctx)
	}
	undo := func() {
		s.runtime.mu.Lock()
		defer s.runtime.mu.Unlock()
		if s.runtime.owners[dataDir] == generation {
			if previous == 0 {
				delete(s.runtime.owners, dataDir)
			} else {
				s.runtime.owners[dataDir] = previous
			}
		}
	}
	return generation, recover, undo, nil
}

func (s *Service) retireRuntime(dataDir string) {
	s.runtime.mu.Lock()
	defer s.runtime.mu.Unlock()
	delete(s.runtime.owners, dataDir)
}

type runtimeInstance struct {
	Instance
	once    sync.Once
	release func()
}

func (i *runtimeInstance) Stop() { i.once.Do(func() { i.Instance.Stop(); i.release() }) }

// WorkerState forwards the runtime's diagnostic worker state ("" when the
// runtime does not report one). managedDocsRuntime promotes it.
func (i *runtimeInstance) WorkerState() WorkerState {
	if r, ok := i.Instance.(StateReporter); ok {
		return r.WorkerState()
	}
	return ""
}

var _ StateReporter = (*runtimeInstance)(nil)
var _ StateReporter = (*managedDocsRuntime)(nil)
