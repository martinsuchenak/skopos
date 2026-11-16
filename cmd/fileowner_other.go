//go:build !unix

package cmd

// otherOwner: only Unix runs the worker as a service user.
func otherOwner(string) (int, bool) { return 0, false }
