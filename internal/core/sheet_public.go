package core

// withdrawSheetPublic closes the Public connections and responses of a
// sheet whose Public route was revoked.
func (s *Service) withdrawSheetPublic(slugName string) {
	if in := s.liveSheet(slugName); in != nil {
		in.WithdrawPublic()
	}
}
