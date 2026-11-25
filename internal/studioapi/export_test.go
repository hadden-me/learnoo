package studioapi

// SetTestHookBeforeRecord installs a hook that runs inside the upload's
// recording transaction, before the permission recheck. It returns a restore
// function.
func SetTestHookBeforeRecord(f func()) (restore func()) {
	old := testHookBeforeRecord
	testHookBeforeRecord = f
	return func() { testHookBeforeRecord = old }
}
