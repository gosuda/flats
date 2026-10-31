package core

// Withdrawal must also remove handles whose socket has already disconnected.
func (s *Service) withdrawNotebookPublic(slugName string) {
	if in := s.liveNotebook(slugName); in != nil {
		in.CancelPublic()
	}
}
