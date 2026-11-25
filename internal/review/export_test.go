package review

import "github.com/jackc/pgx/v5"

// SetTestHooks installs hooks that run inside a decision or publication
// transaction just before and just after the in-transaction permission
// check. Either may be nil. It returns a restore function.
func SetTestHooks(before func(), after func(tx pgx.Tx)) (restore func()) {
	oldBefore, oldAfter := testHookBeforeCheck, testHookAfterCheck
	testHookBeforeCheck, testHookAfterCheck = before, after
	return func() { testHookBeforeCheck, testHookAfterCheck = oldBefore, oldAfter }
}
