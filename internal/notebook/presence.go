package notebook

type liveFocus struct {
	Block    *Ref `json:"block,omitempty"`
	Database *Ref `json:"database,omitempty"`
	Row      *Ref `json:"row,omitempty"`
	Property *Ref `json:"property,omitempty"`
}

type livePresence struct {
	Label    string     `json:"label"`
	Verified bool       `json:"verified"`
	Page     ID         `json:"page"`
	Edit     *liveFocus `json:"edit"`
}

func (c *liveConn) setPresence(data []byte) error {
	var wire struct {
		Type string     `json:"type"`
		Page ID         `json:"page"`
		Edit *liveFocus `json:"edit"`
	}
	if err := strictDecode(data, &wire); err != nil {
		return err
	}
	if err := wire.Page.ValidateKind(PageKind); err != nil {
		return err
	}
	if e := wire.Edit; e != nil {
		if e.Block != nil {
			if e.Database != nil || e.Row != nil || e.Property != nil {
				return invalidRead("presence")
			}
			if err := e.Block.ValidateKind(BlockKind); err != nil {
				return err
			}
		} else {
			if e.Database == nil || e.Row == nil || e.Property == nil {
				return invalidRead("presence")
			}
			if err := e.Database.ValidateKind(DatabaseKind); err != nil {
				return err
			}
			if err := e.Row.ValidateKind(RowKind); err != nil {
				return err
			}
			if err := e.Property.ValidateKind(PropertyKind); err != nil {
				return err
			}
		}
	}
	c.in.live.mu.Lock()
	c.presence = livePresence{Label: c.session.Label, Verified: c.session.Verified, Page: wire.Page, Edit: wire.Edit}
	c.in.live.mu.Unlock()
	c.in.broadcastPresence()
	return nil
}

func (in *Instance) broadcastPresence() {
	in.live.mu.Lock()
	defer in.live.mu.Unlock()
	for c := range in.live.conns {
		if !c.public {
			c.presencePending = true
			select {
			case c.wake <- struct{}{}:
			default:
			}
		}
	}
}

func (c *liveConn) takePresence() []byte {
	c.in.live.mu.Lock()
	defer c.in.live.mu.Unlock()
	if c.public || !c.presencePending {
		return nil
	}
	c.presencePending = false
	editors := []livePresence{}
	for other := range c.in.live.conns {
		if other != c && !other.public && other.welcomed && other.presence.Page != "" {
			editors = append(editors, other.presence)
		}
	}
	data, _ := liveEnvelope("presence", map[string]any{"editors": editors})
	return data
}
