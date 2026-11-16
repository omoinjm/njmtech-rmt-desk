//go:build !darwin

package main

import "errors"

func openPrivacyPane(string) error { return errors.New("only needed on macOS") }
