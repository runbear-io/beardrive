package store

// Quarantine is the per-mount list of local contents the scan must never
// journal: path → sha256 of bytes a local writer refused to commit (the
// local MCP's compare-and-swap, when it could not put the other version
// back). It lives in the volume store, not in file permissions, so it holds
// across restarts and for any user, root included. The scan skips a path
// only while its content still hashes to the recorded sha; any other content
// is ordinary local work and syncs, which also ends the entry.

func (s *Store) quarantinePath(mountID string) (string, error) {
	return s.mountStatePath("quarantine-", mountID)
}

// LoadQuarantine reads the mount's quarantine; a missing file is empty.
func (s *Store) LoadQuarantine(mountID string) (map[string]string, error) {
	p, err := s.quarantinePath(mountID)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if err := readJSON(p, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SaveQuarantine writes it atomically. Callers hold the volume flock, which
// is what orders it against a scan.
func (s *Store) SaveQuarantine(mountID string, q map[string]string) error {
	p, err := s.quarantinePath(mountID)
	if err != nil {
		return err
	}
	return WriteJSONAtomic(p, q)
}
