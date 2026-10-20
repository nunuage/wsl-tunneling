//go:build !windows

package gvisor

func consoleFreeProxy(path string) (string, error) { return path, nil }