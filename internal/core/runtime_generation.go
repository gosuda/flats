package core

import (
	"context"
	"sync"
)

// runtimeGeneration orders starts and gives crash recovery only to the newest
// instance using a data directory. A draining worker must not restart with a
// newer generation and thereby make its obsolete source authoritative again.
func (s *Service) runtimeGeneration(ctx context.Context, dataDir string) (int64, func(context.Context) (int64, error), func(), error) {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	generation, err := s.st.NextRuntimeGeneration(ctx)
	if err != nil {
		return 0, nil, nil, err
	}
	previous := s.runtimeOwners[dataDir]
	s.runtimeOwners[dataDir] = generation
	recover := func(ctx context.Context) (int64, error) {
		s.runtimeMu.Lock()
		defer s.runtimeMu.Unlock()
		if s.runtimeOwners[dataDir] != generation {
			return 0, ErrRuntimeUnavailable
		}
		return s.st.NextRuntimeGeneration(ctx)
	}
	undo := func() {
		s.runtimeMu.Lock()
		defer s.runtimeMu.Unlock()
		if s.runtimeOwners[dataDir] == generation {
			if previous == 0 {
				delete(s.runtimeOwners, dataDir)
			} else {
				s.runtimeOwners[dataDir] = previous
			}
		}
	}
	return generation, recover, undo, nil
}

func (s *Service) retireRuntime(dataDir string) {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	delete(s.runtimeOwners, dataDir)
}

type runtimeInstance struct {
	Instance
	once    sync.Once
	release func()
}

func (i *runtimeInstance) Stop() { i.once.Do(func() { i.Instance.Stop(); i.release() }) }
