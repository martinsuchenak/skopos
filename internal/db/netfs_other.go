//go:build !linux && !darwin

package db

func networkFS(string) string { return "" }
