package core

import (
	"path/filepath"
	"sync"

	"github.com/gosuda/flats/internal/notebook"
)

var notebookSpillOwners = struct {
	sync.Mutex
	services map[*Service]*notebook.SpillManager
}{services: map[*Service]*notebook.SpillManager{}}

func (s *Service) notebookSpills() *notebook.SpillManager {
	notebookSpillOwners.Lock()
	defer notebookSpillOwners.Unlock()
	if manager := notebookSpillOwners.services[s]; manager != nil {
		return manager
	}
	manager := notebook.NewSpillManager(filepath.Join(s.cfg.DataDir, "cache", "notebook-spills"), 0, s.now)
	notebookSpillOwners.services[s] = manager
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		<-s.stop
		if err := manager.Close(); err != nil {
			s.logf("notebook scratch cleanup: %v", err)
		}
		notebookSpillOwners.Lock()
		delete(notebookSpillOwners.services, s)
		notebookSpillOwners.Unlock()
	}()
	return manager
}
