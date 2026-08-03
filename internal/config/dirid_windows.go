package config

// Windows exposes no stable directory identity through os.FileInfo (os.SameFile
// uses one internally but does not export it), so the registry records none and
// the caller keeps the conservative answer: a folder claiming an enrolled
// mount's id does not take its row while the recorded path still holds that
// mount's config.
func dirID(string) (uint64, uint64) { return 0, 0 }
