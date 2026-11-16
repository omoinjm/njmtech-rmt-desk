//go:build !windows

package main

import "errors"

func relaunchElevated(...string) error {
	return errors.New("elevation is only used on Windows")
}

func messageBox(string, string) {}
