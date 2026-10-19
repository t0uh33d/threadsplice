package client

// MachineMemberships returns the identities a session member can join under:
// the top-level identities in the room named room, with the local name alias
// (either empty: any), narrowed to those marked Machine when any of them is.
// Files from before session members aren't marked, and neither are the fixed
// identities join -as makes for one agent (Codex, Copilot, a bridge), so they
// count only when no candidate is. Identities that can't be read are left out.
func MachineMemberships(room, alias string) ([]Identity, error) {
	names, err := List()
	if err != nil {
		return nil, err
	}
	var all, marked []Identity
	for _, n := range names {
		if alias != "" && n != alias {
			continue
		}
		id, err := Load(n)
		if err != nil || (room != "" && id.RoomName != room) {
			continue
		}
		all = append(all, id)
		if id.Machine {
			marked = append(marked, id)
		}
	}
	if len(marked) > 0 {
		return marked, nil
	}
	return all, nil
}
